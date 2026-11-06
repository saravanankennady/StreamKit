package routes

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/monitor"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
)

// grantList reaches every room in the project, so it is not something an admin of one room has. The reference draws the
// same line in EnsureListPermission.
func grantList(v *auth.VideoGrant) bool   { return v.RoomList }
func grantCreate(v *auth.VideoGrant) bool { return v.RoomCreate }

func grantAdmin(c *reqCtx, roomName string) error {
	return needTokenGrant(c, func(v *auth.VideoGrant) bool { return v.RoomAdmin && v.Room == roomName })
}

func (s *Server) roomStat(projectID, name string) (monitor.RoomStat, bool) {
	for _, stat := range s.monitor.Project(projectID).Rooms {
		if stat.Name == name {
			return stat, true
		}
	}
	return monitor.RoomStat{}, false
}

// roomView is an open room. The monitor can lag a room created a moment ago, so this reads the room itself.
func (s *Server) roomView(x *room.Room) monitor.RoomStat {
	info := x.ToProto()
	stat, ok := s.roomStat(x.ProjectID(), x.Name())
	if !ok {
		stat = monitor.RoomStat{
			SID: x.SID(), Name: x.Name(), CreatedAt: info.GetCreationTime(), DurationSeconds: time.Now().Unix() - info.GetCreationTime(),
		}
	}
	stat.State = string(x.State())
	stat.Metadata = info.GetMetadata()
	stat.Participants = int(info.GetNumParticipants())
	stat.MaxParticipants = info.GetMaxParticipants()
	stat.EmptyTimeout = info.GetEmptyTimeout()
	stat.DepartureTimeout = info.GetDepartureTimeout()
	stat.ActiveRecording = info.GetActiveRecording()
	return stat
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantList); err != nil {
		return err
	}
	rooms := s.rooms.List(c.projectID(), nil)
	slices.SortFunc(rooms, func(a, b *room.Room) int { return strings.Compare(a.Name(), b.Name()) })
	items := make([]monitor.RoomStat, 0, len(rooms))
	for _, x := range rooms {
		if st := x.State(); st == room.StateClosed || st == room.StateClosing {
			continue
		}
		items = append(items, s.roomView(x))
	}
	// On a cluster, other nodes' rooms come from their shared stats.
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.SID] = true
	}
	for _, stat := range s.monitor.View(c.projectID()).Rooms {
		if !seen[stat.SID] {
			items = append(items, stat)
		}
	}
	slices.SortFunc(items, func(a, b monitor.RoomStat) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantCreate); err != nil {
		return err
	}
	var in schemas.CreateRoom
	if err := decode(w, r, &in); err != nil {
		return err
	}
	x, created := s.rooms.GetOrCreate(c.projectID(), in.Name, room.CreateOptions{
		EmptyTimeout:     in.EmptyTimeout,
		DepartureTimeout: in.DepartureTimeout,
		MaxParticipants:  in.MaxParticipants,
		Metadata:         in.Metadata,
	})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.audit(r, c, "room.created", in.Name, nil)
	}
	writeJSON(w, status, s.roomView(x))
	return nil
}

