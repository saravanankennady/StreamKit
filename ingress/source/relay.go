package source

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tlsHandshakeTimeout = 15 * time.Second
	// How far into a connection the key can be. Commands come first, and after them all that flows is media.
	relayScanLimit = 1 << 20
	// C0, C1 and C2, which come before anything else.
	rtmpHandshakeBytes = 1 + 1536 + 1536
)

// Relay is what ffmpeg's RTMP connection goes through. ffmpeg connects to it on a loopback port, with a stand-in where the
// stream key would be, and it fills in the real key on the way through. So ffmpeg's command line, which any process on the
// machine can read, holds no key. The relay makes the outgoing connection itself, which means two more things: the
// address is checked at the moment of connecting, so a name that changes its answer gets nowhere, and for rtmps:// the
// certificate is checked against the name, which ffmpeg does not do.
//
// The stand-in is the same length as the key, so no RTMP message changes size and nothing but those bytes has to change.
type Relay struct {
	target       *url.URL
	allowPrivate bool
	dialer       *net.Dialer
	listener     net.Listener
	standIn      []byte
	secret       []byte
	localPath    string
	bytesIn      atomic.Int64

	mu   sync.Mutex
	err  error
	conn net.Conn
}

// NewRelay starts a relay for one connection to target, which must be an rtmp:// or rtmps:// address.
func NewRelay(target *url.URL, allowPrivate bool) (*Relay, error) {
	scheme := strings.ToLower(target.Scheme)
	if scheme != "rtmp" && scheme != "rtmps" {
		return nil, ErrOutput
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &Relay{target: target, allowPrivate: allowPrivate, dialer: guardedDialer(allowPrivate), listener: l}
	r.splitKey()
	go r.serve()
	return r, nil
}

// splitKey separates the part of the address that acts as a password: the last path segment, and any query.
func (r *Relay) splitKey() {
	path := r.target.EscapedPath()
	i := strings.LastIndexByte(path, '/')
	if i < 0 || i == len(path)-1 {
		r.localPath = path
		return
	}
	key := path[i+1:]
	if r.target.RawQuery != "" {
		key += "?" + r.target.RawQuery
	}
	r.secret = []byte(key)
	r.standIn = randomLetters(len(key))
	r.localPath = path[:i+1] + string(r.standIn)
}

// amfString is a string as an RTMP command carries it, which is how the stream key travels. Looking for this rather than
// for the bare stand-in means media can pass through with only a byte or two held back now and then.
func amfString(s []byte) []byte {
	if len(s) == 0 {
		return nil
	}
	return append([]byte{2, byte(len(s) >> 8), byte(len(s))}, s...)
}

func randomLetters(n int) []byte {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, n)
	_, _ = rand.Read(out)
	for i, b := range out {
		out[i] = letters[int(b)%len(letters)]
	}
	return out
}

// URL is where ffmpeg should connect. It holds no secret.
func (r *Relay) URL() string { return "rtmp://" + r.listener.Addr().String() + r.localPath }

// Port is the port ffmpeg should be allowed to connect to.
func (r *Relay) Port() int { return r.listener.Addr().(*net.TCPAddr).Port }

// TCURL is the address the far server should see in the RTMP connect command, for ffmpeg's -rtmp_tcurl. Servers that host
// several sites tell them apart by this, so it has to name the real host. It holds no key: that goes in a later command.
func (r *Relay) TCURL() string {
	u := *r.target
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	path := u.EscapedPath()
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		path = path[:i]
	} else {
		path = ""
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host + path
}

// Secret is the part of the address that is hidden, so it can be removed from anything the far server says back.
func (r *Relay) Secret() string { return string(r.secret) }

// BytesIn reports how much the far side has sent so far.
func (r *Relay) BytesIn() int64 { return r.bytesIn.Load() }

// Err returns why the outgoing connection could not be made, if it could not.
func (r *Relay) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Relay) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// Close ends the relay and its connection.
func (r *Relay) Close() {
	_ = r.listener.Close()
	r.mu.Lock()
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.mu.Unlock()
}

// serve takes one connection, which is ffmpeg's. There is no reason for a second.
func (r *Relay) serve() {
	client, err := r.listener.Accept()
	_ = r.listener.Close()
	if err != nil {
		return
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+tlsHandshakeTimeout)
	defer cancel()
	upstream, err := r.dial(ctx)
	if err != nil {
		r.setErr(err)
		return
	}
	defer upstream.Close()
	r.mu.Lock()
	r.conn = upstream
	r.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() {
		// The handshake is random bytes, and nothing in it can be the key.
		if _, err := io.CopyN(upstream, client, rtmpHandshakeBytes); err == nil {
			_ = copyReplacing(upstream, client, amfString(r.standIn), amfString(r.secret))
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, countingReader{upstream, &r.bytesIn})
		done <- struct{}{}
	}()
	<-done
}

func (r *Relay) dial(ctx context.Context) (net.Conn, error) {
	host := r.target.Hostname()
	secure := strings.EqualFold(r.target.Scheme, "rtmps")
	port := r.target.Port()
	if port == "" {
		port = "1935"
		if secure {
			port = "443"
		}
	}
	if !r.allowPrivate {
		if _, err := PublicAddress(ctx, host); err != nil {
			return nil, err
		}
	}
	conn, err := r.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		if errors.Is(err, ErrPrivate) {
			return nil, ErrPrivate
		}
		return nil, fmt.Errorf("could not connect to %s:%s: %w", host, port, err)
	}
	if !secure {
		return conn, nil
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("could not make a TLS connection to %s: %w", host, err)
	}
	return tlsConn, nil
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// copyReplacing copies src to dst, changing every old to replacement within the first relayScanLimit bytes. They are the
// same length. After that, it is a plain copy. It only holds bytes back if they could be the start of old, because RTMP is
// a conversation: holding back the end of a command would wait forever for a reply to a command the server has not seen.
func copyReplacing(dst io.Writer, src io.Reader, old, replacement []byte) error {
	if len(old) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}
	buf := make([]byte, 64<<10)
	held := 0 // Bytes at the start of buf, carried over from the last read, that may be the start of old.
	scanned := 0
	for scanned < relayScanLimit {
		n, err := src.Read(buf[held:])
		total := held + n
		for {
			i := bytes.Index(buf[:total], old)
			if i < 0 {
				break
			}
			copy(buf[i:], replacement)
		}
		send := total
		if err == nil {
			send -= partialMatch(buf[:total], old)
		}
		if send > 0 {
			if _, werr := dst.Write(buf[:send]); werr != nil {
				return werr
			}
		}
		held = copy(buf, buf[send:total])
		scanned += send
		if err != nil {
			return err
		}
	}
	if held > 0 {
		if _, err := dst.Write(buf[:held]); err != nil {
			return err
		}
	}
	_, err := io.Copy(dst, src)
	return err
}

// partialMatch returns how many bytes at the end of data are the start of pattern.
func partialMatch(data, pattern []byte) int {
	for n := min(len(pattern)-1, len(data)); n > 0; n-- {
		if bytes.HasSuffix(data, pattern[:n]) {
			return n
		}
	}
	return 0
}
