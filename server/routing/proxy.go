package routing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/joinrequest"
)

// hopHeader marks a proxied request. A node that receives one always handles it, even if its routing has since changed,
// because the alternative is requests bouncing between nodes. That is a power worth having, since handling a request here
// can make a second copy of a room, so the value is signed. From anyone else, the header means nothing.
const hopHeader = "X-StreamKit-Forwarded-By"

// hopMaxAge is how long a signed hop is good for, either way, as nodes' clocks can differ a little. It only limits replaying
// one seen on the network between nodes.
const hopMaxAge = time.Minute

const (
	routeTimeout      = 2 * time.Second
	maxInspectedBytes = 1 << 20
)

// Proxy sends room requests to the node hosting the room.
type Proxy struct {
	router Router
	keys   auth.KeyProvider
	secret []byte
	log    *slog.Logger

	mu      sync.Mutex
	proxies map[string]*httputil.ReverseProxy
}

// NewProxy creates a Proxy. secret is what the nodes of a cluster all share to prove a hop came from one of them.
func NewProxy(router Router, keys auth.KeyProvider, secret []byte, log *slog.Logger) *Proxy {
	return &Proxy{router: router, keys: keys, secret: secret, log: log, proxies: map[string]*httputil.ReverseProxy{}}
}

func hopSignature(secret []byte, node, at, method, path string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%s", node, at, method, path)
	return mac.Sum(nil)
}

// SignHop makes the value of the hop header for a request one node is passing to another.
func SignHop(secret []byte, node, method, path string, at time.Time) string {
	when := strconv.FormatInt(at.Unix(), 10)
	return node + ";" + when + ";" + hex.EncodeToString(hopSignature(secret, node, when, method, path))
}

// FromPeer reports whether another node of this cluster passed this request on. Such a request has been through this
// check already, at the node that first took it.
func (p *Proxy) FromPeer(r *http.Request) bool {
	if p.router.IsLocal() {
		return false
	}
	parts := strings.Split(r.Header.Get(hopHeader), ";")
	if len(parts) != 3 {
		return false
	}
	at, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	if age := time.Since(time.Unix(at, 0)); age > hopMaxAge || age < -hopMaxAge {
		return false
	}
	got, err := hex.DecodeString(parts[2])
	return err == nil && hmac.Equal(got, hopSignature(p.secret, parts[0], parts[1], r.Method, r.URL.Path))
}

// target says which room a request is about.
type target struct {
	project string
	room    string
	create  bool
}

// Wrap returns next with routing in front of it.
func (p *Proxy) Wrap(next http.Handler) http.Handler {
	if p.router.IsLocal() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.FromPeer(r) {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del(hopHeader)
		t, ok := p.targetOf(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
		node, found, err := p.router.Route(ctx, t.project, t.room, t.create)
		cancel()
		if err != nil {
			if t.create {
				p.log.Warn("could not route room", "room", t.room, "error", err)
				http.Error(w, "the server is temporarily unable to route this room", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !found || node.ID == p.router.SelfID() || node.URL == "" {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Set(hopHeader, SignHop(p.secret, p.router.SelfID(), r.Method, r.URL.Path, time.Now()))
		p.proxyFor(node.URL).ServeHTTP(w, r)
	})
}

func (p *Proxy) proxyFor(rawURL string) *httputil.ReverseProxy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rp := p.proxies[rawURL]; rp != nil {
		return rp
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		u = &url.URL{Scheme: "http", Host: rawURL}
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.Host = pr.In.Host
		// The next node should see the same client this one did, not that a proxy passed through here.
		for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto"} {
			if v, ok := pr.In.Header[name]; ok {
				pr.Out.Header[name] = v
			}
		}
		if _, ok := pr.Out.Header["X-Forwarded-For"]; !ok {
			if host, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				pr.Out.Header.Set("X-Forwarded-For", host)
			}
		}
	}}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.log.Warn("could not reach the node hosting a room", "node", rawURL, "error", err)
		http.Error(w, "the server hosting this room cannot be reached", http.StatusBadGateway)
	}
	p.proxies[rawURL] = rp
	return rp
}

func (p *Proxy) projectFromToken(raw string) (string, *auth.Verified) {
	if raw == "" {
		return "", nil
	}
	v, err := auth.Verify(raw, p.keys)
	if err != nil {
		return "", nil
	}
	return v.Key.ProjectID, v
}

