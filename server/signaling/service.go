// Package signaling serves the WebSocket endpoint clients connect to, translating between protobuf
// frames and room and participant operations.
package signaling

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/joinrequest"
	"elnova-streamkit.local/server/metrics"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
)

// Keep at most this many unsent messages per connection. A client that falls this far behind cannot
// catch up, so its connection is closed and it resumes.
const outboundQueueSize = 4096

const (
	writeTimeout = 10 * time.Second
	// A client pings every PingIntervalSeconds, so this much silence means the connection is dead.
	readTimeout = (room.PingTimeoutSeconds + room.PingIntervalSeconds) * time.Second
)

// Service serves the signalling endpoints.
type Service struct {
	cfg      *config.Config
	keys     auth.KeyProvider
	rooms    *room.Manager
	log      *slog.Logger
	upgrader websocket.Upgrader
}

// NewService creates a Service.
func NewService(cfg *config.Config, keys auth.KeyProvider, rooms *room.Manager, log *slog.Logger) *Service {
	return &Service{
		cfg:   cfg,
		keys:  keys,
		rooms: rooms,
		log:   log,
		upgrader: websocket.Upgrader{
			// Access tokens authenticate these connections, not cookies, so any page may open one.
			CheckOrigin:     func(*http.Request) bool { return true },
			ReadBufferSize:  16 << 10,
			WriteBufferSize: 16 << 10,
		},
	}
}

// Register adds the service's routes to mux.
//
// /rtc/v1 and /rtc/v1/validate are the single peer connection protocol. With it turned off they return 404, which is what
// tells a client to use /rtc instead. A 200 from the validate endpoint ends that fallback, so this cannot be half on.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("/rtc", func(w http.ResponseWriter, r *http.Request) { s.handleConnect(w, r, s.parseConnect) })
	mux.HandleFunc("/rtc/validate", func(w http.ResponseWriter, r *http.Request) { s.handleValidate(w, r, s.parseConnect) })
	if s.cfg.RTC.SinglePeerConnection {
		mux.HandleFunc("/rtc/v1", func(w http.ResponseWriter, r *http.Request) { s.handleConnect(w, r, s.parseConnectV1) })
		mux.HandleFunc("/rtc/v1/validate", func(w http.ResponseWriter, r *http.Request) { s.handleValidate(w, r, s.parseConnectV1) })
		return
	}
	mux.HandleFunc("/rtc/v1", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/rtc/v1/validate", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
}

// ConnectLimits bounds how fast signalling connections can start. Valid tokens are cheap to hold on to and each can open a
// session, so this is what stops a flood of them from one place.
type ConnectLimits struct {
	// New answers for a connection that would start a session, by the client's address.
	New func(*http.Request) bool
	// Resume answers for a client coming back to the session a valid token names. These do not share a limit by address: a
	// network change gives someone a new address, and when an office's connection drops, everyone there comes back at once
	// from one address that may have nothing left. The key is the session, so all it can spend is its own.
	Resume func(key string) bool
	// FromPeer says another node passed this request on. That node has counted it, and the address here would be its own.
	FromPeer func(*http.Request) bool
	// Refused answers a connection over the limit.
	Refused http.Handler
}

// TooManyConnects is the refusal for a connection over the limit. Clients ask /rtc/validate why a connection failed, so the
// words reach people.
func TooManyConnects(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "too many connections from this address: wait a moment and try again", http.StatusTooManyRequests)
}

// Limit puts limits in front of next, which is everything this server serves. It goes in front of routing, so a connection is
// counted once, on the node that first took it, and before it costs a room anything.
func (s *Service) Limit(limits ConnectLimits, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path != "/rtc" && r.URL.Path != "/rtc/validate") || limits.FromPeer(r) || s.allowConnect(limits, r) {
			next.ServeHTTP(w, r)
			return
		}
		metrics.SignalConnects.WithLabelValues("rate_limited").Inc()
		limits.Refused.ServeHTTP(w, r)
	})
}

func (s *Service) allowConnect(limits ConnectLimits, r *http.Request) bool {
	if boolParam(r, "reconnect") {
		if v, err := auth.Verify(tokenOf(r), s.keys); err == nil {
			return limits.Resume(v.Key.ProjectID + "\x00" + v.Grants.Video.Room + "\x00" + v.Grants.Identity)
		}
	}
	return limits.New(r)
}

func tokenOf(r *http.Request) string {
	if token := r.FormValue("access_token"); token != "" {
		return token
	}
	return auth.TokenFromHeader(r.Header.Get("Authorization"))
}

