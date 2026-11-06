package routes

// These tests run the REST API against a real PostgreSQL. Set STREAMKIT_TEST_DATABASE_URL to run them, for example
// postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable (from docker compose up -d postgres).
// Each test creates its own database.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"elnova-streamkit.local/api/recorder"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/api/webhook"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/monitor"
	"elnova-streamkit.local/server/room"
)

const testMasterKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	store   *store.Store
	keys    *store.KeyCache
	rooms   *room.Manager
	monitor *monitor.Monitor
	mailDir string
	cfg     *config.Config
	api     *Server
}

func newHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()
	base := os.Getenv("STREAMKIT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("STREAMKIT_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("streamkit_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String(), testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		bg := context.Background()
		_, _ = admin.Exec(bg, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(bg)
	})

	cfg := config.Default()
	cfg.ControlPlane.DatabaseURL = u.String()
	cfg.ControlPlane.MasterKey = testMasterKey
	cfg.Development = true
	mailDir := t.TempDir()
	cfg.ControlPlane.Mail.Dir = mailDir
	if tweak != nil {
		tweak(cfg)
	}
	log := slog.New(slog.DiscardHandler)
	rooms := room.NewManager(ctx, cfg, nil, log)
	mon := monitor.New(rooms)
	go mon.Run(ctx)
	keys := store.NewKeyCache(st, nil)
	dispatcher := webhook.New(st, log, cfg.ControlPlane.AllowPrivateWebhooks)
	go dispatcher.Run(ctx)
	rec := recorder.New(st, mon, rooms, dispatcher, cfg, log)
	go rec.Run(ctx)
	api := New(cfg, st, keys, rooms, mon, dispatcher, NewMailer(cfg.ControlPlane.Mail, log), log)
	mux := http.NewServeMux()
	api.Register(mux)
	rooms.SetProjectConfig(api.ProjectConfig)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, store: st, keys: keys, rooms: rooms, monitor: mon, mailDir: mailDir, cfg: cfg, api: api}
}

// client is a browser: it keeps cookies and can name a project.
type client struct {
	h       *harness
	http    *http.Client
	project string
	bearer  string
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, http: &http.Client{Jar: jar}}
}

type response struct {
	status int
	body   map[string]any
	raw    string
}

func (c *client) do(method, path string, body any) response {
	c.h.t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			c.h.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, c.h.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if c.project != "" {
		req.Header.Set("X-Project-ID", c.project)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, raw: string(data)}
	_ = json.Unmarshal(data, &out.body)
	return out
}

func (r response) errCode() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func (r response) items() []any {
	items, _ := r.body["items"].([]any)
	return items
}

func expect(t *testing.T, r response, status int) response {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.raw)
	}
	return r
}

const password = "a-good-long-password"

func (h *harness) signUp(email string) *client {
	h.t.Helper()
	c := h.client()
	expect(h.t, c.do("POST", "/api/v1/auth/register", map[string]string{
		"email": email, "password": password, "name": "Test", "organization": email + " org",
	}), http.StatusCreated)
	return c
}

func (c *client) newProject(name string) string {
	c.h.t.Helper()
	r := expect(c.h.t, c.do("POST", "/api/v1/projects", map[string]string{"name": name}), http.StatusCreated)
	id := r.body["id"].(string)
	c.project = id
	return id
}

var linkToken = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

