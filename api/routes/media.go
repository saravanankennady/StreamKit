package routes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"elnova-streamkit.local/api/middleware"
	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/storage"
	"elnova-streamkit.local/ingress/source"
	"elnova-streamkit.local/ingress/whip"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/room"
)

const (
	maxStreamURLs   = 5
	reapInterval    = 5 * time.Second
	sweepInterval   = 30 * time.Second
	maxMediaNameLen = 256
	// A stream key is 192 random bits, so this is not about guessing one. Each attempt costs the database some work, and this
	// endpoint needs nothing else from whoever calls it.
	whipAttemptsPerMinute = 120
	whipAttemptBurst      = 30
)

// mediaServices holds what the media routes open on first use.
type mediaServices struct {
	mu      sync.Mutex
	storage storage.Storage
}

// recordings returns the storage backend. A failure is not remembered, so storage that was down comes back by itself.
func (s *Server) recordings() (storage.Storage, error) {
	s.media.mu.Lock()
	defer s.media.mu.Unlock()
	if s.media.storage != nil {
		return s.media.storage, nil
	}
	backend, err := storage.New(s.cfg.Media.Storage)
	if err != nil {
		return nil, err
	}
	s.media.storage = backend
	return backend, nil
}

// StartMedia starts what the media services need from the server itself: WHIP, which runs here because the publisher
// connects straight to the SFU, and failing the jobs of workers that have died. Any worker does this too, but with no
// workers left, nothing else would.
func (s *Server) StartMedia(ctx context.Context, mux *http.ServeMux) {
	if _, err := s.recordings(); err != nil {
		s.log.Error("media storage is not available, so recordings cannot be served", "error", err)
	}
	if err := s.store.SealOldSourceURLs(ctx); err != nil {
		s.log.Error("could not seal the addresses of URL inputs saved in the open", "error", err)
	}
	service := whip.New(s.store, s.rooms, auth.StaticKeys{ProjectID: auth.StaticProject, Keys: s.cfg.Keys}, s.log)
	limit := middleware.NewRateLimiter(whipAttemptsPerMinute, whipAttemptBurst)
	service.Allow = func(r *http.Request) bool {
		return limit.Allow(middleware.ClientIP(r, s.cfg.ControlPlane.TrustProxyHeaders))
	}
	service.Start(ctx, mux)
	go s.store.ReapLoop(ctx, reapInterval, s.log, func() {
		if err := s.store.ReapCalls(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("could not check SIP call leases", "error", err)
		}
		s.webhook.Notify()
	})
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepRecordings(ctx)
			}
		}
	}()
}

// sweepRecordings removes what nobody can ask for any more: recordings of deleted projects, and any that have outlived the
// retention setting. Nothing here is urgent, so what cannot be deleted now is tried again on the next pass. A file on a
// backend this server is not using stays, along with its row, so it can still be found.
func (s *Server) sweepRecordings(ctx context.Context) {
	jobs, err := s.store.RecordingsToPurge(ctx, s.cfg.Media.Storage.RetentionDays)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not look for recordings to remove", "error", err)
		}
		return
	}
	backend, err := s.recordings()
	if err != nil {
		return
	}
	for _, e := range jobs {
		if e.OutputPath != "" {
			if e.Storage != backend.Kind() {
				continue
			}
			if err := backend.Delete(ctx, e.OutputPath); err != nil {
				s.log.Warn("could not remove a recording's file", "egress", e.ID, "error", err)
				continue
			}
		}
		if _, err := s.store.DeleteEgress(ctx, e.ProjectID, e.ID); err != nil {
			s.log.Warn("could not remove a recording", "egress", e.ID, "error", err)
		}
	}
	if err := s.store.PurgeDeletedIngresses(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("could not remove ingresses of deleted projects", "error", err)
	}
}

func grantRecord(v *auth.VideoGrant) bool  { return v.RoomRecord }
func grantIngress(v *auth.VideoGrant) bool { return v.IngressAdmin }

func checkMediaName(errs schemas.FieldErrors, field, value string, required bool) {
	if value == "" {
		if required {
			errs[field] = "is required"
		}
		return
	}
	if len(value) > maxMediaNameLen {
		errs[field] = fmt.Sprintf("must be at most %d characters", maxMediaNameLen)
		return
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			errs[field] = "cannot contain control characters"
			return
		}
	}
}