func (p *Proxy) targetOf(r *http.Request) (target, bool) {
	path := r.URL.Path
	switch {
	case path == "/rtc" || path == "/rtc/validate" || path == "/rtc/v1" || path == "/rtc/v1/validate":
		token := r.FormValue("access_token")
		if token == "" {
			token = auth.TokenFromHeader(r.Header.Get("Authorization"))
		}
		project, v := p.projectFromToken(token)
		if v == nil || v.Grants.Video.Room == "" {
			return target{}, false
		}
		// A resume must find its session, and must not start a new room somewhere else. On /rtc/v1 the join request says
		// whether this is one. One that cannot be read creates nothing either, and the service refuses it with a 400.
		create := path == "/rtc" && r.FormValue("reconnect") != "1"
		if path == "/rtc/v1" {
			join, err := joinrequest.Decode(r.FormValue("join_request"))
			create = err == nil && !join.GetReconnect()
		}
		return target{project, v.Grants.Video.Room, create}, true

	case strings.HasPrefix(path, "/twirp/livekit.RoomService/"):
		return p.twirpTarget(r, strings.TrimPrefix(path, "/twirp/livekit.RoomService/"))

	case strings.HasPrefix(path, "/twirp/livekit.AgentDispatchService/"):
		return p.twirpTarget(r, strings.TrimPrefix(path, "/twirp/livekit.AgentDispatchService/"))

	case strings.HasPrefix(path, "/api/v1/"):
		return p.restTarget(r, strings.TrimPrefix(path, "/api/v1/"))
	}
	return target{}, false
}

// twirpRoomField gives the request field holding the room, for each method that concerns one room.
var twirpRoomField = map[string]struct {
	field  protoreflect.Name
	create bool
}{
	"CreateRoom":          {"name", true},
	"DeleteRoom":          {"room", false},
	"ListParticipants":    {"room", false},
	"GetParticipant":      {"room", false},
	"RemoveParticipant":   {"room", false},
	"MutePublishedTrack":  {"room", false},
	"UpdateParticipant":   {"room", false},
	"UpdateSubscriptions": {"room", false},
	"SendData":            {"room", false},
	"UpdateRoomMetadata":  {"room", false},
	"CreateDispatch":      {"room", true},
	"DeleteDispatch":      {"room", false},
	"ListDispatch":        {"room", false},
}

var twirpRequestTypes = map[string]proto.Message{
	"CreateRoom": &pb.CreateRoomRequest{}, "DeleteRoom": &pb.DeleteRoomRequest{}, "ListParticipants": &pb.ListParticipantsRequest{},
	"GetParticipant": &pb.RoomParticipantIdentity{}, "RemoveParticipant": &pb.RoomParticipantIdentity{},
	"MutePublishedTrack": &pb.MuteRoomTrackRequest{}, "UpdateParticipant": &pb.UpdateParticipantRequest{},
	"UpdateSubscriptions": &pb.UpdateSubscriptionsRequest{}, "SendData": &pb.SendDataRequest{}, "UpdateRoomMetadata": &pb.UpdateRoomMetadataRequest{},
	"CreateDispatch": &pb.CreateAgentDispatchRequest{}, "DeleteDispatch": &pb.DeleteAgentDispatchRequest{}, "ListDispatch": &pb.ListAgentDispatchRequest{},
}

func (p *Proxy) twirpTarget(r *http.Request, method string) (target, bool) {
	spec, ok := twirpRoomField[method]
	if !ok {
		return target{}, false
	}
	project, _ := p.projectFromToken(auth.TokenFromHeader(r.Header.Get("Authorization")))
	if project == "" {
		return target{}, false
	}
	body, ok := readBody(r)
	if !ok {
		return target{}, false
	}
	msg := dynamicpb.NewMessage(twirpRequestTypes[method].ProtoReflect().Descriptor())
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, msg)
	} else {
		err = proto.Unmarshal(body, msg)
	}
	if err != nil {
		return target{}, false
	}
	field := msg.Descriptor().Fields().ByName(spec.field)
	room := msg.Get(field).String()
	if room == "" {
		return target{}, false
	}
	return target{project, room, spec.create}, true
}

func readBody(r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxInspectedBytes))
	if err != nil {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

func (p *Proxy) restTarget(r *http.Request, rest string) (target, bool) {
	project := r.Header.Get("X-Project-ID")
	if project == "" {
		project = r.URL.Query().Get("project_id")
	}
	if project == "" {
		project, _ = p.projectFromToken(auth.TokenFromHeader(r.Header.Get("Authorization")))
	}
	if project == "" {
		return target{}, false
	}
	parts := strings.Split(rest, "/")
	switch parts[0] {
	case "rooms":
		switch {
		case len(parts) == 1 && r.Method == http.MethodPost:
			body, ok := readBody(r)
			if !ok {
				return target{}, false
			}
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(body, &in) != nil || in.Name == "" {
				return target{}, false
			}
			return target{project, in.Name, true}, true
		case len(parts) == 2 && parts[1] != "history":
			return target{project, parts[1], false}, true
		}
	case "meetings":
		// Making a meeting opens no room, so only what is under one meeting has a node to go to.
		if len(parts) >= 2 {
			return target{project, parts[1], false}, true
		}
	case "participants", "tracks":
		if len(parts) >= 2 {
			return target{project, parts[1], false}, true
		}
		if room := r.URL.Query().Get("room"); room != "" {
			return target{project, room, false}, true
		}
	}
	return target{}, false
}
