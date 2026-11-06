package routing

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
)

func TestLeastLoaded(t *testing.T) {
	nodes := []Node{
		{ID: "busy", CPUPercent: 90},
		{ID: "many-people", CPUPercent: 15, Participants: 50},
		{ID: "few-people", CPUPercent: 12, Participants: 2},
		{ID: "draining", CPUPercent: 0, Draining: true},
	}
	got, ok := leastLoaded(nodes)
	if !ok || got.ID != "few-people" {
		t.Fatalf("got %+v", got)
	}
	if _, ok := leastLoaded([]Node{{ID: "d", Draining: true}}); ok {
		t.Fatal("a draining node took a room")
	}
	if _, ok := leastLoaded(nil); ok {
		t.Fatal("no nodes at all")
	}
}

func TestRoomKeysDoNotCollide(t *testing.T) {
	if roomKey("ab", "c") == roomKey("a", "bc") {
		t.Fatal("different project and room pairs share a key")
	}
	if roomKey("a:b", "c") == roomKey("a", "b:c") {
		t.Fatal("colons in names can collide")
	}
}

func testRedis(t *testing.T, id string) *Redis {
	t.Helper()
	url := os.Getenv("STREAMKIT_TEST_REDIS_URL")
	if url == "" {
		t.Skip("STREAMKIT_TEST_REDIS_URL is not set")
	}
	r, err := NewRedis(context.Background(), url, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func uniqueProject() string { return fmt.Sprintf("test-%d", time.Now().UnixNano()) }

func TestRouteClaimsAndFollows(t *testing.T) {
	ctx := context.Background()
	a, b := testRedis(t, "node-a-"+uniqueProject()), testRedis(t, "node-b-"+uniqueProject())
	project := uniqueProject()
	if _, err := a.Refresh(ctx, Node{ID: a.SelfID(), URL: "http://a", CPUPercent: 0}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Refresh(ctx, Node{ID: b.SelfID(), URL: "http://b", CPUPercent: 95}, nil); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := a.Route(ctx, project, "r", false); err != nil || ok {
		t.Fatalf("an unhosted room without create: ok=%v err=%v", ok, err)
	}
	first, ok, err := b.Route(ctx, project, "r", true)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if first.ID == b.SelfID() {
		t.Fatal("assigned to the busy node, though it asked")
	}
	second, ok, _ := a.Route(ctx, project, "r", false)
	if !ok || second.ID != first.ID {
		t.Fatalf("nodes disagree on the room's host: %v and %v", first.ID, second.ID)
	}

	// Releasing as a non-owner must not take the room from its owner.
	if err := b.Release(ctx, project, "r"); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := a.Route(ctx, project, "r", false); !ok || got.ID != first.ID {
		t.Fatal("a non-owner released the room")
	}
	if err := a.Release(ctx, project, "r"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.Route(ctx, project, "r", false); ok {
		t.Fatal("the room is still claimed after release")
	}
}

func TestConcurrentClaimsAgree(t *testing.T) {
	ctx := context.Background()
	project := uniqueProject()
	nodes := make([]*Redis, 8)
	for i := range nodes {
		nodes[i] = testRedis(t, fmt.Sprintf("node-%d-%s", i, project))
		if _, err := nodes[i].Refresh(ctx, Node{ID: nodes[i].SelfID(), URL: "http://x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	results := make([]string, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, ok, err := n.Route(ctx, project, "contested", true)
			if err != nil || !ok {
				t.Errorf("route: %v %v", ok, err)
				return
			}
			results[i] = got.ID
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r != results[0] {
			t.Fatalf("nodes disagree: %v", results)
		}
	}
}

func TestDeadOwnerIsReplaced(t *testing.T) {
	ctx := context.Background()
	project := uniqueProject()
	live := testRedis(t, "live-"+project)
	if _, err := live.Refresh(ctx, Node{ID: live.SelfID(), URL: "http://live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := live.client.Set(ctx, roomKey(project, "r"), "a-node-that-no-longer-exists", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := live.Route(ctx, project, "r", false); ok {
		t.Fatal("a dead owner should not count, without create")
	}
	got, ok, err := live.Route(ctx, project, "r", true)
	if err != nil || !ok || got.ID != live.SelfID() {
		t.Fatalf("got %+v, %v, %v", got, ok, err)
	}
}

func TestRefreshKeepsAClaimOnlyIfNoLiveNodeHasIt(t *testing.T) {
	ctx := context.Background()
	project := uniqueProject()
	a, b := testRedis(t, "a-"+project), testRedis(t, "b-"+project)
	self := Node{ID: a.SelfID(), URL: "http://a"}
	refs := []RoomRef{{project, "nobodys"}, {project, "mine"}, {project, "theirs"}, {project, "a-dead-nodes"}}
	if _, err := b.Refresh(ctx, Node{ID: b.SelfID(), URL: "http://b"}, nil); err != nil {
		t.Fatal(err)
	}
	for name, owner := range map[string]string{"mine": a.SelfID(), "theirs": b.SelfID(), "a-dead-nodes": "a-node-that-no-longer-exists"} {
		if err := a.client.Set(ctx, roomKey(project, name), owner, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
	}
	lost, err := a.Refresh(ctx, self, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].Name != "theirs" {
		t.Fatalf("lost %v", lost)
	}
	for name, owner := range map[string]string{"nobodys": a.SelfID(), "mine": a.SelfID(), "theirs": b.SelfID(), "a-dead-nodes": a.SelfID()} {
		if got, _ := a.client.Get(ctx, roomKey(project, name)).Result(); got != owner {
			t.Errorf("%s is claimed by %s, want %s", name, got, owner)
		}
	}
	// A refresh keeps a claim going, and one that Redis lost comes back.
	if err := a.client.PExpire(ctx, roomKey(project, "mine"), time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(ctx, self, refs[:2]); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := a.client.PTTL(ctx, roomKey(project, "mine")).Result(); ttl < time.Second*2 {
		t.Fatalf("the claim was not renewed: %v", ttl)
	}
	if err := a.client.Del(ctx, roomKey(project, "nobodys")).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(ctx, self, refs[:2]); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.client.Get(ctx, roomKey(project, "nobodys")).Result(); got != a.SelfID() {
		t.Fatalf("a lost claim did not come back: %q", got)
	}
}

func TestLocalRouterNeverProxies(t *testing.T) {
	called := false
	h := NewProxy(Local{ID: "n"}, auth.StaticKeys{}, testHopSecret, slog.New(slog.DiscardHandler)).Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/rtc", nil))
	if !called {
		t.Fatal("not handled locally")
	}
}

// fakeRouter sends every room to one node.
type fakeRouter struct {
	self  string
	node  Node
	found bool
	calls []string
	mu    sync.Mutex
}

func (f *fakeRouter) Route(_ context.Context, project, room string, create bool) (Node, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/%v", project, room, create))
	return f.node, f.found, nil
}
func (*fakeRouter) Release(context.Context, string, string) error { return nil }
func (*fakeRouter) Refresh(context.Context, Node, []RoomRef) ([]RoomRef, error) {
	return nil, nil
}
func (*fakeRouter) Nodes(context.Context) ([]Node, error) { return nil, nil }
func (f *fakeRouter) SelfID() string                      { return f.self }
func (*fakeRouter) IsLocal() bool                         { return false }

var testHopSecret = []byte("a shared secret")

// joinRequest encodes the join_request parameter of the single peer connection protocol, gzipped as a browser sends it.
func joinRequest(t *testing.T, reconnect bool) string {
	t.Helper()
	contents, err := proto.Marshal(&pb.JoinRequest{Reconnect: reconnect})
	if err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	if _, err := w.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&pb.WrappedJoinRequest{Compression: pb.WrappedJoinRequest_GZIP, JoinRequest: zipped.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func TestProxyRoutesRequestsAboutRooms(t *testing.T) {
	keys := auth.StaticKeys{ProjectID: "proj", Keys: map[string]string{"k": "a-long-enough-secret-for-testing-0001"}}
	key, _ := keys.Lookup("k")
	token, _ := auth.Mint(key, &auth.Grants{Identity: "i", Video: &auth.VideoGrant{RoomJoin: true, Room: "tokroom"}}, time.Minute)

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _, _ := strings.Cut(r.Header.Get(hopHeader), ";")
		fmt.Fprintf(w, "remote:%s", from)
	}))
	defer remote.Close()
	router := &fakeRouter{self: "me", node: Node{ID: "other", URL: remote.URL}, found: true}
	h := NewProxy(router, keys, testHopSecret, slog.New(slog.DiscardHandler)).Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("local"))
	}))
	rtc := "/rtc?access_token=" + token
	signed := func(secret []byte, method, path string, at time.Time) map[string]string {
		return map[string]string{hopHeader: SignHop(secret, "elsewhere", method, path, at)}
	}

	cases := []struct {
		name, method, path, body string
		headers                  map[string]string
		want                     string
		call                     string
	}{
		{"join", "GET", "/rtc?access_token=" + token, "", nil, "remote:me", "proj/tokroom/true"},
		{"resume must not create", "GET", "/rtc?reconnect=1&access_token=" + token, "", nil, "remote:me", "proj/tokroom/false"},
		{"validate", "GET", "/rtc/validate?access_token=" + token, "", nil, "remote:me", "proj/tokroom/false"},
		{"single connection join", "GET", "/rtc/v1?join_request=" + joinRequest(t, false) + "&access_token=" + token, "", nil, "remote:me", "proj/tokroom/true"},
		{"single connection resume must not create", "GET", "/rtc/v1?join_request=" + joinRequest(t, true) + "&access_token=" + token, "", nil, "remote:me", "proj/tokroom/false"},
		{"single connection validate", "GET", "/rtc/v1/validate?join_request=" + joinRequest(t, false) + "&access_token=" + token, "", nil, "remote:me", "proj/tokroom/false"},
		{"an unreadable join request must not create", "GET", "/rtc/v1?join_request=garbage&access_token=" + token, "", nil, "remote:me", "proj/tokroom/false"},
		{"bad token stays local", "GET", "/rtc?access_token=garbage", "", nil, "local", ""},
		{"twirp json", "POST", "/twirp/livekit.RoomService/DeleteRoom", `{"room":"r1"}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token}, "remote:me", "proj/r1/false"},
		{"twirp create", "POST", "/twirp/livekit.RoomService/CreateRoom", `{"name":"r2"}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token}, "remote:me", "proj/r2/true"},
		{"agent dispatch", "POST", "/twirp/livekit.AgentDispatchService/CreateDispatch", `{"room":"r3","agent_name":"a"}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token}, "remote:me", "proj/r3/true"},
		{"agent dispatch list", "POST", "/twirp/livekit.AgentDispatchService/ListDispatch", `{"room":"r3"}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token}, "remote:me", "proj/r3/false"},
		{"twirp list stays local", "POST", "/twirp/livekit.RoomService/ListRooms", `{}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token}, "local", ""},
		{"rest room", "GET", "/api/v1/rooms/r3", "", map[string]string{"X-Project-ID": "p9"}, "remote:me", "p9/r3/false"},
		{"rest create", "POST", "/api/v1/rooms", `{"name":"r4"}`, map[string]string{"X-Project-ID": "p9"}, "remote:me", "p9/r4/true"},
		{"rest history stays local", "GET", "/api/v1/rooms/history", "", map[string]string{"X-Project-ID": "p9"}, "local", ""},
		{"rest list stays local", "GET", "/api/v1/rooms", "", map[string]string{"X-Project-ID": "p9"}, "local", ""},
		{"rest participant", "DELETE", "/api/v1/participants/r5/alice", "", map[string]string{"X-Project-ID": "p9"}, "remote:me", "p9/r5/false"},
		{"rest participants query", "GET", "/api/v1/participants?room=r6", "", map[string]string{"X-Project-ID": "p9"}, "remote:me", "p9/r6/false"},
		{"rest track", "POST", "/api/v1/tracks/r7/alice/TR/mute", `{}`, map[string]string{"X-Project-ID": "p9"}, "remote:me", "p9/r7/false"},
		{"other routes stay local", "GET", "/api/v1/projects", "", nil, "local", ""},
		{"a proxied request is never proxied again", "GET", rtc, "", signed(testHopSecret, "GET", "/rtc", time.Now()), "local", ""},
		{"a hop anyone could type gets routed like anything else", "GET", rtc, "", map[string]string{hopHeader: "elsewhere"}, "remote:me", "proj/tokroom/true"},
		{"a hop signed with another key", "GET", rtc, "", signed([]byte("not the secret"), "GET", "/rtc", time.Now()), "remote:me", "proj/tokroom/true"},
		{"a hop seen on one path, tried on another", "GET", rtc, "", signed(testHopSecret, "GET", "/rtc/validate", time.Now()), "remote:me", "proj/tokroom/true"},
		{"a hop with another method", "GET", rtc, "", signed(testHopSecret, "POST", "/rtc", time.Now()), "remote:me", "proj/tokroom/true"},
		{"an old hop", "GET", rtc, "", signed(testHopSecret, "GET", "/rtc", time.Now().Add(-2*hopMaxAge)), "remote:me", "proj/tokroom/true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router.calls = nil
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			body, _ := io.ReadAll(rec.Body)
			if string(body) != c.want {
				t.Fatalf("response %q, want %q", body, c.want)
			}
			got := ""
			if len(router.calls) > 0 {
				got = router.calls[0]
			}
			if got != c.call {
				t.Fatalf("route call %q, want %q", got, c.call)
			}
		})
	}
}

func TestProxyPreservesBody(t *testing.T) {
	keys := auth.StaticKeys{ProjectID: "proj", Keys: map[string]string{"k": "a-long-enough-secret-for-testing-0001"}}
	key, _ := keys.Lookup("k")
	token, _ := auth.Mint(key, &auth.Grants{Identity: "i", Video: &auth.VideoGrant{RoomAdmin: true, Room: "r"}}, time.Minute)
	var received string
	remote := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
	}))
	defer remote.Close()
	router := &fakeRouter{self: "me", node: Node{ID: "other", URL: remote.URL}, found: true}
	h := NewProxy(router, keys, testHopSecret, slog.New(slog.DiscardHandler)).Wrap(http.NotFoundHandler())
	req := httptest.NewRequest("POST", "/twirp/livekit.RoomService/UpdateRoomMetadata", strings.NewReader(`{"room":"r","metadata":"m"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if received != `{"room":"r","metadata":"m"}` {
		t.Fatalf("the proxied body changed: %q", received)
	}
}

// The node that gets a request passed to it should see the same client as the first one did, for rate limits and audit logs.
func TestProxyKeepsTheClientAddress(t *testing.T) {
	var got, host string
	remote := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, host = r.Header.Get("X-Forwarded-For"), r.Host
	}))
	defer remote.Close()
	router := &fakeRouter{self: "me", node: Node{ID: "other", URL: remote.URL}, found: true}
	h := NewProxy(router, auth.StaticKeys{}, testHopSecret, slog.New(slog.DiscardHandler)).Wrap(http.NotFoundHandler())
	req := httptest.NewRequest("GET", "/api/v1/rooms/x", nil)
	req.Host = "rtc.example.com"
	req.Header.Set("X-Project-ID", "p")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "203.0.113.9" {
		t.Fatalf("the next node saw X-Forwarded-For: %q", got)
	}
	if host != "rtc.example.com" {
		t.Fatalf("the next node saw Host: %q", host)
	}
}

func TestProxyHandlesUnreachableNode(t *testing.T) {
	router := &fakeRouter{self: "me", node: Node{ID: "gone", URL: "http://127.0.0.1:1"}, found: true}
	h := NewProxy(router, auth.StaticKeys{}, testHopSecret, slog.New(slog.DiscardHandler)).Wrap(http.NotFoundHandler())
	req := httptest.NewRequest("GET", "/api/v1/rooms/x", nil)
	req.Header.Set("X-Project-ID", "p")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
}
