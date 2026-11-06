package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
)

// The attributes a meeting puts on its participants. Only the server can set them: guest tokens do not carry
// canUpdateOwnMetadata, so nobody can give themselves a role, or hand down someone else's.
const (
	meetingRoleAttribute = "meeting.role"
	meetingHandAttribute = "meeting.hand"
	admissionTimeout     = 3 * time.Second
	roomCheckTimeout     = 2 * time.Second
	// recordingFormat and recordingLayout are what the record button in a meeting asks for.
	recordingFormat = "mp4"
	recordingLayout = "grid"
)

// What the room's admission check tells clients. The meeting app reads these to choose its words.
var (
	refusedRemoved = &room.RefusedError{Reason: "you were removed from this meeting"}
	refusedLocked  = &room.RefusedError{Reason: "this meeting is locked"}
	refusedWaiting = &room.RefusedError{Reason: "this meeting now has a waiting room"}
	refusedDenied  = &room.RefusedError{Reason: "the host did not let you in"}
	refusedEnded   = &room.RefusedError{Reason: "this meeting has ended"}
	// Says "turned off" because that is what the link's own page says.
	refusedLinkRevoked = &room.RefusedError{Reason: "the link you came in with has been turned off"}
	refusedNoRow       = &room.RefusedError{Reason: "this invitation is no longer valid"}
	// Fails closed: a door that cannot be checked is a door that stays shut.
	errAdmissionUnavailable = errors.New("could not check whether this meeting can be joined")
)

var (
	errMeetingEnded  = errorf(http.StatusGone, "meeting_ended", "This meeting has ended.")
	errMeetingLocked = errorf(http.StatusLocked, "meeting_locked", "This meeting is locked. Ask the host to unlock it.")
	errRoomElsewhere = errorf(http.StatusServiceUnavailable, "room_unreachable", "Could not reach the server holding this meeting, so nothing has changed. Try again in a moment.")
)

// roleRank orders authority, so a route can name the least it needs.
func roleRank(role string) int {
	switch role {
	case store.RoleHost:
		return 3
	case store.RoleModerator:
		return 2
	case store.RoleGuest:
		return 1
	}
	return 0
}

// meetingMetadata is the room metadata a meeting's participants read, and every one of them gets it. It is for
// showing state, nothing enforces anything from it, and a host can only overwrite what their own meeting displays.
func meetingMetadata(m store.Meeting) string {
	data, err := json.Marshal(map[string]any{"title": m.Title, "locked": m.Locked, "waiting_room": m.WaitingRoom})
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *Server) pushMeetingState(m store.Meeting) {
	if x := s.rooms.Get(m.ProjectID, m.Room); x != nil {
		x.UpdateMetadata(meetingMetadata(m))
	}
}

// Admit implements room.Admission: the last word on whether someone a share link handed a token to may start a
// session. Tokens no link handed out are not the meeting's business: a project's own, and recorders.
func (s *Server) Admit(projectID, roomName string, g *auth.Grants, commit bool) (room.Admitted, error) {
	if g.Attributes[meetingRoleAttribute] == "" {
		return room.Admitted{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), admissionTimeout)
	defer cancel()
	role, err := s.store.CheckMeetingMember(ctx, projectID, roomName, g.Identity, commit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// A token only a link hands out, with no row to go with it. The row may have been purged, and any refusal in it. A
		// door that cannot tell stays shut, as it does below.
		return room.Admitted{}, refusedNoRow
	case errors.Is(err, store.ErrMemberRemoved):
		return room.Admitted{}, refusedRemoved
	case errors.Is(err, store.ErrMemberDenied):
		return room.Admitted{}, refusedDenied
	case errors.Is(err, store.ErrMemberEnded):
		return room.Admitted{}, refusedEnded
	case errors.Is(err, store.ErrMemberRevoked):
		return room.Admitted{}, refusedLinkRevoked
	case errors.Is(err, store.ErrMemberLocked):
		return room.Admitted{}, refusedLocked
	case errors.Is(err, store.ErrMemberWaiting):
		return room.Admitted{}, refusedWaiting
	case err != nil:
		s.log.Error("could not check a meeting member", "room", roomName, "error", err)
		return room.Admitted{}, errAdmissionUnavailable
	}
	if !commit {
		return room.Admitted{}, nil
	}
	// A role can change after a token was issued.
	g.Attributes[meetingRoleAttribute] = role
	admitted := room.Admitted{MaxParticipants: s.meetingParticipantCap(projectID)}
	if m, err := s.store.MeetingByRoom(ctx, projectID, roomName); err == nil {
		admitted.Metadata = meetingMetadata(m)
	}
	return admitted, nil
}

