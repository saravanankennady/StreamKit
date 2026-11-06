package main

// These tests run the built binary against real running servers: this server, and the reference server wherever a
// command exists there. A test that the reference fails tests the wrong thing.
//
// Set STREAMKIT_TEST_DATABASE_URL to run them, and STREAMKIT_TEST_WORKERS=1 for the recording tests, which need Chrome
// and ffmpeg. Without a database only TestConfig and the checks that need no server run.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
	"elnova-streamkit.local/tests/sdk/fixture"
)

var (
	fx  *fixture.Fixture
	cli string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "streamkit-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cli = filepath.Join(dir, "streamkit")
	if out, err := exec.Command("go", "build", "-o", cli, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "could not build the CLI: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	if os.Getenv("STREAMKIT_TEST_DATABASE_URL") == "" {
		fmt.Println("STREAMKIT_TEST_DATABASE_URL is not set: only the tests that need no server will run")
		code := m.Run()
		os.RemoveAll(dir)
		os.Exit(code)
	}
	fx, err = fixture.Start(context.Background(), fixture.Options{Workers: os.Getenv("STREAMKIT_TEST_WORKERS") != "", Log: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start the servers:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	fx.Stop()
	os.RemoveAll(dir)
	os.Exit(code)
}

// result is one run of the binary.
type result struct {
	stdout, stderr string
	code           int
}

func (r result) all() string { return r.stdout + r.stderr }

// env is an isolated place to run the CLI: its own config directory, and none of the caller's STREAMKIT_ variables.
type env struct {
	t     *testing.T
	dir   string
	extra []string
	stdin string
}

func newEnv(t *testing.T) *env {
	return &env{t: t, dir: t.TempDir()}
}

func (e *env) with(vars ...string) *env {
	out := *e
	out.extra = append(append([]string{}, e.extra...), vars...)
	return &out
}

func (e *env) environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "STREAMKIT_") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			out = append(out, kv)
		}
	}
	return append(append(out, "STREAMKIT_CONFIG_DIR="+e.dir), e.extra...)
}

func (e *env) run(args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(e.stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !asExit(err, &exit) {
			e.t.Fatalf("streamkit %v: %v", args, err)
		}
		r.code = exit.ExitCode()
	}
	return r
}

func asExit(err error, out **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*out = e
	}
	return ok
}

func (e *env) ok(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("streamkit %v: exit %d\n%s", args, r.code, r.all())
	}
	return r
}

func (e *env) fails(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code == 0 {
		e.t.Fatalf("streamkit %v should have failed\n%s", args, r.all())
	}
	return r
}

func want(t *testing.T, got, part string) {
	t.Helper()
	if !strings.Contains(got, part) {
		t.Errorf("want %q in:\n%s", part, got)
	}
}

func refuse(t *testing.T, got, part string) {
	t.Helper()
	if strings.Contains(got, part) {
		t.Errorf("%q must not appear in:\n%s", part, got)
	}
}

func decodeJSON[T any](t *testing.T, text string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
	return out
}

const testSecret = "a-distinctive-secret-that-must-never-be-printed"

