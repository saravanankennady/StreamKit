package ingress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/confine"
	"elnova-streamkit.local/ingress/source"
	"elnova-streamkit.local/server/auth"
)

const (
	renewInterval = time.Second
	statsInterval = 2 * time.Second
	watchInterval = time.Second
	// A tick this late means this process did not run for a while. Under memory pressure, a whole machine can stop for tens
	// of seconds, and nothing that happened meanwhile says anything about the stream.
	frozenAfter = 3 * time.Second
	// Long enough to join. After that the server keeps the client supplied with new tokens.
	tokenTTL        = 5 * time.Minute
	stderrTailLines = 8
)

// session is one stream, from whichever input, through the transcoder and out as a participant in the room.
type session struct {
	w   *Worker
	in  *store.Ingress
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	// mu guards what setup creates, and tornDown, because a stream can be stopped while it is still being set up. Whatever
	// setup makes after that, it has to clean up itself.
	mu                   sync.Mutex
	tornDown             bool
	videoConn, audioConn *net.UDPConn
	pub                  *publisher
	cmd                  *exec.Cmd
	stdin                io.WriteCloser
	proxy                *source.Proxy
	relay                *source.Relay
	exited               chan error

	// secrets are the exact strings to take out of anything ffmpeg says, beyond what looks like an address.
	secrets []string

	info     inputInfo
	progress progress

	// bytesIn counts what the source sent, where this worker can see it. RTMP counts here. HTTP counts in the proxy.
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	lastPkt   atomic.Int64
	started   time.Time
	published atomic.Bool

	stderrMu   sync.Mutex
	stderrTail []string
	endOnce    sync.Once
	// onEnd lets the input react, so an RTMP connection is closed when the stream ends for another reason.
	onEnd func()
}

// newSession takes over an ingress this worker has just claimed. The lease is held from here on, not from when media
// starts: joining a room can take a while on a busy server, and meanwhile the claim has to stay this worker's.
func newSession(w *Worker, in *store.Ingress) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		w: w, in: in, log: w.log.With("ingress", in.ID, "room", in.RoomName, "input", in.InputType),
		ctx: ctx, cancel: cancel, started: time.Now(),
	}
	go s.holdLease()
	return s
}

// holdLease renews the lease until the session ends, and that same call says if anyone has asked it to stop.
func (s *session) holdLease() {
	t := time.NewTicker(renewInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		stop, err := s.w.opts.Store.RenewIngress(ctx, s.in.ID, s.w.id, s.w.lease)
		cancel()
		switch {
		case errors.Is(err, store.ErrJobLost):
			s.log.Warn("stream taken away from this worker, or the ingress was deleted")
			s.endOnce.Do(s.teardown)
			return
		case err == nil && stop:
			s.finish(store.IngressInactive, "", "Stopped by request.")
			return
		}
		// Any other error is the database, which the lease has room for.
	}
}

func (s *session) openSockets() error {
	video, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	audio, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = video.Close()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tornDown {
		_ = video.Close()
		_ = audio.Close()
		return errStopped
	}
	s.videoConn, s.audioConn = video, audio
	return nil
}

// errStopped means the session ended while it was still starting.
var errStopped = errors.New("stopped while starting")

func (s *session) ports() (video, audio int) {
	return s.videoConn.LocalAddr().(*net.UDPAddr).Port, s.audioConn.LocalAddr().(*net.UDPAddr).Port
}

