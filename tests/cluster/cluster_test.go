// Package cluster runs real server processes sharing Redis and PostgreSQL, as make test-multinode does, for what can only
// go wrong between nodes. No browser and no media: signalling is enough to tell which node holds a room. Set
// STREAMKIT_TEST_DATABASE_URL and STREAMKIT_TEST_REDIS_URL to run them. Each test takes a Redis database of its own and
// empties it, so point the second at an instance where databases 10 to 15 are free to use.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/routing"
)

const (
	testMasterKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	password      = "a-good-long-password"
	// How often a node refreshes its claims, and how long it can take to hear of something it has to act on.
	refreshInterval = 3 * time.Second
)

var devKey = auth.Key{APIKey: "devkey", Secret: "secret", ProjectID: "default"}

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// serverBinary builds the server once for the whole run.
func serverBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "streamkit-cluster-")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "streamkit-server")
		cmd := exec.Command("go", "build", "-o", binary, "elnova-streamkit.local/server/cmd/streamkit-server")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%w: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

// gate stands between a node and Redis, so a test can take Redis away from one node and leave it for the others.
type gate struct {
	listener net.Listener
	target   string

	mu    sync.Mutex
	shut  bool
	conns map[net.Conn]bool
}

func newGate(t *testing.T, target string) *gate {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &gate{listener: l, target: target, conns: map[net.Conn]bool{}}
	t.Cleanup(func() { _ = l.Close(); g.close(true) })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go g.serve(c)
		}
	}()
	return g
}

func (g *gate) serve(c net.Conn) {
	g.mu.Lock()
	if g.shut {
		g.mu.Unlock()
		_ = c.Close()
		return
	}
	g.mu.Unlock()
	up, err := net.Dial("tcp", g.target)
	if err != nil {
		_ = c.Close()
		return
	}
	g.mu.Lock()
	g.conns[c], g.conns[up] = true, true
	g.mu.Unlock()
	go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
	_, _ = io.Copy(c, up)
	_ = c.Close()
}

// close stops traffic through the gate, until open.
func (g *gate) close(shut bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut = shut
	for c := range g.conns {
		_ = c.Close()
	}
	g.conns = map[net.Conn]bool{}
}

type node struct {
	http  string
	ws    string
	gate  *gate
	child *exec.Cmd
}

type cluster struct {
	t     *testing.T
	nodes []*node
	redis *redis.Client
	db    string
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// trustProxy is for the control plane's part of the config: this server believes X-Forwarded-For.
const trustProxy = "  trust_proxy_headers: true\n"

// newCluster starts count real servers. extra is added to each one's config file, and controlPlane to the part of it about
// the control plane.
func newCluster(t *testing.T, redisDB, count int, extra string, controlPlane ...string) *cluster {
	t.Helper()
	base, redisBase := os.Getenv("STREAMKIT_TEST_DATABASE_URL"), os.Getenv("STREAMKIT_TEST_REDIS_URL")
	if base == "" || redisBase == "" {
		t.Skip("STREAMKIT_TEST_DATABASE_URL and STREAMKIT_TEST_REDIS_URL are not both set")
	}
	bin := serverBinary(t)
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("streamkit_cluster_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name

	r, err := url.Parse(redisBase)
	if err != nil {
		t.Fatal(err)
	}
	opts := &redis.Options{Addr: r.Host, DB: redisDB}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	c := &cluster{t: t, redis: client, db: u.String()}
	for range count {
		c.nodes = append(c.nodes, c.start(bin, r.Host, redisDB, extra, strings.Join(controlPlane, "")))
	}
	// A room can only go to a node the others have heard of.
	eventually(t, "the nodes to find each other", func() bool {
		ids, err := client.SMembers(ctx, "streamkit:nodes").Result()
		return err == nil && len(ids) == count
	})
	for _, n := range c.nodes {
		eventually(t, "a node to answer", func() bool {
			res, err := http.Get(n.http + "/healthz")
			if err != nil {
				return false
			}
			_ = res.Body.Close()
			return res.StatusCode == 200
		})
	}
	return c
}

func (c *cluster) start(bin, redisAddr string, redisDB int, extra, controlPlane string) *node {
	t := c.t
	t.Helper()
	port := freePort(t)
	g := newGate(t, redisAddr)
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	text := fmt.Sprintf(`port: %d
log_level: warn
rtc:
  udp_port: %d
  tcp_port: %d
redis:
  url: redis://%s/%d
  node_url: http://127.0.0.1:%d
control_plane:
  database_url: %q
  master_key: %s
  public_dashboard_url: http://localhost:3000
  allow_registration: true
  mail:
    dir: %s
%s%s`, port, freePort(t), freePort(t), g.listener.Addr(), redisDB, port, c.db, testMasterKey, dir, controlPlane, extra)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin, "--dev", "--config", config)
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
		_ = log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("server on port %d said:\n%s", port, data)
		}
	})
	return &node{http: fmt.Sprintf("http://127.0.0.1:%d", port), ws: fmt.Sprintf("ws://127.0.0.1:%d", port), gate: g, child: child}
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var roomsGauge = regexp.MustCompile(`(?m)^streamkit_rooms (\d+)`)