func TestConfigFilesArePrivateAndHoldNoSecret(t *testing.T) {
	e := newEnv(t)
	e.stdin = testSecret + "\n"
	e.ok("project", "add", "one", "--url", "ws://127.0.0.1:1", "--api-key", "key-one", "--api-secret-stdin")

	config := filepath.Join(e.dir, configFile)
	key := filepath.Join(e.dir, sealKeyFile)
	for _, path := range []string{config, key} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s is %o", path, perm)
		}
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	refuse(t, string(data), testSecret)
	want(t, string(data), "api_secret_sealed: v1:")

	// The sealed secret still works: a token signed through the project verifies with the real secret.
	token := strings.TrimSpace(e.ok("token", "create", "--project", "one", "--join", "--room", "r", "--identity", "i", "--token-only").stdout)
	claims, err := streamkit.VerifyToken(token, streamkit.Keys{"key-one": testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if claims.Identity != "i" || claims.Video.Room != "r" {
		t.Errorf("claims: %+v", claims)
	}

	// The first project becomes the default.
	token = strings.TrimSpace(e.ok("token", "create", "--join", "--room", "r", "--identity", "i", "--token-only").stdout)
	if _, err := streamkit.VerifyToken(token, streamkit.Keys{"key-one": testSecret}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRefusesFilesOthersCanRead(t *testing.T) {
	e := newEnv(t)
	e.stdin = testSecret + "\n"
	e.ok("project", "add", "one", "--url", "ws://127.0.0.1:1", "--api-key", "k", "--api-secret-stdin")
	config := filepath.Join(e.dir, configFile)
	if err := os.Chmod(config, 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.fails("project", "list")
	want(t, r.stderr, "can be read by other users")
	want(t, r.stderr, "chmod 600")
	if err := os.Chmod(config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(e.dir, sealKeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	r = e.fails("token", "create", "--project", "one", "--list")
	want(t, r.stderr, "can be read by other users")
}

func TestSecretFromAnEnvironmentVariable(t *testing.T) {
	e := newEnv(t)
	e.ok("project", "add", "env-project", "--url", "ws://127.0.0.1:1", "--api-key", "k", "--api-secret-env", "MY_PROJECT_SECRET")
	data, err := os.ReadFile(filepath.Join(e.dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	want(t, string(data), "api_secret_env: MY_PROJECT_SECRET")
	if _, err := os.Stat(filepath.Join(e.dir, sealKeyFile)); !os.IsNotExist(err) {
		t.Error("no key is needed when nothing is sealed")
	}

	r := e.fails("token", "create", "--list")
	want(t, r.stderr, "MY_PROJECT_SECRET, which is not set")

	token := strings.TrimSpace(e.with("MY_PROJECT_SECRET="+testSecret).ok("token", "create", "--list", "--token-only").stdout)
	if _, err := streamkit.VerifyToken(token, streamkit.Keys{"k": testSecret}); err != nil {
		t.Fatal(err)
	}
	listed := decodeJSON[[]projectView](t, e.ok("project", "list", "--json").stdout)
	if listed[0].SecretSource != "env:MY_PROJECT_SECRET" {
		t.Errorf("listed: %+v", listed)
	}
}

func TestSealKeyFromTheEnvironment(t *testing.T) {
	e := newEnv(t)
	sealKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	keyed := e.with("STREAMKIT_CONFIG_KEY=" + sealKey)
	keyed.stdin = testSecret + "\n"
	keyed.ok("project", "add", "one", "--url", "ws://127.0.0.1:1", "--api-key", "k", "--api-secret-stdin")
	if _, err := os.Stat(filepath.Join(e.dir, sealKeyFile)); !os.IsNotExist(err) {
		t.Error("the key must not be written beside the config when it comes from the environment")
	}
	token := strings.TrimSpace(keyed.ok("token", "create", "--list", "--token-only").stdout)
	if _, err := streamkit.VerifyToken(token, streamkit.Keys{"k": testSecret}); err != nil {
		t.Fatal(err)
	}
	// Without it the secret cannot be opened, and the message says what to do.
	r := e.fails("token", "create", "--list")
	want(t, r.stderr, "Add the project again")
	refuse(t, r.all(), testSecret)
	r = e.with("STREAMKIT_CONFIG_KEY=not-a-key").fails("token", "create", "--list")
	want(t, r.stderr, "STREAMKIT_CONFIG_KEY")
}

func TestSealedSecretCannotMoveToAnotherProject(t *testing.T) {
	e := newEnv(t)
	e.stdin = testSecret + "\n"
	e.ok("project", "add", "one", "--url", "ws://127.0.0.1:1", "--api-key", "k", "--api-secret-stdin")
	path := filepath.Join(e.dir, configFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte("name: one"), []byte("name: two"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.fails("token", "create", "--project", "two", "--list")
	want(t, r.stderr, "could not be opened")
}

func TestProjectCommands(t *testing.T) {
	e := newEnv(t)
	e.stdin = testSecret + "\n"
	e.ok("project", "add", "one", "--url", "ws://one:1", "--api-key", "key-one", "--api-secret-stdin")
	e.ok("project", "add", "two", "--url", "ws://two:2", "--api-key", "key-two", "--api-secret-stdin", "--default")

	listed := decodeJSON[[]projectView](t, e.ok("project", "list", "--json").stdout)
	if len(listed) != 2 || listed[0].Default || !listed[1].Default || listed[0].SecretSource != "sealed" {
		t.Errorf("listed: %+v", listed)
	}
	text := e.ok("project", "list").stdout
	want(t, text, "key-one")
	want(t, text, "ws://two:2")

	e.ok("project", "set-default", "one")
	listed = decodeJSON[[]projectView](t, e.ok("project", "list", "--json").stdout)
	if !listed[0].Default {
		t.Errorf("listed: %+v", listed)
	}
	r := e.fails("project", "set-default", "three")
	want(t, r.stderr, `no project named "three"`)

	// Adding a name again replaces it.
	e.ok("project", "add", "one", "--url", "ws://new:3", "--api-key", "key-new", "--api-secret-stdin")
	listed = decodeJSON[[]projectView](t, e.ok("project", "list", "--json").stdout)
	if len(listed) != 2 || listed[0].URL != "ws://new:3" {
		t.Errorf("listed: %+v", listed)
	}

	e.ok("project", "remove", "one")
	listed = decodeJSON[[]projectView](t, e.ok("project", "list", "--json").stdout)
	if len(listed) != 1 || listed[0].Name != "two" {
		t.Errorf("listed: %+v", listed)
	}
	e.fails("project", "remove", "one")
	r = e.fails("token", "create", "--project", "nope", "--list")
	want(t, r.stderr, `no project named "nope"`)
}

func TestWhichCredentialsAreUsed(t *testing.T) {
	e := newEnv(t)
	e.stdin = "project-secret\n"
	e.ok("project", "add", "one", "--url", "ws://one:1", "--api-key", "project-key", "--api-secret-stdin")
	issuer := func(r result) string {
		t.Helper()
		claims, err := streamkit.VerifyToken(strings.TrimSpace(r.stdout), streamkit.Keys{
			"project-key": "project-secret", "flag-key": "flag-secret", "env-key": "env-secret", devKey: devSecret,
		})
		if err != nil {
			t.Fatalf("%v: %s", err, r.all())
		}
		return claims.APIKey
	}
	args := []string{"token", "create", "--list", "--token-only"}
	if got := issuer(e.ok(args...)); got != "project-key" {
		t.Errorf("default project: %s", got)
	}
	withEnv := e.with("STREAMKIT_API_KEY=env-key", "STREAMKIT_API_SECRET=env-secret", "STREAMKIT_URL=ws://env:1")
	if got := issuer(withEnv.ok(args...)); got != "env-key" {
		t.Errorf("environment: %s", got)
	}
	if got := issuer(withEnv.ok(append(args, "--api-key", "flag-key", "--api-secret", "flag-secret")...)); got != "flag-key" {
		t.Errorf("flags over the environment: %s", got)
	}
	if got := issuer(withEnv.ok(append(args, "--project", "one")...)); got != "project-key" {
		t.Errorf("--project over the environment: %s", got)
	}
	if got := issuer(e.ok(append(args, "--dev")...)); got != devKey {
		t.Errorf("--dev: %s", got)
	}
	empty := newEnv(t)
	r := empty.fails(args...)
	want(t, r.stderr, "no API key and secret")
	r = empty.fails("room", "list", "--api-key", "k", "--api-secret", "s")
	want(t, r.stderr, "no server address")
}

func TestUsageMistakesExplainThemselves(t *testing.T) {
	e := newEnv(t).with("STREAMKIT_API_KEY=k", "STREAMKIT_API_SECRET=s", "STREAMKIT_URL=ws://127.0.0.1:1")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"nonsense"}, `unknown command "nonsense"`},
		{[]string{"room", "nonsense"}, `unknown command "nonsense"`},
		{[]string{"room", "create"}, "give one room name"},
		{[]string{"room", "update", "r"}, "--metadata is required"},
		{[]string{"token", "create"}, "say what the token may do"},
		{[]string{"token", "create", "--join"}, "--join needs --room and --identity"},
		{[]string{"token", "create", "--admin"}, "--admin needs --room"},
		{[]string{"token", "create", "--list", "--valid-for", "soon"}, "--valid-for must be a time"},
		{[]string{"token", "create", "--list", "--grant", "{"}, "--grant is not valid JSON"},
		{[]string{"token", "create", "--list", "--kind", "alien"}, `unknown --kind "alien"`},
		{[]string{"token", "create", "--join", "--room", "r", "--identity", "i", "--attribute", "nope"}, "not in key=value form"},
		{[]string{"participant", "get", "--room", "r"}, "--room and --identity are required"},
		{[]string{"participant", "mute", "--room", "r", "--identity", "i"}, "--room, --identity and --track are required"},
		{[]string{"data", "send", "--room", "r"}, "--room and --data are required"},
		{[]string{"egress", "start"}, "--room is required"},
		{[]string{"egress", "start", "--room", "r"}, "say where the media goes"},
		{[]string{"egress", "start", "--room", "r", "--format", "mp4", "--type", "web"}, `unknown --type "web"`},
		{[]string{"ingress", "create"}, "--input, --room and --identity are required"},
		{[]string{"webhook", "test"}, "--target is required"},
		{[]string{"webhook", "test", "--target", "http://x", "--event", "nope"}, `unknown --event "nope"`},
		{[]string{"perf", "load-test"}, "say how many clients"},
		{[]string{"room", "list", "--no-such-flag"}, "flag provided but not defined"},
	} {
		r := e.fails(tc.args...)
		want(t, r.stderr, tc.want)
	}
	r := e.ok("--help")
	want(t, r.stdout, "project")
	want(t, r.stdout, "perf")
	want(t, e.ok("room", "--help").stdout, "update-subscriptions")
	want(t, e.ok("room", "create", "--help").stdout, "--empty-timeout")
	want(t, e.ok("version").stdout, version)
}

// Nothing may ever print a secret, whatever the command does, whether it works or not.
func TestSecretsAreNeverPrinted(t *testing.T) {
	e := newEnv(t)
	e.stdin = testSecret + "\n"
	e.ok("project", "add", "one", "--url", "ws://127.0.0.1:1", "--api-key", "k", "--api-secret-stdin")
	withSecret := []string{"--api-key", "k", "--api-secret", testSecret, "--url", "ws://127.0.0.1:1"}
	commands := [][]string{
		{"project", "list"},
		{"project", "list", "--json"},
		{"--project", "one", "room", "--help"},
		append(append([]string{}, withSecret...), "--help"),
		append(append([]string{}, withSecret...), "room", "--help"),
		append(append([]string{}, withSecret...), "room", "create", "--help"),
		append(append([]string{}, withSecret...), "room", "nonsense"),
		append(append([]string{}, withSecret...), "room", "list"),
		append(append([]string{}, withSecret...), "room", "create", "r"),
		append(append([]string{}, withSecret...), "room", "create"),
		append(append([]string{}, withSecret...), "token", "create"),
		append(append([]string{}, withSecret...), "token", "create", "--list"),
		append(append([]string{}, withSecret...), "token", "create", "--list", "--json"),
		append(append([]string{}, withSecret...), "token", "verify", "not.a.token"),
		append(append([]string{}, withSecret...), "room", "join", "r", "--identity", "i", "--duration", "1s"),
		append(append([]string{}, withSecret...), "webhook", "test", "--target", "http://127.0.0.1:1/"),
		append(append([]string{}, withSecret...), "egress", "list"),
		append(append([]string{}, withSecret...), "ingress", "list"),
		append(append([]string{}, withSecret...), "perf", "load-test"),
		{"token", "create", "--project", "one", "--list", "--json"},
		{"token", "create", "--project", "one", "--list"},
	}
	for _, args := range commands {
		r := e.run(args...)
		if strings.Contains(r.all(), testSecret) {
			t.Errorf("streamkit %v printed the secret:\n%s", args, r.all())
		}
	}
	// Secrets from the environment, against a real server when there is one.
	withEnv := e.with("STREAMKIT_API_SECRET="+testSecret, "STREAMKIT_API_KEY=k", "STREAMKIT_URL=ws://127.0.0.1:1")
	for _, args := range [][]string{{"room", "list"}, {"token", "create", "--list"}} {
		if r := withEnv.run(args...); strings.Contains(r.all(), testSecret) {
			t.Errorf("streamkit %v printed the secret:\n%s", args, r.all())
		}
	}
}

func TestTokenCreateCarriesWhatItIsTold(t *testing.T) {
	e := newEnv(t).with("STREAMKIT_API_KEY=k", "STREAMKIT_API_SECRET="+testSecret)
	configFile := filepath.Join(t.TempDir(), "room.json")
	if err := os.WriteFile(configFile, []byte(`{"emptyTimeout": 9, "maxParticipants": 8}`), 0o600); err != nil {
		t.Fatal(err)
	}
	attributeFile := filepath.Join(t.TempDir(), "attributes.json")
	if err := os.WriteFile(attributeFile, []byte(`{"from": "file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := e.ok("token", "create", "--json", "--room", "r", "--identity", "alice", "--name", "Alice", "--metadata", "m", "--valid-for", "10m",
		"--attribute", "a=1", "--attribute-file", attributeFile, "--join", "--admin", "--create", "--list", "--egress", "--ingress",
		"--allow-update-metadata", "--allow-source", "camera,microphone", "--no-subscribe", "--hidden", "--kind", "agent",
		"--grant", `{"canPublishData": false, "destinationRoom": "d"}`, "--room-config", configFile, "--room-preset", "p",
		"--sip-admin", "--sip-call", "--agent-admin", "--agent-dispatch-admin", "--inference", "--observability-write").stdout
	view := decodeJSON[map[string]any](t, out)
	claims, err := streamkit.VerifyToken(view["token"].(string), streamkit.Keys{"k": testSecret})
	if err != nil {
		t.Fatal(err)
	}
	v := claims.Video
	if claims.Identity != "alice" || claims.Name != "Alice" || claims.Metadata != "m" || claims.Kind != "agent" || claims.RoomPreset != "p" {
		t.Errorf("claims: %+v", claims)
	}
	if claims.Attributes["a"] != "1" || claims.Attributes["from"] != "file" {
		t.Errorf("attributes: %v", claims.Attributes)
	}
	if !v.RoomJoin || !v.RoomAdmin || !v.RoomCreate || !v.RoomList || !v.RoomRecord || !v.IngressAdmin || !v.Hidden || v.Room != "r" || v.DestinationRoom != "d" {
		t.Errorf("video: %+v", v)
	}
	if !*v.CanUpdateOwnMetadata || *v.CanSubscribe || *v.CanPublishData || len(v.CanPublishSources) != 2 {
		t.Errorf("video: %+v", v)
	}
	if !claims.SIP.Admin || !claims.SIP.Call || !claims.Agent.Admin || !claims.Agent.DispatchAdmin || !claims.Inference.Perform || !claims.Observability.Write {
		t.Errorf("grants: %+v", claims.ClaimGrants)
	}
	if claims.RoomConfig.GetEmptyTimeout() != 9 || claims.RoomConfig.GetMaxParticipants() != 8 {
		t.Errorf("room config: %v", claims.RoomConfig)
	}
	if d := time.Until(claims.ExpiresAt); d > 10*time.Minute || d < 9*time.Minute {
		t.Errorf("expires in %s", d)
	}

	// The default lifetime, and token verify agreeing with all of that.
	token := strings.TrimSpace(e.ok("token", "create", "--list", "--token-only").stdout)
	short, err := streamkit.VerifyToken(token, streamkit.Keys{"k": testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(short.ExpiresAt); d > 5*time.Minute || d < 4*time.Minute {
		t.Errorf("default expires in %s", d)
	}
	verified := decodeJSON[map[string]any](t, e.ok("token", "verify", view["token"].(string)).stdout)
	if verified["identity"] != "alice" {
		t.Errorf("verified: %v", verified)
	}
	r := e.with("STREAMKIT_API_SECRET=not-the-secret").fails("token", "verify", view["token"].(string))
	want(t, r.stderr, "invalid token")
}

// target is a server under test, with the flags that reach it.
type target struct {
	name   string
	server *fixture.Server
	env    *env
	room   *streamkit.RoomClient
}

func eachServer(t *testing.T, fn func(t *testing.T, tg target)) {
	t.Helper()
	if fx == nil {
		t.Skip("needs running servers")
	}
	for _, c := range []struct {
		name string
		s    *fixture.Server
	}{{"ours", fx.Info.Ours}, {"reference", fx.Info.Reference}} {
		t.Run(c.name, func(t *testing.T) {
			if c.s == nil {
				t.Skip("the reference server is not available")
			}
			e := newEnv(t).with("STREAMKIT_URL="+c.s.URL, "STREAMKIT_API_KEY="+c.s.APIKey, "STREAMKIT_API_SECRET="+c.s.APISecret)
			fn(t, target{c.name, c.s, e, streamkit.NewRoomClient(c.s.URL, c.s.APIKey, c.s.APISecret)})
		})
	}
}

var roomCounter atomic.Int64

func roomName(t *testing.T) string {
	name := regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(t.Name(), "-")
	return fmt.Sprintf("cli-%s-%d-%d", strings.ToLower(name), os.Getpid(), roomCounter.Add(1))
}

func join(t *testing.T, tg target, room string, o fixture.PeerOptions) *fixture.Peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	p, err := fx.Join(ctx, tg.server, room, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestRoomCommands(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		e := tg.env
		room := roomName(t)
		created := decodeJSON[map[string]any](t, e.ok("room", "create", room, "--empty-timeout", "120", "--departure-timeout", "30", "--max-participants", "7", "--metadata", "hello").stdout)
		if created["name"] != room || created["empty_timeout"] != float64(120) || created["departure_timeout"] != float64(30) || created["max_participants"] != float64(7) || created["metadata"] != "hello" {
			t.Errorf("created: %v", created)
		}
		table := e.ok("room", "list").stdout
		want(t, table, room)
		want(t, table, created["sid"].(string))

		listed := decodeJSON[map[string][]map[string]any](t, e.ok("room", "list", room, room+"-not-open", "--json").stdout)
		if len(listed["rooms"]) != 1 || listed["rooms"][0]["sid"] != created["sid"] {
			t.Errorf("listed: %v", listed)
		}
		updated := decodeJSON[map[string]any](t, e.ok("room", "update", room, "--metadata", "changed").stdout)
		if updated["metadata"] != "changed" {
			t.Errorf("updated: %v", updated)
		}
		cleared := decodeJSON[map[string]any](t, e.ok("room", "update", room, "--metadata", "").stdout)
		if _, set := cleared["metadata"]; set {
			t.Errorf("cleared: %v", cleared)
		}
		e.ok("room", "delete", room)
		refuse(t, e.ok("room", "list").stdout, room)
		r := e.fails("room", "delete", room)
		want(t, r.stderr, "not_found")

		bad := e.with("STREAMKIT_API_SECRET=not-the-secret").fails("room", "list")
		want(t, bad.stderr, "unauthenticated")
	})
}

func TestProjectReachesARealServer(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		e := newEnv(t)
		e.stdin = tg.server.APISecret + "\n"
		e.ok("project", "add", "real", "--url", tg.server.URL, "--api-key", tg.server.APIKey, "--api-secret-stdin")
		room := roomName(t)
		e.ok("room", "create", room)
		want(t, e.ok("room", "list", "--project", "real").stdout, room)
		e.ok("room", "delete", room)
	})
}

func TestTokensFromTheCLIJoinRealServers(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		room := roomName(t)
		token := strings.TrimSpace(tg.env.ok("token", "create", "--join", "--room", room, "--identity", "cli-token", "--name", "From CLI", "--attribute", "a=1", "--token-only").stdout)
		p := join(t, tg, room, fixture.PeerOptions{Identity: "cli-token", Token: token})
		joined, err := p.Wait(30*time.Second, "joining", func(e fixture.Event) bool { return e.Event == "joined" })
		if err != nil {
			t.Fatal(err)
		}
		if joined.Identity != "cli-token" || joined.Name != "From CLI" || joined.Attributes["a"] != "1" {
			t.Errorf("joined: %+v", joined)
		}
	})
}

func TestReferenceCLITokens(t *testing.T) {
	if fx == nil || fx.Info.Lk == "" {
		t.Skip("no built reference CLI at .toolchain/bin/lk")
	}
	eachServer(t, func(t *testing.T, tg target) {
		room := roomName(t)
		cmd := exec.Command(fx.Info.Lk, "token", "create", "--url", tg.server.URL, "--api-key", tg.server.APIKey, "--api-secret", tg.server.APISecret,
			"--join", "--admin", "--room", room, "--identity", "from-lk", "--name", "From lk", "--valid-for", "10m", "--token-only")
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("lk: %v", err)
		}
		token := strings.TrimSpace(string(out))
		// Our CLI reads it, and both servers take it.
		verified := decodeJSON[map[string]any](t, tg.env.ok("token", "verify", token).stdout)
		if verified["identity"] != "from-lk" || verified["name"] != "From lk" {
			t.Errorf("verified: %v", verified)
		}
		p := join(t, tg, room, fixture.PeerOptions{Identity: "from-lk", Token: token})
		if err := p.WaitConnected(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReferenceCLIAgainstOurServer(t *testing.T) {
	if fx == nil || fx.Info.Lk == "" {
		t.Skip("no built reference CLI at .toolchain/bin/lk")
	}
	s := fx.Info.Ours
	lk := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(fx.Info.Lk, append([]string{"--url", s.URL, "--api-key", s.APIKey, "--api-secret", s.APISecret}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lk %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	room := roomName(t)
	lk("room", "create", room)
	want(t, lk("room", "list"), room)
	want(t, newEnv(t).with("STREAMKIT_URL="+s.URL, "STREAMKIT_API_KEY="+s.APIKey, "STREAMKIT_API_SECRET="+s.APISecret).ok("room", "list").stdout, room)
	lk("room", "delete", room)
}

func TestParticipantCommands(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		e := tg.env
		room := roomName(t)
		publisher := join(t, tg, room, fixture.PeerOptions{Identity: "publisher", Publish: true})
		tracks, err := publisher.WaitPublished(2)
		if err != nil {
			t.Fatal(err)
		}
		var audio string
		for _, track := range tracks {
			if track.Kind == "audio" {
				audio = track.SID
			}
		}
		other := join(t, tg, room, fixture.PeerOptions{Identity: "other"})
		if err := other.WaitDataChannels(); err != nil {
			t.Fatal(err)
		}
		// A server lists a track once media has arrived for it, which seeing it arrive proves.
		for _, kind := range []string{"video", "audio"} {
			if _, err := other.Wait(30*time.Second, "real "+kind, func(ev fixture.Event) bool { return ev.Event == "media" && ev.Kind == kind }); err != nil {
				t.Fatal(err)
			}
		}

		table := e.ok("participant", "list", "--room", room).stdout
		want(t, table, "publisher")
		want(t, table, "other")
		listed := decodeJSON[map[string][]map[string]any](t, e.ok("room", "participants", "list", room, "--json").stdout)
		if len(listed["participants"]) != 2 {
			t.Errorf("listed: %v", listed)
		}
		got := decodeJSON[map[string]any](t, e.ok("participant", "get", "--room", room, "--identity", "publisher").stdout)
		if tracks, _ := got["tracks"].([]any); got["identity"] != "publisher" || len(tracks) != 2 {
			t.Errorf("got: %v", got)
		}
		decodeJSON[map[string]any](t, e.ok("room", "participants", "get", "--room", room, "other").stdout)
		r := e.fails("participant", "get", "--room", room, "--identity", "nobody")
		want(t, r.stderr, "participant does not exist")

		muted := decodeJSON[map[string]map[string]any](t, e.ok("participant", "mute", "--room", room, "--identity", "publisher", "--track", audio).stdout)
		if muted["track"]["muted"] != true {
			t.Errorf("muted: %v", muted)
		}
		if _, err := publisher.Wait(15*time.Second, "being muted", func(ev fixture.Event) bool { return ev.Event == "muted" && ev.SID == audio }); err != nil {
			t.Fatal(err)
		}
		r = e.fails("room", "mute-track", "--room", room, "--identity", "publisher", "--unmute", audio)
		want(t, r.stderr, "failed_precondition")

		updated := decodeJSON[map[string]any](t, e.ok("participant", "update", "--room", room, "--identity", "other", "--metadata", "new", "--name", "New", "--attribute", "a=1").stdout)
		if updated["metadata"] != "new" || updated["name"] != "New" {
			t.Errorf("updated: %v", updated)
		}
		if _, err := other.Wait(15*time.Second, "the update", func(ev fixture.Event) bool {
			return ev.Event == "participant_update" && ev.Identity == "other" && ev.Metadata != nil && *ev.Metadata == "new"
		}); err != nil {
			t.Fatal(err)
		}

		e.ok("data", "send", "--room", room, "--data", "hello from the cli", "--topic", "news")
		encoded := base64.StdEncoding.EncodeToString([]byte("hello from the cli"))
		data, err := other.Wait(15*time.Second, "the data", func(ev fixture.Event) bool { return ev.Event == "data" && ev.Data == encoded })
		if err != nil {
			t.Fatal(err)
		}
		if data.Topic != "news" || data.Channel != "_reliable" {
			t.Errorf("data: %+v", data)
		}
		e.ok("room", "send-data", "--room", room, "--identity", "other", "--lossy", "just for you")
		encoded = base64.StdEncoding.EncodeToString([]byte("just for you"))
		data, err = other.Wait(15*time.Second, "the lossy data", func(ev fixture.Event) bool { return ev.Event == "data" && ev.Data == encoded })
		if err != nil {
			t.Fatal(err)
		}
		if data.Channel != "_lossy" {
			t.Errorf("data: %+v", data)
		}

		e.ok("participant", "remove", "--room", room, "--identity", "other")
		left, err := other.Wait(15*time.Second, "being removed", func(ev fixture.Event) bool { return ev.Event == "left" })
		if err != nil {
			t.Fatal(err)
		}
		if left.Reason != "PARTICIPANT_REMOVED" {
			t.Errorf("left: %+v", left)
		}
		e.fails("participant", "remove", "--room", room, "--identity", "other")
	})
}

func TestUpdateSubscriptionsCommand(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		room := roomName(t)
		publisher := join(t, tg, room, fixture.PeerOptions{Identity: "publisher", Publish: true})
		tracks, err := publisher.WaitPublished(2)
		if err != nil {
			t.Fatal(err)
		}
		viewer := join(t, tg, room, fixture.PeerOptions{Identity: "viewer", NoAutoSubscribe: true})
		if _, err := viewer.Wait(30*time.Second, "joining", func(e fixture.Event) bool { return e.Event == "joined" }); err != nil {
			t.Fatal(err)
		}
		tg.env.ok("room", "update-subscriptions", "--room", room, "--identity", "viewer", "--subscribe", tracks[0].SID, tracks[1].SID)
		for _, kind := range []string{"video", "audio"} {
			if _, err := viewer.Wait(30*time.Second, "subscribed "+kind, func(e fixture.Event) bool { return e.Event == "media" && e.Kind == kind && e.Packets > 10 }); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// `room join` publishes a real test pattern, which a second real participant receives. The same command is the second
// participant, in JSON and in text.
func TestJoinPublishesATestPattern(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		room := roomName(t)
		publisher := join(t, tg, room, fixture.PeerOptions{Identity: "pattern", Publish: true})
		tracks, err := publisher.WaitPublished(2)
		if err != nil {
			t.Fatal(err)
		}
		sources := map[string]string{tracks[0].Kind: tracks[0].Source, tracks[1].Kind: tracks[1].Source}
		if sources["video"] != "camera" || sources["audio"] != "microphone" {
			t.Errorf("tracks: %v", tracks)
		}
		r := tg.env.ok("room", "join", room, "--identity", "text-viewer", "--duration", "4s")
		want(t, r.stdout, "joined ")
		want(t, r.stdout, "track_subscribed")
		want(t, r.stdout, "media ")

		// The viewer above received media, so the server has these tracks by now.
		info, err := tg.room.GetParticipant(context.Background(), &pb.RoomParticipantIdentity{Room: room, Identity: "pattern"})
		if err != nil {
			t.Fatal(err)
		}
		if len(info.GetTracks()) != 2 {
			t.Errorf("tracks at the server: %v", info.GetTracks())
		}

		videoOnly := join(t, tg, room, fixture.PeerOptions{Identity: "video-only", Publish: true, Extra: []string{"--video-only"}})
		if _, err := videoOnly.WaitPublished(1); err != nil {
			t.Fatal(err)
		}
		dir, err := filepath.Abs("media")
		if err != nil {
			t.Fatal(err)
		}
		fromFiles := join(t, tg, room, fixture.PeerOptions{Identity: "from-files", Extra: []string{"--publish", dir + "/pattern.ivf," + dir + "/tone.ogg"}})
		if _, err := fromFiles.WaitPublished(2); err != nil {
			t.Fatal(err)
		}
		r = tg.env.fails("room", "join", room, "--identity", "bad-file", "--publish", "movie.mkv")
		want(t, r.stderr, "only .ivf (VP8) and .ogg (Opus) are supported")
	})
}

func TestJoinReportsARefusal(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		r := tg.env.with("STREAMKIT_API_SECRET=not-the-secret").fails("room", "join", roomName(t), "--identity", "refused", "--json")
		want(t, r.stdout, `"error"`)
		want(t, r.stdout, "HTTP 401")
	})
}

func TestEgressAndIngressNeedThisServersAPI(t *testing.T) {
	if fx == nil {
		t.Skip("needs running servers")
	}
	for name, s := range map[string]*fixture.Server{"reference": fx.Info.Reference, "plain": fx.Info.Plain} {
		t.Run(name, func(t *testing.T) {
			if s == nil {
				t.Skip("the reference server is not available")
			}
			e := newEnv(t).with("STREAMKIT_URL="+s.URL, "STREAMKIT_API_KEY="+s.APIKey, "STREAMKIT_API_SECRET="+s.APISecret)
			want(t, e.fails("egress", "list").stderr, "no management API")
			want(t, e.fails("ingress", "list").stderr, "no management API")
		})
	}
}

func ourEnv(t *testing.T) *env {
	t.Helper()
	if fx == nil {
		t.Skip("needs running servers")
	}
	s := fx.Info.Ours
	return newEnv(t).with("STREAMKIT_URL="+s.URL, "STREAMKIT_API_KEY="+s.APIKey, "STREAMKIT_API_SECRET="+s.APISecret)
}

func TestIngressCommands(t *testing.T) {
	e := ourEnv(t)
	room := roomName(t)
	created := decodeJSON[streamkit.Ingress](t, e.ok("ingress", "create", "--input", "rtmp", "--room", room, "--identity", "stage", "--name", "Stage", "--participant-name", "On stage").stdout)
	if created.ID == "" || created.StreamKey == "" || created.RoomName != room || created.ParticipantName != "On stage" {
		t.Errorf("created: %+v", created)
	}
	got := decodeJSON[streamkit.Ingress](t, e.ok("ingress", "get", created.ID).stdout)
	if got.StreamKey != "" || got.Name != "Stage" {
		t.Errorf("got: %+v", got)
	}
	want(t, e.ok("ingress", "list").stdout, created.ID)
	listed := decodeJSON[[]streamkit.Ingress](t, e.ok("ingress", "list", "--room", room, "--json").stdout)
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("listed: %+v", listed)
	}
	if none := decodeJSON[[]streamkit.Ingress](t, e.ok("ingress", "list", "--room", room+"-other", "--json").stdout); len(none) != 0 {
		t.Errorf("other room: %+v", none)
	}
	updated := decodeJSON[streamkit.Ingress](t, e.ok("ingress", "update", created.ID, "--name", "Renamed", "--disable").stdout)
	if updated.Name != "Renamed" || updated.Enabled {
		t.Errorf("updated: %+v", updated)
	}
	rotated := decodeJSON[streamkit.Ingress](t, e.ok("ingress", "rotate-key", created.ID).stdout)
	if rotated.StreamKey == "" || rotated.StreamKey == created.StreamKey {
		t.Errorf("rotated: %+v", rotated)
	}
	decodeJSON[[]streamkit.IngressEvent](t, e.ok("ingress", "logs", created.ID, "--limit", "5").stdout)
	decodeJSON[streamkit.Ingress](t, e.ok("ingress", "stop", created.ID).stdout)
	e.ok("ingress", "delete", "--id", created.ID)
	want(t, e.fails("ingress", "get", created.ID).stderr, "not_found")
	want(t, e.fails("ingress", "create", "--input", "carrier-pigeon", "--room", room, "--identity", "x").stderr, "validation_failed")
}

func TestEgressCommands(t *testing.T) {
	e := ourEnv(t)
	if !fx.Info.Workers {
		want(t, e.fails("egress", "start", "--room", roomName(t), "--format", "mp4").stderr, "egress_unavailable")
		t.Skip("STREAMKIT_TEST_WORKERS is not set")
	}
	room := roomName(t)
	want(t, e.fails("egress", "start", "--room", room, "--format", "mp4").stderr, "room_not_found")

	tg := target{"ours", fx.Info.Ours, e, nil}
	publisher := join(t, tg, room, fixture.PeerOptions{Identity: "on-camera", Publish: true})
	if _, err := publisher.WaitPublished(2); err != nil {
		t.Fatal(err)
	}
	job := decodeJSON[streamkit.Egress](t, e.ok("egress", "start", "--room", room, "--format", "mp4", "--layout", "grid").stdout)
	if job.ID == "" || job.Type != streamkit.EgressRoomComposite || job.Format != "mp4" {
		t.Errorf("job: %+v", job)
	}
	want(t, e.ok("egress", "list", "--room", room).stdout, job.ID)
	listed := decodeJSON[[]streamkit.Egress](t, e.ok("egress", "list", "--active", "--id", job.ID, "--json").stdout)
	if len(listed) != 1 {
		t.Errorf("listed: %+v", listed)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		current := decodeJSON[streamkit.Egress](t, e.ok("egress", "get", job.ID).stdout)
		if current.Status == streamkit.EgressActive {
			break
		}
		if current.Ended() || time.Now().After(deadline) {
			t.Fatalf("job: %+v", current)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Let it record something worth checking.
	time.Sleep(5 * time.Second)
	done := decodeJSON[streamkit.Egress](t, e.ok("egress", "stop", "--id", job.ID, "--wait").stdout)
	if done.Status != streamkit.EgressComplete || done.FileSize == 0 {
		t.Fatalf("job: %+v", done)
	}
	want(t, e.fails("egress", "stop", job.ID).stderr, "egress_not_running")
	if active := decodeJSON[[]streamkit.Egress](t, e.ok("egress", "list", "--active", "--id", job.ID, "--json").stdout); len(active) != 0 {
		t.Errorf("active: %+v", active)
	}

	path := filepath.Join(t.TempDir(), "recording.mp4")
	e.ok("egress", "download", job.ID, "--out", path)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "video") {
		t.Errorf("the file is not a video: %v: %s", err, out)
	}
	retried := decodeJSON[streamkit.Egress](t, e.ok("egress", "retry", job.ID).stdout)
	if retried.ID == job.ID {
		t.Errorf("retry: %+v", retried)
	}
	e.ok("egress", "stop", "--id", retried.ID, "--wait")
	e.ok("egress", "delete", job.ID)
	want(t, e.fails("egress", "get", job.ID).stderr, "not_found")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestWebhookTestAndListen(t *testing.T) {
	e := newEnv(t).with("STREAMKIT_API_KEY=k", "STREAMKIT_API_SECRET="+testSecret)
	port := freePort(t)
	listen := exec.Command(cli, "webhook", "listen", "--port", fmt.Sprint(port), "--count", "2", "--timeout", "30s")
	listen.Env = e.environ()
	var stdout, stderr bytes.Buffer
	listen.Stdout, listen.Stderr = &stdout, &stderr
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listen.Process.Kill() }()
	target := fmt.Sprintf("http://127.0.0.1:%d/", port)
	waitListening(t, target)

	r := e.ok("webhook", "test", "--target", target, "--event", "participant_joined", "--room", "webhook-room")
	want(t, r.stdout, "HTTP 200")

	// A wrong secret is rejected by the listener, and the sender reports it.
	bad := e.with("STREAMKIT_API_SECRET=not-the-secret").fails("webhook", "test", "--target", target)
	want(t, bad.stderr, "HTTP 401")

	body := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(body, []byte(`{"event":"room_finished","id":"EV_file","room":{"name":"from-a-file"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ok("webhook", "test", "--target", target, "--body-file", body, "--json")

	if err := listen.Wait(); err != nil {
		t.Fatalf("listen: %v\n%s", err, stderr.String())
	}
	want(t, stdout.String(), `"event":"participant_joined"`)
	want(t, stdout.String(), "webhook-room")
	want(t, stdout.String(), "from-a-file")
	want(t, stderr.String(), "Rejected a webhook")

	r = e.fails("webhook", "test", "--target", "http://127.0.0.1:1/")
	want(t, r.stderr, "could not reach the receiver")
}

func waitListening(t *testing.T, target string) {
	t.Helper()
	for range 100 {
		res, err := http.Post(target, "application/json", strings.NewReader("{}"))
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the listener did not start")
}

// The receiver the CLI signs for is the same one a real server's deliveries pass.
func TestWebhookTestReachesTheCapture(t *testing.T) {
	if fx == nil {
		t.Skip("needs running servers")
	}
	e := newEnv(t).with("STREAMKIT_API_KEY=k", "STREAMKIT_API_SECRET="+testSecret)
	e.ok("webhook", "test", "--target", fx.Info.Webhooks+"/hook/cli-test", "--event", "room_started", "--room", "captured")
	captured, err := fx.CapturedWebhooks("cli-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0].ContentType != "application/webhook+json" {
		t.Fatalf("captured: %+v", captured)
	}
	event, err := streamkit.NewWebhookReceiver(streamkit.Keys{"k": testSecret}).Receive(captured[0].Body, captured[0].Authorization)
	if err != nil {
		t.Fatal(err)
	}
	if event.GetRoom().GetName() != "captured" || event.GetEvent() != "room_started" {
		t.Errorf("event: %v", event)
	}
}

func TestLoadTest(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		r := tg.env.ok("perf", "load-test", "--publishers", "1", "--subscribers", "1", "--duration", "3s", "--room", roomName(t))
		out := decodeJSON[map[string]any](t, r.stdout)
		if out["connected"] != float64(2) || out["failed"] != float64(0) {
			t.Errorf("result: %v", out)
		}
		if out["tracks_expected"] != float64(2) || out["tracks_received"] != float64(2) {
			t.Errorf("result: %v", out)
		}
		if out["received_packets"].(float64) == 0 {
			t.Errorf("no media arrived: %v", out)
		}

		audio := decodeJSON[map[string]any](t, tg.env.ok("perf", "load-test", "--audio-publishers", "1", "--subscribers", "1", "--duration", "2s", "--no-video").stdout)
		if audio["tracks_expected"] != float64(1) || audio["tracks_received"] != float64(1) {
			t.Errorf("audio only: %v", audio)
		}
	})
}