type connectParams struct {
	verified          *auth.Verified
	roomName          string
	client            *pb.ClientInfo
	autoSubscribe     bool
	autoSubscribeData bool
	adaptiveStream    bool
	allowPause        *bool
	reconnect         bool
	participantSID    string
	reconnectFor      pb.ReconnectReason
	// join is set on the single peer connection protocol.
	join *pb.JoinRequest
}

// parseConnectV1 reads a connection on the single peer connection protocol. All but the token comes from the join request,
// including a default that differs from /rtc: auto-subscribe is off unless the request says otherwise.
func (s *Service) parseConnectV1(r *http.Request) (*connectParams, *httpError) {
	cp, herr := s.parseConnect(r)
	if herr != nil {
		return nil, herr
	}
	join, err := joinrequest.Decode(r.FormValue("join_request"))
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, err}
	}
	settings := join.GetConnectionSettings()
	cp.join = join
	cp.client = joinClientInfo(join, r)
	cp.autoSubscribe = settings.GetAutoSubscribe()
	cp.autoSubscribeData = settings.GetAutoSubscribeDataTrack()
	cp.adaptiveStream = settings.GetAdaptiveStream()
	cp.allowPause = nil
	if settings != nil && settings.SubscriberAllowPause != nil {
		allow := settings.GetSubscriberAllowPause()
		cp.allowPause = &allow
	}
	cp.reconnect = join.GetReconnect()
	cp.reconnectFor = join.GetReconnectReason()
	cp.participantSID = join.GetParticipantSid()

	g := cp.verified.Grants
	if limit := s.cfg.Limits.MaxMetadataBytes; limit > 0 && len(join.GetMetadata()) > limit {
		return nil, &httpError{http.StatusBadRequest, errors.New("metadata is too large")}
	}
	if m := join.GetMetadata(); m != "" {
		g.Metadata = m
	}
	if attributes := join.GetParticipantAttributes(); len(attributes) > 0 {
		merged := make(map[string]string, len(g.Attributes)+len(attributes))
		for k, v := range g.Attributes {
			merged[k] = v
		}
		for k, v := range attributes {
			merged[k] = v
		}
		g.Attributes = merged
	}
	return cp, nil
}

// httpError carries a status with its message. Clients branch on the message text.
type httpError struct {
	status int
	err    error
}

func (s *Service) parseConnect(r *http.Request) (*connectParams, *httpError) {
	token := tokenOf(r)
	if token == "" {
		return nil, &httpError{http.StatusUnauthorized, auth.ErrInvalidToken}
	}
	v, err := auth.Verify(token, s.keys)
	if err != nil {
		return nil, &httpError{http.StatusUnauthorized, err}
	}
	g := v.Grants
	if !g.Video.RoomJoin {
		return nil, &httpError{http.StatusUnauthorized, errors.New("permissions denied")}
	}
	if g.Identity == "" {
		return nil, &httpError{http.StatusBadRequest, errors.New("identity cannot be empty")}
	}
	if g.Video.Room == "" {
		return nil, &httpError{http.StatusBadRequest, errors.New("no room name")}
	}

	cp := &connectParams{
		verified:          v,
		roomName:          g.Video.Room,
		client:            parseClientInfo(r),
		autoSubscribe:     true,
		autoSubscribeData: boolParam(r, "auto_subscribe_data_track"),
		adaptiveStream:    boolParam(r, "adaptive_stream"),
		reconnect:         boolParam(r, "reconnect"),
		participantSID:    r.FormValue("sid"),
	}
	if v := r.FormValue("auto_subscribe"); v != "" {
		cp.autoSubscribe = isTrue(v)
	}
	if v := r.FormValue("subscriber_allow_pause"); v != "" {
		allow := isTrue(v)
		cp.allowPause = &allow
	}
	if v, err := strconv.Atoi(r.FormValue("reconnect_reason")); err == nil {
		cp.reconnectFor = pb.ReconnectReason(v)
	}
	return cp, nil
}

var errOriginDenied = errors.New("this web origin is not allowed to connect to the project")