// lastMailToken reads the newest message to an address and pulls out its token.
func (h *harness) lastMailToken(email string) string {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.mailDir, "*"+strings.ReplaceAll(email, "@", "_at_")+".eml"))
	if len(files) == 0 {
		h.t.Fatalf("no mail to %s", email)
	}
	data, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		h.t.Fatal(err)
	}
	m := linkToken.FindSubmatch(data)
	if m == nil {
		h.t.Fatalf("no token in: %s", data)
	}
	return string(m[1])
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRegistrationAndLogin(t *testing.T) {
	h := newHarness(t, nil)
	first := h.signUp("first@example.com")
	me := expect(t, first.do("GET", "/api/v1/me", nil), 200)
	if !me.body["user"].(map[string]any)["is_platform_admin"].(bool) {
		t.Fatal("the first account should administer the installation")
	}
	if len(me.body["organizations"].([]any)) != 1 {
		t.Fatalf("organizations: %v", me.body["organizations"])
	}
	if strings.Contains(me.raw, "password") {
		t.Fatalf("a password field leaked into a response: %s", me.raw)
	}

	second := h.signUp("second@example.com")
	me = expect(t, second.do("GET", "/api/v1/me", nil), 200)
	if me.body["user"].(map[string]any)["is_platform_admin"].(bool) {
		t.Fatal("only the first account is an administrator")
	}

	dup := h.client().do("POST", "/api/v1/auth/register", map[string]string{"email": "FIRST@example.com", "password": password, "name": "x", "organization": "y"})
	if dup.status != 409 || dup.errCode() != "email_taken" {
		t.Fatalf("email should be case-insensitively unique: %s", dup.raw)
	}

	weak := h.client().do("POST", "/api/v1/auth/register", map[string]string{"email": "w@example.com", "password": "short", "name": "x", "organization": "y"})
	if weak.status != 422 || weak.body["error"].(map[string]any)["fields"].(map[string]any)["password"] == nil {
		t.Fatalf("weak password: %s", weak.raw)
	}

	c := h.client()
	wrongPassword := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "first@example.com", "password": "wrong-password-here"})
	noAccount := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "wrong-password-here"})
	if wrongPassword.status != 401 || wrongPassword.raw != noAccount.raw {
		t.Fatalf("login must not reveal which emails exist:\n%s\n%s", wrongPassword.raw, noAccount.raw)
	}
	if c.do("GET", "/api/v1/me", nil).status != 401 {
		t.Fatal("not signed in yet")
	}
	expect(t, c.do("POST", "/api/v1/auth/login", map[string]string{"email": "first@example.com", "password": password}), 200)
	expect(t, c.do("GET", "/api/v1/me", nil), 200)

	sessions := expect(t, c.do("GET", "/api/v1/auth/sessions", nil), 200).items()
	if len(sessions) != 2 {
		t.Fatalf("sessions: %v", sessions)
	}
	expect(t, c.do("POST", "/api/v1/auth/logout", map[string]string{}), 204)
	if c.do("GET", "/api/v1/me", nil).status != 401 {
		t.Fatal("logout did not end the session")
	}
	expect(t, first.do("GET", "/api/v1/me", nil), 200)
}