// startTranscoder launches ffmpeg, with its output going to the RTP sockets, and joins the room as those start flowing.
// connect is the TCP ports it may reach: the proxy or relay it goes through, and nothing else.
func (s *session) startTranscoder(args []string, withStdin bool, connect []int) error {
	binary, confined := confine.Command(s.w.ffmpeg, args, confine.Spec{Connect: connect})
	cmd := exec.Command(binary, confined...)
	// Nothing from this worker's environment, such as its database address or master key, belongs in a process parsing
	// media from strangers. Without a proxy variable, ffmpeg's -http_proxy is the only way out.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	var stdin io.WriteCloser
	if withStdin {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			return err
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start ffmpeg: %w", err)
	}
	s.lastPkt.Store(time.Now().UnixNano())
	// Nothing returns from here without something waiting for this process, however the session ends. Wait closes the pipes,
	// so it has to come after they have been read to the end, or the last thing ffmpeg said can be lost, which is usually
	// the reason it stopped.
	var output sync.WaitGroup
	output.Add(2)
	go func() {
		defer output.Done()
		s.scan(stdout, s.progress.parseLine)
	}()
	go func() {
		defer output.Done()
		s.scan(stderr, func(line string) {
			s.info.parseLine(line)
			s.stderrMu.Lock()
			s.stderrTail = append(s.stderrTail, line)
			if len(s.stderrTail) > stderrTailLines {
				s.stderrTail = s.stderrTail[1:]
			}
			s.stderrMu.Unlock()
		})
	}()
	exited := make(chan error, 1)
	go func() {
		output.Wait()
		exited <- cmd.Wait()
	}()
	s.mu.Lock()
	if s.tornDown {
		s.mu.Unlock()
		_ = cmd.Process.Kill()
		return errStopped
	}
	s.cmd, s.stdin, s.exited = cmd, stdin, exited
	s.mu.Unlock()
	return nil
}

func (s *session) scan(r io.Reader, fn func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fn(strings.TrimRight(sc.Text(), "\r"))
	}
}

func (s *session) lastError() string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	return s.redact(strings.Join(s.stderrTail, " | "))
}

// redact keeps a source's address, which may have credentials in it, out of a message. ffmpeg may be given the address in
// a different form from the one that was saved, and it names others of its own, such as the segments of a playlist.
func (s *session) redact(text string) string {
	for _, secret := range s.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "the source")
		}
	}
	return store.RedactURLsIn(text)
}

func (s *session) accessToken() (string, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	key, err := s.w.signingKey(ctx, s.in.ProjectID)
	if err != nil {
		return "", err
	}
	publish, deny := true, false
	return auth.Mint(key, &auth.Grants{
		Identity: s.in.ParticipantIdentity, Name: s.in.ParticipantName, Kind: "INGRESS",
		Attributes: map[string]string{"ingress.ingressID": s.in.ID},
		Video:      &auth.VideoGrant{RoomJoin: true, Room: s.in.RoomName, CanPublish: &publish, CanSubscribe: &deny, CanPublishData: &deny},
	}, tokenTTL)
}

// joinRoom connects the publisher.
func (s *session) joinRoom() error {
	token, err := s.accessToken()
	if err != nil {
		return fmt.Errorf("could not get an access token for the room: %w", err)
	}
	pub := newPublisher(s.log, s.w.wsURL, token, s.w.opts.Config.RTC.IncludeLoopback)
	ctx, cancel := context.WithTimeout(s.ctx, publishTimeout)
	defer cancel()
	if err := pub.start(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tornDown {
		pub.close()
		return errStopped
	}
	s.pub = pub
	return nil
}

// isRTP tells a media packet from the rest of what can reach these sockets. ffmpeg sends its RTCP here too, which is dropped
// because the WebRTC stack makes its own reports, and a stray packet to a port someone recently used must not count.
func isRTP(p []byte) bool {
	return len(p) >= 12 && p[0]>>6 == 2 && (p[1] < 200 || p[1] > 207)
}

// forward moves RTP from a socket to a track. The track is published when its first packet arrives. The sockets are bound
// to loopback, so what reaches them comes from this machine.
func (s *session) forward(kind string, conn *net.UDPConn) {
	buf := make([]byte, 1600)
	var track *webrtc.TrackLocalStaticRTP
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !isRTP(buf[:n]) {
			continue
		}
		s.lastPkt.Store(time.Now().UnixNano())
		if track == nil {
			w, h, _, _, _ := s.info.snapshot()
			t, err := s.pub.publish(kind, w, h)
			if err != nil {
				s.fail(fmt.Sprintf("Could not publish the %s track: %v", kind, err))
				return
			}
			track = t
		}
		if _, err := track.Write(buf[:n]); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				return
			}
			continue
		}
		s.bytesOut.Add(int64(n))
		s.notePublishing()
	}
}

