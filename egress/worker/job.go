package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/confine"
	"elnova-streamkit.local/egress/storage"
	"elnova-streamkit.local/ingress/source"
	"elnova-streamkit.local/server/auth"
)

const (
	pageLoadTimeout    = 30 * time.Second
	roomConnectTimeout = 45 * time.Second
	chunkStallTimeout  = 20 * time.Second
	frozenAfter        = 3 * time.Second
	finishTimeout      = 15 * time.Second
	streamFinishWait   = 10 * time.Second
	finalizeTimeout    = 2 * time.Hour
	renewInterval      = time.Second
	cpuSampleInterval  = 2 * time.Second
	// Long enough to join. After that the server keeps the page supplied with new tokens.
	tokenTTL = 5 * time.Minute
	// The page reports every two seconds whatever else it is doing, so this long of nothing means it has stopped running.
	pageStallTimeout   = 30 * time.Second
	diskCheckEvery     = 10 * time.Second
	defaultWaitMedia   = 15 * time.Minute
	defaultMaxDuration = 24 * time.Hour
	finalStatusTimeout = time.Minute
	finalStatusRetry   = 2 * time.Second
	videoGrace         = 5 * time.Second
	markerFile         = "capture.json"
)

// captureMarker lets a later worker make sense of a capture this one left behind.
type captureMarker struct {
	Mime  string `json:"mime"`
	Audio bool   `json:"audio"`
	Video bool   `json:"video"`
}

// job is one recording. Only the goroutine in run touches e after it starts: others use id.
type job struct {
	w   *Worker
	id  string
	e   *store.Egress
	log *slog.Logger
	dir string

	audio, video bool
	meter        *cpuMeter
	ctx          context.Context
	cancel       context.CancelFunc

	stopOnce sync.Once
	stopCh   chan struct{}
	lost     chan struct{}
	lostOnce sync.Once

	capture     *os.File
	capturePath string
	mime        string
	streams     []*streamOutput
	chunks      int64
	captured    int64
	status      string
	// capturing says the page has started recording, whatever the job's row says.
	capturing bool
	// silence watches for audio that arrives and does not get recorded.
	silence audioWatch
}

func newJob(w *Worker, e *store.Egress) *job {
	ctx, cancel := context.WithCancel(context.Background())
	return &job{
		w: w, id: e.ID, e: e, log: w.log.With("egress", e.ID, "room", e.RoomName), dir: filepath.Join(w.workDir, e.ID),
		audio: e.Type != "video_only", video: e.Type != "audio_only", meter: newCPUMeter(),
		ctx: ctx, cancel: cancel, stopCh: make(chan struct{}), lost: make(chan struct{}), status: store.EgressStarting,
	}
}

func (j *job) requestStop() { j.stopOnce.Do(func() { close(j.stopCh) }) }

// jobError is a failure with what to tell the customer. The worker's own log gets more.
type jobError struct{ msg string }

func (e *jobError) Error() string { return e.msg }

func failf(format string, args ...any) error { return &jobError{fmt.Sprintf(format, args...)} }

// run takes the job from claimed to a final status. Nothing returns without setting one, unless the job has been
// taken away from this worker, in which case the status is someone else's to set.
func (j *job) run() {
	defer j.cancel()
	j.log.Info("job started", "type", j.e.Type, "format", j.e.Format)
	if err := os.MkdirAll(j.dir, 0o700); err != nil {
		j.end(store.EgressFailed, "Could not create a working directory: "+err.Error(), store.EgressResult{})
		return
	}
	// Held until this job has finished with its files, so that another worker sharing the directory leaves them alone.
	if release, ok := lockDir(j.dir); ok {
		defer release()
	}
	go j.keepLease()
	go j.sampleCPU()

	reason, err := j.record()
	j.closeStreams()
	if j.capture != nil {
		_ = j.capture.Sync()
		_ = j.capture.Close()
	}

	select {
	case <-j.lost:
		j.log.Warn("job taken away from this worker, leaving its files for recovery")
		return
	default:
	}

	var saved bool
	switch {
	case err != nil && j.chunks == 0:
		// Nothing was recorded, so there is nothing to save.
		saved = j.end(store.EgressFailed, describe(err), store.EgressResult{})
	case err != nil:
		// It broke part way through. Whatever was captured is still worth keeping.
		saved = j.finishBroken(err)
	case j.chunks == 0:
		saved = j.end(store.EgressAborted, reason, store.EgressResult{})
	default:
		saved = j.finishClean(reason)
	}
	// Otherwise the files stay: for a person if finishing the file failed, or for the next pass to recover from if this
	// worker lost the job along the way.
	if saved {
		_ = os.RemoveAll(j.dir)
	}
}

