package source

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// What ffmpeg sees is an address on the proxy, and what the source sees is a request for itself, with its credentials.
func TestProxyStandsInForTheSource(t *testing.T) {
	type seen struct{ path, query, auth string }
	got := make(chan seen, 8)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")}
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/app/elsewhere.ts", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("media"))
	}))
	defer origin.Close()
	u, _ := url.Parse(origin.URL)
	u.User = url.UserPassword("operator", "hunter2")
	u.Path = "/app/index.m3u8"
	u.RawQuery = "token=abcdef"
	p, err := NewProxy(true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	local := p.Serve(u)
	for _, secret := range []string{"hunter2", "abcdef", u.Host} {
		if strings.Contains(local, secret) {
			t.Fatalf("%s shows %s", local, secret)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(address string) *http.Response {
		t.Helper()
		res, err := client.Get(address)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res
	}
	fetch(local)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("operator:hunter2"))
	if s := <-got; s != (seen{"/app/index.m3u8", "token=abcdef", wantAuth}) {
		t.Errorf("the source itself: %+v", s)
	}
	// A segment named by a relative address in the playlist resolves against the address ffmpeg was given.
	fetch(strings.TrimSuffix(local, "index.m3u8") + "seg1.ts?part=1")
	if s := <-got; s != (seen{"/app/seg1.ts", "part=1", wantAuth}) {
		t.Errorf("a segment: %+v", s)
	}
	fetch(strings.TrimSuffix(local, "index.m3u8") + "seg2.ts")
	if s := <-got; s != (seen{"/app/seg2.ts", "", wantAuth}) {
		t.Errorf("the source's query should stay with the source: %+v", s)
	}
	// A relative redirect has to mean relative to the real source.
	res := fetch(p.URL() + p.prefix + "/moved")
	<-got
	if location := res.Header.Get("Location"); location != origin.URL+"/app/elsewhere.ts" {
		t.Errorf("redirected to %s", location)
	}
	// Anything else on this port is not a way to reach the source with its password.
	if res := fetch(p.URL() + "/app/index.m3u8"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("no prefix: HTTP %d", res.StatusCode)
	}
}

func TestProxyCountsBytesAsTheyCome(t *testing.T) {
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{1}, 4096))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer origin.Close()
	defer close(release)
	u, _ := url.Parse(origin.URL + "/live.ts")
	p, err := NewProxy(true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	res, err := http.Get(p.Serve(u))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatal(err)
	}
	if p.BytesIn() < 4096 {
		t.Fatalf("a connection still open counts too: %d", p.BytesIn())
	}
}

func TestCopyReplacingFindsTheKeyWhereverReadsSplitIt(t *testing.T) {
	old, replacement := []byte("standinstandin"), []byte("the-real-key-1")
	input := bytes.Repeat([]byte{0xab}, 70000)
	copy(input[100:], old)
	copy(input[65530:], old) // Across the end of the copy's buffer.
	want := bytes.ReplaceAll(input, old, replacement)
	for name, src := range map[string]io.Reader{
		"whole":        bytes.NewReader(input),
		"byte by byte": iotest.OneByteReader(bytes.NewReader(input)),
		"half reads":   iotest.HalfReader(bytes.NewReader(input)),
	} {
		var out bytes.Buffer
		err := copyReplacing(&out, src, old, replacement)
		if err != nil && err != io.EOF {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(out.Bytes(), want) {
			t.Errorf("%s: got %d bytes, with the stand-in left in: %v", name, out.Len(), bytes.Contains(out.Bytes(), old))
		}
	}
}

func TestCopyReplacingStopsLookingOnceMediaIsFlowing(t *testing.T) {
	old, replacement := []byte("standin"), []byte("realkey")
	input := make([]byte, relayScanLimit+200000)
	copy(input[relayScanLimit+100000:], old)
	var out bytes.Buffer
	_ = copyReplacing(&out, bytes.NewReader(input), old, replacement)
	if !bytes.Equal(out.Bytes(), input) {
		t.Fatal("media after the commands must pass through untouched")
	}
}

func TestRelayAddressHoldsNoSecret(t *testing.T) {
	for _, raw := range []string{
		"rtmp://a.example.com/live2/abcd-efgh-ijkl-mnop",
		"rtmps://live.example.com:443/rtmp/FB-123-0-abc?s_bl=1&a=topsecret",
		"rtmp://a.example.com/a/b/abcd-efgh-ijkl-mnop",
	} {
		u, _ := url.Parse(raw)
		r, err := NewRelay(u, true)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for _, shown := range []string{r.URL(), r.TCURL()} {
			if strings.Contains(shown, "abcd-efgh") || strings.Contains(shown, "FB-123") || strings.Contains(shown, "topsecret") {
				t.Errorf("%s shows the key: %s", raw, shown)
			}
		}
		if len(r.standIn) != len(r.secret) {
			t.Errorf("the stand-in has to be as long as the key: %q %q", r.standIn, r.secret)
		}
		if !strings.Contains(r.TCURL(), u.Host) {
			t.Errorf("%s should name the real host: %s", raw, r.TCURL())
		}
	}
}

// fakeServer accepts one connection and reports what it received.
func fakeServer(t *testing.T, l net.Listener) <-chan []byte {
	t.Helper()
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		got <- data
	}()
	return got
}

