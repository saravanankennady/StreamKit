// Package whip accepts streams over WHIP. It runs in the server rather than a worker: a WHIP client already speaks
// WebRTC, so it connects straight to the SFU and nothing needs transcoding. All this adds is finding the ingress
// from the stream key, and reporting on the session.
package whip

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
)

const (
	maxOffer      = 256 << 10
	sessionLease  = 30 * time.Second
	sessionTick   = 2 * time.Second
	requestLimit  = 10 * time.Second
	exposeHeaders = "Location, ETag"
)

// Service serves the WHIP endpoint.
type Service struct {
	store  *store.Store
	rooms  *room.Manager
	static auth.StaticKeys
	log    *slog.Logger
	ctx    context.Context
	// Allow says whether to look at a new publisher at all. Nil lets everyone through.
	Allow func(*http.Request) bool

	mu       sync.Mutex
	sessions map[string]*session
}

// New creates a Service.
func New(st *store.Store, rooms *room.Manager, static auth.StaticKeys, log *slog.Logger) *Service {
	return &Service{store: st, rooms: rooms, static: static, log: log.With("service", "whip"), sessions: map[string]*session{}}
}

// Start adds the routes to mux, for the lifetime of ctx.
func (s *Service) Start(ctx context.Context, mux *http.ServeMux) {
	s.ctx = ctx
	mux.HandleFunc("POST /whip/v1", s.handleCreate)
	mux.HandleFunc("DELETE /whip/v1/{id}", s.handleDelete)
	// Trickle is optional in WHIP, and every candidate is in the answer already.
	mux.HandleFunc("PATCH /whip/v1/{id}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this endpoint does not use trickle ICE or ICE restarts", http.StatusMethodNotAllowed)
	})
}

// session is one WHIP publisher. It is the participant's signalling sink too, which is how it hears the
// participant has closed, whatever the reason.
type session struct {
	svc     *Service
	id      string
	keyHash []byte
	ingress *store.Ingress
	worker  string
	started time.Time
	done    chan struct{}
	once    sync.Once

	mu          sync.Mutex
	participant *participant.Participant
}

// Send implements participant.Sink. A WHIP client cannot receive signalling, and its session needs none.
func (*session) Send(*pb.SignalResponse) error { return nil }

// Close implements participant.Sink.
func (w *session) Close(reason string) { w.finish(store.IngressInactive, "", reason) }

func (w *session) finish(status, message, reason string) {
	w.once.Do(func() {
		close(w.done)
		s := w.svc
		s.mu.Lock()
		delete(s.sessions, w.id)
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
		defer cancel()
		in, err := s.store.ReleaseIngress(ctx, w.ingress.ID, w.worker, status, message)
		if err != nil {
			// Deleted, or its lease has run out. The final state is someone else's to report now.
			if !errors.Is(err, store.ErrJobLost) {
				s.log.Warn("could not release ingress", "ingress", w.ingress.ID, "error", err)
			}
			return
		}
		seconds := int(time.Since(w.started).Seconds())
		_ = s.store.RecordIngressEvent(ctx, in, "ingress_ended", "info", "ingress_ended", "WHIP stream ended: "+reason, map[string]any{"seconds": seconds})
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)
	if s.Allow != nil && !s.Allow(r) {
		http.Error(w, "too many attempts: try again in a moment", http.StatusTooManyRequests)
		return
	}
	key := bearer(r)
	if key == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "send the stream key as a bearer token", http.StatusUnauthorized)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/sdp") {
		http.Error(w, "send an SDP offer as application/sdp", http.StatusUnsupportedMediaType)
		return
	}
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOffer))
	if err != nil {
		http.Error(w, "the offer is too large", http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestLimit)
	defer cancel()

	worker := s.rooms.NodeID()
	in, err := s.store.ClaimIngressByKey(ctx, key, store.InputWHIP, worker, sessionLease)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "unknown stream key", http.StatusUnauthorized)
		return
	case errors.Is(err, store.ErrIngressDisabled):
		_ = s.store.RecordIngressEvent(ctx, in, "", "warn", "ingress_rejected", "Refused a WHIP publisher: this ingress is disabled.", nil)
		http.Error(w, "this ingress is disabled", http.StatusForbidden)
		return
	case errors.Is(err, store.ErrIngressBusy):
		_ = s.store.RecordIngressEvent(ctx, in, "", "warn", "ingress_rejected", "Refused a second WHIP publisher on the same stream key.", nil)
		http.Error(w, "this ingress already has a publisher", http.StatusConflict)
		return
	case errors.Is(err, store.ErrProjectAtLimit):
		_ = s.store.RecordIngressEvent(ctx, in, "", "warn", "ingress_rejected", "Refused a WHIP publisher: this project is running as many streams as it may.", nil)
		http.Error(w, "this project is running as many streams as it may", http.StatusTooManyRequests)
		return
	case err != nil:
		s.log.Warn("could not look up stream key", "error", err)
		http.Error(w, "something went wrong on our side", http.StatusInternalServerError)
		return
	}

	session := &session{
		svc: s, id: participant.NewID("WH_"), keyHash: store.HashToken(key), ingress: in, worker: worker,
		started: time.Now(), done: make(chan struct{}),
	}
	answer, status, err := s.start(ctx, session, string(offer))
	if err != nil {
		session.finish(store.IngressError, err.Error(), err.Error())
		http.Error(w, err.Error(), status)
		return
	}
	s.mu.Lock()
	s.sessions[session.id] = session
	s.mu.Unlock()
	go session.run()
	// It may have ended already, such as with the room closing in that same moment. Then run cleans up nothing, and
	// neither did finish, which found no entry to remove.
	select {
	case <-session.done:
		s.mu.Lock()
		delete(s.sessions, session.id)
		s.mu.Unlock()
	default:
	}

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/whip/v1/"+session.id)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(answer))
}