// rooms is how many rooms a node holds, by its own count.
func (n *node) rooms(t *testing.T) int {
	t.Helper()
	res, err := http.Get(n.http + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	m := roomsGauge.FindSubmatch(body)
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(string(m[1]))
	return v
}

// holding says which of the nodes holds a room, and which does not. There are two, and one room between them.
func (c *cluster) holding(t *testing.T) (holder, other *node) {
	t.Helper()
	a, b := c.nodes[0].rooms(t), c.nodes[1].rooms(t)
	if a+b != 1 {
		t.Fatalf("expected one room between the nodes, and they hold %d and %d", a, b)
	}
	if a == 1 {
		return c.nodes[0], c.nodes[1]
	}
	return c.nodes[1], c.nodes[0]
}

func claimKey(project, room string) string {
	return fmt.Sprintf("streamkit:room:%d:%s:%s", len(project), project, room)
}

func mint(t *testing.T, key auth.Key, g *auth.Grants) string {
	t.Helper()
	token, err := auth.Mint(key, g, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type reply struct {
	status int
	body   map[string]any
	raw    string
}

// send makes one request to a node.
func send(t *testing.T, cl *http.Client, method, address, bearer string, headers map[string]string, body any) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, address, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	out := reply{status: res.StatusCode, raw: string(data)}
	_ = json.Unmarshal(data, &out.body)
	return out
}

func want(t *testing.T, r reply, status int) reply {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.raw)
	}
	return r
}

func createRoom(t *testing.T, n *node, name string, headers map[string]string) reply {
	t.Helper()
	token := mint(t, devKey, &auth.Grants{Identity: "api", Video: &auth.VideoGrant{RoomCreate: true}})
	return send(t, http.DefaultClient, "POST", n.http+"/twirp/livekit.RoomService/CreateRoom", token, headers, map[string]any{"name": name})
}

// session is one client's signalling connection.
type session struct {
	t    *testing.T
	conn *websocket.Conn
	join *pb.JoinResponse
}