// meetingParticipantCap is the limit for a room a share link opens. It is a default: whatever an operator said about rooms
// in general, or a project about its own, says more. A room a project opened for itself keeps what it was opened with.
func (s *Server) meetingParticipantCap(projectID string) uint32 {
	if project := s.ProjectConfig(projectID).MaxParticipants; project != 0 {
		return project
	}
	if s.cfg.Room.MaxParticipants != 0 {
		return s.cfg.Room.MaxParticipants
	}
	return s.cfg.Room.MeetingMaxParticipants
}

// Removed implements room.Admission: whoever was removed stays out, whichever API removed them.
func (s *Server) Removed(projectID, roomName, identity string) {
	ctx, cancel := context.WithTimeout(context.Background(), admissionTimeout)
	defer cancel()
	if _, err := s.store.RemoveMeetingMember(ctx, projectID, roomName, identity); err != nil {
		s.log.Error("could not record a removal", "room", roomName, "error", err)
	}
}

// meetingMemberOf finds who a call from inside a meeting comes from, by the token they joined with, in whatever state.
func (s *Server) meetingMemberOf(r *http.Request, c *reqCtx) (*store.MeetingMember, error) {
	raw := auth.TokenFromHeader(r.Header.Get("Authorization"))
	if raw == "" {
		return nil, errUnauthenticated
	}
	v, err := auth.Verify(raw, s.keys)
	if err != nil {
		return nil, errorf(http.StatusUnauthorized, "invalid_token", err.Error())
	}
	name := r.PathValue("room")
	if v.Grants.Video.Room != name {
		return nil, errForbidden
	}
	c.token = v
	m, err := s.store.MeetingMemberByIdentity(r.Context(), v.Key.ProjectID, name, v.Grants.Identity)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errForbidden
	}
	return m, err
}

// meetingCaller authenticates someone calling from inside a meeting. The token only says who they are. What they may
// do is read now from their row, which only the server writes, so a removal or a change of role takes effect at once,
// however long the token has left.
func (s *Server) meetingCaller(r *http.Request, c *reqCtx, least string) (*store.MeetingMember, error) {
	m, err := s.meetingMemberOf(r, c)
	if err != nil {
		return nil, err
	}
	if !m.CanAct() || roleRank(m.Role) < roleRank(least) {
		return nil, errForbidden
	}
	return m, nil
}

// createMeeting starts a meeting from the meeting app. Only someone signed in can, on a project they belong to,
// because this mints a host link: anyone who can reach a public server could otherwise run rooms on it. No room
// exists until the first person connects, and each account can only make so many of these.
func (s *Server) createMeeting(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if !s.meetingLimit.Allow(c.user.ID) {
		return errRateLimited
	}
	var in schemas.CreateMeeting
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if _, err := s.signingKey(r.Context(), c.project.ID); err != nil {
		return err
	}
	ttl := defaultJoinLinkTTL
	if in.ExpiresInSeconds > 0 {
		ttl = time.Duration(in.ExpiresInSeconds) * time.Second
	}
	title := strings.TrimSpace(in.Title)
	meeting, err := s.store.CreateMeeting(r.Context(), store.Meeting{
		ProjectID: c.project.ID, Room: "meet-" + store.RandomToken(9), Title: title, WaitingRoom: in.WaitingRoom,
	}, c.user.ID, s.cfg.MeetingsPerProject())
	if errors.Is(err, store.ErrTooManyMeetings) {
		return errorf(http.StatusTooManyRequests, "too_many_meetings", "This project has as many open meetings as it may. End one, or wait for its links to run out.")
	}
	if err != nil {
		return err
	}
	link := store.JoinLink{ProjectID: c.project.ID, Room: meeting.Room, Label: title, CanPublish: true, ExpiresAt: time.Now().Add(ttl)}
	host := link
	host.Role = store.RoleHost
	hostLink, err := s.store.CreateJoinLink(r.Context(), host, c.user.ID)
	if err != nil {
		return err
	}
	guestLink, err := s.store.CreateJoinLink(r.Context(), link, c.user.ID)
	if err != nil {
		return err
	}
	s.audit(r, c, "meeting.created", meeting.Room, map[string]any{"title": title, "waiting_room": in.WaitingRoom})
	writeJSON(w, http.StatusCreated, map[string]any{
		"room": meeting.Room, "title": meeting.Title, "waiting_room": meeting.WaitingRoom, "expires_at": hostLink.ExpiresAt,
		"host_url": s.joinLinkView(hostLink).URL, "guest_url": s.joinLinkView(guestLink).URL,
	})
	return nil
}

