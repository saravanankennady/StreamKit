package ingress

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

func TestWriteFLVTag(t *testing.T) {
	var b bytes.Buffer
	if err := writeFLVTag(&b, tagVideo, 0x01020304, []byte{0xAA, 0xBB, 0xCC}); err != nil {
		t.Fatal(err)
	}
	got := b.Bytes()
	want := []byte{
		9,       // type
		0, 0, 3, // size
		2, 3, 4, // timestamp, low 24 bits
		1,       // timestamp, high byte
		0, 0, 0, // stream ID
		0xAA, 0xBB, 0xCC,
		0, 0, 0, 14, // tag size: 11 + 3
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
	if binary.BigEndian.Uint32(got[len(got)-4:]) != uint32(11+3) {
		t.Fatal("previous tag size")
	}
}

func TestIsRTP(t *testing.T) {
	rtp := func(second byte) []byte {
		p := make([]byte, 20)
		p[0], p[1] = 0x80, second
		return p
	}
	for name, c := range map[string]struct {
		packet []byte
		want   bool
	}{
		"media":                 {rtp(96), true},
		"media with the marker": {rtp(0x80 | 96), true},
		"second stream, marker": {rtp(0x80 | 97), true},
		"sender report":         {rtp(200), false},
		"bye":                   {rtp(203), false},
		"too short":             {[]byte{0x80, 96, 0, 0}, false},
		"empty":                 {nil, false},
		// A STUN binding request, as an ICE agent still checking an old candidate would send to a reused port.
		"stun": {append([]byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xa4, 0x42}, make([]byte, 12)...), false},
	} {
		if got := isRTP(c.packet); got != c.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestFLVHeaderSaysAudioAndVideo(t *testing.T) {
	if string(flvHeader[:3]) != "FLV" || flvHeader[4] != 5 || len(flvHeader) != 13 {
		t.Fatalf("% x", flvHeader)
	}
}

func TestStreamKeyFrom(t *testing.T) {
	for in, want := range map[string]string{
		"key":            "key",
		"live/key":       "key",
		"a/b/key":        "key",
		"key?token=x":    "key",
		"live/key?a=b&c": "key",
		"":               "",
		"/":              "",
		"  spaced  ":     "spaced",
	} {
		if got := streamKeyFrom(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestInputInfoReadsFFmpegOutput(t *testing.T) {
	var i inputInfo
	for _, line := range strings.Split(`Input #0, flv, from 'pipe:0':
  Metadata:
    encoder         : Lavf62.3.100
  Duration: 00:00:00.00, start: 0.000000, bitrate: N/A
  Stream #0:0: Video: h264 (High), yuv420p(progressive), 1280x720 [SAR 1:1 DAR 16:9], 29.97 fps, 30 tbr, 1k tbn
  Stream #0:1: Audio: aac (LC), 48000 Hz, stereo, fltp, 128 kb/s
Stream mapping:
  Stream #0:0 -> #0:0 (h264 (native) -> h264 (libx264))
Output #0, tee, to '[select=v:f=rtp:onfail=ignore]rtp://127.0.0.1:1|':
  Stream #0:0: Video: h264, yuv420p(tv, progressive), 640x360, q=2-31, 2500 kb/s, 30 fps, 30 tbn
  Stream #0:1: Audio: opus, 48000 Hz, stereo, flt, 96 kb/s`, "\n") {
		i.parseLine(line)
	}
	w, h, fps, video, audio := i.snapshot()
	if w != 1280 || h != 720 || fps != 29.97 || video != "h264" || audio != "aac" {
		t.Fatalf("read the output, or missed the input: %d %d %v %s %s", w, h, fps, video, audio)
	}
}

func TestProgress(t *testing.T) {
	var p progress
	for _, line := range []string{"frame=91", "fps=29.97", "bitrate=N/A", "progress=continue", "frame=  120"} {
		p.parseLine(line)
	}
	if p.frameCount() != 120 {
		t.Fatalf("%d", p.frameCount())
	}
}

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestOutputIsWhatBrowsersDecode(t *testing.T) {
	args := outputArgs(5000, 5002)
	for _, seq := range [][]string{{"-c:v", "libx264"}, {"-profile:v", "baseline"}, {"-bf", "0"}, {"-c:a", "libopus"}, {"-ar", "48000"}, {"-map", "0:v:0?"}, {"-map", "0:a:0?"}, {"-x264-params", "repeat-headers=1"}} {
		if !hasSeq(args, seq...) {
			t.Errorf("missing %v in %v", seq, args)
		}
	}
	tee := args[len(args)-1]
	if !strings.Contains(tee, "rtp://127.0.0.1:5000?pkt_size=1200&rtcpport=5000") || !strings.Contains(tee, "rtp://127.0.0.1:5002?pkt_size=1200&rtcpport=5002") {
		t.Errorf("%s", tee)
	}
	if strings.Count(tee, "onfail=ignore") != 2 {
		t.Errorf("a source may lack audio or video: %s", tee)
	}
}

func TestPullArgsLimitProtocols(t *testing.T) {
	for raw, wantProtocols := range map[string]string{
		"https://example.com/a.m3u8": "http,https,tcp,tls,crypto,httpproxy",
		"rtmp://example.com/live/x":  "rtmp,tcp",
		"srt://example.com:9000":     "srt,udp",
	} {
		u, _ := url.Parse(raw)
		args := urlArgs(u, "http://127.0.0.1:1", "", 5000, 5002)
		if !hasSeq(args, "-protocol_whitelist", wantProtocols) {
			t.Errorf("%s: %v", raw, args)
		}
		if strings.HasPrefix(raw, "http") != slices.Contains(args, "-http_proxy") {
			t.Errorf("%s: only HTTP uses the proxy: %v", raw, args)
		}
		if strings.HasPrefix(raw, "http") != slices.Contains(args, "-re") {
			t.Errorf("%s: only files need pacing: %v", raw, args)
		}
		if !hasSeq(args, "-i", raw) {
			t.Errorf("%s: %v", raw, args)
		}
	}
}

func TestRedactKeepsTheSourceOutOfMessages(t *testing.T) {
	s := &session{secrets: []string{"srt://1.2.3.4:9000?passphrase=hunter2"}}
	got := s.redact("[http @ 0x1] could not open http://user:pw@example.com/a: refused")
	if strings.Contains(got, "pw") || !strings.Contains(got, "refused") {
		t.Fatal(got)
	}
	// What ffmpeg was really given, which is not what was saved, and how it talks about it.
	for _, line := range []string{
		"Error opening input file rtmp://user:pw@1.2.3.4/live/the-key-12345.",
		"Input #0, flv, from 'rtmp://user:pw@1.2.3.4/live/the-key-12345':",
		"Opening 'https://cdn.example.com/seg1.ts?token=the-key-12345' for reading",
		"failed: srt://1.2.3.4:9000?passphrase=hunter2 refused",
	} {
		got := s.redact(line)
		for _, secret := range []string{"pw", "the-key-12345", "hunter2"} {
			if strings.Contains(got, secret) {
				t.Errorf("%q still holds %q: %s", line, secret, got)
			}
		}
	}
}

func TestPullsAreLimitedToKnownContainers(t *testing.T) {
	for _, raw := range []string{"https://example.com/a.m3u8", "rtmp://example.com/live/x", "srt://example.com:9000"} {
		u, _ := url.Parse(raw)
		if args := urlArgs(u, "http://127.0.0.1:1", "", 5000, 5002); !hasSeq(args, "-format_whitelist", allowedFormats) {
			t.Errorf("%s: %v", raw, args)
		}
	}
	if slices.Contains(strings.Split(allowedFormats, ","), "concat") {
		t.Error("concat opens other addresses, without going through the proxy")
	}
}

func TestSessionTokensAreShortLived(t *testing.T) {
	w := &Worker{opts: Options{StaticKeys: map[string]string{"k": "a-long-enough-secret-for-signing-tokens"}}}
	s := &session{w: w, ctx: context.Background(), in: &store.Ingress{ProjectID: auth.StaticProject, RoomName: "r", ParticipantIdentity: "i"}}
	token, err := s.accessToken()
	if err != nil {
		t.Fatal(err)
	}
	expires := tokenExpiry(t, token)
	if left := time.Until(expires); left <= 0 || left > 10*time.Minute {
		t.Fatalf("a token that is used once, to join, lasts %s", left)
	}
}

// tokenExpiry reads a token's expiry without checking its signature.
func tokenExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	parts := strings.Split(token, ".")
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatal(err)
	}
	return time.Unix(claims.Exp, 0)
}

func TestRTMPAddressLimit(t *testing.T) {
	var c ipCounter
	for range rtmpMaxPerAddress {
		if !c.add("1.2.3.4", rtmpMaxPerAddress) {
			t.Fatal("refused below the limit")
		}
	}
	if c.add("1.2.3.4", rtmpMaxPerAddress) {
		t.Fatal("over the limit")
	}
	if !c.add("5.6.7.8", rtmpMaxPerAddress) {
		t.Fatal("one address should not affect another")
	}
	c.done("1.2.3.4")
	if !c.add("1.2.3.4", rtmpMaxPerAddress) {
		t.Fatal("a slot should free up")
	}
}

func TestSlowConnectionsAreDroppedBeforePublishing(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	var published atomic.Bool
	var received atomic.Int64
	conn := &deadlineConn{Conn: server, bytes: &received, setupBy: time.Now().Add(150 * time.Millisecond), published: &published}
	// A byte now and then, well inside the limit for silence.
	go func() {
		for range 20 {
			if _, err := client.Write([]byte{1}); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	started := time.Now()
	buf := make([]byte, 1)
	var err error
	for err == nil {
		_, err = conn.Read(buf)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("still connected after %s", took)
	}
	if !errors.Is(err, errRTMPSetupTooSlow) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	published.Store(true)
}