// notePublishing moves the status on once media really reaches the room: the connection is up and a packet went out.
func (s *session) notePublishing() {
	select {
	case <-s.pub.connected:
	default:
		return
	}
	if !s.published.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats := s.stats(0, 0, statsInterval)
	in, err := s.w.opts.Store.UpdateIngressRunning(ctx, s.in.ID, s.w.id, store.IngressPublishing, stats)
	if err != nil {
		if errors.Is(err, store.ErrJobLost) {
			s.cancel()
		}
		return
	}
	_ = s.w.opts.Store.RecordIngressEvent(ctx, in, "ingress_started", "info", "ingress_publishing", "Publishing into the room.", statsDetails(stats))
	s.log.Info("publishing")
}

func statsDetails(st store.IngressStats) map[string]any {
	return map[string]any{"width": st.Width, "height": st.Height, "fps": st.FPS, "video_codec": st.VideoCodec, "audio_codec": st.AudioCodec, "bitrate_in": st.BitrateIn}
}

func (s *session) stats(lastBytes, lastFrames int64, elapsed time.Duration) store.IngressStats {
	w, h, fps, video, audio := s.info.snapshot()
	st := store.IngressStats{Width: w, Height: h, FPS: fps, VideoCodec: video, AudioCodec: audio, StartedAt: &s.started}
	if frames := s.progress.frameCount(); frames > lastFrames && elapsed > 0 {
		st.FPS = float64(frames-lastFrames) / elapsed.Seconds()
	}
	if elapsed > 0 {
		st.BitrateIn = int64(float64(s.inputBytes()-lastBytes) * 8 / elapsed.Seconds())
	}
	return st
}

// inputBytes is what came in, as best this worker can tell. A pull over SRT goes directly from ffmpeg to the source, so all
// there is to count is what the transcoder produced.
func (s *session) inputBytes() int64 {
	if s.proxy != nil {
		return s.proxy.BytesIn()
	}
	if s.relay != nil {
		return s.relay.BytesIn()
	}
	if s.in.InputType == store.InputRTMP {
		return s.bytesIn.Load()
	}
	return s.bytesOut.Load()
}

// supervise reports health and watches for a stalled stream, until the session ends. It returns once the transcoder has
// exited.
func (s *session) supervise(exited <-chan error) {
	watch := time.NewTicker(watchInterval)
	defer watch.Stop()
	report := time.NewTicker(statsInterval)
	defer report.Stop()
	var lastBytes, lastFrames int64
	lastAt, lastTick := time.Now(), time.Now()
	seenInput, seenInputAt := s.inputBytes(), time.Now()
	for {
		select {
		case err := <-exited:
			if err != nil && s.ctx.Err() == nil {
				if blocked := s.proxyBlocked(); blocked != nil {
					s.fail("The source is not allowed: " + blocked.Error())
				} else {
					s.fail("The transcoder failed: " + s.lastError() + s.relayReason())
				}
				return
			}
			s.finish(store.IngressInactive, "", "The stream ended.")
			return
		case <-s.pub.done:
			if s.pub.endErr != nil {
				s.fail("Lost the room: " + s.pub.endErr.Error())
			} else {
				s.finish(store.IngressInactive, "", "The stream ended.")
			}
			<-exited
			return
		case <-s.ctx.Done():
			// Ended from somewhere else, which has set the final state.
			<-exited
			return
		case <-watch.C:
			now := time.Now()
			if now.Sub(lastTick) > frozenAfter {
				s.log.Warn("this worker did not run for a while, so the stall timer starts again", "seconds", now.Sub(lastTick).Seconds())
				s.lastPkt.Store(now.UnixNano())
				seenInputAt = now
			}
			lastTick = now
			if b := s.inputBytes(); b != seenInput {
				seenInput, seenInputAt = b, now
			}
			if now.Sub(time.Unix(0, s.lastPkt.Load())) > s.w.stall {
				s.log.Warn("no media", "bytesIn", s.inputBytes(), "rtpOut", s.bytesOut.Load(), "frames", s.progress.frameCount(), "ffmpeg", s.lastError())
				s.fail(s.stallMessage(now.Sub(seenInputAt) < s.w.stall))
				<-exited
				return
			}
		case <-report.C:
			now := time.Now()
			st := s.stats(lastBytes, lastFrames, now.Sub(lastAt))
			lastBytes, lastFrames, lastAt = s.inputBytes(), s.progress.frameCount(), now
			status := store.IngressBuffering
			if s.published.Load() {
				status = store.IngressPublishing
			}
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			_, err := s.w.opts.Store.UpdateIngressRunning(ctx, s.in.ID, s.w.id, status, st)
			cancel()
			if err != nil && !errors.Is(err, store.ErrJobLost) && s.ctx.Err() == nil {
				s.log.Warn("could not report stream health", "error", err)
			}
		}
	}
}

