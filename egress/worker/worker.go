// Package worker runs recording jobs. Each job is one headless Chrome on a page that joins the room as a hidden
// participant and records it: see ../template. The page streams its recording here over a local WebSocket. This
// writes it to disk and to any RTMP destinations, and when it ends, ffmpeg turns the capture into the final file.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/confine"
	"elnova-streamkit.local/egress/heartbeat"
	"elnova-streamkit.local/egress/storage"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/participant"
)

const (
	claimInterval      = time.Second
	salvageInterval    = 30 * time.Second
	salvageRetryFirst  = time.Minute
	salvageRetryMost   = time.Hour
	salvageStateFile   = "salvage.json"
	lockFile           = ".lock"
	defaultLease       = 30 * time.Second
	minFreeDisk        = 1 << 30
	chromeCheckTimeout = 45 * time.Second
)

// Options configures a Worker.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	Storage storage.Storage
	Log     *slog.Logger
	// StaticKeys are the server's configured keys, which sign tokens for the project that has no database row.
	StaticKeys map[string]string
}

// Worker claims and runs jobs.
type Worker struct {
	opts     Options
	cfg      config.EgressWorker
	log      *slog.Logger
	id       string
	bridge   *bridge
	reporter *heartbeat.Reporter
	maxJobs  int
	lease    time.Duration
	workDir  string
	// waitForMedia and maxDuration are the limits on one job. See the config for what they are.
	waitForMedia time.Duration
	maxDuration  time.Duration
	chrome       string
	ffmpeg       string
	ffprobe      string
	wsURL        string

	active atomic.Int32
	jobs   sync.WaitGroup

	mu      sync.Mutex
	running map[string]*job
}

// workDir says where jobs keep their files. It is always absolute, because Chrome opens its launcher page by a file://
// address, and a relative path there would read as a host name.
func workDir(dir string) string {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "streamkit-egress")
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// New checks that everything a job needs exists, so a broken install fails at startup rather than job by job.
func New(opts Options) (*Worker, error) {
	cfg := opts.Config.Media.Egress
	w := &Worker{
		opts: opts, cfg: cfg, log: opts.Log.With("service", "egress"), id: participant.NewID("EW_"),
		maxJobs: cfg.MaxJobs, lease: time.Duration(cfg.LeaseSeconds) * time.Second, workDir: cfg.WorkDir, chrome: cfg.ChromePath,
		ffmpeg: "ffmpeg", ffprobe: "ffprobe", running: map[string]*job{},
	}
	if w.maxJobs <= 0 {
		w.maxJobs = max(1, runtime.NumCPU()/3)
	}
	if w.lease <= 0 {
		w.lease = defaultLease
	}
	w.workDir = workDir(w.workDir)
	w.waitForMedia = time.Duration(cfg.WaitForMediaSeconds) * time.Second
	if w.waitForMedia <= 0 {
		w.waitForMedia = defaultWaitMedia
	}
	w.maxDuration = time.Duration(cfg.MaxDurationSeconds) * time.Second
	if w.maxDuration <= 0 {
		w.maxDuration = defaultMaxDuration
	}
	if w.chrome == "" {
		w.chrome = "google-chrome"
	}
	if v := os.Getenv("STREAMKIT_FFMPEG"); v != "" {
		w.ffmpeg = v
	}
	w.wsURL = opts.Config.Media.WSURL
	if w.wsURL == "" {
		w.wsURL = opts.Config.ControlPlane.PublicWSURL
	}
	if w.wsURL == "" {
		return nil, errors.New("workers need the server's address: set media.ws_url")
	}
	for _, binary := range []*string{&w.chrome, &w.ffmpeg, &w.ffprobe} {
		path, err := lookPath(*binary)
		if err != nil {
			return nil, fmt.Errorf("%s is required: %w", *binary, err)
		}
		*binary = path
	}
	if err := os.MkdirAll(w.workDir, 0o700); err != nil {
		return nil, err
	}
	b, err := newBridge(w.log)
	if err != nil {
		return nil, err
	}
	w.bridge = b
	w.reporter = &heartbeat.Reporter{
		Store: opts.Store, Log: w.log, NodeID: w.id, Kind: store.WorkerKindEgress, Region: opts.Config.Region, MaxJobs: w.maxJobs,
		Active: func() int { return int(w.active.Load()) },
		Check:  w.checkHealth,
	}
	if err := w.checkChromeRuns(); err != nil {
		b.close()
		return nil, err
	}
	if cfg.ChromeNoSandbox {
		w.log.Warn("Chrome's sandbox is off, so a bug in its media decoders gives whoever sends the media everything this worker's user can reach")
	}
	return w, nil
}