// start joins the room and answers the offer. A publisher's token would normally say what it may do: this is the
// equivalent, built from the ingress it is publishing to.
func (s *Service) start(ctx context.Context, w *session, offer string) (string, int, error) {
	in := w.ingress
	key, err := s.store.SigningKeyFor(ctx, in.ProjectID, s.static)
	if err != nil {
		s.log.Warn("no signing key for WHIP publisher", "project", in.ProjectID, "error", err)
		return "", http.StatusInternalServerError, errors.New("this project has no usable API key")
	}
	publish, deny := true, false
	verified := &auth.Verified{
		Key: key,
		Grants: &auth.Grants{
			Identity: in.ParticipantIdentity, Name: in.ParticipantName, Kind: "INGRESS",
			Video: &auth.VideoGrant{RoomJoin: true, Room: in.RoomName, CanPublish: &publish, CanSubscribe: &deny, CanPublishData: &deny},
		},
	}
	p, err := s.rooms.Join(room.JoinRequest{
		RoomName: in.RoomName, Verified: verified, Sink: w,
		Client: &pb.ClientInfo{Protocol: room.ProtocolVersion, Browser: "whip"},
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, room.ErrRoomNotFound) || errors.Is(err, room.ErrRoomClosed) {
			status = http.StatusNotFound
		} else if errors.Is(err, room.ErrDraining) || errors.Is(err, room.ErrMaxParticipants) {
			status = http.StatusServiceUnavailable
		}
		return "", status, err
	}
	w.mu.Lock()
	w.participant = p
	w.mu.Unlock()
	answer, err := p.PublishOffer(ctx, offer)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, participant.ErrBadOffer) {
			status = http.StatusBadRequest
		}
		// Reported first: closing the participant would otherwise report an ordinary end.
		w.finish(store.IngressError, err.Error(), err.Error())
		p.Close(false, pb.DisconnectReason_JOIN_FAILURE)
		return "", status, err
	}
	return answer, http.StatusCreated, nil
}

// run holds the lease, honours stop requests, and reports health.
func (w *session) run() {
	s := w.svc
	t := time.NewTicker(sessionTick)
	defer t.Stop()
	publishing := false
	stats := store.IngressStats{StartedAt: &w.started}
	for {
		select {
		case <-w.done:
			return
		case <-s.ctx.Done():
			w.participantClose(pb.DisconnectReason_SERVER_SHUTDOWN)
			return
		case <-t.C:
		}
		stop, err := s.store.RenewIngress(s.ctx, w.ingress.ID, w.worker, sessionLease)
		if errors.Is(err, store.ErrJobLost) || stop {
			w.participantClose(pb.DisconnectReason_ROOM_DELETED)
			return
		}
		if err != nil {
			continue // A database hiccup. The lease has room for a few of these.
		}
		w.mu.Lock()
		p := w.participant
		w.mu.Unlock()
		media := p.Stats()
		stats.BitrateIn = media.PublishedBitrate
		for _, t := range p.Tracks() {
			mime := t.Info().GetMimeType()
			codec := strings.ToLower(mime[strings.Index(mime, "/")+1:])
			if t.Kind() == pb.TrackType_VIDEO {
				stats.VideoCodec = codec
			} else {
				stats.AudioCodec = codec
			}
		}
		status := store.IngressBuffering
		if media.PublishPackets > 0 {
			status = store.IngressPublishing
		}
		in, err := s.store.UpdateIngressRunning(s.ctx, w.ingress.ID, w.worker, status, stats)
		if err == nil && status == store.IngressPublishing && !publishing {
			publishing = true
			_ = s.store.RecordIngressEvent(s.ctx, in, "ingress_started", "info", "ingress_started", "WHIP stream is publishing.", nil)
		}
	}
}

func (w *session) participantClose(reason pb.DisconnectReason) {
	w.mu.Lock()
	p := w.participant
	w.mu.Unlock()
	if p != nil {
		p.Close(false, reason)
	}
}

// handleDelete ends a session. It must carry the session's key, so a leaked resource URL alone cannot end a stream.
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	session := s.sessions[r.PathValue("id")]
	s.mu.Unlock()
	if session == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare(store.HashToken(bearer(r)), session.keyHash) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "send the stream key as a bearer token", http.StatusUnauthorized)
		return
	}
	session.participantClose(pb.DisconnectReason_CLIENT_INITIATED)
	w.WriteHeader(http.StatusOK)
}
