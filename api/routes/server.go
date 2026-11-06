// Package routes serves the REST API at /api/v1. Browsers authenticate with a session cookie and name their
// project in the X-Project-ID header. Programs authenticate with an access token, whose API key names
// the project.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"elnova-streamkit.local/api/middleware"
	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/api/webhook"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/monitor"
	"elnova-streamkit.local/server/room"
	"elnova-streamkit.local/server/routing"
	sipapi "elnova-streamkit.local/sip/api"
)

const (
	sessionCookie   = "streamkit_session"
	maxBody         = 1 << 20
	defaultPage     = 50
	maxPage         = 200
	verifyTokenTTL  = 24 * time.Hour
	resetTokenTTL   = time.Hour
	liveSendTimeout = 5 * time.Second
)

// Mailer sends account emails.
type Mailer interface {
	Send(to, subject, body string) error
}

// Server handles the REST API.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	keys    *store.KeyCache
	rooms   *room.Manager
	monitor *monitor.Monitor
	webhook *webhook.Dispatcher
	mailer  Mailer
	log     *slog.Logger

	authLimit *middleware.RateLimiter
	apiLimit  *middleware.RateLimiter
	// A whole office can join from one address, so this is looser than authLimit. Codes cannot be guessed.
	joinLimit *middleware.RateLimiter
	// A person does not start meetings all day, and each one is a host link and a place to hold a room.
	meetingLimit *middleware.RateLimiter

	// router says which node holds a room. Nil means this is the only node.
	router routing.Router

	memberLimits store.MemberLimits

	settingsMu sync.Mutex
	settings   map[string]cachedSettings

	media mediaServices
	sip   *sipapi.Service
}

// New creates a Server.
func New(cfg *config.Config, st *store.Store, keys *store.KeyCache, rooms *room.Manager, mon *monitor.Monitor,
	wh *webhook.Dispatcher, mailer Mailer, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, store: st, keys: keys, rooms: rooms, monitor: mon, webhook: wh, mailer: mailer, log: log,
		sip: sipapi.New(cfg, keys, st, log),
		// Password guessing is the threat here, so this is deliberately tight.
		authLimit:    middleware.NewRateLimiter(20, 10),
		apiLimit:     middleware.NewRateLimiter(1200, 200),
		joinLimit:    middleware.NewRateLimiter(120, 60),
		meetingLimit: middleware.NewRateLimiter(2, 20),
		memberLimits: store.MemberLimits{PerLink: maxMembersPerLink, PerMeeting: maxMembersPerMeeting},
	}
}

// SetRouter tells the API how to find out which node holds a room.
func (s *Server) SetRouter(router routing.Router) { s.router = router }

// APIError is a failure returned to the client.
type APIError struct {
	Status int
	Code   string
	Msg    string
	Fields schemas.FieldErrors
}

func (e *APIError) Error() string { return e.Msg }

func errorf(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Msg: msg}
}

var (
	errUnauthenticated = errorf(http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
	errForbidden       = errorf(http.StatusForbidden, "forbidden", "You do not have permission to do that.")
	errNotFound        = errorf(http.StatusNotFound, "not_found", "Not found.")
	errRateLimited     = errorf(http.StatusTooManyRequests, "rate_limited", "Too many requests. Wait a moment and try again.")
)

// reqCtx is one authenticated request.
type reqCtx struct {
	user      *store.User
	sessionID string
	// project is set when the route needs one. token is set when an access token authenticated this call.
	project *store.Project
	token   *auth.Verified
	ip      string
}

func (c *reqCtx) userID() string {
	if c.user != nil {
		return c.user.ID
	}
	return ""
}

func (c *reqCtx) projectID() string {
	if c.project != nil {
		return c.project.ID
	}
	if c.token != nil {
		return c.token.Key.ProjectID
	}
	return ""
}

type handler func(w http.ResponseWriter, r *http.Request, c *reqCtx) error

// Role requirements for project routes.
type access int

const (
	// public routes run without authentication.
	public access = iota
	// signedIn needs a browser session.
	signedIn
	// projectMember needs access to the project in X-Project-ID. Access tokens are refused, including one with every
	// grant, so a route that says nothing more is closed to them.
	projectMember
	// projectOrToken is the same, or an access token. A valid token only proves who someone is, so each handler at this
	// level must check its grant before doing anything else.
	projectOrToken
	// projectAdmin needs the owner or admin role. Access tokens cannot use these routes.
	projectAdmin
	// platformAdmin operates the whole installation.
	platformAdmin
)

func (s *Server) wrap(level access, fn handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &reqCtx{ip: middleware.ClientIP(r, s.cfg.ControlPlane.TrustProxyHeaders)}
		if err := s.authorize(r, c, level); err != nil {
			s.fail(w, r, c, err)
			return
		}
		limitKey := c.ip
		if c.user != nil {
			limitKey = c.user.ID
		}
		if !s.apiLimit.Allow(limitKey) {
			s.fail(w, r, c, errRateLimited)
			return
		}
		if err := fn(w, r, c); err != nil {
			s.fail(w, r, c, err)
		}
	}
}