func describe(err error) string {
	var je *jobError
	if errors.As(err, &je) {
		return je.msg
	}
	return "Recording failed: " + err.Error()
}

// finishClean reports whether the job has been fully dealt with.
func (j *job) finishClean(reason string) bool {
	j.setEnding("Recording is finishing: " + reason)
	select {
	case <-j.lost:
		return false
	default:
	}
	if j.e.Format == "" {
		return j.end(store.EgressComplete, "", store.EgressResult{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	result, err := finalize(ctx, j.w, j.e, j.dir, j.capturePath, j.mime, j.audio, j.video, j.meter)
	if err != nil {
		// The capture stays for a day, so a person can get at it.
		j.end(store.EgressFailed, "Could not finish the recording file: "+err.Error(), store.EgressResult{})
		return false
	}
	return j.end(store.EgressComplete, "", result)
}

func (j *job) finishBroken(cause error) bool {
	msg := describe(cause)
	if j.e.Format == "" {
		return j.end(store.EgressFailed, msg, store.EgressResult{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	result, err := finalize(ctx, j.w, j.e, j.dir, j.capturePath, j.mime, j.audio, j.video, j.meter)
	if err != nil {
		j.log.Warn("could not save a partial recording", "error", err)
		j.end(store.EgressFailed, msg, store.EgressResult{})
		return false
	}
	return j.end(store.EgressFailed, fmt.Sprintf("%s Saved %.1f seconds of it.", msg, float64(result.DurationMs)/1000), result)
}

// setEnding keeps trying, since from here on a final status depends on it, and a database that is away for a moment must not
// turn a good recording into a failed one.
func (j *job) setEnding(message string) {
	deadline := time.Now().Add(finalStatusTimeout)
	for j.status != store.EgressEnding && time.Now().Before(deadline) {
		j.setStatus(store.EgressEnding, store.EgressResult{}, "egress_ending", message)
		select {
		case <-j.lost:
			return
		default:
		}
		if j.status != store.EgressEnding {
			time.Sleep(finalStatusRetry)
		}
	}
}

// setStatus moves a running job on, and always sends egress_updated.
func (j *job) setStatus(status string, r store.EgressResult, eventType, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := j.w.opts.Store.TransitionEgress(ctx, j.id, j.w.id, status, r)
	if err != nil {
		if errors.Is(err, store.ErrJobLost) {
			j.markLost()
		} else {
			j.log.Warn("could not update job status", "status", status, "error", err)
		}
		return
	}
	j.e = e
	j.status = status
	if err := j.w.opts.Store.RecordEgressEvent(ctx, e, "egress_updated", "info", eventType, message, nil); err != nil {
		j.log.Warn("could not record event", "error", err)
	}
}

// end sets the final status and reports it, with what the job cost, and says whether that was saved. The lease keeps being
// renewed until it has been. A database that is briefly away must not turn a finished recording into a failed one.
func (j *job) end(status, message string, r store.EgressResult) bool {
	defer j.cancel()
	j.meter.sample()
	cpu, wall := j.meter.seconds()
	r.CPUSeconds = cpu
	r.Error = message
	ctx, cancel := context.WithTimeout(context.Background(), finalStatusTimeout)
	defer cancel()
	var e *store.Egress
	for {
		var err error
		e, err = j.w.opts.Store.TransitionEgress(ctx, j.id, j.w.id, status, r)
		if err == nil {
			break
		}
		if errors.Is(err, store.ErrJobLost) || ctx.Err() != nil {
			j.log.Error("could not save final job status", "status", status, "error", err)
			return false
		}
		j.log.Warn("could not save final job status, trying again", "status", status, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(finalStatusRetry):
		}
	}
	j.e = e
	level, text := "info", "Recording finished."
	switch status {
	case store.EgressFailed:
		level, text = "error", message
	case store.EgressAborted:
		text = message
	}
	details := map[string]any{"cpu_seconds": cpu, "wall_seconds": wall, "file_size": r.FileSize, "duration_ms": r.DurationMs}
	if wall > 0 {
		details["avg_cores"] = cpu / wall
	}
	if err := j.w.opts.Store.RecordEgressEvent(ctx, e, "egress_ended", level, "egress_"+eventName(status), text, details); err != nil {
		j.log.Warn("could not record event", "error", err)
	}
	j.log.Info("job ended", "status", status, "cpuSeconds", cpu, "wallSeconds", wall, "error", message)
	return true
}

func eventName(status string) string {
	switch status {
	case store.EgressComplete:
		return "completed"
	case store.EgressAborted:
		return "aborted"
	}
	return "failed"
}

func (j *job) markLost() {
	j.lostOnce.Do(func() {
		close(j.lost)
		j.cancel()
	})
}

// keepLease renews the lease about once a second, and that same call says whether anyone has asked this job to stop.
func (j *job) keepLease() {
	t := time.NewTicker(renewInterval)
	defer t.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(j.ctx, 3*time.Second)
		stop, err := j.w.opts.Store.RenewEgress(ctx, j.id, j.w.id, j.w.lease)
		cancel()
		switch {
		case errors.Is(err, store.ErrJobLost):
			j.markLost()
			return
		case err != nil:
			// The lease has room for a database hiccup. If this lasts, the job is reaped and renewing reports it.
			j.log.Warn("could not renew lease", "error", err)
		case stop:
			j.requestStop()
		}
	}
}

func (j *job) sampleCPU() {
	t := time.NewTicker(cpuSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case <-t.C:
			j.meter.sample()
		}
	}
}

func (j *job) accessToken(ctx context.Context) (string, error) {
	key, err := j.w.signingKey(ctx, j.e.ProjectID)
	if err != nil {
		return "", err
	}
	publish, subscribe, data := false, true, false
	return auth.Mint(key, &auth.Grants{
		Identity: j.e.ID, Name: "Recorder", Kind: "EGRESS",
		Video: &auth.VideoGrant{
			RoomJoin: true, Room: j.e.RoomName, Hidden: true, Recorder: true,
			CanPublish: &publish, CanSubscribe: &subscribe, CanPublishData: &data,
		},
	}, tokenTTL)
}

// pageEvent is what the reader goroutine passes along.
type pageEvent struct {
	message *pageMessage
	chunk   []byte
	err     error
}

// record launches the browser and runs until the recording ends. It returns why it ended, or what broke.
func (j *job) record() (string, error) {
	token, err := j.accessToken(j.ctx)
	if err != nil {
		return "", failf("Could not get an access token for the room: %v", err)
	}
	oneTime := store.RandomToken(32)
	connCh := j.w.bridge.expect(oneTime)
	defer j.w.bridge.forget(oneTime)

	chrome, chromeExit, stderr, err := j.startChrome(oneTime)
	if err != nil {
		return "", failf("Could not start Chrome: %v", err)
	}
	defer func() {
		killTree(chrome.Process.Pid)
		_ = os.RemoveAll(filepath.Join(j.dir, "chrome"))
	}()

	var conn *websocket.Conn
	select {
	case conn = <-connCh:
	case <-chromeExit:
		return "", failf("Chrome exited before the recorder page loaded: %s", chromeReason(stderr))
	case <-time.After(pageLoadTimeout):
		return "", failf("Chrome did not load the recorder page within %s.", pageLoadTimeout)
	case <-j.stopCh:
		return "Stopped before it started.", nil
	case <-j.ctx.Done():
		return "", failf("The job was cancelled.")
	}
	defer conn.Close()
	// It has served its purpose, and the token in it has been used.
	_ = os.Remove(filepath.Join(j.dir, "chrome", launcherFile))

	if err := writeJSON(conn, map[string]any{"type": "config", "config": pageConfig{
		URL: j.w.wsURL, Token: token, Type: j.e.Type, Participant: j.e.Request.Participant, Layout: j.e.Request.Layout,
		Width: frameWidth, Height: frameHeight, FPS: frameRate, VideoBitrate: videoBitrate, AudioBitrate: audioBitrate,
		KeyframeMs: keyframeMs, TimesliceMs: timesliceMs, MimeTypes: mimeTypesFor(j.e.Type, j.e.Format),
		VideoGraceMs: int(videoGrace / time.Millisecond),
	}}); err != nil {
		return "", failf("Could not reach the recorder page: %v", err)
	}

	events := make(chan pageEvent, 64)
	go readPage(j.ctx, conn, events)

	var stopping, connected bool
	stopReason := ""
	stop := j.stopCh
	timers := pageTimers{connect: time.NewTimer(roomConnectTimeout), waitMedia: stoppedTimer(), maxDuration: stoppedTimer()}
	defer timers.stop()
	finishDeadline := stoppedTimer()
	defer finishDeadline.Stop()
	lastChunk, lastPage, lastTick, lastDiskCheck := time.Now(), time.Now(), time.Now(), time.Now()
	stall := time.NewTicker(time.Second)
	defer stall.Stop()

	// beginStop asks the page to finish what it has, and reports false if it can no longer be reached.
	beginStop := func() bool {
		stop = nil
		stopping = true
		if err := writeJSON(conn, map[string]string{"type": "stop"}); err != nil {
			return false
		}
		finishDeadline.Reset(finishTimeout)
		return true
	}

	for {
		select {
		case ev := <-events:
			switch {
			case ev.err != nil:
				if stopping {
					return "Stopped by request.", nil
				}
				j.log.Warn("lost the recorder page", "error", ev.err, "chrome", chromeReason(stderr))
				return "", failf("Lost the recorder page: Chrome or its tab may have crashed.")
			case ev.chunk != nil:
				lastChunk, lastPage = time.Now(), time.Now()
				if err := j.onChunk(ev.chunk); err != nil {
					return "", err
				}
				j.noteMediaStarted()
			default:
				lastPage = time.Now()
				done, reason, err := j.onMessage(ev.message, &timers)
				if err != nil {
					return "", err
				}
				if done {
					if stopReason != "" {
						reason = stopReason
					}
					return reason, nil
				}
				switch ev.message.Type {
				case "connected":
					connected = true
				case "recording":
					lastChunk = time.Now()
				}
			}

		case <-stop:
			if !beginStop() {
				return "Stopped by request.", nil
			}

		case <-timers.maxDuration.C:
			stopReason = fmt.Sprintf("It reached the longest a recording may be, which is %s.", j.w.maxDuration)
			if !beginStop() {
				return stopReason, nil
			}

		case <-timers.waitMedia.C:
			return fmt.Sprintf("Nothing to record arrived within %s.", j.w.waitForMedia), nil

		case <-finishDeadline.C:
			return "", failf("The recorder page did not finish within %s of being stopped.", finishTimeout)

		case <-timers.connect.C:
			return "", failf("Could not join room %q within %s.", j.e.RoomName, roomConnectTimeout)

		case <-chromeExit:
			if stopping {
				return "Stopped by request.", nil
			}
			j.log.Warn("Chrome exited", "chrome", chromeReason(stderr))
			return "", failf("Chrome exited unexpectedly.")

		case <-stall.C:
			// A tick this late means this process did not run for a while, as happens to a whole machine under memory
			// pressure. Chunks that could not be read meanwhile say nothing about the recorder.
			if late := time.Since(lastTick); late > frozenAfter {
				j.log.Warn("this worker did not run for a while, so the stall timers start again", "seconds", late.Seconds())
				lastChunk = lastChunk.Add(late)
				lastPage = lastPage.Add(late)
			}
			lastTick = time.Now()
			if j.capturing && time.Since(lastChunk) > chunkStallTimeout {
				return "", failf("The recorder stopped producing media: nothing arrived for %s.", chunkStallTimeout)
			}
			if connected && !j.capturing && time.Since(lastPage) > pageStallTimeout {
				return "", failf("The recorder page stopped responding while it waited for something to record.")
			}
			j.noteMediaStarted()
			if time.Since(lastDiskCheck) > diskCheckEvery {
				lastDiskCheck = time.Now()
				if free, err := freeDisk(j.w.workDir); err == nil && free < minFreeDisk {
					return "", failf("The worker is running out of disk space, so it stopped this recording while it could still finish the file.")
				}
			}
			if err := j.checkStreams(); err != nil {
				return "", err
			}

		case <-j.ctx.Done():
			return "", failf("The job was cancelled.")
		}
	}
}

// pageTimers are the limits on a page that depend on what it has said so far.
type pageTimers struct {
	connect, waitMedia, maxDuration *time.Timer
}

func (t *pageTimers) stop() {
	t.connect.Stop()
	t.waitMedia.Stop()
	t.maxDuration.Stop()
}

func stoppedTimer() *time.Timer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

// noteMediaStarted says the job is active, the first time there is media to show for it. Having asked the browser to record
// does not mean anything is being recorded: its encoders can take a while to start. It gets called again until it has
// worked, because a database that is away for a moment must not leave a recording that looks as if it never started.
func (j *job) noteMediaStarted() {
	if j.chunks > 0 && j.status == store.EgressStarting {
		j.setStatus(store.EgressActive, store.EgressResult{}, "egress_active", "Recording started.")
	}
}

// readPage ends when the connection does. ctx is for the case where nothing is reading any more.
func readPage(ctx context.Context, conn *websocket.Conn, events chan<- pageEvent) {
	deliver := func(ev pageEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			deliver(pageEvent{err: err})
			return
		}
		if kind == websocket.BinaryMessage {
			if !deliver(pageEvent{chunk: data}) {
				return
			}
			continue
		}
		var m pageMessage
		if json.Unmarshal(data, &m) == nil && !deliver(pageEvent{message: &m}) {
			return
		}
	}
}

func (j *job) onMessage(m *pageMessage, timers *pageTimers) (done bool, reason string, err error) {
	switch m.Type {
	case "connected":
		timers.connect.Stop()
		timers.waitMedia.Reset(j.w.waitForMedia)
		j.log.Info("recorder joined the room")
	case "recording":
		timers.waitMedia.Stop()
		timers.maxDuration.Reset(j.w.maxDuration)
		if err := j.beginCapture(m.Mime); err != nil {
			return false, "", err
		}
		j.capturing = true
	case "finished":
		switch m.Reason {
		case "room_closed":
			if m.Recorded {
				return true, "The room closed.", nil
			}
			return true, "The room closed before anything was recorded.", nil
		case "disconnected":
			// The room may well be going on without us, so whatever was recorded is not all of it.
			return false, "", failf("The recorder lost its connection to the room (%s), so this recording may have stopped before the room ended.", m.Message)
		}
		if m.Recorded {
			return true, "Stopped by request.", nil
		}
		return true, "Stopped before anything was recorded.", nil
	case "error":
		return false, "", failf("%s", m.Message)
	case "log":
		switch m.Level {
		case "error", "warn":
			j.log.Warn("recorder page", "message", m.Message)
		default:
			j.log.Debug("recorder page", "message", m.Message)
		}
	case "stats":
		j.log.Debug("recorder stats", "frames", m.FramesDrawn, "bytes", m.Bytes, "video", m.VideoTracks, "audio", m.AudioTracks, "audioState", m.AudioState, "audioTime", m.AudioTime, "audioPeak", m.AudioPeak, "audioPlaying", m.AudioPlaying, "audioMuted", m.AudioMuted, "audioIn", m.AudioIn)
		if j.capturing && j.silence.note(m.AudioIn) {
			j.reportAudioStuck()
		}
	}
	return false, "", nil
}

// beginCapture opens the outputs, now that the page has said what format it is producing.
func (j *job) beginCapture(mime string) error {
	j.mime = mime
	if j.e.Format != "" {
		j.capturePath = filepath.Join(j.dir, "capture"+captureExtension(mime))
		f, err := os.OpenFile(j.capturePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return failf("Could not write the recording: %v", err)
		}
		j.capture = f
		marker, _ := json.Marshal(captureMarker{Mime: mime, Audio: j.audio, Video: j.video})
		if err := os.WriteFile(filepath.Join(j.dir, markerFile), marker, 0o600); err != nil {
			return failf("Could not write the recording: %v", err)
		}
	}
	urls, err := j.w.opts.Store.EgressStreamURLs(j.e)
	if err != nil {
		return failf("Could not read the stream destinations: %v", err)
	}
	var last error
	for _, raw := range urls {
		s, err := j.openStream(raw, mime)
		if err == nil {
			j.streams = append(j.streams, s)
			continue
		}
		// As for a destination that fails later: a file keeps recording without it.
		redacted := store.RedactStreamURL(raw)
		last = fmt.Errorf("%s: %s", redacted, redactError(err))
		j.reportStreamFailure(redacted, err)
	}
	if last != nil && len(j.streams) == 0 && j.e.Format == "" {
		return failf("Every stream destination has failed. Last: %s", last)
	}
	return nil
}

// openStream starts sending to a destination. The address is checked again now that it is about to be used, and once more
// when the relay connects, since a name can point somewhere new.
func (j *job) openStream(raw, mime string) (*streamOutput, error) {
	allow := j.w.opts.Config.Media.AllowPrivateAddresses
	u, err := source.CheckOutput(j.ctx, raw, allow)
	if err != nil {
		return nil, err
	}
	relay, err := source.NewRelay(u, allow)
	if err != nil {
		return nil, err
	}
	s, err := startStream(j.w.ffmpeg, mime, relay, j.audio, j.video, store.RedactStreamURL(raw), j.meter)
	if err != nil {
		relay.Close()
		return nil, err
	}
	return s, nil
}

// audioStuckAfter is how many of the page's reports in a row have to show the problem. They come every two seconds.
const audioStuckAfter = 5

// audioWatch notices audio packets arriving with no samples coming out of them, which means the browser is not decoding that
// track, and whatever it is recording of it is silence. A room that is merely quiet still produces samples.
type audioWatch struct {
	stuck    int
	reported bool
}

// note takes one report from the page, and returns true the first time it is clear that audio is not being decoded.
func (w *audioWatch) note(raw json.RawMessage) bool {
	var tracks []struct {
		Packets int64 `json:"packets"`
		Samples int64 `json:"samples"`
	}
	if json.Unmarshal(raw, &tracks) != nil {
		return false
	}
	stuck := false
	for _, t := range tracks {
		if t.Packets > 0 && t.Samples == 0 {
			stuck = true
		}
	}
	if !stuck {
		w.stuck = 0
		return false
	}
	w.stuck++
	if w.stuck < audioStuckAfter || w.reported {
		return false
	}
	w.reported = true
	return true
}

// reportAudioStuck says a recording may come out silent, while there may still be time to do something about it.
func (j *job) reportAudioStuck() {
	j.log.Error("audio is arriving, but the recorder is not decoding it, so this recording may be silent")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = j.w.opts.Store.RecordEgressEvent(ctx, j.e, "", "error", "egress_audio_stuck", "Audio is arriving, but the recorder is not decoding it, so this recording may be silent.", nil)
}

// reportStreamFailure logs a destination that has failed, for the operator and the customer.
func (j *job) reportStreamFailure(redacted string, err error) {
	j.log.Warn("stream destination failed", "url", redacted, "error", redactError(err))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = j.w.opts.Store.RecordEgressEvent(ctx, j.e, "", "warn", "egress_stream_failed", "Stream destination failed: "+redacted, map[string]any{"error": redactError(err)})
}

func (j *job) onChunk(chunk []byte) error {
	j.chunks++
	j.captured += int64(len(chunk))
	if j.capture != nil {
		if _, err := j.capture.Write(chunk); err != nil {
			return failf("Could not write the recording: %v", err)
		}
	}
	for _, s := range j.streams {
		if !s.send(chunk) {
			s.kill()
			j.log.Warn("stream destination fell too far behind", "url", s.redacted)
		}
	}
	return nil
}

// checkStreams reacts to destinations that have gone away. A file keeps recording without them. A job that only
// streams has nothing left to do.
func (j *job) checkStreams() error {
	live := j.streams[:0]
	var last error
	for _, s := range j.streams {
		err, failed := s.failure()
		if !failed {
			live = append(live, s)
			continue
		}
		s.finish(0)
		last = fmt.Errorf("%s: %s", s.redacted, err)
		j.reportStreamFailure(s.redacted, err)
	}
	j.streams = live
	if last != nil && len(j.streams) == 0 && j.e.Format == "" {
		return failf("Every stream destination has failed. Last: %s", redactError(last))
	}
	return nil
}

// redactError keeps ffmpeg's output from carrying a stream key into a message a person will read.
func redactError(err error) string {
	return redactKeys(err.Error())
}

func redactKeys(text string) string { return store.RedactURLsIn(text) }

func (j *job) closeStreams() {
	var wg sync.WaitGroup
	for _, s := range j.streams {
		wg.Add(1)
		go func(s *streamOutput) {
			defer wg.Done()
			s.finish(streamFinishWait)
		}(s)
	}
	wg.Wait()
}

// startChrome launches headless Chrome on the recorder page. There is no display and no sound server: the page renders
// to its own canvas, and WebAudio mixes into a stream, so neither needs one.
func (j *job) startChrome(oneTime string) (*exec.Cmd, <-chan struct{}, *tailBuffer, error) {
	profile := filepath.Join(j.dir, "chrome")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, nil, nil, err
	}
	launcher, err := writeLauncher(profile, j.w.bridge.pageURL(oneTime))
	if err != nil {
		return nil, nil, nil, err
	}
	binary, args := chromeCommand(j.w.chrome, append(chromeArgs(profile, j.w.cfg.ChromeNoSandbox), launcher), profile)
	cmd := exec.Command(binary, args...)
	cmd.Env = chromeEnv(profile)
	cmd.SysProcAttr = childAttrs()
	stderr := &tailBuffer{}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	// A helper left behind by a browser process that has died can hold its output open.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	j.meter.add(cmd.Process.Pid)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return cmd, exited, stderr, nil
}

// launcherFile is a page that sends Chrome on to the recorder page. A command line can be read by any process on the machine,
// but this file only by this user, so the one-time token goes here.
const launcherFile = "launch.html"

func writeLauncher(profile, pageURL string) (string, error) {
	path := filepath.Join(profile, launcherFile)
	page := "<!doctype html><script>location.replace(" + strconv.Quote(pageURL) + ")</script>"
	if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
		return "", err
	}
	return "file://" + path, nil
}

// chromeRead is what Chrome needs to read, on top of what any program does, when the worker limits it. It does not include any
// other job's files, finished recordings, or a configuration file.
var chromeRead = []string{"/opt", "/sys", "/etc/opt", "/etc/chromium", "/etc/passwd", "/etc/group", "/etc/machine-id", "/dev/zero", "/dev/fd"}

// chromeCommand limits Chrome to its own profile, as ffmpeg is limited, if the worker already runs with no_new_privs, as it does
// in the container. Otherwise that would be a new restriction on Chrome, and it would disable the setuid sandbox that some
// machines use, which is worth more than this.
func chromeCommand(chrome string, args []string, profile string) (string, []string) {
	if !confine.NoNewPrivileges() {
		return chrome, args
	}
	// Chrome's sandbox writes to /proc/self/uid_map to set up its user namespace, and without that it cannot start at all. The
	// kernel's own permissions still decide what can be changed in /proc, as they would with no limit on it here.
	return confine.Command(chrome, args, confine.Spec{Read: chromeRead, Write: []string{profile, "/proc", "/dev/shm", "/tmp"}, AnyNetwork: true})
}

// chromeEnv gives Chrome nothing of this worker's, and no display or sound server to find.
func chromeEnv(profile string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + profile, "LANG=C.UTF-8", "XDG_CONFIG_HOME=" + profile, "XDG_CACHE_HOME=" + profile}
}

