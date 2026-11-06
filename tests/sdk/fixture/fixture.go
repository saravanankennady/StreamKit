// Package fixture starts what the server SDK and CLI tests run against: this server with its control plane, the
// reference server, and if asked, the egress and ingress workers. Nothing here is simulated. Go tests use it directly.
// Node and Python tests run cmd/sdkfixture, which prints the same Info as one line of JSON and stops everything when
// its standard input closes.
package fixture

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultDatabase = "postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable"

// Server is one server under test.
type Server struct {
	URL       string `json:"url"`
	HTTP      string `json:"http"`
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

// Info says where everything is.
type Info struct {
	// Ours has the control plane. Its key belongs to a real project.
	Ours *Server `json:"ours"`
	// Reference is the reference server. Nil if its binary is not available.
	Reference *Server `json:"reference"`
	// Plain is this server with no database, as --dev runs it.
	Plain *Server `json:"plain"`
	// Webhooks is the base address of the webhook capture.
	Webhooks string `json:"webhooks"`
	// CLI is the built streamkit binary.
	CLI string `json:"cli"`
	// ReferenceBinary is the reference server's binary, which can also create join tokens.
	ReferenceBinary string `json:"reference_binary"`
	// Lk is the reference CLI, if a built copy is available.
	Lk string `json:"lk"`
	// RTMP is the address the ingress worker listens on, when workers are running.
	RTMP string `json:"rtmp"`
	// Workers says the egress and ingress workers are running.
	Workers bool `json:"workers"`
}

// Options chooses what to start.
type Options struct {
	// Workers starts the egress and ingress workers, which need Chrome and ffmpeg. Otherwise those tests skip.
	Workers bool
	// Log receives progress.
	Log io.Writer
}

// Fixture is a running set of servers.
type Fixture struct {
	Info Info

	opts     Options
	repo     string
	dir      string
	bin      string
	children []*child
	capture  *capture
	dbName   string
	adminDB  string
	stopOnce sync.Once
}

type child struct {
	name string
	cmd  *exec.Cmd
	done chan error
}

func freePort(network string) (int, error) {
	if network == "udp" {
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port, nil
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// RepoRoot finds the repository from the working directory, so a test can run from anywhere inside it.
func RepoRoot() (string, error) {
	if v := os.Getenv("STREAMKIT_REPO"); v != "" {
		return v, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(data), "module elnova-streamkit.local") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find the repository: run inside it, or set STREAMKIT_REPO")
		}
		dir = parent
	}
}

// findUpward looks for rel in dir and each directory above it. The reference tree sits beside the repository, and a
// worktree sits below it.
func findUpward(dir, rel string) string {
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Start builds what is needed and starts it.
func Start(ctx context.Context, opts Options) (*Fixture, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	repo, err := RepoRoot()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "sdkfixture-")
	if err != nil {
		return nil, err
	}
	f := &Fixture{opts: opts, repo: repo, dir: dir}
	if err := f.start(ctx); err != nil {
		f.Stop()
		return nil, err
	}
	return f, nil
}

func (f *Fixture) start(ctx context.Context) error {
	builds := map[string]string{"streamkit-server": "./server/cmd/streamkit-server", "streamkit": "./cli"}
	if f.opts.Workers {
		builds["streamkit-egress"] = "./egress/cmd/streamkit-egress"
		builds["streamkit-ingress"] = "./ingress/cmd/streamkit-ingress"
	}
	// Into this fixture's own directory: replacing a binary that another fixture is running can fail.
	f.bin = filepath.Join(f.dir, "bin")
	for name, pkg := range builds {
		fmt.Fprintln(f.opts.Log, "building", name)
		cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(f.bin, name), pkg)
		cmd.Dir = f.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %s: %w\n%s", pkg, err, out)
		}
	}
	f.Info.CLI = filepath.Join(f.bin, "streamkit")

	capture, err := startCapture()
	if err != nil {
		return err
	}
	f.capture = capture
	f.Info.Webhooks = capture.base

	if err := f.startPlain(ctx); err != nil {
		return err
	}
	if err := f.startOurs(ctx); err != nil {
		return err
	}
	if bin := findUpward(f.repo, "reference/oracle/bin/x/livekit-server"); bin != "" {
		f.Info.ReferenceBinary = bin
		if err := f.startReference(ctx, bin); err != nil {
			return err
		}
	}
	if bin := findUpward(f.repo, ".toolchain/bin/lk"); bin != "" {
		f.Info.Lk = bin
	}
	return nil
}