// checkHealth is what a job would need: somewhere to work, what it runs, and somewhere to put the result.
func (w *Worker) checkHealth(ctx context.Context) error {
	if err := checkWorkDir(w.workDir); err != nil {
		return err
	}
	for _, binary := range []string{w.ffmpeg, w.ffprobe, w.chrome} {
		if err := checkRuns(ctx, binary); err != nil {
			return err
		}
	}
	return w.opts.Storage.Check(ctx)
}

// checkWorkDir writes to the working directory and looks at how much room is left, since recordings wait there.
func checkWorkDir(dir string) error {
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return fmt.Errorf("the working directory cannot be written: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	free, err := freeDisk(dir)
	if err != nil {
		return fmt.Errorf("the working directory cannot be read: %w", err)
	}
	if free < minFreeDisk {
		return fmt.Errorf("the working directory has only %d MB free", free>>20)
	}
	return nil
}

func freeDisk(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// lockDir takes a job directory for this process, or reports false if another has it. The kernel lets go if this process dies,
// which is what makes it safe for workers sharing a volume: a job's own worker holds it until it has finished with the files.
func lockDir(dir string) (release func(), ok bool) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false
	}
	return func() { _ = f.Close() }, true
}

func checkRuns(ctx context.Context, binary string) error {
	flag := "-version"
	if strings.Contains(filepath.Base(binary), "chrome") {
		flag = "--version"
	}
	cmd := exec.CommandContext(ctx, binary, flag)
	cmd.Env = mediaEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		text := strings.TrimSpace(string(out))
		if len(text) > 200 {
			text = text[:200]
		}
		return fmt.Errorf("%s cannot run: %v %s", filepath.Base(binary), err, text)
	}
	return nil
}

// checkChromeRuns really opens a page, once, at startup. A browser that cannot sandbox itself still answers --version,
// and would otherwise fail every job, one at a time, with a much less clear message.
func (w *Worker) checkChromeRuns() error {
	profile, err := os.MkdirTemp(w.workDir, "chrome-check-")
	if err != nil {
		return fmt.Errorf("the working directory cannot be written: %w", err)
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(context.Background(), chromeCheckTimeout)
	defer cancel()
	binary, args := chromeCommand(w.chrome, append(chromeArgs(profile, w.cfg.ChromeNoSandbox), "--dump-dom", "about:blank"), profile)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = chromeEnv(profile)
	cmd.SysProcAttr = childAttrs()
	var stderr tailBuffer
	cmd.Stderr = &stderr
	cmd.Cancel = func() error {
		killTree(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "<html") {
		hint := ""
		if !w.cfg.ChromeNoSandbox {
			hint = " If this is a container, it needs the seccomp profile in infrastructure/docker/chrome-seccomp.json, so Chrome's sandbox can start. Only if that cannot be done, set media.egress.chrome_no_sandbox, and read what that costs in docs/api/media-services.md."
		}
		return fmt.Errorf("Chrome cannot open a page here (%v): %s.%s", err, chromeReason(&stderr), hint)
	}
	return nil
}

// Run takes jobs until ctx ends, then asks running jobs to finish and waits for them, so a clean shutdown leaves
// finished recordings rather than failed ones.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("egress worker started", "worker", w.id, "maxJobs", w.maxJobs, "workDir", w.workDir, "storage", w.opts.Storage.Kind(), "confinement", confine.Status())
	reportCtx, stopReport := context.WithCancel(context.Background())
	defer stopReport()
	var background sync.WaitGroup
	background.Add(4)
	go func() { defer background.Done(); w.reporter.Run(reportCtx) }()
	go func() { defer background.Done(); heartbeat.Reap(reportCtx, w.opts.Store, w.log) }()
	go func() { defer background.Done(); confine.ReapOrphans(reportCtx) }()
	// Finishing a long capture can take a long time, and new jobs must not wait for that.
	go func() { defer background.Done(); w.salvageLoop(ctx) }()

	claims := time.NewTicker(claimInterval)
	defer claims.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-claims.C:
			w.claim(ctx)
		}
	}
	w.reporter.Leave(reportCtx)
	w.mu.Lock()
	for _, j := range w.running {
		j.requestStop()
	}
	w.mu.Unlock()
	w.jobs.Wait()
	stopReport()
	background.Wait()
	w.bridge.close()
	w.log.Info("egress worker stopped")
}

func (w *Worker) claim(ctx context.Context) {
	for int(w.active.Load()) < w.maxJobs && !w.reporter.Draining() && w.reporter.Healthy() {
		e, err := w.opts.Store.ClaimEgress(ctx, w.id, w.lease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("could not claim a job", "error", err)
			}
			return
		}
		if e == nil {
			return
		}
		j := newJob(w, e)
		w.active.Add(1)
		w.jobs.Add(1)
		w.mu.Lock()
		w.running[e.ID] = j
		w.mu.Unlock()
		go func() {
			defer w.jobs.Done()
			defer w.active.Add(-1)
			defer func() {
				w.mu.Lock()
				delete(w.running, e.ID)
				w.mu.Unlock()
			}()
			j.run()
		}()
	}
}