func (s *Server) authorize(r *http.Request, c *reqCtx, level access) error {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if user, sessionID, err := s.store.UserBySession(r.Context(), cookie.Value); err == nil {
			c.user, c.sessionID = user, sessionID
		}
	}
	// Cookies are sent by browsers automatically, so a state-changing request must also prove it was made
	// by a page, not by a form on another site. A JSON content type cannot be sent by a plain form.
	if c.user != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			return errorf(http.StatusUnsupportedMediaType, "unsupported_media_type", "Send requests as application/json.")
		}
	}
	switch level {
	case public:
		return nil
	case signedIn:
		if c.user == nil {
			return errUnauthenticated
		}
		return nil
	case platformAdmin:
		if c.user == nil {
			return errUnauthenticated
		}
		if !c.user.IsPlatformAdmin {
			return errForbidden
		}
		return nil
	}

	if c.user == nil {
		if level == projectAdmin {
			return errUnauthenticated
		}
		token := auth.TokenFromHeader(r.Header.Get("Authorization"))
		if token == "" {
			return errUnauthenticated
		}
		verified, err := auth.Verify(token, s.keys)
		if err != nil {
			return errorf(http.StatusUnauthorized, "invalid_token", err.Error())
		}
		if level != projectOrToken {
			return errForbidden
		}
		c.token = verified
		return nil
	}

	id := r.Header.Get("X-Project-ID")
	if id == "" {
		id = r.URL.Query().Get("project_id")
	}
	if id == "" {
		return errorf(http.StatusBadRequest, "project_required", "Name a project with the X-Project-ID header.")
	}
	project, err := s.store.ProjectByID(r.Context(), id)
	if err != nil {
		return errNotFound
	}
	role, err := s.store.RoleIn(r.Context(), c.user.ID, project.OrgID)
	if err != nil {
		// Answering 404, not 403, does not reveal that the project exists.
		return errNotFound
	}
	if level == projectAdmin && role == "member" {
		return errForbidden
	}
	c.project = project
	return nil
}

// needTokenGrant checks the grant an access token must carry to reach a route.
func needTokenGrant(c *reqCtx, check func(*auth.VideoGrant) bool) error {
	if c.token != nil && !check(c.token.Grants.Video) {
		return errForbidden
	}
	return nil
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, c *reqCtx, err error) {
	var api *APIError
	if !errors.As(err, &api) {
		switch {
		case errors.Is(err, store.ErrNotFound):
			api = errNotFound
		case errors.Is(err, store.ErrConflict):
			api = errorf(http.StatusConflict, "conflict", "That already exists.")
		case errors.Is(err, context.Canceled):
			return
		case store.IsUnstorableText(err):
			api = errorf(http.StatusBadRequest, "bad_text", "That contains a character this server cannot store.")
		default:
			// The details stay in the log. They can include things a client should not see.
			s.log.Error("api request failed", "error", err, "method", r.Method, "path", r.URL.Path)
			s.recordAPIError(r, c, err)
			api = errorf(http.StatusInternalServerError, "internal", "Something went wrong on our side.")
		}
	}
	body := map[string]any{"code": api.Code, "message": api.Msg}
	if len(api.Fields) > 0 {
		body["fields"] = api.Fields
	}
	writeJSON(w, api.Status, map[string]any{"error": body})
}

