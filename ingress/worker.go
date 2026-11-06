// Package ingress brings outside media into rooms. Push over RTMP and pull from a URL both run here: ffmpeg transcodes to
// what browsers can decode and sends RTP to local sockets, and a Pion client publishes those into the room. WHIP is
// handled by the server, in the whip package, because a WHIP client can talk straight to the SFU.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/confine"
	"elnova-streamkit.local/egress/heartbeat"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/participant"
)

const (
	claimInterval = time.Second
	defaultLease  = 30 * time.Second
	defaultStall  = time.Minute
)

// Options configures a Worker.
type Options struct {
	Config     *config.Config
	Store      *store.Store
	Log        *slog.Logger
	StaticKeys map[string]string
}

// Worker runs ingest sessions.
type Worker struct {
	opts     Options
	log      *slog.Logger
	id       string
	lease    time.Duration
	stall    time.Duration
	ffmpeg   string
	wsURL    string
	maxJobs  int
	reporter *heartbeat.Reporter

	listenAddr atomic.Pointer[string]
	rtmpConns  atomic.Int64
	rtmpByIP   ipCounter
	active     atomic.Int32
	sessions   sync.WaitGroup

	mu      sync.Mutex
	running map[*session]bool
}

// New checks that everything a session needs exists.
func New(opts Options) (*Worker, error) {
	w := &Worker{
		opts: opts, log: opts.Log.With("service", "ingress"), id: participant.NewID("IW_"),
		lease: time.Duration(opts.Config.Media.Ingress.LeaseSeconds) * time.Second, maxJobs: opts.Config.Media.Ingress.MaxSessions,
		stall:   time.Duration(opts.Config.Media.Ingress.StallSeconds) * time.Second,
		running: map[*session]bool{},
	}
	if w.lease <= 0 {
		w.lease = defaultLease
	}
	if w.stall <= 0 {
		w.stall = defaultStall
	}
	if w.maxJobs <= 0 {
		w.maxJobs = max(1, runtime.NumCPU()/2)
	}
	w.wsURL = opts.Config.Media.WSURL
	if w.wsURL == "" {
		w.wsURL = opts.Config.ControlPlane.PublicWSURL
	}
	if w.wsURL == "" {
		return nil, errors.New("workers need the server's address: set media.ws_url")
	}
	w.ffmpeg = "ffmpeg"
	if v := os.Getenv("STREAMKIT_FFMPEG"); v != "" {
		w.ffmpeg = v
	}
	path, err := exec.LookPath(w.ffmpeg)
	if err != nil {
		return nil, fmt.Errorf("%s is required: %w", w.ffmpeg, err)
	}
	w.ffmpeg = path
	addr := opts.Config.Media.Ingress.RTMPAddr
	if addr == "" {
		addr = ":1935"
	}
	w.reporter = &heartbeat.Reporter{
		Store: opts.Store, Log: w.log, NodeID: w.id, Kind: store.WorkerKindIngress, Region: opts.Config.Region, Address: "rtmp://" + addr, MaxJobs: w.maxJobs,
		Active: func() int { return int(w.active.Load()) },
		Check:  w.checkHealth,
	}
	return w, nil
}

// checkHealth is what a stream would need: the transcoder, and a listener that is still answering.
func (w *Worker) checkHealth(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, w.ffmpeg, "-version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg cannot run: %w", err)
	}
	addr := w.listenAddr.Load()
	if addr == nil {
		return nil // Not listening yet: Run fails the whole worker if it cannot.
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("the RTMP port is not answering: %w", err)
	}
	return conn.Close()
}

func (w *Worker) hasRoom() bool {
	return int(w.active.Load()) < w.maxJobs && !w.reporter.Draining() && w.reporter.Healthy()
}

func (w *Worker) track(s *session) {
	w.active.Add(1)
	w.sessions.Add(1)
	w.mu.Lock()
	w.running[s] = true
	w.mu.Unlock()
}

func (w *Worker) untrack(s *session) {
	w.mu.Lock()
	delete(w.running, s)
	w.mu.Unlock()
	w.active.Add(-1)
	w.sessions.Done()
}

// Run serves until ctx ends, then ends every stream cleanly, so their ingresses are free to be used again at once.
func (w *Worker) Run(ctx context.Context) error {
	addr := w.opts.Config.Media.Ingress.RTMPAddr
	if addr == "" {
		addr = ":1935"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("RTMP port: %w", err)
	}
	// The health check connects to this from inside, so a wildcard address needs a real one.
	port := listener.Addr().(*net.TCPAddr)
	self := net.JoinHostPort("127.0.0.1", strconv.Itoa(port.Port))
	if !port.IP.IsUnspecified() {
		self = listener.Addr().String()
	}
	w.listenAddr.Store(&self)
	w.log.Info("ingress worker started", "worker", w.id, "rtmp", addr, "maxSessions", w.maxJobs, "confinement", confine.Status())
	reportCtx, stopReport := context.WithCancel(context.Background())
	defer stopReport()
	var background sync.WaitGroup
	background.Add(4)
	go func() { defer background.Done(); w.reporter.Run(reportCtx) }()
	go func() { defer background.Done(); heartbeat.Reap(reportCtx, w.opts.Store, w.log) }()
	go func() { defer background.Done(); w.serveRTMP(reportCtx, listener) }()
	go func() { defer background.Done(); confine.ReapOrphans(reportCtx) }()

	t := time.NewTicker(claimInterval)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			w.claimPulls(ctx)
		}
	}
	_ = listener.Close()
	w.mu.Lock()
	for s := range w.running {
		s.finishForRestart("The worker is shutting down.")
	}
	w.mu.Unlock()
	w.sessions.Wait()
	stopReport()
	background.Wait()
	w.log.Info("ingress worker stopped")
	return nil
}

func (w *Worker) claimPulls(ctx context.Context) {
	for w.hasRoom() {
		in, err := w.opts.Store.ClaimIngressPull(ctx, w.id, w.lease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("could not claim a URL input", "error", err)
			}
			return
		}
		if in == nil {
			return
		}
		s := newSession(w, in)
		w.track(s)
		go func() {
			defer w.untrack(s)
			s.runPull()
		}()
	}
}

// signingKey finds the key for a session's access token.
func (w *Worker) signingKey(ctx context.Context, projectID string) (auth.Key, error) {
	return w.opts.Store.SigningKeyFor(ctx, projectID, auth.StaticKeys{ProjectID: auth.StaticProject, Keys: w.opts.StaticKeys})
}
