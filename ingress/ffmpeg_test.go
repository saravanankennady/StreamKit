package ingress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"elnova-streamkit.local/ingress/source"
)

// These run a real ffmpeg with the arguments a session would give it, because what matters is what ffmpeg really does
// with them, not what they say.

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
}

func sampleFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.mp4")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=160x90:r=10", "-f", "lavfi", "-i", "sine=r=48000",
		"-t", "1", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", path).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return path
}

func runFFmpeg(t *testing.T, args []string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// As in a session: nothing from the environment, so no proxy settings.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// countingProxy is a forward proxy that remembers each host it was asked for.
type countingProxy struct {
	mu    sync.Mutex
	hosts []string
}

func (p *countingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hosts = append(p.hosts, r.URL.Host)
	p.mu.Unlock()
	req := r.Clone(r.Context())
	req.RequestURI = ""
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// A check of the first URL says nothing about where a redirect goes. So a redirect has to come back through the proxy,
// where the connection is judged again.
func TestFFmpegSendsRedirectsThroughTheProxy(t *testing.T) {
	needFFmpeg(t)
	sample := sampleFile(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, sample) }))
	defer target.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/moved.mp4", http.StatusFound)
	}))
	defer redirecting.Close()
	counting := &countingProxy{}
	proxy := httptest.NewServer(counting)
	defer proxy.Close()

	u, _ := url.Parse(redirecting.URL + "/a.mp4")
	out, err := runFFmpeg(t, urlArgs(u, proxy.URL, "", 40000, 40002))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	counting.mu.Lock()
	defer counting.mu.Unlock()
	seen := strings.Join(counting.hosts, " ")
	if !strings.Contains(seen, hostOf(t, redirecting.URL)) || !strings.Contains(seen, hostOf(t, target.URL)) {
		t.Fatalf("the proxy should have seen both hops: %v", counting.hosts)
	}
}

// The real proxy with the real ffmpeg: a private address is refused, and the session can tell why.
func TestFFmpegCannotReachPrivateAddressesThroughTheProxy(t *testing.T) {
	needFFmpeg(t)
	sample := sampleFile(t)
	var hits int
	var mu sync.Mutex
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.ServeFile(w, r, sample)
	}))
	defer internal.Close()
	proxy, err := source.NewProxy(false)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	u, _ := url.Parse(internal.URL + "/secret.mp4")
	out, err := runFFmpeg(t, urlArgs(u, proxy.URL(), "", 40004, 40006))
	if err == nil {
		t.Fatalf("ffmpeg read from a private address: %s", out)
	}
	if proxy.Blocked() == nil {
		t.Fatalf("the proxy did not refuse it: %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the internal server was reached %d times", hits)
	}
}

// A playlist is someone else's text, and ffmpeg follows it: it must not be able to name a local file.
func TestPlaylistsCannotReachLocalFiles(t *testing.T) {
	needFFmpeg(t)
	secret := filepath.Join(t.TempDir(), "secret.ts")
	if err := os.WriteFile(secret, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	playlist := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\nfile://%s\n#EXT-X-ENDLIST\n", secret)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, playlist)
	}))
	defer origin.Close()
	proxy := httptest.NewServer(&countingProxy{})
	defer proxy.Close()
	u, _ := url.Parse(origin.URL + "/live.m3u8")
	out, err := runFFmpeg(t, urlArgs(u, proxy.URL, "", 40008, 40010))
	if err == nil {
		t.Fatalf("expected a failure: %s", out)
	}
	if !strings.Contains(out, "not on whitelist") && !strings.Contains(out, "whitelist") {
		t.Fatalf("expected the protocol list to stop it: %s", out)
	}
}

// A concat list served from a good address names further files, which ffmpeg opens without passing on -http_proxy. So a name
// that changes what it points at gets past the proxy, unless the format is refused. Here the second fetch of the same server,
// without the proxy being asked, is what would show that.
func TestConcatListsCannotGetPastTheProxy(t *testing.T) {
	needFFmpeg(t)
	sample := sampleFile(t)
	var mu sync.Mutex
	var seen []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".ffconcat") {
			_, _ = w.Write([]byte("ffconcat version 1.0\nfile segment.mp4\n"))
			return
		}
		http.ServeFile(w, r, sample)
	}))
	defer origin.Close()
	proxied := &countingProxy{}
	proxy := httptest.NewServer(proxied)
	defer proxy.Close()
	u, _ := url.Parse(origin.URL + "/list.ffconcat")
	out, _ := runFFmpeg(t, urlArgs(u, proxy.URL, "", 40016, 40018))
	mu.Lock()
	defer mu.Unlock()
	proxied.mu.Lock()
	defer proxied.mu.Unlock()
	for _, path := range seen {
		if strings.HasSuffix(path, "segment.mp4") {
			t.Fatalf("ffmpeg followed a concat list: the origin saw %v, and the proxy was asked %d times: %s", seen, len(proxied.hosts), out)
		}
	}
}