func (f *Fixture) startProcess(name, binary string, args ...string) (*child, error) {
	logFile, err := os.Create(filepath.Join(f.dir, name+".log"))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = f.dir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Its own process group, so a terminal's signal reaches only the fixture, which then stops these in order.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	c := &child{name: name, cmd: cmd, done: make(chan error, 1)}
	go func() {
		c.done <- cmd.Wait()
		logFile.Close()
	}()
	f.children = append(f.children, c)
	return c, nil
}

// LogTail returns the end of a process's log, to explain a failure.
func (f *Fixture) LogTail(name string) string {
	data, err := os.ReadFile(filepath.Join(f.dir, name+".log"))
	if err != nil {
		return err.Error()
	}
	const keep = 4000
	if len(data) > keep {
		data = data[len(data)-keep:]
	}
	return string(data)
}

func (f *Fixture) waitHTTP(ctx context.Context, c *child, base string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.done:
			return fmt.Errorf("%s exited: %v\n%s", c.name, err, f.LogTail(c.name))
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s did not start\n%s", c.name, f.LogTail(c.name))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// startPlain runs this server the way --dev does, with no database.
func (f *Fixture) startPlain(ctx context.Context) error {
	port, udp, tcp, err := threePorts()
	if err != nil {
		return err
	}
	path := filepath.Join(f.dir, "plain.yaml")
	config := fmt.Sprintf("port: %d\nlog_level: info\nrtc:\n  udp_port: %d\n  tcp_port: %d\n", port, udp, tcp)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return err
	}
	c, err := f.startProcess("plain", filepath.Join(f.bin, "streamkit-server"), "--dev", "--config", path)
	if err != nil {
		return err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := f.waitHTTP(ctx, c, base); err != nil {
		return err
	}
	f.Info.Plain = &Server{URL: fmt.Sprintf("ws://127.0.0.1:%d", port), HTTP: base, APIKey: "devkey", APISecret: "secret"}
	return nil
}

func threePorts() (http, udp, tcp int, err error) {
	if http, err = freePort("tcp"); err != nil {
		return
	}
	if udp, err = freePort("udp"); err != nil {
		return
	}
	tcp, err = freePort("tcp")
	return
}

// startOurs runs this server with a database of its own, and signs up for a project with a key and a webhook.
func (f *Fixture) startOurs(ctx context.Context) error {
	admin := os.Getenv("STREAMKIT_TEST_DATABASE_URL")
	if admin == "" {
		admin = defaultDatabase
	}
	f.adminDB = admin
	name := fmt.Sprintf("streamkit_sdk_%d_%s", os.Getpid(), randomHex(4))
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return fmt.Errorf("the control plane needs PostgreSQL (STREAMKIT_TEST_DATABASE_URL): %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	f.dbName = name
	u, err := url.Parse(admin)
	if err != nil {
		return err
	}
	u.Path = "/" + name

	port, udp, tcp, err := threePorts()
	if err != nil {
		return err
	}
	rtmp, err := freePort("tcp")
	if err != nil {
		return err
	}
	ws := fmt.Sprintf("ws://127.0.0.1:%d", port)
	config := fmt.Sprintf(`port: %d
log_level: info
rtc:
  udp_port: %d
  tcp_port: %d
room:
  auto_create: true
  empty_timeout: 300
  departure_timeout: 20
control_plane:
  database_url: %q
  master_key: %s
  public_dashboard_url: http://localhost:3000
  public_ws_url: %s
  allow_private_webhooks: true
  mail:
    dir: %s
media:
  ws_url: %s
  allow_private_addresses: true
  storage:
    kind: local
    dir: %s
  egress:
    work_dir: %s
    lease_seconds: 6
  ingress:
    rtmp_addr: 127.0.0.1:%d
    lease_seconds: 6
    stall_seconds: 60
`, port, udp, tcp, u.String(), randomHex(32), ws, filepath.Join(f.dir, "mail"), ws,
		filepath.Join(f.dir, "recordings"), filepath.Join(f.dir, "work"), rtmp)
	path := filepath.Join(f.dir, "ours.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return err
	}
	bin := func(name string) string { return filepath.Join(f.bin, name) }
	c, err := f.startProcess("ours", bin("streamkit-server"), "--dev", "--config", path)
	if err != nil {
		return err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := f.waitHTTP(ctx, c, base); err != nil {
		return err
	}
	session, key, secret, err := signUp(ctx, base, f.capture.base+"/hook/ours")
	if err != nil {
		return fmt.Errorf("could not sign up: %w", err)
	}
	f.Info.Ours = &Server{URL: ws, HTTP: base, APIKey: key, APISecret: secret}

	if f.opts.Workers {
		if _, err := f.startProcess("egress", bin("streamkit-egress"), "--dev", "--config", path); err != nil {
			return err
		}
		if _, err := f.startProcess("ingress", bin("streamkit-ingress"), "--dev", "--config", path); err != nil {
			return err
		}
		for _, kind := range []string{"egress_worker", "ingress_worker"} {
			if err := waitForWorker(ctx, session, kind); err != nil {
				return fmt.Errorf("%w\n%s", err, f.LogTail(strings.TrimSuffix(kind, "_worker")))
			}
		}
		f.Info.RTMP = fmt.Sprintf("127.0.0.1:%d", rtmp)
		f.Info.Workers = true
	}
	return nil
}

// session is a signed-in browser, as far as the API can tell.
type session struct {
	base, cookie, project string
}

func (s *session) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cookie != "" {
		req.Header.Set("Cookie", s.cookie)
	}
	if s.project != "" {
		req.Header.Set("X-Project-ID", s.project)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, res.StatusCode, data)
	}
	if v := res.Header.Get("Set-Cookie"); v != "" {
		s.cookie, _, _ = strings.Cut(v, ";")
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func signUp(ctx context.Context, base, webhook string) (s *session, apiKey, apiSecret string, err error) {
	s = &session{base: base}
	if err = s.call(ctx, http.MethodPost, "/auth/register", map[string]string{
		"email": "sdk-tests@example.com", "password": "a-good-long-password-for-tests", "name": "SDK tests", "organization": "SDK tests",
	}, nil); err != nil {
		return
	}
	var project struct {
		ID string `json:"id"`
	}
	if err = s.call(ctx, http.MethodPost, "/projects", map[string]string{"name": "SDK tests"}, &project); err != nil {
		return
	}
	s.project = project.ID
	var key struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
		Secret string `json:"secret"`
	}
	if err = s.call(ctx, http.MethodPost, "/keys", map[string]string{"name": "SDK tests"}, &key); err != nil {
		return
	}
	err = s.call(ctx, http.MethodPost, "/webhooks", map[string]any{"url": webhook, "api_key_id": key.ID, "events": []string{}}, nil)
	return s, key.APIKey, key.Secret, err
}

func waitForWorker(ctx context.Context, s *session, kind string) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var servers struct {
			Items []struct {
				Stats struct {
					Kind    string `json:"kind"`
					Healthy *bool  `json:"healthy"`
				} `json:"stats"`
			} `json:"items"`
		}
		if err := s.call(ctx, http.MethodGet, "/servers", nil, &servers); err != nil {
			return err
		}
		for _, row := range servers.Items {
			if row.Stats.Kind == kind && (row.Stats.Healthy == nil || *row.Stats.Healthy) {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("no healthy %s appeared", kind)
}

func (f *Fixture) startReference(ctx context.Context, bin string) error {
	port, udp, tcp, err := threePorts()
	if err != nil {
		return err
	}
	config := fmt.Sprintf(`port: %d
bind_addresses: ["127.0.0.1"]
rtc:
  udp_port: %d
  tcp_port: %d
  use_external_ip: false
  enable_loopback_candidate: true
keys:
  devkey: secret
webhook:
  api_key: devkey
  urls:
    - %s/hook/reference
`, port, udp, tcp, f.capture.base)
	c, err := f.startProcess("reference", bin, "--dev", "--node-ip", "127.0.0.1", "--config-body", config)
	if err != nil {
		return err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := f.waitHTTP(ctx, c, base); err != nil {
		return err
	}
	f.Info.Reference = &Server{URL: fmt.Sprintf("ws://127.0.0.1:%d", port), HTTP: base, APIKey: "devkey", APISecret: "secret"}
	return nil
}

// Stop stops everything this fixture started, by PID, and removes its files and database.
func (f *Fixture) Stop() {
	f.stopOnce.Do(func() {
		// Workers first: they report to the server on their way out.
		for i := len(f.children) - 1; i >= 0; i-- {
			c := f.children[i]
			_ = c.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-c.done:
			case <-time.After(15 * time.Second):
				_ = c.cmd.Process.Kill()
				<-c.done
			}
		}
		if f.capture != nil {
			f.capture.close()
		}
		if f.dbName != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if conn, err := pgx.Connect(ctx, f.adminDB); err == nil {
				_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+f.dbName+" WITH (FORCE)")
				_ = conn.Close(ctx)
			}
		}
		if f.dir != "" {
			_ = os.RemoveAll(f.dir)
		}
	})
}

// Captured is one webhook as it arrived.
type Captured struct {
	Authorization string `json:"authorization"`
	ContentType   string `json:"content_type"`
	UserAgent     string `json:"user_agent"`
	Body          []byte `json:"body"`
}

// capture keeps every webhook it is sent, so that a test in any language can run the exact bytes through its SDK.
type capture struct {
	base   string
	server *http.Server
	mu     sync.Mutex
	got    map[string][]Captured
}

func startCapture() (*capture, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	c := &capture{base: "http://" + l.Addr().String(), got: map[string][]Captured{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook/{target}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.got[r.PathValue("target")] = append(c.got[r.PathValue("target")], Captured{
			Authorization: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"), UserAgent: r.Header.Get("User-Agent"), Body: body,
		})
		c.mu.Unlock()
	})
	mux.HandleFunc("GET /captured/{target}", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		items := c.got[r.PathValue("target")]
		if items == nil {
			items = []Captured{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})
	c.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.server.Serve(l) }()
	return c, nil
}

func (c *capture) close() { _ = c.server.Close() }

// CapturedWebhooks reads what a server has delivered so far.
func (f *Fixture) CapturedWebhooks(target string) ([]Captured, error) {
	res, err := http.Get(f.Info.Webhooks + "/captured/" + target)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out []Captured
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// ReferenceJoinToken asks the reference server's own tool for a token.
func (f *Fixture) ReferenceJoinToken(ctx context.Context, s *Server, room, identity string) (string, error) {
	if f.Info.ReferenceBinary == "" {
		return "", errors.New("the reference server is not available")
	}
	cmd := exec.CommandContext(ctx, f.Info.ReferenceBinary, "--keys", s.APIKey+": "+s.APISecret, "create-join-token", "--room", room, "--identity", identity)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strconv.Quote(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(line), "Token:"); ok {
			return strings.TrimSpace(token), nil
		}
		if strings.Count(line, ".") == 2 && strings.HasPrefix(strings.TrimSpace(line), "eyJ") {
			return strings.TrimSpace(line), nil
		}
	}
	return "", fmt.Errorf("no token in the tool's output: %q", out)
}