// signingKey finds the key for a job's access token.
func (w *Worker) signingKey(ctx context.Context, projectID string) (auth.Key, error) {
	return w.opts.Store.SigningKeyFor(ctx, projectID, auth.StaticKeys{ProjectID: auth.StaticProject, Keys: w.opts.StaticKeys})
}

// salvageLoop looks for what jobs have left behind, until ctx ends.
func (w *Worker) salvageLoop(ctx context.Context) {
	w.salvage(ctx)
	t := time.NewTicker(salvageInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.salvage(ctx)
		}
	}
}

// salvage goes through job directories left behind. A worker that was killed cannot finish its file, but its capture
// is still there, and any complete part of it is still a recording.
func (w *Worker) salvage(ctx context.Context) {
	entries, err := os.ReadDir(w.workDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "EG_") {
			continue
		}
		w.salvageDir(ctx, entry.Name())
	}
}

// salvageState is what a directory remembers about attempts to recover it.
type salvageState struct {
	Attempts int       `json:"attempts"`
	Next     time.Time `json:"next"`
}

func readSalvageState(dir string) salvageState {
	var state salvageState
	if data, err := os.ReadFile(filepath.Join(dir, salvageStateFile)); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}

// next is the state after one more failed attempt. Storage that is down comes back, and a disk that was full has room again,
// so each attempt waits longer, for as long as the capture is kept.
func (s salvageState) next(now time.Time) salvageState {
	wait := salvageRetryFirst
	for i := 0; i < s.Attempts && wait < salvageRetryMost; i++ {
		wait *= 2
	}
	return salvageState{Attempts: s.Attempts + 1, Next: now.Add(min(wait, salvageRetryMost))}
}

func (w *Worker) salvageDir(ctx context.Context, id string) {
	w.mu.Lock()
	_, ours := w.running[id]
	w.mu.Unlock()
	if ours {
		return
	}
	dir := filepath.Join(w.workDir, id)
	// Another worker on the same volume may be running this job, or recovering it.
	release, ok := lockDir(dir)
	if !ok {
		return
	}
	defer release()
	e, err := w.opts.Store.EgressForWorker(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		_ = os.RemoveAll(dir)
		return
	}
	if err != nil || !store.EgressIsTerminal(e.Status) {
		return // Still running somewhere, or the lease has not run out yet.
	}
	if e.Status != store.EgressFailed || e.OutputPath != "" || e.Format == "" {
		// Kept a day for a person to look at, in case the failure was in finishing the file.
		if info, err := os.Stat(dir); err == nil && time.Since(info.ModTime()) > finalizeWindow {
			_ = os.RemoveAll(dir)
		}
		return
	}
	capture, mime, audio, video := findCapture(dir)
	if capture == "" {
		_ = os.RemoveAll(dir)
		return
	}
	if info, err := os.Stat(capture); err == nil && time.Since(info.ModTime()) > finalizeWindow {
		w.log.Error("giving up on recovering a recording, and removing its capture", "egress", id, "attempts", readSalvageState(dir).Attempts)
		_ = os.RemoveAll(dir)
		return
	}
	state := readSalvageState(dir)
	if time.Now().Before(state.Next) {
		return
	}
	w.log.Info("recovering a recording from a failed job", "egress", id, "attempt", state.Attempts+1)
	result, err := finalize(ctx, w, e, dir, capture, mime, audio, video, nil)
	if err != nil {
		if ctx.Err() != nil {
			return // This worker is stopping, which says nothing about the capture. The next one can try.
		}
		state = state.next(time.Now())
		w.log.Warn("could not recover a recording, and will try again", "egress", id, "error", err, "next", state.Next)
		if data, err := json.Marshal(state); err == nil {
			_ = os.WriteFile(filepath.Join(dir, salvageStateFile), data, 0o600)
		}
		return
	}
	result.Error = fmt.Sprintf("%s Recovered %.1f seconds of the recording.", e.Error, float64(result.DurationMs)/1000)
	updated, err := w.opts.Store.AttachSalvagedFile(ctx, id, result)
	if errors.Is(err, store.ErrNotFound) {
		// Deleted while it was being recovered, and a file that belongs to nothing must not stay behind.
		w.log.Info("a recording was deleted while it was being recovered", "egress", id)
		if err := w.opts.Storage.Delete(ctx, result.OutputPath); err != nil {
			w.log.Warn("could not remove a recovered file", "egress", id, "error", err)
		}
		_ = os.RemoveAll(dir)
		return
	}
	if err != nil {
		w.log.Warn("could not save a recovered recording", "egress", id, "error", err)
		return
	}
	_ = w.opts.Store.RecordEgressEvent(ctx, updated, "", "warn", "egress_recovered", result.Error, map[string]any{"file_size": result.FileSize})
	_ = os.RemoveAll(dir)
}