func TestSessionsAreStoredHashed(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("a@example.com")
	u, _ := url.Parse(h.srv.URL)
	var cookie string
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == sessionCookie {
			cookie = ck.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	var found int
	if err := h.store.DB.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE token_hash = $1`, []byte(cookie)).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Fatal("the session token is stored in plain form")
	}
	res, err := http.Post(h.srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"email":"a@example.com","password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	set := res.Header.Get("Set-Cookie")
	if !strings.Contains(set, "HttpOnly") || !strings.Contains(set, "SameSite=Lax") {
		t.Fatalf("cookie attributes: %s", set)
	}
}

func TestEmailVerification(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("v@example.com")
	token := h.lastMailToken("v@example.com")
	bad := c.do("POST", "/api/v1/auth/verify-email", map[string]string{"token": "made-up"})
	if bad.status != 400 {
		t.Fatalf("bad token: %s", bad.raw)
	}
	expect(t, c.do("POST", "/api/v1/auth/verify-email", map[string]string{"token": token}), 200)
	me := expect(t, c.do("GET", "/api/v1/me", nil), 200)
	if me.body["user"].(map[string]any)["email_verified_at"] == nil {
		t.Fatal("not marked verified")
	}
	if c.do("POST", "/api/v1/auth/verify-email", map[string]string{"token": token}).status != 400 {
		t.Fatal("a verification link worked twice")
	}
}

func TestRequiredVerificationBlocksLogin(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ControlPlane.RequireEmailVerification = true })
	c := h.client()
	r := expect(t, c.do("POST", "/api/v1/auth/register", map[string]string{"email": "r@example.com", "password": password, "name": "x", "organization": "y"}), 201)
	if r.body["verification_required"] != true {
		t.Fatalf("response: %s", r.raw)
	}
	if c.do("GET", "/api/v1/me", nil).status != 401 {
		t.Fatal("signed in before verifying")
	}
	login := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "r@example.com", "password": password})
	if login.status != 403 || login.errCode() != "email_not_verified" {
		t.Fatalf("login: %s", login.raw)
	}
	expect(t, c.do("POST", "/api/v1/auth/verify-email", map[string]string{"token": h.lastMailToken("r@example.com")}), 200)
	expect(t, c.do("POST", "/api/v1/auth/login", map[string]string{"email": "r@example.com", "password": password}), 200)
}

func TestPasswordReset(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("p@example.com")

	unknown := h.client().do("POST", "/api/v1/auth/forgot-password", map[string]string{"email": "nobody@example.com"})
	known := h.client().do("POST", "/api/v1/auth/forgot-password", map[string]string{"email": "p@example.com"})
	if unknown.status != 202 || unknown.raw != known.raw {
		t.Fatalf("reset must not reveal which emails exist:\n%s\n%s", unknown.raw, known.raw)
	}
	token := h.lastMailToken("p@example.com")
	short := h.client().do("POST", "/api/v1/auth/reset-password", map[string]string{"token": token, "password": "short"})
	if short.status != 422 {
		t.Fatalf("short password: %s", short.raw)
	}

	const newPassword = "another-good-long-password"
	expect(t, h.client().do("POST", "/api/v1/auth/reset-password", map[string]string{"token": token, "password": newPassword}), 200)
	if c.do("GET", "/api/v1/me", nil).status != 401 {
		t.Fatal("a password reset must end existing sessions")
	}
	if h.client().do("POST", "/api/v1/auth/login", map[string]string{"email": "p@example.com", "password": password}).status != 401 {
		t.Fatal("the old password still works")
	}
	expect(t, h.client().do("POST", "/api/v1/auth/login", map[string]string{"email": "p@example.com", "password": newPassword}), 200)
	if h.client().do("POST", "/api/v1/auth/reset-password", map[string]string{"token": token, "password": newPassword}).status != 400 {
		t.Fatal("a reset link worked twice")
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	limited := false
	for range 40 {
		if c.do("POST", "/api/v1/auth/login", map[string]string{"email": "x@example.com", "password": "wrong-password-here"}).status == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated failed logins were never limited")
	}
}

func TestCrossSiteFormsAreRejected(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("csrf@example.com")
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/projects", strings.NewReader(`{"name":"x"}`))
	// A form on another site can send the cookie, but only with these content types.
	req.Header.Set("Content-Type", "text/plain")
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t, nil)
	alice := h.signUp("alice@example.com")
	bob := h.signUp("bob@example.com")
	aliceProject := alice.newProject("alice project")
	expect(t, alice.do("POST", "/api/v1/rooms", map[string]string{"name": "private"}), 201)
	key := expect(t, alice.do("POST", "/api/v1/keys", map[string]string{"name": "k"}), 201)

	if got := expect(t, bob.do("GET", "/api/v1/projects", nil), 200).items(); len(got) != 0 {
		t.Fatalf("bob sees %v", got)
	}
	if bob.do("GET", "/api/v1/projects/"+aliceProject, nil).status != 404 {
		t.Fatal("bob can read alice's project")
	}
	if bob.do("PATCH", "/api/v1/projects/"+aliceProject, map[string]string{"name": "stolen"}).status != 404 {
		t.Fatal("bob can change alice's project")
	}
	if bob.do("DELETE", "/api/v1/projects/"+aliceProject, nil).status != 404 {
		t.Fatal("bob can delete alice's project")
	}

	bob.project = aliceProject
	for _, path := range []string{"/api/v1/rooms", "/api/v1/keys", "/api/v1/webhooks", "/api/v1/logs", "/api/v1/analytics/overview", "/api/v1/monitor", "/api/v1/audit"} {
		if r := bob.do("GET", path, nil); r.status != 404 {
			t.Errorf("%s with alice's project id: status %d: %s", path, r.status, r.raw)
		}
	}
	if bob.do("DELETE", "/api/v1/rooms/private", nil).status != 404 {
		t.Fatal("bob can close alice's room")
	}
	if bob.do("POST", "/api/v1/tokens", map[string]string{"identity": "x", "room": "private"}).status != 404 {
		t.Fatal("bob can mint a token for alice's project")
	}

	// The same room name in another project is a different room.
	bob.newProject("bob project")
	r := expect(t, bob.do("POST", "/api/v1/rooms", map[string]string{"name": "private"}), 201)
	if rooms := expect(t, bob.do("GET", "/api/v1/rooms", nil), 200).items(); len(rooms) != 1 || rooms[0].(map[string]any)["sid"] != r.body["sid"] {
		t.Fatalf("bob's rooms: %v", rooms)
	}
	if rooms := expect(t, alice.do("GET", "/api/v1/rooms", nil), 200).items(); len(rooms) != 1 || rooms[0].(map[string]any)["sid"] == r.body["sid"] {
		t.Fatalf("alice's rooms: %v", rooms)
	}

	// A key from one project cannot reach another's data.
	apiClient := h.client()
	var verified *auth.Verified
	verified, err := auth.Verify(mustMint(t, key, &auth.Grants{Identity: "x", Video: &auth.VideoGrant{RoomList: true}}), h.keys)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Key.ProjectID != aliceProject {
		t.Fatalf("project = %s", verified.Key.ProjectID)
	}
	apiClient.bearer = mustMint(t, key, &auth.Grants{Identity: "x", Video: &auth.VideoGrant{RoomList: true}})
	apiClient.project = "this-header-is-ignored-for-tokens"
	rooms := expect(t, apiClient.do("GET", "/api/v1/rooms", nil), 200).items()
	if len(rooms) != 1 || rooms[0].(map[string]any)["sid"] == r.body["sid"] {
		t.Fatalf("token sees %v", rooms)
	}
}

func mustMint(t *testing.T, key response, g *auth.Grants) string {
	t.Helper()
	token, err := auth.Mint(auth.Key{APIKey: key.body["api_key"].(string), Secret: key.body["secret"].(string)}, g, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAPIKeyLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("k@example.com")
	c.newProject("p")

	created := expect(t, c.do("POST", "/api/v1/keys", map[string]string{"name": "ci"}), 201)
	secret, _ := created.body["secret"].(string)
	apiKey := created.body["api_key"].(string)
	if len(secret) < 32 {
		t.Fatalf("weak secret: %q", secret)
	}

	listed := expect(t, c.do("GET", "/api/v1/keys", nil), 200)
	if strings.Contains(listed.raw, secret) || strings.Contains(listed.raw, `"secret"`) {
		t.Fatalf("a secret leaked in a listing: %s", listed.raw)
	}

	var stored []byte
	if err := h.store.DB.QueryRow(context.Background(), `SELECT secret_ciphertext FROM api_keys WHERE api_key = $1`, apiKey).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(secret)) {
		t.Fatal("the secret is stored in plain form")
	}

	if _, ok := h.keys.Lookup(apiKey); !ok {
		t.Fatal("a new key should verify")
	}

	rotated := expect(t, c.do("POST", "/api/v1/keys/"+created.body["id"].(string)+"/rotate", map[string]int{"overlap_seconds": 3600}), 201)
	if rotated.body["api_key"] == apiKey || rotated.body["secret"] == secret {
		t.Fatal("rotation must issue new credentials")
	}
	if _, ok := h.keys.Lookup(apiKey); !ok {
		t.Fatal("the old key should keep working during the overlap")
	}

	expect(t, c.do("DELETE", "/api/v1/keys/"+created.body["id"].(string), nil), 204)
	if _, ok := h.keys.Lookup(apiKey); ok {
		t.Fatal("a revoked key still verifies")
	}
	if _, ok := h.keys.Lookup(rotated.body["api_key"].(string)); !ok {
		t.Fatal("the replacement should still work")
	}
	if c.do("DELETE", "/api/v1/keys/"+created.body["id"].(string), nil).status != 404 {
		t.Fatal("revoking twice should report not found")
	}
}

func TestKeyWithExpiredOverlapStopsWorking(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("o@example.com")
	c.newProject("p")
	created := expect(t, c.do("POST", "/api/v1/keys", map[string]string{"name": "x"}), 201)
	expect(t, c.do("POST", "/api/v1/keys/"+created.body["id"].(string)+"/rotate", map[string]int{"overlap_seconds": 1}), 201)
	time.Sleep(1100 * time.Millisecond)
	h.keys.InvalidateAll()
	if _, ok := h.keys.Lookup(created.body["api_key"].(string)); ok {
		t.Fatal("the old key outlived its overlap")
	}
}

func TestSuspendedAndDeletedProjectsLoseKeys(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("s@example.com")
	id := c.newProject("p")
	apiKey := expect(t, c.do("POST", "/api/v1/keys", map[string]string{}), 201).body["api_key"].(string)
	expect(t, c.do("PATCH", "/api/v1/projects/"+id, map[string]string{"status": "suspended"}), 200)
	if _, ok := h.keys.Lookup(apiKey); ok {
		t.Fatal("a suspended project's key still verifies")
	}
	expect(t, c.do("PATCH", "/api/v1/projects/"+id, map[string]string{"status": "active"}), 200)
	if _, ok := h.keys.Lookup(apiKey); !ok {
		t.Fatal("reactivating should restore the key")
	}
	expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "r"}), 201)
	expect(t, c.do("DELETE", "/api/v1/projects/"+id, nil), 204)
	if _, ok := h.keys.Lookup(apiKey); ok {
		t.Fatal("a deleted project's key still verifies")
	}
	if got := h.rooms.List(id, nil); len(got) != 0 {
		t.Fatalf("a deleted project's rooms are still open: %d", len(got))
	}
}

func TestTokens(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("t@example.com")
	c.newProject("p")
	if r := c.do("POST", "/api/v1/tokens", map[string]string{"identity": "a", "room": "r"}); r.status != 409 || r.errCode() != "no_api_key" {
		t.Fatalf("no key yet: %s", r.raw)
	}
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{}), 201)
	r := expect(t, c.do("POST", "/api/v1/tokens", map[string]any{"identity": "a", "room": "r", "name": "Alice", "ttl_seconds": 60, "room_admin": true}), 201)
	verified, err := auth.Verify(r.body["token"].(string), h.keys)
	if err != nil {
		t.Fatal(err)
	}
	g := verified.Grants
	if g.Identity != "a" || g.Name != "Alice" || !g.Video.RoomJoin || g.Video.Room != "r" || !g.Video.RoomAdmin {
		t.Fatalf("grants: %+v %+v", g, g.Video)
	}
	if verified.Key.APIKey != key.body["api_key"] {
		t.Fatal("signed with the wrong key")
	}
	if time.Until(verified.ExpiresAt) > 61*time.Second {
		t.Fatalf("ttl ignored: %v", time.Until(verified.ExpiresAt))
	}

	apiClient := h.client()
	apiClient.bearer = mustMint(t, key, &auth.Grants{Identity: "x", Video: &auth.VideoGrant{RoomAdmin: true, Room: "r"}})
	if apiClient.do("POST", "/api/v1/tokens", map[string]string{"identity": "b", "room": "r"}).status != 403 {
		t.Fatal("an access token must not be able to mint tokens")
	}
}

func TestRoomsAndTokenGrants(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("r@example.com")
	c.newProject("p")
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{}), 201)

	created := expect(t, c.do("POST", "/api/v1/rooms", map[string]any{"name": "standup", "metadata": "m", "max_participants": 5, "empty_timeout": 60}), 201)
	if created.body["max_participants"] != float64(5) || created.body["metadata"] != "m" {
		t.Fatalf("room: %s", created.raw)
	}
	again := expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "standup"}), 200)
	if again.body["sid"] != created.body["sid"] {
		t.Fatal("creating an open room should return it")
	}
	if got := expect(t, c.do("GET", "/api/v1/rooms/standup", nil), 200); got.body["name"] != "standup" {
		t.Fatalf("get: %s", got.raw)
	}
	updated := expect(t, c.do("PATCH", "/api/v1/rooms/standup", map[string]string{"metadata": "new"}), 200)
	if updated.body["metadata"] != "new" {
		t.Fatalf("update: %s", updated.raw)
	}
	if c.do("GET", "/api/v1/rooms/nope", nil).status != 404 {
		t.Fatal("missing room")
	}
	if c.do("POST", "/api/v1/rooms", map[string]string{"name": ""}).status != 422 {
		t.Fatal("empty room name")
	}

	token := func(g *auth.VideoGrant) *client {
		x := h.client()
		x.bearer = mustMint(t, key, &auth.Grants{Identity: "api", Video: g})
		return x
	}
	if token(&auth.VideoGrant{RoomJoin: true}).do("GET", "/api/v1/rooms", nil).status != 403 {
		t.Fatal("a join token can list rooms")
	}
	expect(t, token(&auth.VideoGrant{RoomList: true}).do("GET", "/api/v1/rooms", nil), 200)
	if token(&auth.VideoGrant{RoomList: true}).do("POST", "/api/v1/rooms", map[string]string{"name": "x"}).status != 403 {
		t.Fatal("a list token can create rooms")
	}
	expect(t, token(&auth.VideoGrant{RoomCreate: true}).do("POST", "/api/v1/rooms", map[string]string{"name": "x"}), 201)
	if token(&auth.VideoGrant{RoomAdmin: true, Room: "other"}).do("PATCH", "/api/v1/rooms/standup", map[string]string{"metadata": "x"}).status != 403 {
		t.Fatal("an admin of another room can change this one")
	}
	expect(t, token(&auth.VideoGrant{RoomAdmin: true, Room: "standup"}).do("PATCH", "/api/v1/rooms/standup", map[string]string{"metadata": "x"}), 200)
	if h.client().do("GET", "/api/v1/rooms", nil).status != 401 {
		t.Fatal("no credentials")
	}
	bad := h.client()
	bad.bearer = "garbage"
	if bad.do("GET", "/api/v1/rooms", nil).status != 401 {
		t.Fatal("a garbage token")
	}

	expect(t, c.do("DELETE", "/api/v1/rooms/standup", nil), 204)
	if c.do("GET", "/api/v1/rooms/standup", nil).status != 404 {
		t.Fatal("closed room is still open")
	}
	eventually(t, "room history", func() bool {
		for _, item := range expect(t, c.do("GET", "/api/v1/rooms/history", nil), 200).items() {
			row := item.(map[string]any)
			if row["name"] == "standup" && row["ended_at"] != nil {
				return true
			}
		}
		return false
	})
}

func TestProjectSettingsApplyToRooms(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("settings@example.com")
	id := c.newProject("p")
	expect(t, c.do("PATCH", "/api/v1/projects/"+id, map[string]any{"settings": map[string]any{
		"empty_timeout": 77, "departure_timeout": 7, "max_participants": 3,
		"allowed_origins": []string{"https://app.example.com"},
		"ice_servers":     []map[string]any{{"urls": []string{"turn:t.example.com:3478"}, "username": "u", "credential": "c"}},
	}}), 200)
	r := expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "defaults"}), 201)
	if r.body["empty_timeout"] != float64(77) || r.body["departure_timeout"] != float64(7) || r.body["max_participants"] != float64(3) {
		t.Fatalf("project defaults ignored: %s", r.raw)
	}
	explicit := expect(t, c.do("POST", "/api/v1/rooms", map[string]any{"name": "explicit", "max_participants": 9}), 201)
	if explicit.body["max_participants"] != float64(9) {
		t.Fatalf("explicit value overridden: %s", explicit.raw)
	}
	cfg := h.api.ProjectConfig(id)
	if len(cfg.AllowedOrigins) != 1 || len(cfg.ICEServers) != 1 || cfg.ICEServers[0].GetCredential() != "c" {
		t.Fatalf("config: %+v", cfg)
	}
	bad := c.do("PATCH", "/api/v1/projects/"+id, map[string]any{"settings": map[string]any{"allowed_origins": []string{"not a url"}}})
	if bad.status != 422 {
		t.Fatalf("bad origin: %s", bad.raw)
	}
}

func TestRolesLimitWhatMembersCanDo(t *testing.T) {
	h := newHarness(t, nil)
	owner := h.signUp("owner@example.com")
	id := owner.newProject("p")
	member := h.signUp("member@example.com")
	ctx := context.Background()
	var orgID, memberID string
	if err := h.store.DB.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, id).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB.QueryRow(ctx, `SELECT id FROM users WHERE email = 'member@example.com'`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatal(err)
	}
	member.project = id
	expect(t, member.do("GET", "/api/v1/rooms", nil), 200)
	for _, attempt := range []struct{ method, path string }{
		{"GET", "/api/v1/keys"},
		{"POST", "/api/v1/keys"},
		{"GET", "/api/v1/audit"},
		{"POST", "/api/v1/webhooks"},
	} {
		if r := member.do(attempt.method, attempt.path, map[string]string{}); r.status != 403 {
			t.Errorf("%s %s: status %d", attempt.method, attempt.path, r.status)
		}
	}
	if member.do("PATCH", "/api/v1/projects/"+id, map[string]string{"name": "x"}).status != 403 {
		t.Fatal("a member can change a project")
	}
	if member.do("DELETE", "/api/v1/projects/"+id, nil).status != 403 {
		t.Fatal("a member can delete a project")
	}
}

func TestWebhookDelivery(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ControlPlane.AllowPrivateWebhooks = true })
	c := h.signUp("w@example.com")
	c.newProject("p")
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{}), 201)

	type received struct {
		body string
		auth string
		ct   string
	}
	got := make(chan received, 20)
	var failures atomic.Int32
	failures.Store(2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if failures.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		got <- received{string(data), r.Header.Get("Authorization"), r.Header.Get("Content-Type")}
	}))
	defer receiver.Close()

	hook := expect(t, c.do("POST", "/api/v1/webhooks", map[string]any{
		"url": receiver.URL, "api_key_id": key.body["id"], "events": []string{"room_started"},
	}), 201)
	expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "hooked"}), 201)

	var delivered received
	select {
	case delivered = <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("no webhook arrived, even after retries")
	}
	if delivered.ct != "application/webhook+json" {
		t.Fatalf("content type %q", delivered.ct)
	}
	verified, err := auth.Verify(delivered.auth, h.keys)
	if err != nil {
		t.Fatalf("signature: %v", err)
	}
	sum := sha256.Sum256([]byte(delivered.body))
	if verified.Grants.Sha256 != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatal("signature does not cover the body")
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(delivered.body), &event); err != nil {
		t.Fatal(err)
	}
	if event["event"] != "room_started" || event["room"].(map[string]any)["name"] != "hooked" {
		t.Fatalf("event: %s", delivered.body)
	}
	if _, snake := event["room"].(map[string]any)["empty_timeout"]; snake {
		t.Fatalf("webhooks use camelCase: %s", delivered.body)
	}

	hookID := hook.body["id"].(string)
	eventually(t, "delivery record", func() bool {
		for _, d := range expect(t, c.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries", nil), 200).items() {
			row := d.(map[string]any)
			if row["status"] == "delivered" && row["attempts"] == float64(3) {
				return true
			}
		}
		return false
	})

	// Filtering: this hook only wants room_started.
	expect(t, c.do("DELETE", "/api/v1/rooms/hooked", nil), 204)
	time.Sleep(300 * time.Millisecond)
	select {
	case extra := <-got:
		t.Fatalf("received an event the hook did not ask for: %s", extra.body)
	default:
	}

	test := expect(t, c.do("POST", "/api/v1/webhooks/"+hookID+"/test", map[string]string{}), 202)
	if test.body["delivery_id"] == "" {
		t.Fatal("no delivery id")
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("test delivery did not arrive")
	}

	expect(t, c.do("PATCH", "/api/v1/webhooks/"+hookID, map[string]any{"url": receiver.URL, "api_key_id": key.body["id"], "enabled": false, "events": []string{}}), 200)
	expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "off"}), 201)
	time.Sleep(300 * time.Millisecond)
	select {
	case extra := <-got:
		t.Fatalf("a disabled hook received: %s", extra.body)
	default:
	}
	expect(t, c.do("DELETE", "/api/v1/webhooks/"+hookID, nil), 204)
	if c.do("GET", "/api/v1/webhooks/"+hookID, nil).status != 404 {
		t.Fatal("hook not deleted")
	}
}

func TestWebhookFailureIsRecordedAndRetryable(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ControlPlane.AllowPrivateWebhooks = true })
	c := h.signUp("f@example.com")
	c.newProject("p")
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{}), 201)
	var hits atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer receiver.Close()
	hook := expect(t, c.do("POST", "/api/v1/webhooks", map[string]any{"url": receiver.URL, "api_key_id": key.body["id"]}), 201)
	hookID := hook.body["id"].(string)
	expect(t, c.do("POST", "/api/v1/webhooks/"+hookID+"/test", map[string]string{}), 202)

	var deliveryID string
	eventually(t, "the failure to be recorded", func() bool {
		items := expect(t, c.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries", nil), 200).items()
		if len(items) == 0 {
			return false
		}
		row := items[0].(map[string]any)
		deliveryID, _ = row["id"].(string)
		return row["status"] == "failed" && row["response_status"] == float64(400) && row["last_error"] != ""
	})
	if hits.Load() != 1 {
		t.Fatalf("a 400 is a considered answer and should not be retried: %d attempts", hits.Load())
	}

	fail.Store(false)
	expect(t, c.do("POST", "/api/v1/webhooks/deliveries/"+deliveryID+"/retry", map[string]string{}), 202)
	eventually(t, "the retry to succeed", func() bool {
		row := expect(t, c.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries", nil), 200).items()[0].(map[string]any)
		return row["status"] == "delivered"
	})
}

func TestWebhooksCannotReachInternalNetworks(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("ssrf@example.com")
	c.newProject("p")
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{}), 201)
	for _, target := range []string{
		"http://127.0.0.1:5432/", "http://localhost/admin", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/", "http://[::1]/",
	} {
		r := c.do("POST", "/api/v1/webhooks", map[string]any{"url": target, "api_key_id": key.body["id"]})
		if r.status != 422 {
			t.Errorf("%s: status %d: %s", target, r.status, r.raw)
		}
	}

	// A public-looking name can later resolve to an internal address, which only the connection attempt can
	// catch. Saving the URL directly here gets past the check made when saving, as such a name would.
	var hit atomic.Bool
	inner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer inner.Close()
	var projectID string
	if err := h.store.DB.QueryRow(context.Background(), `SELECT project_id FROM api_keys WHERE id = $1`, key.body["id"]).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	saved, err := h.store.CreateWebhook(context.Background(), store.Webhook{
		ProjectID: projectID, URL: inner.URL, APIKeyID: key.body["id"].(string), Events: []string{}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	hookID := saved.ID
	expect(t, c.do("POST", "/api/v1/webhooks/"+hookID+"/test", map[string]string{}), 202)
	eventually(t, "the delivery to fail", func() bool {
		items := expect(t, c.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries", nil), 200).items()
		if len(items) == 0 {
			return false
		}
		row := items[0].(map[string]any)
		return row["status"] == "failed"
	})
	if hit.Load() {
		t.Fatal("a webhook reached an internal service")
	}
}

func TestLogsAndAudit(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("l@example.com")
	c.newProject("p")
	expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "alpha"}), 201)
	expect(t, c.do("POST", "/api/v1/rooms", map[string]string{"name": "beta"}), 201)
	expect(t, c.do("DELETE", "/api/v1/rooms/beta", nil), 204)

	eventually(t, "events", func() bool {
		return len(expect(t, c.do("GET", "/api/v1/logs?category=room", nil), 200).items()) >= 3
	})
	alpha := expect(t, c.do("GET", "/api/v1/logs?room=alpha", nil), 200).items()
	if len(alpha) != 1 || alpha[0].(map[string]any)["type"] != "room_started" {
		t.Fatalf("room filter: %v", alpha)
	}
	finished := expect(t, c.do("GET", "/api/v1/logs?q=finished", nil), 200).items()
	if len(finished) != 1 || finished[0].(map[string]any)["room_name"] != "beta" {
		t.Fatalf("search: %v", finished)
	}
	if got := expect(t, c.do("GET", "/api/v1/logs?category=api", nil), 200).items(); len(got) != 0 {
		t.Fatalf("category filter: %v", got)
	}

	page1 := expect(t, c.do("GET", "/api/v1/logs?limit=2", nil), 200)
	if len(page1.items()) != 2 || page1.body["next_cursor"] == "" {
		t.Fatalf("page 1: %s", page1.raw)
	}
	page2 := expect(t, c.do("GET", "/api/v1/logs?limit=2&cursor="+page1.body["next_cursor"].(string), nil), 200)
	if len(page2.items()) != 1 {
		t.Fatalf("page 2: %s", page2.raw)
	}
	if page1.items()[0].(map[string]any)["id"] == page2.items()[0].(map[string]any)["id"] {
		t.Fatal("pages overlap")
	}

	audit := expect(t, c.do("GET", "/api/v1/audit", nil), 200)
	actions := map[string]bool{}
	for _, item := range audit.items() {
		actions[item.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"project.created", "room.created", "room.deleted"} {
		if !actions[want] {
			t.Errorf("audit log is missing %s: %v", want, actions)
		}
	}
}

func TestAnalytics(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("a@example.com")
	id := c.newProject("p")
	ctx := context.Background()
	now := time.Now().Truncate(time.Minute)
	if err := h.store.AddUsage(ctx, store.UsageDelta{ProjectID: id, Bucket: now, ParticipantSeconds: 600, RoomSeconds: 300, BytesIn: 1000, BytesOut: 2000, PeakParticipants: 3, PeakRooms: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddUsage(ctx, store.UsageDelta{ProjectID: id, Bucket: now, ParticipantSeconds: 600, BytesIn: 500, PeakParticipants: 2}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddUsage(ctx, store.UsageDelta{ProjectID: "other-project", Bucket: now, ParticipantSeconds: 99999}); err != nil {
		t.Fatal(err)
	}
	series := expect(t, c.do("GET", "/api/v1/analytics/usage?interval=hour", nil), 200).items()
	if len(series) != 1 {
		t.Fatalf("series: %v", series)
	}
	row := series[0].(map[string]any)
	if row["participant_minutes"] != float64(20) || row["bytes_in"] != float64(1500) || row["peak_participants"] != float64(3) {
		t.Fatalf("usage does not add up: %v", row)
	}
	overview := expect(t, c.do("GET", "/api/v1/analytics/overview", nil), 200)
	if overview.body["totals"].(map[string]any)["participant_minutes"] != float64(20) {
		t.Fatalf("overview: %s", overview.raw)
	}
	expect(t, c.do("GET", "/api/v1/monitor", nil), 200)
}

func TestServersAndDraining(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signUp("admin@example.com")
	other := h.signUp("other@example.com")
	eventually(t, "heartbeat", func() bool {
		return len(expect(t, admin.do("GET", "/api/v1/servers", nil), 200).items()) == 1
	})
	node := expect(t, admin.do("GET", "/api/v1/servers", nil), 200).items()[0].(map[string]any)
	id := node["node_id"].(string)
	if node["stats"].(map[string]any)["num_cpu"].(float64) < 1 {
		t.Fatalf("stats: %v", node)
	}
	// Other organizations must not learn the operator's hosts, addresses or overall load.
	seen := expect(t, other.do("GET", "/api/v1/servers", nil), 200).items()[0].(map[string]any)
	if seen["address"] != "" {
		t.Fatalf("a non-admin sees an address: %v", seen)
	}
	stats := seen["stats"].(map[string]any)
	for _, private := range []string{"hostname", "cpu_percent", "memory_used", "memory_total", "active_rooms", "participants", "goroutines", "num_cpu"} {
		if _, leaked := stats[private]; leaked {
			t.Errorf("a non-admin sees %s: %v", private, stats)
		}
	}
	if stats["healthy"] != true {
		t.Fatalf("whether a node is up should stay visible: %v", stats)
	}
	if other.do("POST", "/api/v1/servers/"+id+"/drain", map[string]bool{"draining": true}).status != 403 {
		t.Fatal("anyone can drain a server")
	}
	if h.rooms.Draining() {
		t.Fatal("drained without permission")
	}
	expect(t, admin.do("POST", "/api/v1/servers/"+id+"/drain", map[string]bool{"draining": true}), 200)
	if !h.rooms.Draining() {
		t.Fatal("not draining")
	}
	expect(t, admin.do("POST", "/api/v1/servers/"+id+"/drain", map[string]bool{"draining": false}), 200)
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("b@example.com")
	if r := c.do("GET", "/api/v1/rooms", nil); r.status != 400 || r.errCode() != "project_required" {
		t.Fatalf("no project: %s", r.raw)
	}
	unknown := c.do("POST", "/api/v1/projects", map[string]any{"name": "x", "made_up_field": true})
	if unknown.status != 400 {
		t.Fatalf("unknown field: %s", unknown.raw)
	}
	spec, err := http.Get(h.srv.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer spec.Body.Close()
	data, _ := io.ReadAll(spec.Body)
	if spec.StatusCode != 200 || !strings.Contains(string(data), "openapi: 3.1.0") {
		t.Fatalf("spec: %d", spec.StatusCode)
	}
}