// chromeReason picks out what might explain a failure. Chrome complains about things it can do without, such as the
// session bus this worker deliberately does not give it.
func chromeReason(stderr *tailBuffer) string {
	var keep []string
	for _, line := range strings.Split(stderr.tail(), " | ") {
		if line == "" || strings.Contains(line, "dbus") || strings.Contains(line, "UPower") || strings.Contains(line, "gcm/") {
			continue
		}
		keep = append(keep, line)
	}
	text := strings.Join(keep, " | ")
	if len(text) > 400 {
		text = text[len(text)-400:]
	}
	return text
}

func chromeArgs(profile string, noSandbox bool) []string {
	args := []string{
		"--headless=new",
		"--user-data-dir=" + profile,
		fmt.Sprintf("--window-size=%d,%d", frameWidth, frameHeight),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-sync",
		"--disable-default-apps",
		"--disable-component-update",
		"--disable-background-networking",
		"--disable-gpu",
		"--password-store=basic",
		"--use-mock-keychain",
		"--mute-audio",
		// Decoding a room's audio takes something pulling on it, and a sound card is no use for that here. Waiting to be
		// told about one can stop the whole page for a long time, and the audio with it.
		"--disable-audio-output",
		"--autoplay-policy=no-user-gesture-required",
		// Nothing on this page is ever in the foreground, and all of it has to keep running as if it were.
		"--disable-background-timer-throttling",
		"--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows",
		"--disable-features=Translate,OptimizationHints,MediaRouter,DialMediaRouteProvider",
	}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	return args
}