// originAllowed applies a project's allowed origins. A token already proves the caller's rights, so this
// is a second fence: it limits the damage of a token leaked to another site. Requests with no Origin come
// from outside a browser, and pass.
func (s *Service) originAllowed(r *http.Request, cp *connectParams) bool {
	allowed := s.rooms.ProjectConfigFor(cp.verified.Key.ProjectID).AllowedOrigins
	origin := r.Header.Get("Origin")
	if len(allowed) == 0 || origin == "" {
		return true
	}
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

func isTrue(v string) bool { return v == "1" || strings.EqualFold(v, "true") }

func boolParam(r *http.Request, name string) bool { return isTrue(r.FormValue(name)) }

func parseClientInfo(r *http.Request) *pb.ClientInfo {
	info := &pb.ClientInfo{
		Version:        r.FormValue("version"),
		Os:             r.FormValue("os"),
		OsVersion:      r.FormValue("os_version"),
		DeviceModel:    r.FormValue("device_model"),
		Browser:        r.FormValue("browser"),
		BrowserVersion: r.FormValue("browser_version"),
		Network:        r.FormValue("network"),
		Address:        r.RemoteAddr,
	}
	if v, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		info.Protocol = int32(v)
	}
	if v, err := strconv.Atoi(r.FormValue("client_protocol")); err == nil {
		info.ClientProtocol = int32(v)
	}
	if sdk, ok := pb.ClientInfo_SDK_value[strings.ToUpper(r.FormValue("sdk"))]; ok {
		info.Sdk = pb.ClientInfo_SDK(sdk)
	}
	return info
}

func (s *Service) handleValidate(w http.ResponseWriter, r *http.Request, parse func(*http.Request) (*connectParams, *httpError)) {
	cp, herr := parse(r)
	if herr != nil {
		http.Error(w, herr.err.Error(), herr.status)
		return
	}
	if !s.originAllowed(r, cp) {
		http.Error(w, errOriginDenied.Error(), http.StatusUnauthorized)
		return
	}
	// A client reconnecting needs no room check: its session either exists or it will start over.
	if !cp.reconnect {
		existing := s.rooms.Get(cp.verified.Key.ProjectID, cp.roomName)
		if existing == nil && !s.cfg.Room.AutoCreate {
			http.Error(w, room.ErrRoomNotFound.Error(), http.StatusNotFound)
			return
		}
		// Browsers hide why a WebSocket failed, so clients ask here. Reporting a full room as 401 stops the
		// client with a readable error; a 403 would send it looking for another region.
		if existing != nil && existing.IsFull() && existing.Participant(cp.verified.Grants.Identity) == nil {
			http.Error(w, room.ErrMaxParticipants.Error(), http.StatusUnauthorized)
			return
		}
		if err := s.rooms.CheckAdmission(cp.verified.Key.ProjectID, cp.roomName, cp.verified.Grants); err != nil {
			http.Error(w, err.Error(), statusFor(err))
			return
		}
	}
	_, _ = w.Write([]byte("success"))
}

func (s *Service) handleConnect(w http.ResponseWriter, r *http.Request, parse func(*http.Request) (*connectParams, *httpError)) {
	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	cp, herr := parse(r)
	if herr != nil {
		metrics.SignalConnects.WithLabelValues("rejected").Inc()
		http.Error(w, herr.err.Error(), herr.status)
		return
	}

	if !s.originAllowed(r, cp) {
		metrics.SignalConnects.WithLabelValues("rejected").Inc()
		http.Error(w, errOriginDenied.Error(), http.StatusUnauthorized)
		return
	}

	conn := newConn(s.log)
	req := room.JoinRequest{
		RoomName:             cp.roomName,
		Verified:             cp.verified,
		Client:               cp.client,
		AutoSubscribe:        cp.autoSubscribe,
		AutoSubscribeData:    cp.autoSubscribeData,
		AdaptiveStream:       cp.adaptiveStream,
		SubscriberAllowPause: cp.allowPause,
		SinglePC:             cp.join != nil,
		Sink:                 conn,
	}

	// Set up the session before upgrading, so failures still reach the client as HTTP errors. The
	// session's first messages wait in the connection's queue.
	var p *participant.Participant
	var err error
	if cp.reconnect {
		p, err = s.rooms.Resume(req, cp.participantSID, cp.reconnectFor)
		if errors.Is(err, room.ErrCannotResume) {
			s.rejectResume(w, r, conn)
			return
		}
	} else {
		p, err = s.rooms.Join(req)
	}
	if err != nil {
		metrics.SignalConnects.WithLabelValues("failed").Inc()
		http.Error(w, err.Error(), statusFor(err))
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		metrics.SignalConnects.WithLabelValues("upgrade_failed").Inc()
		p.SignalClosed(conn)
		return
	}
	metrics.SignalConnects.WithLabelValues("connected").Inc()
	conn.attach(ws)
	// The tracks and offer that came with the join. They go after the join response, and before anything the client sends
	// on this connection: the tracks first, because the offer refers to them.
	for _, add := range cp.join.GetAddTrackRequests() {
		p.HandleRequest(&pb.SignalRequest{Message: &pb.SignalRequest_AddTrack{AddTrack: add}})
	}
	if offer := cp.join.GetPublisherOffer(); offer != nil {
		p.HandleRequest(&pb.SignalRequest{Message: &pb.SignalRequest_Offer{Offer: offer}})
	}
	s.serve(conn, ws, p)
}

