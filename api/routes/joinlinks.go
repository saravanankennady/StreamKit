package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

const (
	defaultJoinLinkTTL = 24 * time.Hour
	// guestTokenTTL only has to last until the browser connects. After that the server keeps refreshing it.
	guestTokenTTL    = 10 * time.Minute
	guestDefaultName = "Guest"
	// meetingJoinPath is where the meeting app takes a code. The code goes in the fragment, which browsers do
	// not send to servers, so it stays out of access logs and link previews.
	meetingJoinPath = "/meet/join#"
	// These are well past any real meeting, with everyone reloading the page a few times. Rows from anyone who never got in
	// go after a day, and a host can make another link, so reaching one of these from a flood does not end the meeting.
	maxMembersPerLink    = 5000
	maxMembersPerMeeting = 20000
	// maxWaitingShown is how many people in a waiting room a host is shown at once.
	maxWaitingShown = 100
)

// joinLinkView is a link as project members see it.
type joinLinkView struct {
	store.JoinLink
	URL string `json:"url"`
}

func (s *Server) joinLinkView(l store.JoinLink) joinLinkView {
	return joinLinkView{JoinLink: l, URL: s.dashboardBase() + meetingJoinPath + l.Code}
}

// createJoinLink makes a link. A link mints tokens, so an access token cannot make one, for the same reason it cannot
// mint them: the route is for project members only.
func (s *Server) createJoinLink(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.CreateJoinLink
	if err := decode(w, r, &in); err != nil {
		return err
	}
	// Failing here beats handing out a link that can never admit anyone.
	if _, err := s.signingKey(r.Context(), c.project.ID); err != nil {
		return err
	}
	ttl := defaultJoinLinkTTL
	if in.ExpiresInSeconds > 0 {
		ttl = time.Duration(in.ExpiresInSeconds) * time.Second
	}
	link, err := s.store.CreateJoinLink(r.Context(), store.JoinLink{
		ProjectID: c.project.ID, Room: strings.TrimSpace(in.Room), Label: strings.TrimSpace(in.Label),
		CanPublish: in.CanPublish == nil || *in.CanPublish, Role: in.Role, ExpiresAt: time.Now().Add(ttl),
	}, c.user.ID)
	if err != nil {
		return err
	}
	s.audit(r, c, "join_link.created", link.ID, map[string]any{"room": link.Room, "can_publish": link.CanPublish, "role": link.Role, "expires_at": link.ExpiresAt})
	writeJSON(w, http.StatusCreated, s.joinLinkView(link))
	return nil
}