// stallMessage says which end of the transcoder the problem is on, as far as this worker can tell. An RTMP connection that
// goes quiet is dropped well before this, and one still connected has stopped only if ffmpeg has stopped reading it.
func (s *session) stallMessage(inputMoving bool) string {
	if s.in.InputType == store.InputRTMP || inputMoving && (s.proxy != nil || s.relay != nil) {
		return fmt.Sprintf("The transcoder has produced nothing for %s, although the source is still sending.", s.w.stall)
	}
	return fmt.Sprintf("No media for %s: the source has stalled.", s.w.stall)
}

// relayReason says why the relay could not reach the source, which ffmpeg only knows as a closed connection.
func (s *session) relayReason() string {
	if s.relay == nil || s.relay.Err() == nil {
		return ""
	}
	return " | " + s.redact(s.relay.Err().Error())
}

func (s *session) proxyBlocked() error {
	if s.relay != nil && errors.Is(s.relay.Err(), source.ErrPrivate) {
		return s.relay.Err()
	}
	if s.proxy == nil {
		return nil
	}
	return s.proxy.Blocked()
}

// teardown stops everything this session runs, however far it had got.
func (s *session) teardown() {
	s.cancel()
	s.mu.Lock()
	s.tornDown = true
	stdin, cmd, pub, video, audio, proxy, relay := s.stdin, s.cmd, s.pub, s.videoConn, s.audioConn, s.proxy, s.relay
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		// A moment to flush, so the last audio reaches the room, then it goes regardless.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		proc := cmd.Process
		time.AfterFunc(2*time.Second, func() { _ = proc.Kill() })
	}
	if pub != nil {
		pub.close()
	}
	if video != nil {
		_ = video.Close()
	}
	if audio != nil {
		_ = audio.Close()
	}
	if proxy != nil {
		proxy.Close()
	}
	if relay != nil {
		relay.Close()
	}
	if s.onEnd != nil {
		s.onEnd()
	}
}

func (s *session) fail(message string) {
	s.finish(store.IngressError, message, message)
}

// finish ends the session and reports its final state, once.
func (s *session) finish(status, message, logMessage string) {
	s.release(status, message, logMessage, s.w.opts.Store.ReleaseIngress)
}

// finishForRestart ends the session because this worker is going away. A pull goes back in the queue, as nobody asked for
// it to stop.
func (s *session) finishForRestart(logMessage string) {
	s.release(store.IngressInactive, "", logMessage, s.w.opts.Store.ReleaseIngressForRestart)
}