// getMeeting is the meeting as this caller may see it. Who is waiting and how to invite people is for those running it.
func (s *Server) getMeeting(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleGuest)
	if err != nil {
		return err
	}
	out := map[string]any{"room": me.Room, "role": me.Role, "identity": me.Identity}
	meeting, err := s.store.MeetingByRoom(r.Context(), me.ProjectID, me.Room)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	out["title"], out["locked"], out["waiting_room"] = meeting.Title, meeting.Locked, meeting.WaitingRoom
	if roleRank(me.Role) >= roleRank(store.RoleModerator) {
		waiting, count, err := s.store.ListWaiting(r.Context(), me.ProjectID, me.Room, maxWaitingShown)
		if err != nil {
			return err
		}
		out["waiting"], out["waiting_count"] = waiting, count
		links, err := s.store.ListJoinLinks(r.Context(), me.ProjectID, me.Room)
		if err != nil {
			return err
		}
		for _, l := range links {
			// Never a host link: it is what people get when they ask for something to share.
			if l.Role == store.RoleGuest && l.CanPublish {
				out["invite_url"] = s.joinLinkView(l).URL
				break
			}
		}
		rec, err := s.store.ActiveRecording(r.Context(), me.ProjectID, me.Room)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if rec != nil {
			out["recording"] = map[string]string{"id": rec.ID, "status": rec.Status}
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) updateMeeting(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleHost)
	if err != nil {
		return err
	}
	var in schemas.UpdateMeeting
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Title != nil {
		trimmed := strings.TrimSpace(*in.Title)
		in.Title = &trimmed
	}
	meeting, err := s.store.UpdateMeeting(r.Context(), me.ProjectID, me.Room, in.Title, in.Locked, in.WaitingRoom)
	if err != nil {
		return err
	}
	s.pushMeetingState(meeting)
	s.audit(r, c, "meeting.updated", me.Room, map[string]any{"by": me.Identity, "locked": meeting.Locked, "waiting_room": meeting.WaitingRoom})
	writeJSON(w, http.StatusOK, meeting)
	return nil
}

// endMeeting ends the meeting for everyone, and every token it handed out with it. Whatever is recording or streaming the
// room stops first, so a recording finishes as a whole file rather than as a recorder that lost its room. That is every
// job, because a job with no room to watch only waits, taking a slot.
func (s *Server) endMeeting(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleHost)
	if err != nil {
		return err
	}
	if err := s.checkRoomIsHere(r.Context(), me.ProjectID, me.Room); err != nil {
		return err
	}
	jobs, err := s.store.ActiveEgressForRoom(r.Context(), me.ProjectID, me.Room)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if _, err := s.stopJob(r.Context(), me.ProjectID, job.ID); err != nil && !errors.Is(err, store.ErrNotRunning) {
			return err
		}
	}
	if err := s.store.EndMeeting(r.Context(), me.ProjectID, me.Room); err != nil {
		return err
	}
	s.audit(r, c, "meeting.ended", me.Room, map[string]any{"by": me.Identity})
	if err := s.rooms.Delete(me.ProjectID, me.Room); err != nil && !errors.Is(err, room.ErrRoomNotFound) {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// checkRoomIsHere is for what cannot honestly report success unless it reached everyone in a room. Requests normally reach
// the node holding it by routing. They can land elsewhere when Redis cannot be read, or the room has just moved, and then
// closing what is here would leave the real room carrying on. With nobody holding the room, there is nothing to miss.
func (s *Server) checkRoomIsHere(ctx context.Context, projectID, room string) error {
	if s.router == nil || s.router.IsLocal() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, roomCheckTimeout)
	defer cancel()
	node, found, err := s.router.Route(ctx, projectID, room, false)
	if err != nil {
		s.log.Error("could not find which node holds a room", "room", room, "error", err)
		return errRoomElsewhere
	}
	if found && node.ID != s.router.SelfID() {
		s.log.Warn("a request about a room reached the wrong node", "room", room, "node", node.ID)
		return errRoomElsewhere
	}
	return nil
}