// dial opens a signalling connection and returns the HTTP status if it is refused.
func dial(t *testing.T, n *node, token string, params url.Values, headers http.Header) (*session, int, string) {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	params.Set("access_token", token)
	params.Set("protocol", "16")
	conn, res, err := websocket.DefaultDialer.Dial(n.ws+"/rtc?"+params.Encode(), headers)
	if err != nil {
		if res == nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		return nil, res.StatusCode, string(body)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &session{t: t, conn: conn}
	return s, res.StatusCode, ""
}

func connect(t *testing.T, n *node, token string) *session {
	t.Helper()
	s, status, body := dial(t, n, token, nil, nil)
	if s == nil {
		t.Fatalf("could not connect: %d %s", status, body)
	}
	if first := s.next(); first.GetJoin() == nil {
		t.Fatalf("the first message is not a join: %v", first)
	} else {
		s.join = first.GetJoin()
	}
	return s
}

func (s *session) next() *pb.SignalResponse {
	s.t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		kind, data, err := s.conn.ReadMessage()
		if err != nil {
			s.t.Fatalf("reading from the server: %v", err)
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		msg := &pb.SignalResponse{}
		if err := proto.Unmarshal(data, msg); err != nil {
			s.t.Fatal(err)
		}
		return msg
	}
}

// waitForLeave reads until the server says to leave.
func (s *session) waitForLeave() *pb.LeaveRequest {
	s.t.Helper()
	for {
		if leave := s.next().GetLeave(); leave != nil {
			return leave
		}
	}
}

// meetingOn starts a meeting through a node, signed in as someone, and returns its room and its links' codes.
type started struct {
	room, hostCode, guestCode string
	browser                   *http.Client
	project                   string
}

func startMeeting(t *testing.T, n *node, email string) started {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	want(t, send(t, browser, "POST", n.http+"/api/v1/auth/register", "", nil, map[string]string{
		"email": email, "password": password, "name": "Test", "organization": email,
	}), 201)
	project := want(t, send(t, browser, "POST", n.http+"/api/v1/projects", "", nil, map[string]string{"name": "p"}), 201).body["id"].(string)
	inProject := map[string]string{"X-Project-ID": project}
	want(t, send(t, browser, "POST", n.http+"/api/v1/keys", "", inProject, map[string]string{"name": "k"}), 201)
	made := want(t, send(t, browser, "POST", n.http+"/api/v1/meetings", "", inProject, map[string]any{"title": "Planning"}), 201)
	code := func(u any) string { _, c, _ := strings.Cut(u.(string), "#"); return c }
	return started{room: made.body["room"].(string), hostCode: code(made.body["host_url"]), guestCode: code(made.body["guest_url"]), browser: browser, project: project}
}

func joinLink(t *testing.T, n *node, code, name string) string {
	t.Helper()
	return want(t, send(t, http.DefaultClient, "POST", n.http+"/api/v1/join", "", nil, map[string]any{"code": code, "name": name}), 201).body["token"].(string)
}

// A client can send the header that marks a request as passed on by another node. Doing so must not make the node it
// reaches handle the request itself, because that is how a second copy of a room gets made.
func TestAClientCannotForgeTheHopBetweenNodes(t *testing.T) {
	c := newCluster(t, 10, 2, "")
	first := want(t, createRoom(t, c.nodes[0], "forged", nil), 200)
	_, other := c.holding(t)

	for _, header := range []string{"X-StreamKit-Forwarded-By", "X-Rea-Forwarded-By"} {
		again := want(t, createRoom(t, other, "forged", map[string]string{header: "any-node"}), 200)
		if again.body["sid"] != first.body["sid"] {
			t.Fatalf("sending %s made a second copy of the room: %s and %s", header, first.raw, again.raw)
		}
	}
	c.holding(t)
}

// Refreshing used to overwrite a claim, so a node holding a stale copy took the room back from whoever had it.
func TestRefreshDoesNotTakeARoomFromItsOwner(t *testing.T) {
	c := newCluster(t, 11, 2, "")
	ctx := context.Background()
	token := mint(t, devKey, &auth.Grants{Identity: "alice", Video: &auth.VideoGrant{RoomJoin: true, Room: "claimed"}})
	alice := connect(t, c.nodes[0], token)
	holder, other := c.holding(t)
	key := claimKey("default", "claimed")
	mine, err := c.redis.Get(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := c.redis.SMembers(ctx, "streamkit:nodes").Result()
	if err != nil {
		t.Fatal(err)
	}
	var theirs string
	for _, id := range ids {
		if id != mine {
			theirs = id
		}
	}

	// As if the holder had dropped off for a while, and the other node had taken the room meanwhile.
	if err := c.redis.Set(ctx, key, theirs, 30*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2*refreshInterval + time.Second)
	if owner, _ := c.redis.Get(ctx, key).Result(); owner != theirs {
		t.Fatalf("a refresh took the room back: the claim says %s, and it was given to %s", owner, theirs)
	}

	// The copy that lost closes, and its people are sent to find the real one.
	leave := alice.waitForLeave()
	if leave.GetAction() != pb.LeaveRequest_RECONNECT {
		t.Fatalf("the leave message says %v", leave)
	}
	eventually(t, "the stale copy to close", func() bool { return holder.rooms(t) == 0 })
	connect(t, holder, token)
	if got := other.rooms(t); got != 1 {
		t.Fatalf("reconnecting through the old holder should reach the real room, and the owner holds %d", got)
	}
	if got := holder.rooms(t); got != 0 {
		t.Fatalf("the old holder made a new copy: it holds %d", got)
	}
}

// A real hop can land on a node that has just stopped holding a room. It must not end the meeting there and call that done.
func TestEndingAMeetingOnTheWrongNodeSaysSo(t *testing.T) {
	c := newCluster(t, 13, 2, "")
	m := startMeeting(t, c.nodes[0], "owner@example.com")
	hostToken := joinLink(t, c.nodes[0], m.hostCode, "Hana")
	connect(t, c.nodes[0], hostToken)
	holder, other := c.holding(t)

	secret := (&config.Config{ControlPlane: config.ControlPlane{MasterKey: testMasterKey}}).HopSecret()
	path := "/api/v1/meetings/" + m.room + "/end"
	hop := routing.SignHop(secret, "ND_elsewhere", "POST", path, time.Now())
	r := send(t, http.DefaultClient, "POST", other.http+path, hostToken, map[string]string{"X-StreamKit-Forwarded-By": hop}, map[string]any{})
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("ending on a node that does not hold the room: %d %s", r.status, r.raw)
	}
	if holder.rooms(t) != 1 {
		t.Fatal("the meeting's room closed")
	}
	preview := want(t, send(t, http.DefaultClient, "POST", holder.http+"/api/v1/join/preview", "", nil, map[string]any{"code": m.guestCode}), 200)
	if preview.body["ended"] != false {
		t.Fatalf("the meeting says it ended, with people still in it: %s", preview.raw)
	}

	// Removing someone is the same: being told it worked means they are gone.
	guestToken := joinLink(t, c.nodes[0], m.guestCode, "Gita")
	guest := connect(t, c.nodes[0], guestToken)
	identity := guest.join.GetParticipant().GetIdentity()
	path = "/api/v1/meetings/" + m.room + "/participants/" + identity + "/remove"
	hop = routing.SignHop(secret, "ND_elsewhere", "POST", path, time.Now())
	r = send(t, http.DefaultClient, "POST", other.http+path, hostToken, map[string]string{"X-StreamKit-Forwarded-By": hop}, map[string]any{})
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("removing on a node that does not hold the room: %d %s", r.status, r.raw)
	}
	// Through any node, the normal way, it reaches them.
	want(t, send(t, http.DefaultClient, "POST", other.http+path, hostToken, nil, map[string]any{}), 204)
	if leave := guest.waitForLeave(); leave.GetReason() != pb.DisconnectReason_PARTICIPANT_REMOVED {
		t.Fatalf("the leave message says %v", leave)
	}
}