func pushThrough(t *testing.T, r *Relay, payload string) {
	t.Helper()
	c, err := net.Dial("tcp", r.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = c.Close()
}

func TestCopyReplacingDoesNotHoldBackACommand(t *testing.T) {
	src, w := io.Pipe()
	r, dst := io.Pipe()
	go func() { _ = copyReplacing(dst, src, []byte("standin"), []byte("realkey")) }()
	go func() { _, _ = w.Write([]byte("publish live")) }()
	buf := make([]byte, 32)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "publish live" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
	// Something that could be the start of the key does wait, for as long as it could still be.
	go func() { _, _ = w.Write([]byte("tail stan")) }()
	if n, err = r.Read(buf); err != nil || string(buf[:n]) != "tail " {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
	go func() { _, _ = w.Write([]byte("dard")) }()
	if n, err = r.Read(buf); err != nil || string(buf[:n]) != "standard" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
	_ = w.Close()
}

func TestRelayFillsInTheKey(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := fakeServer(t, l)
	u, _ := url.Parse("rtmp://" + l.Addr().String() + "/live/the-real-key")
	r, err := NewRelay(u, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	handshake := strings.Repeat("h", rtmpHandshakeBytes)
	pushThrough(t, r, handshake+"publish "+string(amfString(r.standIn))+" live")
	if data := <-got; string(data) != handshake+"publish "+string(amfString([]byte("the-real-key")))+" live" {
		t.Fatalf("the server got %q", data[rtmpHandshakeBytes:])
	}
}

func TestRelayRefusesPrivateAddressesWhenItConnects(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, scheme := range []string{"rtmp", "rtmps"} {
		u, _ := url.Parse(scheme + "://" + l.Addr().String() + "/live/key")
		r, err := NewRelay(u, false)
		if err != nil {
			t.Fatal(err)
		}
		pushThrough(t, r, "x")
		if err := r.Err(); err != ErrPrivate {
			t.Errorf("%s: %v", scheme, err)
		}
		r.Close()
	}
}

// A name that passed its check when the job was accepted can point somewhere else when the connection is made, which is
// what the dial-time check is for. localhost stands in for such a name here: it resolves, and to a private address.
func TestRelayChecksWhatANameResolvesTo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	u, _ := url.Parse("rtmps://localhost:" + port + "/live/key")
	r, err := NewRelay(u, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pushThrough(t, r, "x")
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("got %v", err)
	}
}

func selfSigned(t *testing.T, host string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP(host)}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, path
}

func TestRelayRefusesACertificateItCannotVerify(t *testing.T) {
	cert, _ := selfSigned(t, "127.0.0.1")
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		got <- data
	}()
	u, _ := url.Parse("rtmps://" + l.Addr().String() + "/live/the-real-key")
	r, err := NewRelay(u, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pushThrough(t, r, strings.Repeat("h", rtmpHandshakeBytes)+"publish "+string(amfString(r.standIn)))
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("a certificate nobody vouches for should be refused: %v", err)
	}
	select {
	case data := <-got:
		if bytes.Contains(data, []byte("the-real-key")) {
			t.Fatal("the key reached a server that could not prove who it is")
		}
	case <-time.After(200 * time.Millisecond):
	}
}