// muteMicrophone mutes someone's microphone, and reports whether that changed anything. The server stops forwarding it
// as well as asking their client to mute, so a client that ignores the request is not heard either.
func muteMicrophone(p *participant.Participant) bool {
	muted := false
	for _, t := range p.Tracks() {
		if t.Kind() != pb.TrackType_AUDIO || t.Source() != pb.TrackSource_MICROPHONE || t.Muted() {
			continue
		}
		if _, err := p.SetTrackMuted(t.SID(), true); err == nil {
			muted = true
		}
	}
	return muted
}

// muteAll mutes everyone but the caller. People can unmute themselves afterwards, as in other meeting products: a host
// silences a noisy room, and does not decide who may ever speak.
func (s *Server) muteAll(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleModerator)
	if err != nil {
		return err
	}
	x, err := s.openRoom(c, me.Room)
	if err != nil {
		return err
	}
	count := 0
	for _, p := range x.Participants() {
		if p.Identity() != me.Identity && !p.Hidden() && muteMicrophone(p) {
			count++
		}
	}
	s.audit(r, c, "meeting.muted_all", me.Room, map[string]any{"by": me.Identity, "muted": count})
	writeJSON(w, http.StatusOK, map[string]int{"muted": count})
	return nil
}

// targetOf finds who a route is aimed at, in this room. A recorder is hidden, and cannot be aimed at.
func (s *Server) targetOf(r *http.Request, c *reqCtx, me *store.MeetingMember) (*participant.Participant, error) {
	x, err := s.openRoom(c, me.Room)
	if err != nil {
		return nil, err
	}
	p := x.Participant(r.PathValue("identity"))
	if p == nil || p.Hidden() {
		return nil, errorf(http.StatusNotFound, "participant_not_found", "That person is not in the meeting.")
	}
	return p, nil
}

func (s *Server) muteMember(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleModerator)
	if err != nil {
		return err
	}
	p, err := s.targetOf(r, c, me)
	if err != nil {
		return err
	}
	muted := muteMicrophone(p)
	s.audit(r, c, "meeting.muted", me.Room+"/"+p.Identity(), map[string]any{"by": me.Identity})
	writeJSON(w, http.StatusOK, map[string]bool{"muted": muted})
	return nil
}

// removeMember refuses someone's token from then on, and disconnects them if they are connected. Being offline is no way
// to keep a token good for hours. The refusal is written first, so the message that tells them they were removed cannot
// beat it back to the server. The link itself still works, because it does not name a person: a host who needs someone
// to stay out locks the meeting or turns on the waiting room.
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleModerator)
	if err != nil {
		return err
	}
	identity := r.PathValue("identity")
	if identity == me.Identity {
		return errorf(http.StatusUnprocessableEntity, "cannot_remove_self", "Leave the meeting instead.")
	}
	target, err := s.store.MeetingMemberByIdentity(r.Context(), me.ProjectID, me.Room, identity)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// Without a row, the only way to name someone is by their being here. A recorder is hidden, and cannot be aimed at.
	if target == nil {
		var p *participant.Participant
		if x := s.rooms.Get(me.ProjectID, me.Room); x != nil {
			p = x.Participant(identity)
		}
		if p == nil || p.Hidden() {
			return errorf(http.StatusNotFound, "participant_not_found", "That person is not in the meeting.")
		}
	}
	// A moderator keeps order among guests. Nobody can remove a host but another host, and someone who did not come in
	// through a link, such as a project's own service, is for a host to deal with.
	if me.Role != store.RoleHost && (target == nil || roleRank(target.Role) >= roleRank(me.Role)) {
		return errForbidden
	}
	if err := s.checkRoomIsHere(r.Context(), me.ProjectID, me.Room); err != nil {
		return err
	}
	if _, err := s.store.RemoveMeetingMember(r.Context(), me.ProjectID, me.Room, identity); err != nil {
		return err
	}
	s.rooms.Remove(me.ProjectID, me.Room, identity)
	s.audit(r, c, "meeting.removed", me.Room+"/"+identity, map[string]any{"by": me.Identity})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// setMemberRole makes a guest a moderator, or takes that away. Hosts come from host links, and stay hosts.