// startEgressBody is the body of POST /egress.
type startEgressBody struct {
	Type        string              `json:"type"`
	Room        string              `json:"room"`
	Participant string              `json:"participant"`
	Layout      string              `json:"layout"`
	Output      *store.EgressOutput `json:"output"`
	StreamURLs  []string            `json:"stream_urls"`
}

// Validate implements Validator.
func (b *startEgressBody) Validate() schemas.FieldErrors {
	errs := schemas.FieldErrors{}
	switch b.Type {
	case "room_composite":
		if b.Participant != "" {
			errs["participant"] = "does not apply to a room composite"
		}
	case "participant":
		if b.Participant == "" {
			errs["participant"] = "is required for a participant recording"
		}
	case "audio_only", "video_only":
	default:
		errs["type"] = "must be room_composite, participant, audio_only or video_only"
	}
	checkMediaName(errs, "room", b.Room, true)
	checkMediaName(errs, "participant", b.Participant, false)
	switch b.Layout {
	case "", "grid", "speaker":
	default:
		errs["layout"] = "must be grid or speaker"
	}
	if b.Output != nil {
		if b.Output.Kind != "file" {
			errs["output"] = "kind must be file"
		}
		switch b.Output.Format {
		case "", "mp4", "webm":
		default:
			errs["output"] = "format must be mp4 or webm"
		}
	}
	if b.Output == nil && len(b.StreamURLs) == 0 {
		errs["output"] = "give an output, stream_urls, or both"
	}
	if len(b.StreamURLs) > maxStreamURLs {
		errs["stream_urls"] = fmt.Sprintf("at most %d", maxStreamURLs)
	}
	return errs
}

func fieldError(field, msg string) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Msg: "Some fields need attention.",
		Fields: schemas.FieldErrors{field: msg}}
}

var (
	errEgressUnavailable = errorf(http.StatusServiceUnavailable, "egress_unavailable", "No recording worker is available. Try again in a moment, or check GET /servers.")
	errRecordingActive   = errorf(http.StatusConflict, "recording_active", "This meeting is already being recorded.")
)

// queueEgress is what starting and retrying have in common. Checks run in the order a caller can act on: no worker
// is a problem with the installation, a missing room is a problem with this request.
func (s *Server) queueEgress(r *http.Request, c *reqCtx, job *store.Egress, streamURLs []string) (*store.Egress, error) {
	available, err := s.store.EgressCapacityAvailable(r.Context())
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, errEgressUnavailable
	}
	job.ProjectID = c.projectID()
	sid, ok := s.findRoom(job.ProjectID, job.RoomName)
	if !ok {
		return nil, errorf(http.StatusNotFound, "room_not_found", "That room is not open.")
	}
	job.RoomSID = sid
	created, err := s.store.AdmitEgress(r.Context(), job, streamURLs, s.cfg.Media.Egress.MaxJobsPerProject)
	if errors.Is(err, store.ErrNoCapacity) {
		return nil, errEgressUnavailable
	}
	if errors.Is(err, store.ErrProjectAtLimit) {
		return nil, errorf(http.StatusTooManyRequests, "project_limit", "This project is running as many recordings as it may. Stop one, or wait for one to end.")
	}
	if err != nil {
		return nil, err
	}
	if err := s.store.RecordEgressEvent(r.Context(), created, "egress_started", "info", "egress_started",
		"Recording requested: "+created.Type, map[string]any{"format": created.Format, "streams": len(streamURLs)}); err != nil {
		s.log.Warn("could not record egress event", "error", err)
	}
	s.webhook.Notify()
	return created, nil
}

// findRoom looks on this node, then at what other nodes report.
func (s *Server) findRoom(projectID, name string) (string, bool) {
	if x := s.rooms.Get(projectID, name); x != nil && x.State() != room.StateClosed && x.State() != room.StateClosing {
		return x.SID(), true
	}
	for _, stat := range s.monitor.View(projectID).Rooms {
		if stat.Name == name {
			return stat.SID, true
		}
	}
	return "", false
}