func (s *session) release(status, message, logMessage string, release func(context.Context, string, string, string, string) (*store.Ingress, error)) {
	s.endOnce.Do(func() {
		s.teardown()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		in, err := release(ctx, s.in.ID, s.w.id, status, message)
		if err != nil {
			if !errors.Is(err, store.ErrJobLost) {
				s.log.Warn("could not save the stream's final state", "error", err)
			}
			return
		}
		level := "info"
		if status == store.IngressError {
			level = "error"
		}
		details := map[string]any{"seconds": int(time.Since(s.started).Seconds())}
		if err := s.w.opts.Store.RecordIngressEvent(ctx, in, "ingress_ended", level, "ingress_ended", logMessage, details); err != nil {
			s.log.Warn("could not record event", "error", err)
		}
		s.log.Info("stream ended", "status", status, "message", message)
	})
}

// run drives the session after the input has started the transcoder. It blocks until everything has stopped.
func (s *session) run() {
	go s.forward("video", s.videoConn)
	go s.forward("audio", s.audioConn)
	s.supervise(s.exited)
	s.cancel()
}

// startPipeline is what all inputs share: sockets, room, transcoder.
func (s *session) startPipeline(args func(video, audio int) []string, withStdin bool, connect []int) error {
	if err := s.openSockets(); err != nil {
		return err
	}
	if err := s.joinRoom(); err != nil {
		return err
	}
	video, audio := s.ports()
	return s.startTranscoder(args(video, audio), withStdin, connect)
}

// runPull is a URL input from start to finish.
func (s *session) runPull() {
	allow := s.w.opts.Config.Media.AllowPrivateAddresses
	raw, err := s.w.opts.Store.IngressSourceURL(s.in)
	if err != nil {
		s.fail("Could not read the source's address.")
		return
	}
	s.secrets = append(s.secrets, raw)
	u, err := source.CheckInput(s.ctx, raw, allow)
	if err != nil {
		s.fail("The source is not allowed: " + err.Error())
		return
	}
	input := u
	proxyURL, tcURL := "", ""
	var connect []int
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// Every connection is checked in the proxy, including redirects and playlist entries. ffmpeg is given an address on
		// the proxy, so the real one is not on its command line.
		proxy, err := source.NewProxy(allow)
		if err != nil {
			s.fail("Could not start: " + err.Error())
			return
		}
		s.mu.Lock()
		if s.tornDown {
			s.mu.Unlock()
			proxy.Close()
			return
		}
		s.proxy = proxy
		s.mu.Unlock()
		proxyURL = proxy.URL()
		connect = []int{proxy.Port()}
		if input, err = url.Parse(proxy.Serve(u)); err != nil {
			s.fail("Could not start: the source's address cannot be used.")
			return
		}
	case "rtmp":
		// As with HTTP, ffmpeg connects to something of ours, which checks the real address when it connects.
		relay, err := source.NewRelay(u, allow)
		if err != nil {
			s.fail("Could not start: " + err.Error())
			return
		}
		s.mu.Lock()
		if s.tornDown {
			s.mu.Unlock()
			relay.Close()
			return
		}
		s.relay = relay
		s.mu.Unlock()
		s.secrets = append(s.secrets, relay.Secret())
		connect = []int{relay.Port()}
		tcURL = relay.TCURL()
		if input, err = url.Parse(relay.URL()); err != nil {
			s.fail("Could not start: the source's address cannot be used.")
			return
		}
	default:
		// This goes straight from ffmpeg to the source, so it gets the address that was checked, not a name.
		pinned, err := source.PinHost(s.ctx, u, allow)
		if err != nil {
			s.fail("The source is not allowed: " + err.Error())
			return
		}
		input = pinned
		s.secrets = append(s.secrets, pinned.String())
	}
	err = s.startPipeline(func(video, audio int) []string { return urlArgs(input, proxyURL, tcURL, video, audio) }, false, connect)
	if err != nil {
		s.fail("Could not start: " + s.redact(err.Error()))
		return
	}
	s.run()
	s.cancel()
}