func (s *Server) setMemberRole(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleHost)
	if err != nil {
		return err
	}
	var in schemas.SetMeetingRole
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p, err := s.targetOf(r, c, me)
	if err != nil {
		return err
	}
	target, err := s.store.MeetingMemberByIdentity(r.Context(), me.ProjectID, me.Room, p.Identity())
	if errors.Is(err, store.ErrNotFound) {
		return errorf(http.StatusUnprocessableEntity, "not_a_member", "This person did not join through a link, so they have no role here.")
	}
	if err != nil {
		return err
	}
	if target.Role == store.RoleHost {
		return errorf(http.StatusUnprocessableEntity, "cannot_change_host", "A host stays a host.")
	}
	if err := s.store.SetMeetingMemberRole(r.Context(), me.ProjectID, me.Room, p.Identity(), in.Role); err != nil {
		return err
	}
	p.UpdateInfo(nil, nil, map[string]string{meetingRoleAttribute: in.Role}, nil)
	s.audit(r, c, "meeting.role_changed", me.Room+"/"+p.Identity(), map[string]any{"by": me.Identity, "role": in.Role})
	writeJSON(w, http.StatusOK, map[string]string{"role": in.Role})
	return nil
}

// setHand raises or lowers the caller's own hand. It is an attribute so people who join later see it. It goes through
// here because the only way for a client to set one itself would also let it rename itself.
func (s *Server) setHand(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleGuest)
	if err != nil {
		return err
	}
	var in schemas.SetHand
	if err := decode(w, r, &in); err != nil {
		return err
	}
	x, err := s.openRoom(c, me.Room)
	if err != nil {
		return err
	}
	p := x.Participant(me.Identity)
	if p == nil {
		return errorf(http.StatusNotFound, "participant_not_found", "You are not connected to the meeting.")
	}
	value := ""
	if in.Raised {
		value = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	p.UpdateInfo(nil, nil, map[string]string{meetingHandAttribute: value}, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"raised": in.Raised})
	return nil
}

func (s *Server) lowerHand(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleModerator)
	if err != nil {
		return err
	}
	p, err := s.targetOf(r, c, me)
	if err != nil {
		return err
	}
	p.UpdateInfo(nil, nil, map[string]string{meetingHandAttribute: ""}, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"raised": false})
	return nil
}

func (s *Server) answerWaiting(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleModerator)
	if err != nil {
		return err
	}
	var in schemas.AnswerWaiting
	if err := decode(w, r, &in); err != nil {
		return err
	}
	identity := r.PathValue("identity")
	if err := s.store.AnswerWaiting(r.Context(), me.ProjectID, me.Room, identity, in.Admit); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errorf(http.StatusNotFound, "not_waiting", "That person is no longer waiting.")
		}
		return err
	}
	s.audit(r, c, "meeting.waiting_answered", me.Room+"/"+identity, map[string]any{"by": me.Identity, "admit": in.Admit})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// turnAwayEveryone clears the waiting room, for when it has filled up with people nobody knows. They can knock again,
// so a host who wants this to last locks the meeting as well.
func (s *Server) turnAwayEveryone(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingCaller(r, c, store.RoleModerator)
	if err != nil {
		return err
	}
	count, err := s.store.TurnAwayWaiting(r.Context(), me.ProjectID, me.Room)
	if err != nil {
		return err
	}
	s.audit(r, c, "meeting.waiting_cleared", me.Room, map[string]any{"by": me.Identity, "turned_away": count})
	writeJSON(w, http.StatusOK, map[string]int64{"turned_away": count})
	return nil
}