func (s *Server) startEgress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	grantErr := needTokenGrant(c, grantRecord)
	if grantErr != nil {
		// Without the grant, only a host of their own meeting gets as far as saying what they want.
		host, err := s.isMeetingHost(r, c, c.token.Grants.Video.Room)
		if err != nil {
			return err
		}
		if !host {
			return grantErr
		}
	}
	var in startEgressBody
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if grantErr != nil {
		allowed, err := s.hostMayStartRecording(r, c, &in)
		if err != nil {
			return err
		}
		if !allowed {
			return grantErr
		}
	}
	for _, u := range in.StreamURLs {
		if _, err := source.CheckOutput(r.Context(), u, s.cfg.Media.AllowPrivateAddresses); err != nil {
			return fieldError("stream_urls", err.Error())
		}
	}
	job := &store.Egress{
		RoomName: in.Room, Type: in.Type,
		Request: store.EgressRequest{Type: in.Type, Room: in.Room, Participant: in.Participant, Layout: in.Layout, Output: in.Output},
	}
	if job.Request.Layout == "" {
		job.Request.Layout = "grid"
	}
	if in.Output != nil {
		job.Format = in.Output.Format
		if job.Format == "" {
			job.Format = "mp4"
		}
		job.Request.Output = &store.EgressOutput{Kind: "file", Format: job.Format}
	}
	// The record button starts one recording at a time, whoever else is recording the room. Otherwise a host link could
	// take every slot the installation has. The project's own credentials are limited per project, and can run several.
	if grantErr != nil {
		_, err := s.store.ActiveRecording(r.Context(), c.projectID(), in.Room)
		if err == nil {
			return errRecordingActive
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		job.Request.MeetingRecording = true
	}
	created, err := s.queueEgress(r, c, job, in.StreamURLs)
	if store.IsMeetingRecordingRunning(err) {
		return errRecordingActive
	}
	if err != nil {
		return err
	}
	s.audit(r, c, "egress.started", created.ID, map[string]any{"room": created.RoomName, "type": created.Type})
	writeJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) listEgress(w http.ResponseWriter, r *http.Request, c *reqCtx, onlyFiles bool) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		return err
	}
	items, next, err := s.store.ListEgress(r.Context(), c.projectID(), onlyFiles, r.URL.Query().Get("cursor"), queryInt(r, "limit", defaultPage, maxPage))
	if err != nil {
		return err
	}
	if items == nil {
		items = []*store.Egress{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	return nil
}

func (s *Server) listAllEgress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	return s.listEgress(w, r, c, false)
}

func (s *Server) listRecordings(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	return s.listEgress(w, r, c, true)
}

// recordingFor loads a job with a file output. A job that only streams is not a recording.
func (s *Server) recordingFor(r *http.Request, c *reqCtx) (*store.Egress, error) {
	e, err := s.store.EgressByID(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if e.Format == "" {
		return nil, errNotFound
	}
	return e, nil
}

func (s *Server) getEgress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		return err
	}
	e, err := s.store.EgressByID(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, e)
	return nil
}

func (s *Server) getRecording(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		return err
	}
	e, err := s.recordingFor(r, c)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, e)
	return nil
}

func (s *Server) stopEgress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		// A meeting's host can stop what is recording their own room, and nothing else.
		job, lookup := s.store.EgressByID(r.Context(), c.projectID(), r.PathValue("id"))
		if lookup != nil {
			return err
		}
		if host, hostErr := s.isMeetingHost(r, c, job.RoomName); hostErr != nil || !host {
			return err
		}
	}
	e, err := s.stopJob(r.Context(), c.projectID(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotRunning) {
		return errorf(http.StatusConflict, "egress_not_running", "That job has already ended.")
	}
	if err != nil {
		return err
	}
	s.audit(r, c, "egress.stopped", e.ID, nil)
	writeJSON(w, http.StatusOK, e)
	return nil
}

// stopJob asks a job to stop. A queued job ends here rather than in a worker, so it is reported here too.
func (s *Server) stopJob(ctx context.Context, projectID, id string) (*store.Egress, error) {
	e, err := s.store.RequestEgressStop(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if e.Status == store.EgressAborted {
		if err := s.store.RecordEgressEvent(ctx, e, "egress_ended", "info", "egress_aborted", "Stopped before it started.", nil); err != nil {
			s.log.Warn("could not record egress event", "error", err)
		}
		s.webhook.Notify()
	}
	return e, nil
}

func recordingContentType(e *store.Egress) string {
	kind := "video"
	if e.Type == "audio_only" {
		kind = "audio"
	}
	return kind + "/" + e.Format
}

// recordingFile serves a recording. http.ServeContent answers Range requests, which is what lets a <video> element
// seek. The file is found through the job's row, scoped to the project, and no part of the request names a path.
func (s *Server) recordingFile(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		return err
	}
	e, err := s.recordingFor(r, c)
	if err != nil {
		return err
	}
	if e.OutputPath == "" {
		return errorf(http.StatusNotFound, "recording_not_ready", "This recording has no file yet.")
	}
	backend, err := s.recordings()
	if err != nil {
		return err
	}
	if e.Storage != backend.Kind() {
		return errorf(http.StatusNotFound, "recording_unavailable", "This recording was saved to a storage backend this server is not using.")
	}
	obj, err := backend.Open(r.Context(), e.OutputPath)
	if errors.Is(err, storage.ErrNotFound) {
		return errorf(http.StatusNotFound, "recording_unavailable", "The file for this recording is missing from storage.")
	}
	if err != nil {
		return err
	}
	defer obj.Close()

	h := w.Header()
	h.Set("Content-Type", recordingContentType(e))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-cache")
	disposition := "inline"
	if r.URL.Query().Get("download") != "" {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, e.ID+"."+e.Format))
	http.ServeContent(w, r, path.Base(e.OutputPath), obj.ModTime(), obj)
	return nil
}