// findCapture finds a capture a job left behind, and what it needs to be finished.
func findCapture(dir string) (path, mime string, audio, video bool) {
	data, err := os.ReadFile(filepath.Join(dir, markerFile))
	if err != nil {
		return "", "", false, false
	}
	var marker captureMarker
	if json.Unmarshal(data, &marker) != nil {
		return "", "", false, false
	}
	p := filepath.Join(dir, "capture"+captureExtension(marker.Mime))
	if info, err := os.Stat(p); err != nil || info.Size() == 0 {
		return "", "", false, false
	}
	return p, marker.Mime, marker.Audio, marker.Video
}

// finalize turns a capture into the delivered file and stores it. It only returns success once the file is durable.
func finalize(ctx context.Context, w *Worker, e *store.Egress, dir, capture, mime string, audio, video bool, meter *cpuMeter) (store.EgressResult, error) {
	if capture == "" {
		return store.EgressResult{}, errors.New("nothing was captured")
	}
	output := filepath.Join(dir, "output."+e.Format)
	if err := runFFmpeg(ctx, w.ffmpeg, finalizeArgs(capture, output, e.Format, mime, audio, video), dir, meter); err != nil {
		return store.EgressResult{}, fmt.Errorf("ffmpeg failed: %w", err)
	}
	duration, err := probeDuration(ctx, w.ffprobe, output)
	if err != nil {
		return store.EgressResult{}, fmt.Errorf("the finished file could not be read: %w", err)
	}
	key, err := storage.RecordingKey(e.ProjectID, e.ID, e.Format)
	if err != nil {
		return store.EgressResult{}, err
	}
	kind := "video/"
	if !video {
		kind = "audio/"
	}
	size, err := w.opts.Storage.Put(ctx, key, output, kind+e.Format)
	if err != nil {
		return store.EgressResult{}, fmt.Errorf("could not store the file: %w", err)
	}
	return store.EgressResult{OutputPath: key, Storage: w.opts.Storage.Kind(), FileSize: size, DurationMs: duration.Milliseconds()}, nil
}