// meetingSummary reports what happened, from the room history the server keeps anyway: when it ran, who was there
// under what names, and what was recorded. Anyone who was in the meeting can read it, including once it has ended, and only
// of a session they were in. Holding a token is not being there, and a token can come from a link anyone was sent.
func (s *Server) meetingSummary(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	me, err := s.meetingMemberOf(r, c)
	if err != nil {
		return err
	}
	if !me.CanAct() && me.State != store.MemberEnded {
		return errForbidden
	}
	summary, err := s.store.SummarizeMeeting(r.Context(), me.ProjectID, me.Room, me.Identity)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, summary)
	return nil
}

// isMeetingHost reports whether this token belongs to a host of the named room, as things stand now.
func (s *Server) isMeetingHost(r *http.Request, c *reqCtx, roomName string) (bool, error) {
	if c.token == nil || c.token.Grants.Attributes[meetingRoleAttribute] == "" || c.token.Grants.Video.Room != roomName {
		return false, nil
	}
	m, err := s.store.MeetingMemberByIdentity(r.Context(), c.token.Key.ProjectID, roomName, c.token.Grants.Identity)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.CanAct() && m.Role == store.RoleHost, nil
}

// hostMayStartRecording allows the one job a meeting's record button asks for: a file recording of the whole of the
// host's own room. Hosts do not get the roomRecord grant, which reaches every recording in the project. Stream outputs
// are out too: those send a meeting to an address the host chooses, on the server's bandwidth.
func (s *Server) hostMayStartRecording(r *http.Request, c *reqCtx, in *startEgressBody) (bool, error) {
	if in.Type != "room_composite" || in.Output == nil || len(in.StreamURLs) > 0 {
		return false, nil
	}
	return s.isMeetingHost(r, c, in.Room)
}

// joinWithTicket is how someone in the waiting room asks whether they can come in. The answer can be a token: until
// then they do not have one, so there is nothing to get past a host with.
func (s *Server) joinWithTicket(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.WaitingTicket
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Cancel {
		if err := s.store.LeaveWaiting(r.Context(), in.Ticket); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	m, err := s.store.MemberByTicket(r.Context(), in.Ticket)
	if errors.Is(err, store.ErrNotFound) {
		return errorf(http.StatusNotFound, "ticket_not_found", "This place in the waiting room is no longer valid. Try joining again.")
	}
	if err != nil {
		return err
	}
	meeting, err := s.store.MeetingByRoom(r.Context(), m.ProjectID, m.Room)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	switch {
	case m.State == store.MemberEnded || meeting.EndedAt != nil:
		return errMeetingEnded
	case m.State == store.MemberDenied && meeting.Locked:
		return errMeetingLocked
	case m.State == store.MemberDenied:
		return errorf(http.StatusForbidden, "waiting_denied", "The host did not let you in.")
	// Turning the waiting room off opens the door for whoever is at it.
	case m.State == store.MemberWaiting && (meeting.WaitingRoom || meeting.Locked):
		writeJSON(w, http.StatusOK, map[string]string{"status": "waiting"})
		return nil
	}
	if m.LinkID == nil {
		return errJoinLinkUnknown
	}
	link, err := s.store.JoinLinkByID(r.Context(), *m.LinkID)
	if errors.Is(err, store.ErrNotFound) {
		return errJoinLinkUnknown
	}
	if err != nil {
		return err
	}
	if link.RevokedAt != nil || !link.ExpiresAt.After(time.Now()) {
		return stoppedLinkError(link)
	}
	key, err := s.signingKey(r.Context(), m.ProjectID)
	if errors.Is(err, errNoAPIKey) {
		return errorf(http.StatusConflict, "no_api_key", "This meeting cannot be joined right now. Tell the person who sent you the link.")
	}
	if err != nil {
		return err
	}
	if err := s.store.SpendTicket(r.Context(), in.Ticket); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errorf(http.StatusNotFound, "ticket_not_found", "This place in the waiting room is no longer valid. Try joining again.")
		}
		return err
	}
	token, err := mintMeetingToken(key, link, m.Identity, m.Name, m.Role)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "admitted", "token": token, "url": s.publicURL(), "room": link.Room, "identity": m.Identity, "role": m.Role})
	return nil
}