func (s *Server) retryRecording(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		return err
	}
	old, err := s.recordingFor(r, c)
	if err != nil {
		return err
	}
	if !store.EgressIsTerminal(old.Status) {
		return errorf(http.StatusConflict, "egress_running", "That job is still running.")
	}
	urls, err := s.store.EgressStreamURLs(old)
	if err != nil {
		return err
	}
	job := &store.Egress{RoomName: old.RoomName, Type: old.Type, Format: old.Format, Request: old.Request, RetryOf: &old.ID}
	// This takes the grant, or being in the project, whoever started the first one.
	job.Request.MeetingRecording = false
	created, err := s.queueEgress(r, c, job, urls)
	if err != nil {
		return err
	}
	s.audit(r, c, "recording.retried", created.ID, map[string]any{"retry_of": old.ID})
	writeJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantRecord); err != nil {
		return err
	}
	e, err := s.recordingFor(r, c)
	if err != nil {
		return err
	}
	if !store.EgressIsTerminal(e.Status) {
		return errorf(http.StatusConflict, "egress_running", "Stop this job before deleting it.")
	}
	// The file goes first: if that fails, the record is still there to try again with.
	if e.OutputPath != "" {
		backend, err := s.recordings()
		if err != nil {
			return err
		}
		// Saying it was deleted while the file is still there would be worse than saying it cannot be done from here.
		if e.Storage != backend.Kind() {
			return errorf(http.StatusConflict, "recording_unavailable", "This recording's file is on a storage backend this server is not using, so it cannot be deleted from here.")
		}
		if err := backend.Delete(r.Context(), e.OutputPath); err != nil {
			return err
		}
	}
	if _, err := s.store.DeleteEgress(r.Context(), c.projectID(), e.ID); err != nil {
		return err
	}
	s.audit(r, c, "recording.deleted", e.ID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// createIngressBody is the body of POST /ingress.
type createIngressBody struct {
	Name                string `json:"name"`
	InputType           string `json:"input_type"`
	Room                string `json:"room"`
	ParticipantIdentity string `json:"participant_identity"`
	ParticipantName     string `json:"participant_name"`
	SourceURL           string `json:"source_url"`
}

// Validate implements Validator.
func (b *createIngressBody) Validate() schemas.FieldErrors {
	errs := schemas.FieldErrors{}
	checkMediaName(errs, "name", b.Name, false)
	checkMediaName(errs, "room", b.Room, true)
	checkMediaName(errs, "participant_identity", b.ParticipantIdentity, true)
	checkMediaName(errs, "participant_name", b.ParticipantName, false)
	switch b.InputType {
	case store.InputRTMP, store.InputWHIP:
		if b.SourceURL != "" {
			errs["source_url"] = "applies only to url input"
		}
	case store.InputURL:
		if b.SourceURL == "" {
			errs["source_url"] = "is required for url input"
		}
	default:
		errs["input_type"] = "must be rtmp, whip or url"
	}
	return errs
}

// updateIngressBody is the body of PATCH /ingress/{id}.
type updateIngressBody struct {
	Name                *string `json:"name"`
	Room                *string `json:"room"`
	ParticipantIdentity *string `json:"participant_identity"`
	ParticipantName     *string `json:"participant_name"`
	Enabled             *bool   `json:"enabled"`
}

// Validate implements Validator.
func (b *updateIngressBody) Validate() schemas.FieldErrors {
	errs := schemas.FieldErrors{}
	if b.Name != nil {
		checkMediaName(errs, "name", *b.Name, false)
	}
	if b.Room != nil {
		checkMediaName(errs, "room", *b.Room, true)
	}
	if b.ParticipantIdentity != nil {
		checkMediaName(errs, "participant_identity", *b.ParticipantIdentity, true)
	}
	if b.ParticipantName != nil {
		checkMediaName(errs, "participant_name", *b.ParticipantName, false)
	}
	return errs
}

// requestHost is the host name a client used to reach this API, without its port.
func requestHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}

