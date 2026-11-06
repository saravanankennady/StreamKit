package ingress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	rtmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"

	"elnova-streamkit.local/api/store"
)

const (
	// A publisher that goes silent this long is gone. Encoders send continuously, so this is generous, and it is what
	// frees a key after a connection dies without saying so.
	rtmpReadTimeout = 10 * time.Second
	// A timer this late means this process was not running when it was due.
	rtmpLateTimeout = 200 * time.Millisecond
	rtmpMaxConns    = 512
	// Anyone can connect to this port, and an encoder has nothing to wait for before it publishes. So a connection that has
	// not done that by now is no encoder, however slowly it keeps sending.
	rtmpSetupLimit = 20 * time.Second
	// One address cannot take all the connections, so it cannot keep everyone else out.
	rtmpMaxPerAddress = 32
)

var (
	errRTMPRejected     = errors.New("publish rejected")
	errRTMPNotReady     = errors.New("media before publishing")
	errRTMPSetupTooSlow = errors.New("took too long to start publishing")
)

// streamKeyFrom takes the key from a publishing name. Encoders disagree about where the key goes, so the last path
// segment is the key, whatever the app was called, and any query string is dropped.
func streamKeyFrom(name string) string {
	key, _, _ := strings.Cut(name, "?")
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		key = key[i+1:]
	}
	return strings.TrimSpace(key)
}

// ipCounter counts open connections for each remote address.
type ipCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

// add reports false, and counts nothing, if this address already has as many as it may.
func (c *ipCounter) add(ip string, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[ip] >= limit {
		return false
	}
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[ip]++
	return true
}

func (c *ipCounter) done(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[ip]--; c.counts[ip] <= 0 {
		delete(c.counts, ip)
	}
}

// deadlineConn drops a connection that has gone silent, and one that is taking too long to start publishing.
type deadlineConn struct {
	net.Conn
	bytes *atomic.Int64
	// setupBy is when a connection has to have started publishing by, and published says whether it has.
	setupBy   time.Time
	published *atomic.Bool
}

// Read gives a silent publisher one chance that was really its own. If this process did not run for a while, as happens
// under memory pressure, the deadline can pass with data sitting unread, so a timeout that came later than it should have
// says nothing about the publisher.
func (c *deadlineConn) Read(p []byte) (int, error) {
	for {
		started := time.Now()
		deadline := started.Add(rtmpReadTimeout)
		if !c.published.Load() {
			if !started.Before(c.setupBy) {
				return 0, errRTMPSetupTooSlow
			}
			if c.setupBy.Before(deadline) {
				deadline = c.setupBy
			}
		}
		_ = c.Conn.SetReadDeadline(deadline)
		n, err := c.Conn.Read(p)
		c.bytes.Add(int64(n))
		var timeout net.Error
		if n == 0 && errors.As(err, &timeout) && timeout.Timeout() && time.Since(deadline) > rtmpLateTimeout {
			continue
		}
		return n, err
	}
}

func (w *Worker) serveRTMP(ctx context.Context, listener net.Listener) {
	discard := logrus.New()
	discard.SetOutput(io.Discard)
	server := rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			ip := remoteIP(conn)
			if w.rtmpConns.Add(1) > rtmpMaxConns || !w.rtmpByIP.add(ip, rtmpMaxPerAddress) {
				w.rtmpConns.Add(-1)
				_ = conn.Close()
				return conn, &rtmp.ConnConfig{Logger: discard}
			}
			h := &rtmpHandler{w: w, conn: conn, ip: ip, log: w.log.With("remote", conn.RemoteAddr().String())}
			return &deadlineConn{Conn: conn, bytes: &h.received, setupBy: time.Now().Add(rtmpSetupLimit), published: &h.published}, &rtmp.ConnConfig{
				Handler: h,
				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
				// This library logs at length, including the publishing name, which is the stream key.
				Logger: discard,
			}
		},
	})
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, rtmp.ErrClosed) {
		w.log.Error("RTMP server stopped", "error", err)
	}
}

func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

// rtmpHandler is one RTMP connection.
type rtmpHandler struct {
	rtmp.DefaultHandler
	w         *Worker
	conn      net.Conn
	ip        string
	log       *slog.Logger
	received  atomic.Int64
	published atomic.Bool
	session   *session
}