func (s *Server) roomHistory(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantList); err != nil {
		return err
	}
	var before *time.Time
	if v, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("cursor")); err == nil {
		before = &v
	}
	limit := queryInt(r, "limit", defaultPage, maxPage)
	items, err := s.store.ListRoomHistory(r.Context(), c.projectID(), limit, before)
	if err != nil {
		return err
	}
	next := ""
	if len(items) == limit {
		next = items[len(items)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	return nil
}

func (s *Server) openRoom(c *reqCtx, name string) (*room.Room, error) {
	x := s.rooms.Get(c.projectID(), name)
	if x == nil || x.State() == room.StateClosed || x.State() == room.StateClosing {
		return nil, errorf(http.StatusNotFound, "room_not_found", "That room is not open.")
	}
	return x, nil
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	name := r.PathValue("name")
	if err := needTokenGrant(c, func(v *auth.VideoGrant) bool { return v.RoomList || (v.RoomAdmin && v.Room == name) }); err != nil {
		return err
	}
	x, err := s.openRoom(c, name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.roomView(x))
	return nil
}

func (s *Server) updateRoom(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	name := r.PathValue("name")
	if err := grantAdmin(c, name); err != nil {
		return err
	}
	var in schemas.UpdateRoom
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if limit := s.cfg.Limits.MaxMetadataBytes; limit > 0 && len(in.Metadata) > limit {
		return errorf(http.StatusUnprocessableEntity, "metadata_too_large", "That metadata is too large.")
	}
	x, err := s.openRoom(c, name)
	if err != nil {
		return err
	}
	x.UpdateMetadata(in.Metadata)
	s.audit(r, c, "room.updated", name, nil)
	writeJSON(w, http.StatusOK, s.roomView(x))
	return nil
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantCreate); err != nil {
		return err
	}
	name := r.PathValue("name")
	if err := s.rooms.Delete(c.projectID(), name); err != nil {
		if errors.Is(err, room.ErrRoomNotFound) {
			return errorf(http.StatusNotFound, "room_not_found", "That room is not open.")
		}
		return err
	}
	s.audit(r, c, "room.deleted", name, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// participantStats returns the monitor's view of a project's participants in one room. A participant who
// joined within the last second is added from the room itself, with no media figures yet.
func (s *Server) participantStats(c *reqCtx, x *room.Room) []monitor.ParticipantStat {
	stats := s.monitor.Project(c.projectID()).ParticipantStats
	bySID := map[string]monitor.ParticipantStat{}
	for _, p := range stats {
		bySID[p.SID] = p
	}
	members := x.Participants()
	out := make([]monitor.ParticipantStat, 0, len(members))
	for _, p := range members {
		if stat, ok := bySID[p.SID()]; ok {
			stat.State = p.State().String()
			out = append(out, stat)
			continue
		}
		info := p.ToProto()
		out = append(out, monitor.ParticipantStat{
			SID: p.SID(), Identity: p.Identity(), Name: info.GetName(), Room: x.Name(), State: info.GetState().String(),
			Kind: info.GetKind().String(), JoinedAt: info.GetJoinedAt(), Quality: "UNKNOWN", Permission: info.GetPermission(), Tracks: []monitor.TrackStat{},
		})
	}
	slices.SortFunc(out, func(a, b monitor.ParticipantStat) int { return strings.Compare(a.Identity, b.Identity) })
	return out
}

func (s *Server) listParticipants(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	name := r.URL.Query().Get("room")
	if name == "" {
		if err := needTokenGrant(c, func(v *auth.VideoGrant) bool { return v.RoomAdmin }); err != nil {
			return err
		}
		if c.token != nil {
			return errorf(http.StatusBadRequest, "room_required", "Name a room with the room parameter.")
		}
		stats := s.monitor.View(c.projectID()).ParticipantStats
		writeJSON(w, http.StatusOK, map[string]any{"items": stats})
		return nil
	}
	if err := grantAdmin(c, name); err != nil {
		return err
	}
	x, err := s.openRoom(c, name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.participantStats(c, x)})
	return nil
}

func (s *Server) findParticipant(r *http.Request, c *reqCtx) (*room.Room, *participant.Participant, error) {
	name := r.PathValue("room")
	if err := grantAdmin(c, name); err != nil {
		return nil, nil, err
	}
	x, err := s.openRoom(c, name)
	if err != nil {
		return nil, nil, err
	}
	p := x.Participant(r.PathValue("identity"))
	if p == nil {
		return nil, nil, errorf(http.StatusNotFound, "participant_not_found", "That participant is not in the room.")
	}
	return x, p, nil
}

func (s *Server) getParticipant(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	x, p, err := s.findParticipant(r, c)
	if err != nil {
		return err
	}
	for _, stat := range s.participantStats(c, x) {
		if stat.SID == p.SID() {
			writeJSON(w, http.StatusOK, stat)
			return nil
		}
	}
	return errNotFound
}

func (s *Server) updateParticipant(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	x, p, err := s.findParticipant(r, c)
	if err != nil {
		return err
	}
	var in schemas.UpdateParticipant
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p.UpdateInfo(in.Metadata, in.Name, in.Attributes, permissionToProto(in.Permission))
	s.audit(r, c, "participant.updated", x.Name()+"/"+p.Identity(), in)
	writeJSON(w, http.StatusOK, p.ToProto())
	return nil
}

// removeParticipant also works for someone a share link let in who is not connected right now, because their token is
// still good. Nobody else can be named here without being connected.
func (s *Server) removeParticipant(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	name, identity := r.PathValue("room"), r.PathValue("identity")
	if err := grantAdmin(c, name); err != nil {
		return err
	}
	if err := s.checkRoomIsHere(r.Context(), c.projectID(), name); err != nil {
		return err
	}
	member, err := s.store.RemoveMeetingMember(r.Context(), c.projectID(), name, identity)
	if err != nil {
		return err
	}
	if !s.rooms.Remove(c.projectID(), name, identity) && !member {
		return errorf(http.StatusNotFound, "participant_not_found", "That participant is not in the room.")
	}
	s.audit(r, c, "participant.removed", name+"/"+identity, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listTracks(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	name := r.URL.Query().Get("room")
	if err := grantAdmin(c, name); err != nil {
		return err
	}
	if name == "" {
		return errorf(http.StatusBadRequest, "room_required", "Name a room with the room parameter.")
	}
	x, err := s.openRoom(c, name)
	if err != nil {
		return err
	}
	type trackView struct {
		monitor.TrackStat
		Identity string `json:"identity"`
	}
	items := []trackView{}
	for _, p := range s.participantStats(c, x) {
		for _, t := range p.Tracks {
			items = append(items, trackView{t, p.Identity})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) muteTrack(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	x, p, err := s.findParticipant(r, c)
	if err != nil {
		return err
	}
	var in schemas.MuteTrack
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if !in.Muted && !s.cfg.Room.EnableRemoteUnmute {
		return errorf(http.StatusPreconditionFailed, "remote_unmute_disabled", "Turning on someone's track from here is disabled on this server. Ask them to unmute.")
	}
	info, err := p.SetTrackMuted(r.PathValue("sid"), in.Muted)
	if errors.Is(err, participant.ErrTrackNotFound) {
		return errorf(http.StatusNotFound, "track_not_found", "That track is not published.")
	}
	if err != nil {
		return err
	}
	s.audit(r, c, "track.muted", x.Name()+"/"+p.Identity()+"/"+r.PathValue("sid"), map[string]bool{"muted": in.Muted})
	writeJSON(w, http.StatusOK, map[string]any{"sid": info.GetSid(), "muted": info.GetMuted()})
	return nil
}