// rejectResume tells a client whose session is gone to start over. It has to be a leave message over an
// established connection: an HTTP error here would make the client keep retrying the resume.
func (s *Service) rejectResume(w http.ResponseWriter, r *http.Request, conn *conn) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.attach(ws)
	_ = conn.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Leave{Leave: &pb.LeaveRequest{
		CanReconnect: true,
		Reason:       pb.DisconnectReason_STATE_MISMATCH,
		Action:       pb.LeaveRequest_RECONNECT,
	}}})
	conn.Close("session not found")
}

func statusFor(err error) int {
	// As with a full room, 401 stops the client with a readable error, and sends it nowhere else to try.
	var refused *room.RefusedError
	if errors.As(err, &refused) {
		return http.StatusUnauthorized
	}
	switch {
	case errors.Is(err, room.ErrRoomNotFound):
		return http.StatusNotFound
	case errors.Is(err, room.ErrMaxParticipants):
		return http.StatusUnauthorized
	case errors.Is(err, room.ErrRoomClosed):
		return http.StatusGone
	case errors.Is(err, room.ErrDraining):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// serve reads client messages until the connection ends.
func (s *Service) serve(c *conn, ws *websocket.Conn, p *participant.Participant) {
	ws.SetReadLimit(s.cfg.Limits.MaxSignalMessageBytes)
	defer func() {
		p.SignalClosed(c)
		c.Close("read loop ended")
	}()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
		kind, data, err := ws.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				s.log.Debug("signal read ended", "participant", p.Identity(), "error", err)
			}
			return
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		req := &pb.SignalRequest{}
		if err := proto.Unmarshal(data, req); err != nil {
			s.log.Debug("bad signal message", "participant", p.Identity(), "error", err)
			continue
		}
		// Pings stay on the connection so the reply does not wait behind slower requests.
		switch m := req.GetMessage().(type) {
		case *pb.SignalRequest_Ping:
			_ = c.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Pong{Pong: time.Now().UnixMilli()}})
		case *pb.SignalRequest_PingReq:
			_ = c.Send(&pb.SignalResponse{Message: &pb.SignalResponse_PongResp{PongResp: &pb.Pong{
				LastPingTimestamp: m.PingReq.GetTimestamp(),
				Timestamp:         time.Now().UnixMilli(),
			}}})
		default:
			p.HandleRequest(req)
		}
	}
}

var errConnClosed = errors.New("signal connection closed")

// conn is the outgoing half of a signalling connection. Messages queue from the moment it is created,
// which can be before the WebSocket upgrade finishes.
type conn struct {
	log *slog.Logger

	mu     sync.Mutex
	closed bool
	queue  chan []byte
	done   chan struct{}
}

func newConn(log *slog.Logger) *conn {
	return &conn{log: log, queue: make(chan []byte, outboundQueueSize), done: make(chan struct{})}
}

// attach starts writing to the WebSocket.
func (c *conn) attach(ws *websocket.Conn) { go c.writeLoop(ws) }

func (c *conn) writeLoop(ws *websocket.Conn) {
	defer func() { _ = ws.Close() }()
	for {
		select {
		case data := <-c.queue:
			if !c.write(ws, data) {
				return
			}
		case <-c.done:
			// Flush what was queued before closing, such as a final leave message.
			for {
				select {
				case data := <-c.queue:
					if !c.write(ws, data) {
						return
					}
				default:
					_ = ws.SetWriteDeadline(time.Now().Add(time.Second))
					_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
					return
				}
			}
		}
	}
}

func (c *conn) write(ws *websocket.Conn, data []byte) bool {
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return ws.WriteMessage(websocket.BinaryMessage, data) == nil
}

// Send implements participant.Sink.
func (c *conn) Send(msg *pb.SignalResponse) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errConnClosed
	}
	select {
	case c.queue <- data:
		return nil
	default:
		c.log.Warn("signal queue full, closing connection")
		c.closed = true
		close(c.done)
		return errConnClosed
	}
}

// Close implements participant.Sink. It is safe to call more than once.
func (c *conn) Close(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
}
