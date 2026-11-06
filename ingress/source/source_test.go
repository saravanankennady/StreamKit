package source

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCheckInput(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		raw     string
		private bool
		want    error
	}{
		{"http://93.184.216.34/a.mp4", false, nil},
		{"https://93.184.216.34/a.m3u8", false, nil},
		{"rtmp://93.184.216.34/live/x", false, nil},
		{"srt://93.184.216.34:9000", false, nil},
		{"srt://93.184.216.34:9000?mode=caller", false, nil},
		{"srt://93.184.216.34:9000?mode=listener", false, ErrSRTMode},
		{"srt://93.184.216.34:9000?mode=listener", true, ErrSRTMode},
		{"file:///etc/passwd", false, ErrScheme},
		{"file:///etc/passwd", true, ErrScheme},
		{"ftp://93.184.216.34/x", false, ErrScheme},
		{"gopher://93.184.216.34/x", false, ErrScheme},
		{"concat:a|b", false, ErrScheme},
		{"/etc/passwd", false, ErrScheme},
		{"", false, ErrScheme},
		{"http://", false, ErrHost},
		{"http://127.0.0.1/x", false, ErrPrivate},
		{"http://127.0.0.1/x", true, nil},
		{"http://localhost/x", false, ErrPrivate},
		{"http://foo.localhost/x", false, ErrPrivate},
		{"http://10.0.0.1/x", false, ErrPrivate},
		{"http://192.168.1.1/x", false, ErrPrivate},
		{"http://169.254.169.254/latest/meta-data/", false, ErrPrivate},
		{"http://100.64.0.1/x", false, ErrPrivate},
		{"http://[::1]/x", false, ErrPrivate},
		{"http://[fe80::1]/x", false, ErrPrivate},
		{"http://0.0.0.0/x", false, ErrPrivate},
		{"rtmp://10.1.2.3/live/x", false, ErrPrivate},
		{"srt://172.16.0.1:9000", false, ErrPrivate},
	} {
		_, err := CheckInput(ctx, c.raw, c.private)
		if !errors.Is(err, c.want) {
			t.Errorf("%q (private=%v): got %v, want %v", c.raw, c.private, err, c.want)
		}
	}
}

func TestCheckOutput(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		raw     string
		private bool
		want    error
	}{
		{"rtmp://93.184.216.34/live/key", false, nil},
		{"rtmps://93.184.216.34/live/key", false, nil},
		{"http://93.184.216.34/x", false, ErrOutput},
		{"file:///tmp/x", true, ErrOutput},
		{"rtmp://127.0.0.1/live/key", false, ErrPrivate},
		{"rtmp://127.0.0.1/live/key", true, nil},
	} {
		if _, err := CheckOutput(ctx, c.raw, c.private); !errors.Is(err, c.want) {
			t.Errorf("%q: got %v, want %v", c.raw, err, c.want)
		}
	}
}

func TestPinHostKeepsThePortAndPath(t *testing.T) {
	u, _ := url.Parse("rtmp://93.184.216.34:1935/live/key?a=b")
	out, err := PinHost(context.Background(), u, false)
	if err != nil || out.String() != "rtmp://93.184.216.34:1935/live/key?a=b" {
		t.Fatalf("%v %v", out, err)
	}
	u, _ = url.Parse("rtmp://127.0.0.1/live/key")
	if _, err := PinHost(context.Background(), u, false); !errors.Is(err, ErrPrivate) {
		t.Fatalf("got %v", err)
	}
}

// A redirect is how a checked URL would otherwise reach somewhere it was not checked for. Every request ffmpeg makes
// goes through this proxy, so each hop is judged by where it actually connects.
func TestProxyRefusesPrivateTargets(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer internal.Close()

	p, err := NewProxy(false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse(p.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	res, err := client.Get(internal.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "secret") {
		t.Fatalf("reached a private address through the proxy: %d %s", res.StatusCode, body)
	}
	if !errors.Is(p.Blocked(), ErrPrivate) {
		t.Fatalf("Blocked() = %v", p.Blocked())
	}

	// CONNECT, which HTTPS uses, is held to the same rule.
	tlsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer tlsTarget.Close()
	tlsClient := tlsTarget.Client()
	tlsClient.Transport.(*http.Transport).Proxy = http.ProxyURL(proxyURL)
	if _, err := tlsClient.Get(tlsTarget.URL); err == nil {
		t.Fatal("tunnelled to a private address")
	}
}

func TestProxyForwardsWhenPrivateIsAllowed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=2-" {
			t.Errorf("range header lost: %q", r.Header.Get("Range"))
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("hello"))
	}))
	defer origin.Close()
	p, err := NewProxy(true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse(p.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
	req.Header.Set("Range", "bytes=2-")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "hello" {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if p.BytesIn() != 5 {
		t.Fatalf("counted %d bytes", p.BytesIn())
	}
}