// Playlists are what the list of formats has to allow, or none of this is any use.
func TestFFmpegPullsHLSThroughTheProxy(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=160x90:r=10", "-f", "lavfi", "-i", "sine=r=48000", "-t", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0", filepath.Join(dir, "index.m3u8")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	proxy, err := source.NewProxy(true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	u, _ := url.Parse(origin.URL + "/index.m3u8?token=abc")
	local, err := url.Parse(proxy.Serve(u))
	if err != nil {
		t.Fatal(err)
	}
	if text, err := runFFmpeg(t, urlArgs(local, proxy.URL(), "", 40020, 40022)); err != nil {
		t.Fatalf("%v: %s", err, text)
	} else if strings.Contains(strings.Join(urlArgs(local, proxy.URL(), "", 40020, 40022), " "), "token=abc") {
		t.Fatal("the source's token is on the command line")
	} else if !strings.Contains(text, "Video: h264") {
		t.Fatalf("no video: %s", text)
	}
	if proxy.BytesIn() == 0 {
		t.Fatal("nothing came through the proxy")
	}
}

// HTTPS goes through CONNECT, so it takes a different path through ffmpeg and the proxy from plain HTTP.
func TestFFmpegPullsHTTPSThroughTheProxy(t *testing.T) {
	needFFmpeg(t)
	sample := sampleFile(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, sample) }))
	defer origin.Close()
	proxy, err := source.NewProxy(true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	u, _ := url.Parse(origin.URL + "/a.mp4")
	out, err := runFFmpeg(t, urlArgs(u, proxy.URL(), "", 40012, 40014))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if proxy.BytesIn() == 0 {
		t.Fatalf("nothing came through the proxy: %s", out)
	}
}

// The transcoder really produces what the WebRTC side expects: RTP with SPS and PPS at each keyframe, baseline, with no
// B-frames, so a subscriber can join at any keyframe.
func TestTranscoderProducesDecodableRTP(t *testing.T) {
	needFFmpeg(t)
	video, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer video.Close()
	audio, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	sample := sampleFile(t)
	// A file through the same arguments as a push.
	args := append([]string{"-hide_banner", "-v", "error", "-re", "-i", sample},
		outputArgs(video.LocalAddr().(*net.UDPAddr).Port, audio.LocalAddr().(*net.UDPAddr).Port)...)
	done := make(chan string, 1)
	go func() {
		out, _ := runFFmpeg(t, args)
		done <- out
	}()

	var spsCount, idrCount, rtp int
	buf := make([]byte, 1600)
	_ = video.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		n, _, err := video.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n < 13 || (buf[1] >= 200 && buf[1] <= 207) {
			continue
		}
		rtp++
		if n > 1200 {
			t.Fatalf("a %d-byte packet leaves no room for SRTP", n)
		}
		nal := buf[12] & 0x1f
		switch nal {
		case 7:
			spsCount++
			if profile := buf[13]; profile != 66 {
				t.Fatalf("profile_idc %d, want 66: baseline", profile)
			}
			if constraint := buf[14]; constraint&0x40 == 0 {
				t.Fatalf("constraint flags %#x: constraint_set1 should be set, which is what makes it constrained baseline", constraint)
			}
		case 24: // STAP-A carries SPS and PPS together.
			if buf[15]&0x1f == 7 {
				spsCount++
			}
		case 5:
			idrCount++
		case 28: // FU-A: the real type is in the next byte.
			if buf[13]&0x80 != 0 && buf[13]&0x1f == 5 {
				idrCount++
			}
		}
		if rtp > 0 && spsCount > 0 && idrCount > 0 {
			break
		}
		_ = video.SetReadDeadline(time.Now().Add(5 * time.Second))
	}
	if rtp == 0 || spsCount == 0 || idrCount == 0 {
		out := <-done
		t.Fatalf("rtp=%d sps=%d idr=%d: %s", rtp, spsCount, idrCount, out)
	}
	_ = audio.SetReadDeadline(time.Now().Add(5 * time.Second))
	opus := 0
	for opus == 0 {
		n, _, err := audio.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no audio arrived: %v", err)
		}
		if n >= 12 && (buf[1] < 200 || buf[1] > 207) {
			opus++
		}
	}
}