// OnPublish decides whether this stream may go ahead. Refusing returns an error, and the library closes the connection.
func (h *rtmpHandler) OnPublish(_ *rtmp.StreamContext, _ uint32, cmd *rtmpmsg.NetStreamPublish) error {
	key := streamKeyFrom(cmd.PublishingName)
	if key == "" {
		return errRTMPRejected
	}
	h.published.Store(true)
	if !h.w.hasRoom() {
		h.log.Warn("refused an RTMP publisher: this worker is full")
		return errRTMPRejected
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in, err := h.w.opts.Store.ClaimIngressByKey(ctx, key, store.InputRTMP, h.w.id, h.w.lease)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Deliberately nothing written anywhere: anyone can send anything to this port.
		h.log.Info("refused an RTMP publisher: unknown stream key")
		return errRTMPRejected
	case errors.Is(err, store.ErrIngressDisabled):
		_ = h.w.opts.Store.RecordIngressEvent(ctx, in, "", "warn", "ingress_rejected", "Refused an RTMP publisher: this ingress is disabled.", nil)
		return errRTMPRejected
	case errors.Is(err, store.ErrIngressBusy):
		_ = h.w.opts.Store.RecordIngressEvent(ctx, in, "", "warn", "ingress_rejected", "Refused a second RTMP publisher on the same stream key.", nil)
		return errRTMPRejected
	case errors.Is(err, store.ErrProjectAtLimit):
		_ = h.w.opts.Store.RecordIngressEvent(ctx, in, "", "warn", "ingress_rejected", "Refused an RTMP publisher: this project is running as many streams as it may.", nil)
		return errRTMPRejected
	case err != nil:
		h.log.Warn("could not look up a stream key", "error", err)
		return errRTMPRejected
	}
	s := newSession(h.w, in)
	s.onEnd = func() { _ = h.conn.Close() }
	_ = h.w.opts.Store.RecordIngressEvent(ctx, in, "", "info", "ingress_connected", "RTMP publisher connected from "+h.conn.RemoteAddr().String(), nil)

	if err := s.startPipeline(func(video, audio int) []string { return rtmpArgs(video, audio) }, true, nil); err != nil {
		s.fail("Could not start: " + err.Error())
		return errRTMPRejected
	}
	if _, err := s.stdin.Write(flvHeader); err != nil {
		s.fail("Could not start the transcoder: " + err.Error())
		return errRTMPRejected
	}
	h.session = s
	h.w.track(s)
	go func() {
		defer h.w.untrack(s)
		s.run()
	}()
	return nil
}

func (h *rtmpHandler) write(kind byte, timestamp uint32, payload io.Reader) error {
	s := h.session
	if s == nil {
		return errRTMPNotReady
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, payload)
	if err != nil {
		return err
	}
	s.bytesIn.Add(n)
	if err := writeFLVTag(s.stdin, kind, timestamp, buf.Bytes()); err != nil {
		// The session has already ended, or is about to find out why. Either way this connection is done.
		return err
	}
	return nil
}

// OnSetDataFrame passes the encoder's metadata along.
func (h *rtmpHandler) OnSetDataFrame(timestamp uint32, data *rtmpmsg.NetStreamSetDataFrame) error {
	return h.write(tagScript, timestamp, bytes.NewReader(data.Payload))
}

// OnAudio implements rtmp.Handler.
func (h *rtmpHandler) OnAudio(timestamp uint32, payload io.Reader) error {
	return h.write(tagAudio, timestamp, payload)
}

// OnVideo implements rtmp.Handler.
func (h *rtmpHandler) OnVideo(timestamp uint32, payload io.Reader) error {
	return h.write(tagVideo, timestamp, payload)
}

// OnClose implements rtmp.Handler. Closing stdin ends the transcoder, which ends the session. Reconnecting then works,
// because the key is free as soon as that has happened.
func (h *rtmpHandler) OnClose() {
	h.w.rtmpConns.Add(-1)
	h.w.rtmpByIP.done(h.ip)
	if s := h.session; s != nil {
		_ = s.stdin.Close()
	}
}