func (s *Server) recordAPIError(r *http.Request, c *reqCtx, err error) {
	id := c.projectID()
	if id == "" {
		return
	}
	details, _ := json.Marshal(map[string]string{"method": r.Method, "path": r.URL.Path})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	_ = s.store.InsertEvents(ctx, []store.Event{{
		ProjectID: &id, Category: store.CategoryAPI, Level: "error", Type: "api_error",
		Message: err.Error(), Details: details, CreatedAt: time.Now(),
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Validator is a request body that can check itself.
type Validator interface {
	Validate() schemas.FieldErrors
}

func decode(w http.ResponseWriter, r *http.Request, into Validator) error {
	body := http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return &APIError{Status: http.StatusBadRequest, Code: "bad_request", Msg: "The request body is not valid: " + err.Error()}
	}
	if errs := into.Validate(); len(errs) > 0 {
		return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Msg: "Some fields need attention.", Fields: errs}
	}
	return nil
}

func queryInt(r *http.Request, name string, def, maxValue int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 {
		return def
	}
	return min(v, maxValue)
}

func (s *Server) audit(r *http.Request, c *reqCtx, action, target string, details any) {
	if err := s.store.Audit(r.Context(), c.projectID(), c.userID(), action, target, c.ip, details); err != nil {
		s.log.Warn("could not write audit log", "action", action, "error", err)
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.cfg.ControlPlane.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// route is one line of the API's table.
type route struct {
	pattern string
	level   access
	fn      handler
}

// routes is the API's table. TestEveryRouteSaysWhatATokenGets walks it, so a new line has to say what an access token
// gets from it before the tests pass.
func (s *Server) routes() []route {
	return []route{
		{"POST /api/v1/auth/register", public, s.register},
		{"POST /api/v1/auth/login", public, s.login},
		{"POST /api/v1/auth/logout", signedIn, s.logout},
		{"POST /api/v1/auth/forgot-password", public, s.forgotPassword},
		{"POST /api/v1/auth/reset-password", public, s.resetPassword},
		{"POST /api/v1/auth/verify-email", public, s.verifyEmail},
		{"POST /api/v1/auth/resend-verification", signedIn, s.resendVerification},
		{"GET /api/v1/auth/sessions", signedIn, s.listSessions},
		{"DELETE /api/v1/auth/sessions/{id}", signedIn, s.deleteSession},
		{"GET /api/v1/me", signedIn, s.me},

		{"GET /api/v1/projects", signedIn, s.listProjects},
		{"POST /api/v1/projects", signedIn, s.createProject},
		{"GET /api/v1/projects/{id}", signedIn, s.getProject},
		{"PATCH /api/v1/projects/{id}", signedIn, s.updateProject},
		{"DELETE /api/v1/projects/{id}", signedIn, s.deleteProject},

		{"GET /api/v1/keys", projectAdmin, s.listKeys},
		{"POST /api/v1/keys", projectAdmin, s.createKey},
		{"POST /api/v1/keys/{id}/rotate", projectAdmin, s.rotateKey},
		{"DELETE /api/v1/keys/{id}", projectAdmin, s.revokeKey},
		{"POST /api/v1/tokens", projectMember, s.createToken},

		{"GET /api/v1/join-links", projectMember, s.listJoinLinks},
		{"POST /api/v1/join-links", projectMember, s.createJoinLink},
		{"DELETE /api/v1/join-links/{id}", projectMember, s.revokeJoinLink},
		{"POST /api/v1/join/preview", public, s.previewJoinLink},
		{"POST /api/v1/join", public, s.joinWithLink},
		{"POST /api/v1/join/wait", public, s.joinWithTicket},

		// Everything under one meeting authenticates with the caller's own token, in meetingCaller.
		{"POST /api/v1/meetings", projectMember, s.createMeeting},
		{"GET /api/v1/meetings/{room}", public, s.getMeeting},
		{"PATCH /api/v1/meetings/{room}", public, s.updateMeeting},
		{"POST /api/v1/meetings/{room}/end", public, s.endMeeting},
		{"POST /api/v1/meetings/{room}/mute-all", public, s.muteAll},
		{"POST /api/v1/meetings/{room}/hand", public, s.setHand},
		{"GET /api/v1/meetings/{room}/summary", public, s.meetingSummary},
		{"POST /api/v1/meetings/{room}/waiting/{identity}", public, s.answerWaiting},
		{"DELETE /api/v1/meetings/{room}/waiting", public, s.turnAwayEveryone},
		{"POST /api/v1/meetings/{room}/participants/{identity}/mute", public, s.muteMember},
		{"POST /api/v1/meetings/{room}/participants/{identity}/remove", public, s.removeMember},
		{"POST /api/v1/meetings/{room}/participants/{identity}/lower-hand", public, s.lowerHand},
		{"PATCH /api/v1/meetings/{room}/participants/{identity}", public, s.setMemberRole},

		{"GET /api/v1/rooms", projectOrToken, s.listRooms},
		{"POST /api/v1/rooms", projectOrToken, s.createRoom},
		{"GET /api/v1/rooms/history", projectOrToken, s.roomHistory},
		{"GET /api/v1/rooms/{name}", projectOrToken, s.getRoom},
		{"PATCH /api/v1/rooms/{name}", projectOrToken, s.updateRoom},
		{"DELETE /api/v1/rooms/{name}", projectOrToken, s.deleteRoom},

		{"GET /api/v1/participants", projectOrToken, s.listParticipants},
		{"GET /api/v1/participants/{room}/{identity}", projectOrToken, s.getParticipant},
		{"PATCH /api/v1/participants/{room}/{identity}", projectOrToken, s.updateParticipant},
		{"DELETE /api/v1/participants/{room}/{identity}", projectOrToken, s.removeParticipant},

		{"GET /api/v1/tracks", projectOrToken, s.listTracks},
		{"POST /api/v1/tracks/{room}/{identity}/{sid}/mute", projectOrToken, s.muteTrack},

		{"GET /api/v1/webhooks", projectMember, s.listWebhooks},
		{"POST /api/v1/webhooks", projectAdmin, s.createWebhook},
		{"GET /api/v1/webhooks/events", projectMember, s.webhookEvents},
		{"GET /api/v1/webhooks/{id}", projectMember, s.getWebhook},
		{"PATCH /api/v1/webhooks/{id}", projectAdmin, s.updateWebhook},
		{"DELETE /api/v1/webhooks/{id}", projectAdmin, s.deleteWebhook},
		{"GET /api/v1/webhooks/{id}/deliveries", projectMember, s.listDeliveries},
		{"POST /api/v1/webhooks/{id}/test", projectAdmin, s.testWebhook},
		{"POST /api/v1/webhooks/deliveries/{id}/retry", projectAdmin, s.retryDelivery},

		{"GET /api/v1/analytics/overview", projectOrToken, s.analyticsOverview},
		{"GET /api/v1/analytics/usage", projectOrToken, s.analyticsUsage},
		{"GET /api/v1/monitor", projectMember, s.monitorSnapshot},
		{"GET /api/v1/monitor/live", projectMember, s.monitorLive},

		{"GET /api/v1/servers", signedIn, s.listServers},
		{"POST /api/v1/servers/{id}/drain", platformAdmin, s.drainServer},

		{"GET /api/v1/egress", projectOrToken, s.listAllEgress},
		{"POST /api/v1/egress", projectOrToken, s.startEgress},
		{"GET /api/v1/egress/{id}", projectOrToken, s.getEgress},
		{"POST /api/v1/egress/{id}/stop", projectOrToken, s.stopEgress},
		{"GET /api/v1/recordings", projectOrToken, s.listRecordings},
		{"GET /api/v1/recordings/{id}", projectOrToken, s.getRecording},
		{"GET /api/v1/recordings/{id}/file", projectOrToken, s.recordingFile},
		{"POST /api/v1/recordings/{id}/retry", projectOrToken, s.retryRecording},
		{"DELETE /api/v1/recordings/{id}", projectOrToken, s.deleteRecording},

		{"GET /api/v1/ingress", projectOrToken, s.listIngress},
		{"POST /api/v1/ingress", projectOrToken, s.createIngress},
		{"GET /api/v1/ingress/{id}", projectOrToken, s.getIngress},
		{"PATCH /api/v1/ingress/{id}", projectOrToken, s.updateIngress},
		{"DELETE /api/v1/ingress/{id}", projectOrToken, s.deleteIngress},
		{"POST /api/v1/ingress/{id}/stop", projectOrToken, s.stopIngress},
		{"POST /api/v1/ingress/{id}/rotate-key", projectOrToken, s.rotateIngressKey},
		{"GET /api/v1/ingress/{id}/logs", projectOrToken, s.ingressLogs},

		// The dashboard's SIP pages. A token reads them with the sip admin grant, and the SDKs use Twirp instead.
		{"GET /api/v1/sip/trunks/inbound", projectOrToken, s.listSIPInbound},
		{"POST /api/v1/sip/trunks/inbound", projectAdmin, s.createSIPInbound},
		{"GET /api/v1/sip/trunks/inbound/{id}", projectOrToken, s.getSIPInbound},
		{"PATCH /api/v1/sip/trunks/inbound/{id}", projectAdmin, s.updateSIPInbound},
		{"DELETE /api/v1/sip/trunks/inbound/{id}", projectAdmin, s.deleteSIPTrunk},
		{"GET /api/v1/sip/trunks/outbound", projectOrToken, s.listSIPOutbound},
		{"POST /api/v1/sip/trunks/outbound", projectAdmin, s.createSIPOutbound},
		{"GET /api/v1/sip/trunks/outbound/{id}", projectOrToken, s.getSIPOutbound},
		{"PATCH /api/v1/sip/trunks/outbound/{id}", projectAdmin, s.updateSIPOutbound},
		{"DELETE /api/v1/sip/trunks/outbound/{id}", projectAdmin, s.deleteSIPTrunk},
		{"GET /api/v1/sip/dispatch-rules", projectOrToken, s.listSIPRules},
		{"POST /api/v1/sip/dispatch-rules", projectAdmin, s.createSIPRule},
		{"PATCH /api/v1/sip/dispatch-rules/{id}", projectAdmin, s.updateSIPRule},
		{"DELETE /api/v1/sip/dispatch-rules/{id}", projectAdmin, s.deleteSIPRule},
		{"GET /api/v1/sip/calls", projectOrToken, s.listSIPCalls},
		{"POST /api/v1/sip/calls", projectAdmin, s.createSIPCall},
		{"GET /api/v1/sip/calls/{id}", projectOrToken, s.getSIPCall},
		{"POST /api/v1/sip/calls/{id}/hangup", projectAdmin, s.hangupSIPCall},

		// Provider credentials, and what agent workers report. Workers read and report with the agent grant.
		{"GET /api/v1/provider-credentials", projectAdmin, s.listProviderCredentials},
		{"PUT /api/v1/provider-credentials", projectAdmin, s.saveProviderCredential},
		{"DELETE /api/v1/provider-credentials/{id}", projectAdmin, s.deleteProviderCredential},
		{"POST /api/v1/agent-credentials/resolve", projectOrToken, s.resolveProviderCredential},
		{"POST /api/v1/agent-sessions", projectOrToken, s.reportAgentSession},
		{"GET /api/v1/agent-sessions", projectMember, s.listAgentSessions},
		{"GET /api/v1/agent-sessions/{id}", projectMember, s.getAgentSession},

		{"GET /api/v1/logs", projectMember, s.listLogs},
		{"GET /api/v1/audit", projectAdmin, s.listAudit},
	}
}

// Register adds the API's routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	for _, x := range s.routes() {
		mux.HandleFunc(x.pattern, s.wrap(x.level, x.fn))
	}
	mux.HandleFunc("GET /api/v1/openapi.yaml", s.openAPI)
}

func permissionToProto(p *schemas.Permission) *pb.ParticipantPermission {
	if p == nil {
		return nil
	}
	out := &pb.ParticipantPermission{
		CanPublish:        p.CanPublish,
		CanSubscribe:      p.CanSubscribe,
		CanPublishData:    p.CanPublishData,
		CanUpdateMetadata: p.CanUpdateMetadata,
		Hidden:            p.Hidden,
	}
	for _, name := range p.CanPublishSources {
		if v, ok := pb.TrackSource_value[strings.ToUpper(name)]; ok {
			out.CanPublishSources = append(out.CanPublishSources, pb.TrackSource(v))
		}
	}
	return out
}