func (s *Server) listJoinLinks(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	links, err := s.store.ListJoinLinks(r.Context(), c.project.ID, r.URL.Query().Get("room"))
	if err != nil {
		return err
	}
	items := make([]joinLinkView, 0, len(links))
	for _, l := range links {
		items = append(items, s.joinLinkView(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) revokeJoinLink(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.store.RevokeJoinLink(r.Context(), c.project.ID, r.PathValue("id")); err != nil {
		return err
	}
	s.audit(r, c, "join_link.revoked", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// stoppedLinkError says why a link no longer works. Only someone holding the code can learn this.
func stoppedLinkError(l store.JoinLink) error {
	if l.RevokedAt != nil {
		return errorf(http.StatusGone, "link_revoked", "This link has been turned off. Ask for a new one.")
	}
	return errorf(http.StatusGone, "link_expired", "This link has expired. Ask for a new one.")
}

var errJoinLinkUnknown = errorf(http.StatusNotFound, "link_not_found", "This link is not valid. Check that you have all of it.")

func (s *Server) findJoinLink(ctx context.Context, code string) (store.JoinLink, error) {
	link, err := s.store.JoinLinkByCode(ctx, code)
	if errors.Is(err, store.ErrNotFound) {
		return link, errJoinLinkUnknown
	}
	return link, err
}

// previewJoinLink tells the meeting app what a link opens, before anyone joins.
func (s *Server) previewJoinLink(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.JoinLinkCode
	if err := decode(w, r, &in); err != nil {
		return err
	}
	link, err := s.findJoinLink(r.Context(), in.Code)
	if err != nil {
		return err
	}
	if link.RevokedAt != nil || !link.ExpiresAt.After(time.Now()) {
		return stoppedLinkError(link)
	}
	meeting, err := s.store.MeetingByRoom(r.Context(), link.ProjectID, link.Room)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room": link.Room, "label": link.Label, "can_publish": link.CanPublish, "expires_at": link.ExpiresAt, "role": link.Role,
		"title": meeting.Title, "locked": meeting.Locked, "waiting_room": meeting.WaitingRoom, "ended": meeting.EndedAt != nil,
	})
	return nil
}

// mintMeetingToken makes the token a share link hands out. It never carries an admin grant, whatever the role: it
// proves who someone is, and what they may do is read from their row when they ask.
func mintMeetingToken(key auth.Key, link store.JoinLink, identity, name, role string) (string, error) {
	canPublish, canSubscribe, canPublishData := link.CanPublish, true, true
	return auth.Mint(key, &auth.Grants{
		Identity:   identity,
		Name:       name,
		Attributes: map[string]string{meetingRoleAttribute: role},
		Video: &auth.VideoGrant{
			RoomJoin: true, Room: link.Room,
			CanPublish: &canPublish, CanSubscribe: &canSubscribe, CanPublishData: &canPublishData,
		},
	}, guestTokenTTL)
}

func joinRowError(err error) error {
	if errors.Is(err, store.ErrTooManyMembers) {
		return errorf(http.StatusTooManyRequests, "too_many_joins", "Too many people have joined with this link. Ask whoever sent it for a new one.")
	}
	return err
}

// joinWithLink swaps a code for an access token to the link's room. The server picks the identity, because joining
// under an identity already in the room disconnects whoever holds it. The grants never go beyond joining that room.
// With a waiting room on, a guest gets a ticket instead of a token, and swaps that at /join/wait once a host says so.
func (s *Server) joinWithLink(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if !s.joinLimit.Allow(c.ip) {
		return errRateLimited
	}
	var in schemas.JoinWithLink
	if err := decode(w, r, &in); err != nil {
		return err
	}
	link, err := s.store.UseJoinLink(r.Context(), in.Code)
	if errors.Is(err, store.ErrNotFound) {
		stopped, findErr := s.findJoinLink(r.Context(), in.Code)
		if findErr != nil {
			return findErr
		}
		return stoppedLinkError(stopped)
	}
	if err != nil {
		return err
	}
	key, err := s.signingKey(r.Context(), link.ProjectID)
	if errors.Is(err, errNoAPIKey) {
		return errorf(http.StatusConflict, "no_api_key", "This meeting cannot be joined right now. Tell the person who sent you the link.")
	}
	if err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = guestDefaultName
	}
	isHost := link.Role == store.RoleHost
	var meeting store.Meeting
	if isHost {
		// A host arriving starts a meeting that had ended.
		if meeting, err = s.store.OpenMeetingForHost(r.Context(), link.ProjectID, link.Room, link.Label); err != nil {
			return err
		}
		s.pushMeetingState(meeting)
	} else {
		meeting, err = s.store.MeetingByRoom(r.Context(), link.ProjectID, link.Room)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if meeting.EndedAt != nil {
			return errMeetingEnded
		}
		if meeting.Locked {
			return errMeetingLocked
		}
	}
	identity := link.Role + "-" + store.RandomToken(9)
	member := store.MeetingMember{ProjectID: link.ProjectID, Room: link.Room, Identity: identity, Name: name, Role: link.Role, State: store.MemberMinted, LinkID: &link.ID}
	limits := s.memberLimits
	if isHost {
		// Whoever can do something about a flood of guests must still be able to get in.
		limits.PerMeeting = 0
	}
	if !isHost && meeting.WaitingRoom {
		member.State = store.MemberWaiting
		ticket := store.RandomToken(18)
		if err := s.store.AddMeetingMember(r.Context(), member, ticket, limits); err != nil {
			return joinRowError(err)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "waiting", "ticket": ticket, "room": link.Room})
		return nil
	}
	if isHost {
		member.State = store.MemberAdmitted
	}
	if err := s.store.AddMeetingMember(r.Context(), member, "", limits); err != nil {
		return joinRowError(err)
	}
	token, err := mintMeetingToken(key, link, identity, name, link.Role)
	if err != nil {
		return err
	}
	if err := s.store.Audit(r.Context(), link.ProjectID, "", "join_link.used", link.ID, c.ip, map[string]any{"room": link.Room, "identity": identity, "name": name, "role": link.Role}); err != nil {
		s.log.Warn("could not write audit log", "action", "join_link.used", "error", err)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "url": s.publicURL(), "room": link.Room, "identity": identity, "role": link.Role})
	return nil
}
