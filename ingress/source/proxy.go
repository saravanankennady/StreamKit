package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"elnova-streamkit.local/api/webhook"
)

const (
	dialTimeout   = 10 * time.Second
	headerTimeout = 30 * time.Second
)

// guardedDialer refuses to connect to private addresses. It checks at the moment of connecting, after the name has
// been resolved, so neither a redirect nor a name that changes its answer gets around it.
func guardedDialer(allowPrivate bool) *net.Dialer {
	d := &net.Dialer{Timeout: dialTimeout}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if webhook.IsPrivateIP(net.ParseIP(host)) {
				return fmt.Errorf("%w: %s", ErrPrivate, host)
			}
			return nil
		}
	}
	return d
}

// Proxy is a local forward proxy for ffmpeg's HTTP requests. ffmpeg follows redirects and fetches whatever a
// playlist names, none of which a check of the first URL can see. Sending everything through here means every
// connection it makes, however it came to make it, is checked. It also counts the bytes coming in, which ffmpeg
// does not report.
//
// It also stands in for the source itself, so that ffmpeg's command line, which any process on the machine can read, does
// not hold a source's address, which often has a password or a signed token in it. ffmpeg is given a path on this proxy, and
// relative addresses in a playlist resolve against that path as they would against the real one.
type Proxy struct {
	dialer    *net.Dialer
	transport *http.Transport
	listener  net.Listener
	server    *http.Server
	bytesIn   atomic.Int64
	blocked   atomic.Value
	source    *url.URL
	prefix    string
}

// NewProxy starts a proxy on a loopback port.
func NewProxy(allowPrivate bool) (*Proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{dialer: guardedDialer(allowPrivate), listener: l}
	p.transport = &http.Transport{
		DialContext:           p.dialer.DialContext,
		ResponseHeaderTimeout: headerTimeout,
		// ffmpeg should see what the server sent, compressed or not, and it asks for what it can handle.
		DisableCompression: true,
	}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: headerTimeout}
	go func() { _ = p.server.Serve(l) }()
	return p, nil
}

// URL is the value for ffmpeg's -http_proxy.
func (p *Proxy) URL() string { return "http://" + p.listener.Addr().String() }

// Port is the port ffmpeg should be allowed to connect to.
func (p *Proxy) Port() int { return p.listener.Addr().(*net.TCPAddr).Port }

// Serve sets the source this proxy stands in for, and returns the address to give ffmpeg instead of it.
func (p *Proxy) Serve(source *url.URL) string {
	p.source = source
	p.prefix = "/s/" + string(randomLetters(16))
	path := source.EscapedPath()
	if path == "" {
		path = "/"
	}
	return p.URL() + p.prefix + path
}

// sourceRequest turns a request for a path on this proxy into one for the source.
func (p *Proxy) sourceRequest(r *http.Request) (*http.Request, bool) {
	if p.source == nil || !strings.HasPrefix(r.URL.Path, p.prefix+"/") {
		return nil, false
	}
	if r.URL.IsAbs() && r.URL.Host != p.listener.Addr().String() {
		return nil, false
	}
	u := *p.source
	u.User = nil
	u.Path = strings.TrimPrefix(r.URL.Path, p.prefix)
	u.RawPath = strings.TrimPrefix(r.URL.RawPath, p.prefix)
	// The source's own query goes with the source's own path. Anything else a playlist names carries its own.
	if r.URL.RawQuery != "" || u.EscapedPath() != p.source.EscapedPath() {
		u.RawQuery = r.URL.RawQuery
	}
	req := r.Clone(r.Context())
	req.URL = &u
	req.Host = ""
	if p.source.User != nil {
		password, _ := p.source.User.Password()
		req.SetBasicAuth(p.source.User.Username(), password)
	}
	return req, true
}

// BytesIn reports how much the source has sent so far.
func (p *Proxy) BytesIn() int64 { return p.bytesIn.Load() }

// Blocked returns the error from a connection refused because of where it pointed, if there was one.
func (p *Proxy) Blocked() error {
	if err, ok := p.blocked.Load().(error); ok {
		return err
	}
	return nil
}

// Close stops the proxy and its connections.
func (p *Proxy) Close() {
	_ = p.server.Close()
	p.transport.CloseIdleConnections()
}

func (p *Proxy) note(err error) {
	if errors.Is(err, ErrPrivate) {
		p.blocked.Store(err)
	}
}

// ServeHTTP handles HTTPS through CONNECT, and plain HTTP by fetching the absolute URL it is given.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	req, fromSource := p.sourceRequest(r)
	if !fromSource {
		if !r.URL.IsAbs() || r.URL.Scheme != "http" {
			http.Error(w, "expected an absolute http URL", http.StatusBadRequest)
			return
		}
		req = r.Clone(r.Context())
	}
	req.RequestURI = ""
	req.Header.Del("Proxy-Connection")
	res, err := p.transport.RoundTrip(req)
	if err != nil {
		p.note(err)
		http.Error(w, "the source could not be reached", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	// ffmpeg thinks the source is here, so a redirect relative to the source would lead it astray.
	if location := res.Header.Get("Location"); fromSource && location != "" {
		if next, err := req.URL.Parse(location); err == nil {
			w.Header().Set("Location", next.String())
		}
	}
	w.WriteHeader(res.StatusCode)
	// A live source sends a little at a time, and ffmpeg should get it as it comes.
	flush := http.NewResponseController(w)
	_ = flush.Flush()
	buf := make([]byte, 32<<10)
	body := countingReader{res.Body, &p.bytesIn}
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = flush.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	upstream, err := p.dialer.DialContext(ctx, "tcp", r.Host)
	if err != nil {
		p.note(err)
		http.Error(w, "the source could not be reached", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, countingReader{upstream, &p.bytesIn})
		done <- struct{}{}
	}()
	<-done
}