// With Redis gone, a node cannot know who holds a room. Saying a meeting has ended when it has not leaves everyone in it.
func TestEndingAMeetingWithoutRedisSaysSo(t *testing.T) {
	c := newCluster(t, 12, 2, "")
	m := startMeeting(t, c.nodes[0], "owner@example.com")
	hostToken := joinLink(t, c.nodes[0], m.hostCode, "Hana")
	host := connect(t, c.nodes[0], hostToken)
	holder, other := c.holding(t)
	end := func(n *node) reply {
		return send(t, http.DefaultClient, "POST", n.http+"/api/v1/meetings/"+m.room+"/end", hostToken, nil, map[string]any{})
	}

	other.gate.close(true)
	r := end(other)
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("ending from a node that cannot reach Redis: %d %s", r.status, r.raw)
	}
	if holder.rooms(t) != 1 {
		t.Fatal("the meeting's room closed")
	}
	preview := want(t, send(t, http.DefaultClient, "POST", holder.http+"/api/v1/join/preview", "", nil, map[string]any{"code": m.guestCode}), 200)
	if preview.body["ended"] != false {
		t.Fatalf("the meeting says it ended, with people still in it: %s", preview.raw)
	}

	// Once the node can see Redis again, the same request reaches the node holding the room, and it really ends.
	other.gate.close(false)
	eventually(t, "the node to get Redis back", func() bool {
		return send(t, http.DefaultClient, "GET", other.http+"/readyz", "", nil, nil).status == 200
	})
	want(t, end(other), 204)
	if leave := host.waitForLeave(); leave.GetReason() != pb.DisconnectReason_ROOM_DELETED {
		t.Fatalf("the leave message says %v", leave)
	}
	eventually(t, "the room to close", func() bool { return holder.rooms(t) == 0 })
}