// publishURL says where a publisher should send a stream.
func (s *Server) publishURL(r *http.Request, in *store.Ingress) string {
	switch in.InputType {
	case store.InputRTMP:
		if u := s.cfg.Media.PublicRTMPURL; u != "" {
			return u
		}
		port := "1935"
		if _, p, err := net.SplitHostPort(s.cfg.Media.Ingress.RTMPAddr); err == nil {
			port = p
		}
		return "rtmp://" + net.JoinHostPort(requestHost(r), port) + "/live"
	case store.InputWHIP:
		if u := s.cfg.Media.PublicWHIPURL; u != "" {
			return u
		}
		if u := s.cfg.ControlPlane.PublicWSURL; u != "" {
			return strings.Replace(strings.TrimRight(u, "/"), "ws", "http", 1) + "/whip/v1"
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return scheme + "://" + r.Host + "/whip/v1"
	}
	return ""
}

func (s *Server) createIngress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	var in createIngressBody
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.InputType == store.InputURL {
		if _, err := source.CheckInput(r.Context(), in.SourceURL, s.cfg.Media.AllowPrivateAddresses); err != nil {
			return fieldError("source_url", err.Error())
		}
	}
	created, err := s.store.CreateIngress(r.Context(), &store.Ingress{
		ProjectID: c.projectID(), Name: in.Name, InputType: in.InputType, RoomName: in.Room,
		ParticipantIdentity: in.ParticipantIdentity, ParticipantName: in.ParticipantName, SourceURL: in.SourceURL, Enabled: true,
	})
	if err != nil {
		return err
	}
	created.PublishURL = s.publishURL(r, created)
	s.audit(r, c, "ingress.created", created.ID, map[string]any{"input_type": created.InputType, "room": created.RoomName})
	writeJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) listIngress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	items, err := s.store.ListIngress(r.Context(), c.projectID())
	if err != nil {
		return err
	}
	if items == nil {
		items = []*store.Ingress{}
	}
	for _, in := range items {
		in.PublishURL = s.publishURL(r, in)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) getIngress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	in, err := s.store.IngressByID(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return err
	}
	in.PublishURL = s.publishURL(r, in)
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) updateIngress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	var body updateIngressBody
	if err := decode(w, r, &body); err != nil {
		return err
	}
	in, err := s.store.UpdateIngress(r.Context(), c.projectID(), r.PathValue("id"), store.IngressPatch{
		Name: body.Name, RoomName: body.Room, ParticipantIdentity: body.ParticipantIdentity, ParticipantName: body.ParticipantName, Enabled: body.Enabled,
	})
	if err != nil {
		return err
	}
	in.PublishURL = s.publishURL(r, in)
	s.audit(r, c, "ingress.updated", in.ID, nil)
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) deleteIngress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	if err := s.store.DeleteIngress(r.Context(), c.projectID(), r.PathValue("id")); err != nil {
		return err
	}
	s.audit(r, c, "ingress.deleted", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) stopIngress(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	in, err := s.store.RequestIngressStop(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return err
	}
	in.PublishURL = s.publishURL(r, in)
	s.audit(r, c, "ingress.stopped", in.ID, nil)
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) rotateIngressKey(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	in, err := s.store.RotateIngressKey(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if existing, e := s.store.IngressByID(r.Context(), c.projectID(), r.PathValue("id")); e == nil && existing.InputType == store.InputURL {
				return errorf(http.StatusConflict, "no_stream_key", "A URL input has no stream key.")
			}
		}
		return err
	}
	in.PublishURL = s.publishURL(r, in)
	s.audit(r, c, "ingress.key_rotated", in.ID, nil)
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) ingressLogs(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantIngress); err != nil {
		return err
	}
	if _, err := s.store.IngressByID(r.Context(), c.projectID(), r.PathValue("id")); err != nil {
		return err
	}
	items, err := s.store.IngressLogs(r.Context(), c.projectID(), r.PathValue("id"), queryInt(r, "limit", defaultPage, maxPage))
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}
