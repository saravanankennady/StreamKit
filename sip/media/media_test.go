package media

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

func sine(freq float64, rate, n, offset int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*freq*float64(i+offset)/float64(rate)))
	}
	return out
}

// goertzel measures how much of one frequency is in a signal, as an amplitude.
func goertzel(pcm []int16, freq float64, rate int) float64 {
	var c, s float64
	for i, v := range pcm {
		a := 2 * math.Pi * freq * float64(i) / float64(rate)
		c += float64(v) * math.Cos(a)
		s += float64(v) * math.Sin(a)
	}
	return 2 * math.Hypot(c, s) / float64(len(pcm))
}

func TestG711KnownValues(t *testing.T) {
	// From the tables in ITU-T G.711.
	if got := ulawDecode[0xff]; got != 0 {
		t.Errorf("mu-law 0xff decodes to %d, want 0", got)
	}
	if got := ulawDecode[0x00]; got != -32124 {
		t.Errorf("mu-law 0x00 decodes to %d, want -32124", got)
	}
	if got := ulawDecode[0x80]; got != 32124 {
		t.Errorf("mu-law 0x80 decodes to %d, want 32124", got)
	}
	if got := alawDecode[0xd5]; got != 8 {
		t.Errorf("A-law 0xd5 decodes to %d, want 8", got)
	}
	if got := alawDecode[0x55]; got != -8 {
		t.Errorf("A-law 0x55 decodes to %d, want -8", got)
	}
	if got := alawDecode[0xaa]; got != 32256 {
		t.Errorf("A-law 0xaa decodes to %d, want 32256", got)
	}
}

// Every code decodes and encodes back to itself, for both variants, apart from mu-law's negative zero.
func TestG711RoundTripsEveryCode(t *testing.T) {
	for i := range 256 {
		b := byte(i)
		if got := encodeULaw(ulawDecode[b]); got != b && !(b == 0x7f && got == 0xff) {
			t.Errorf("mu-law %#x came back as %#x", b, got)
		}
		if got := encodeALaw(alawDecode[b]); got != b {
			t.Errorf("A-law %#x came back as %#x", b, got)
		}
	}
}

func TestG711KeepsATone(t *testing.T) {
	for _, name := range []string{CodecPCMU, CodecPCMA} {
		c := Codec{Name: name}
		in := sine(1000, PhoneRate, 8000, 0, 12000)
		wire := make([]byte, len(in))
		c.Encode(in, wire)
		out := make([]int16, len(in))
		c.Decode(wire, out)
		level := goertzel(out, 1000, PhoneRate)
		if level < 11000 || level > 13000 {
			t.Errorf("%s: level %.0f, want about 12000", name, level)
		}
		if other := goertzel(out, 700, PhoneRate); other > level/50 {
			t.Errorf("%s: %.0f at another frequency", name, other)
		}
	}
}

func TestUpsamplerKeepsATone(t *testing.T) {
	var up Upsampler
	in := sine(1000, PhoneRate, PhoneRate, 0, 12000)
	out := make([]int16, len(in)*resampleRatio)
	up.Process(in, out)
	settled := out[2000:]
	level := goertzel(settled, 1000, RoomRate)
	if level < 11000 || level > 13000 {
		t.Errorf("level %.0f, want about 12000", level)
	}
	// Images of the tone around the old sample rate are what a bad filter leaves behind.
	for _, image := range []float64{7000, 9000, 15000, 17000} {
		if v := goertzel(settled, image, RoomRate); v > level/1000 {
			t.Errorf("%.0f at %.0f Hz, for a tone of %.0f", v, image, level)
		}
	}
}

func TestDownsamplerKeepsAToneAndRemovesAliases(t *testing.T) {
	var down Downsampler
	in := sine(1000, RoomRate, RoomRate, 0, 12000)
	out := make([]int16, len(in)/resampleRatio+1)
	n := down.Process(in, out)
	if n != PhoneRate {
		t.Fatalf("got %d samples, want %d", n, PhoneRate)
	}
	level := goertzel(out[400:n], 1000, PhoneRate)
	if level < 11000 || level > 13000 {
		t.Errorf("level %.0f, want about 12000", level)
	}
	// 6 kHz has no place on the phone side. Without a filter it would fold down to 2 kHz.
	var down2 Downsampler
	in = sine(6000, RoomRate, RoomRate, 0, 12000)
	n = down2.Process(in, out)
	if v := goertzel(out[400:n], 2000, PhoneRate); v > 12 {
		t.Errorf("a 6 kHz tone came through as %.1f at 2 kHz", v)
	}
}

// Resampling frame by frame has to give what a single long run gives, or there would be clicks at every frame edge.
func TestResamplersCarryStateAcrossFrames(t *testing.T) {
	in := sine(1000, PhoneRate, 800, 0, 12000)
	var whole, parts Upsampler
	a := make([]int16, 800*resampleRatio)
	whole.Process(in, a)
	b := make([]int16, 800*resampleRatio)
	for i := 0; i < 800; i += PhoneFrame {
		parts.Process(in[i:i+PhoneFrame], b[i*resampleRatio:])
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("upsampler differs at %d: %d vs %d", i, a[i], b[i])
		}
	}
	in48 := sine(1000, RoomRate, 4800, 0, 12000)
	var d1, d2 Downsampler
	x := make([]int16, 900)
	n1 := d1.Process(in48, x)
	y := make([]int16, 900)
	n2 := 0
	for i := 0; i < 4800; i += RoomFrame {
		n2 += d2.Process(in48[i:i+RoomFrame], y[n2:])
	}
	if n1 != n2 {
		t.Fatalf("downsampler made %d samples, then %d", n1, n2)
	}
	for i := range n1 {
		if x[i] != y[i] {
			t.Fatalf("downsampler differs at %d: %d vs %d", i, x[i], y[i])
		}
	}
}

func TestOpusKeepsATone(t *testing.T) {
	enc, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewOpusDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var got []int16
	for i := range 100 {
		pkt, err := enc.Encode(sine(1000, RoomRate, RoomFrame, i*RoomFrame, 8000))
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := dec.Decode(pkt)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, pcm...)
	}
	settled := got[4800:]
	level := goertzel(settled, 1000, RoomRate)
	if level < 5000 {
		t.Errorf("level %.0f, want about 8000", level)
	}
	if other := goertzel(settled, 400, RoomRate); other > level/20 {
		t.Errorf("%.0f at 400 Hz, for a tone of %.0f", other, level)
	}
}

// Many codecs at once, on goroutines with small stacks that have to grow in the middle of a call, is what a worker does. A library that
// took pointers it should not have crashed on exactly this, and only in a real call, so this stays as a check on whichever one is here.
func TestOpusInParallel(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enc, err := NewOpusEncoder()
			if err != nil {
				errs <- err
				return
			}
			defer enc.Close()
			dec, err := NewOpusDecoder()
			if err != nil {
				errs <- err
				return
			}
			defer dec.Close()
			freq := 300 + float64(g)*100
			var got []int16
			for i := range 100 {
				pkt, err := enc.Encode(sine(freq, RoomRate, RoomFrame, i*RoomFrame, 8000))
				if err != nil {
					errs <- err
					return
				}
				pcm, err := dec.Decode(pkt)
				if err != nil {
					errs <- err
					return
				}
				got = append(got, pcm...)
			}
			if level := goertzel(got[4800:], freq, RoomRate); level < 4000 {
				errs <- fmt.Errorf("%.0f Hz came out at %.0f", freq, level)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Garbage must come back as an error, or as sound, and never as a crash: anyone in a room can send it.
func TestOpusDecoderSurvivesGarbage(t *testing.T) {
	dec, err := NewOpusDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	seed := uint32(1)
	next := func() byte {
		seed = seed*1664525 + 1013904223
		return byte(seed >> 24)
	}
	for size := range 400 {
		for range 5 {
			pkt := make([]byte, size)
			for i := range pkt {
				pkt[i] = next()
			}
			if _, err := dec.Decode(pkt); err != nil {
				continue
			}
		}
	}
	// It still works afterwards.
	enc, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	for i := range 20 {
		pkt, err := enc.Encode(sine(1000, RoomRate, RoomFrame, i*RoomFrame, 8000))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Decode(pkt); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkOpusEncode(b *testing.B) {
	enc, err := NewOpusEncoder()
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	in := sine(1000, RoomRate, RoomFrame, 0, 8000)
	for b.Loop() {
		if _, err := enc.Encode(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpusDecode(b *testing.B) {
	enc, err := NewOpusEncoder()
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewOpusDecoder()
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Close()
	pkt, err := enc.Encode(sine(1000, RoomRate, RoomFrame, 0, 8000))
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := dec.Decode(pkt); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMixerSumsTalkers(t *testing.T) {
	m := NewMixer()
	a, b := m.Add(), m.Add()
	var got []int16
	frame := make([]int16, RoomFrame)
	for i := range 50 {
		a.Write(sine(440, RoomRate, RoomFrame, i*RoomFrame, 4000))
		b.Write(sine(1000, RoomRate, RoomFrame, i*RoomFrame, 4000))
		m.Read(frame)
		got = append(got, frame...)
	}
	for _, f := range []float64{440, 1000} {
		if v := goertzel(got[RoomFrame*5:], f, RoomRate); v < 3000 {
			t.Errorf("%.0f Hz is at %.0f, want about 4000", f, v)
		}
	}
	m.Remove(a)
	m.Remove(b)
	m.Read(frame)
	for _, v := range frame {
		if v != 0 {
			t.Fatal("an empty mixer is not silent")
		}
	}
}

func TestMixerClipsInsteadOfWrapping(t *testing.T) {
	m := NewMixer()
	a, b := m.Add(), m.Add()
	loud := make([]int16, RoomFrame)
	for i := range loud {
		loud[i] = 30000
	}
	for range 3 {
		a.Write(loud)
		b.Write(loud)
	}
	frame := make([]int16, RoomFrame)
	m.Read(frame)
	if frame[0] != 32767 {
		t.Errorf("got %d, want 32767", frame[0])
	}
}

// A late talker must not stay late: a burst after a stall gets dropped, or it would delay that person for the rest of the call.
func TestMixerDoesNotKeepOldAudio(t *testing.T) {
	m := NewMixer()
	a := m.Add()
	a.Write(make([]int16, mixLimit+RoomFrame))
	a.m.mu.Lock()
	held := len(a.buf)
	a.m.mu.Unlock()
	if held != mixHold {
		t.Errorf("holds %d samples, want %d", held, mixHold)
	}
}

func TestDTMFReceiverCountsEachPressOnce(t *testing.T) {
	var r DTMFReceiver
	var got []byte
	press := func(digit byte, ts uint32) {
		code := uint8(DigitCode(digit))
		for d := uint16(160); d <= 800; d += 160 {
			if v := r.Receive(ts, DTMFPayload(code, false, d)); v != 0 {
				got = append(got, v)
			}
		}
		for range 3 {
			if v := r.Receive(ts, DTMFPayload(code, true, 960)); v != 0 {
				got = append(got, v)
			}
		}
	}
	press('5', 1000)
	press('5', 3000) // The same digit again, which is a new press.
	press('#', 5000)
	press('A', 5000) // A carrier reusing a timestamp, with a different digit.
	if string(got) != "55#A" {
		t.Errorf("got %q", got)
	}
	if v := r.Receive(9000, []byte{1, 2}); v != 0 {
		t.Errorf("a short packet gave %q", v)
	}
	if v := r.Receive(9000, DTMFPayload(200, false, 160)); v != 0 {
		t.Errorf("an unknown event gave %q", v)
	}
}

const sampleOffer = "v=0\r\no=pjsua 1 1 IN IP4 192.0.2.10\r\ns=pjmedia\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\n" +
	"m=audio 4000 RTP/AVP 9 8 0 101\r\na=rtpmap:9 G722/8000\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n"

func TestAnswerFollowsTheOffersOrder(t *testing.T) {
	d, err := ParseDescription([]byte(sampleOffer))
	if err != nil {
		t.Fatal(err)
	}
	if d.Port != 4000 || d.Addr != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("read %v:%d", d.Addr, d.Port)
	}
	n, err := DefaultPolicy().Answer(d)
	if err != nil {
		t.Fatal(err)
	}
	if n.Codec.Name != CodecPCMA || n.Codec.PT != 8 {
		t.Errorf("chose %+v, want the first it can use", n.Codec)
	}
	if n.DTMF == nil || n.DTMF.PT != 101 {
		t.Errorf("dtmf: %+v", n.DTMF)
	}
	sdp := string(BuildAnswer(netip.MustParseAddr("198.51.100.1"), 20000, d, n, "sendrecv"))
	for _, want := range []string{"c=IN IP4 198.51.100.1", "m=audio 20000 RTP/AVP 8 101", "a=rtpmap:8 PCMA/8000", "a=rtpmap:101 telephone-event/8000", "a=sendrecv"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("answer lacks %q:\n%s", want, sdp)
		}
	}
	if strings.Contains(sdp, "G722") {
		t.Errorf("answer offers something it cannot do:\n%s", sdp)
	}
	again, err := ParseDescription([]byte(sdp))
	if err != nil {
		t.Fatalf("our own answer does not parse: %v", err)
	}
	if again.Port != 20000 || len(again.Codecs) != 1 {
		t.Errorf("read back %+v", again)
	}
}

func TestStaticPayloadTypesNeedNoRtpmap(t *testing.T) {
	d, err := ParseDescription([]byte("v=0\r\no=a 1 1 IN IP4 192.0.2.1\r\ns=-\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\nm=audio 4000 RTP/AVP 0\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Codecs) != 1 || d.Codecs[0].Name != CodecPCMU {
		t.Errorf("codecs: %+v", d.Codecs)
	}
}

func TestNegotiationRefusals(t *testing.T) {
	onlyG722 := "v=0\r\no=a 1 1 IN IP4 192.0.2.1\r\ns=-\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\nm=audio 4000 RTP/AVP 9\r\na=rtpmap:9 G722/8000\r\n"
	d, err := ParseDescription([]byte(onlyG722))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultPolicy().Answer(d); err != ErrNoCodec {
		t.Errorf("got %v", err)
	}
	if _, err := ParseDescription([]byte("not sdp")); err == nil {
		t.Error("garbage parsed")
	}
	if _, err := ParseDescription([]byte("v=0\r\no=a 1 1 IN IP4 192.0.2.1\r\ns=-\r\nt=0 0\r\nm=video 4000 RTP/AVP 96\r\n")); err == nil {
		t.Error("an offer with no audio parsed")
	}
	if _, err := ParseDescription([]byte("v=0\r\no=a 1 1 IN IP4 192.0.2.1\r\ns=-\r\nt=0 0\r\nm=audio 4000 RTP/AVP 0\r\n")); err == nil {
		t.Error("an offer with no address parsed")
	}
}

func cryptoOffer(proto string) string {
	key := "WVNfX19zZW1jdGwgKCkgewkyMjA7fQp9CnVubGVz"
	return "v=0\r\no=a 1 1 IN IP4 192.0.2.1\r\ns=-\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\nm=audio 4000 " + proto + " 0\r\n" +
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + key + "|2^20|1:32\r\n" +
		"a=crypto:2 AES_CM_128_HMAC_SHA1_32 inline:" + key + "\r\n" +
		"a=crypto:3 AES_256_CM_HMAC_SHA1_80 inline:" + key + "\r\n"
}

func TestSDES(t *testing.T) {
	d, err := ParseDescription([]byte(cryptoOffer("RTP/SAVP")))
	if err != nil {
		t.Fatal(err)
	}
	// The first line uses a master key index, which this does not do, and the third a suite it does not.
	if !d.Secure || len(d.Cryptos) != 1 || d.Cryptos[0].Tag != 2 {
		t.Fatalf("read %+v", d)
	}
	if _, err := (Policy{Codecs: []string{CodecPCMU}}).Answer(d); err != ErrNoCrypto {
		t.Errorf("with encryption off, got %v", err)
	}
	n, err := Policy{Codecs: []string{CodecPCMU}, Encryption: EncryptRequire}.Answer(d)
	if err != nil {
		t.Fatal(err)
	}
	if n.Local == nil || n.Remote == nil || n.Local.Tag != 2 || len(n.Local.Key) != 30 {
		t.Fatalf("negotiated %+v", n)
	}
	sdp := string(BuildAnswer(netip.MustParseAddr("198.51.100.1"), 20000, d, n, "sendrecv"))
	if !strings.Contains(sdp, "RTP/SAVP") || !strings.Contains(sdp, "a=crypto:2 AES_CM_128_HMAC_SHA1_32 inline:") {
		t.Errorf("answer:\n%s", sdp)
	}
	plain, err := ParseDescription([]byte(sampleOffer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Policy{Codecs: []string{CodecPCMU}, Encryption: EncryptRequire}).Answer(plain); err != ErrNoCrypto {
		t.Errorf("requiring encryption, a plain offer got %v", err)
	}
	if n, err := (Policy{Codecs: []string{CodecPCMU}, Encryption: EncryptAllow}).Answer(plain); err != nil || n.Local != nil {
		t.Errorf("allowing encryption, a plain offer got %+v, %v", n, err)
	}
}

func TestOfferAndAnswer(t *testing.T) {
	offer, err := BuildOffer(netip.MustParseAddr("198.51.100.1"), 20000, Policy{Codecs: []string{CodecPCMU, CodecPCMA}, Encryption: EncryptRequire})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := ParseDescription(offer.SDP)
	if err != nil {
		t.Fatalf("our own offer does not parse: %v\n%s", err, offer.SDP)
	}
	if !remote.Secure || len(remote.Cryptos) != 2 || len(remote.Codecs) != 2 || remote.DTMF == nil {
		t.Fatalf("read %+v", remote)
	}
	negotiated, err := Policy{Codecs: []string{CodecPCMA}, Encryption: EncryptRequire}.Answer(remote)
	if err != nil {
		t.Fatal(err)
	}
	answerSDP := BuildAnswer(netip.MustParseAddr("192.0.2.1"), 4000, remote, negotiated, "sendrecv")
	answer, err := ParseDescription(answerSDP)
	if err != nil {
		t.Fatal(err)
	}
	result, err := offer.Accept(answer)
	if err != nil {
		t.Fatal(err)
	}
	if result.Codec.Name != CodecPCMA || result.Local == nil || result.Remote == nil {
		t.Errorf("result %+v", result)
	}
	// Each side's sending key is the other's receiving key.
	if string(result.Local.Key) != string(negotiated.Remote.Key) || string(result.Remote.Key) != string(negotiated.Local.Key) {
		t.Error("the two sides do not agree on keys")
	}
}

func TestHoldDirections(t *testing.T) {
	base := "v=0\r\no=a 1 1 IN IP4 192.0.2.1\r\ns=-\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio 4000 RTP/AVP 0\r\na=%s\r\n"
	for _, c := range []struct {
		addr, dir string
		hold      bool
		mirror    string
	}{
		{"192.0.2.1", "sendrecv", false, "sendrecv"},
		{"192.0.2.1", "sendonly", true, "recvonly"},
		{"192.0.2.1", "inactive", true, "inactive"},
		{"192.0.2.1", "recvonly", false, "sendonly"},
		{"0.0.0.0", "sendrecv", true, "sendrecv"},
	} {
		d, err := ParseDescription([]byte(strings.NewReplacer("%s", "").Replace(strings.Replace(strings.Replace(base, "%s", c.addr, 1), "%s", c.dir, 1))))
		if err != nil {
			t.Fatal(err)
		}
		if d.Hold() != c.hold {
			t.Errorf("%s %s: hold is %v", c.addr, c.dir, d.Hold())
		}
		if got := MirrorDirection(d.Direction); got != c.mirror {
			t.Errorf("%s: answers %s, want %s", c.dir, got, c.mirror)
		}
	}
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type capturedPacket struct {
	hdr     rtp.Header
	payload []byte
}

// peer is the other end of a session in these tests: a plain socket, knowing nothing about the code being tested beyond the protocol.
type peer struct {
	conn *net.UDPConn
	got  chan capturedPacket
	recv *srtp.Context
}

func newPeer(t *testing.T) *peer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{conn: conn, got: make(chan capturedPacket, 1024)}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		dec := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var hdr rtp.Header
			offset, err := hdr.Unmarshal(buf[:n])
			if err != nil {
				continue
			}
			payload := buf[offset:n]
			if p.recv != nil {
				plain, err := p.recv.DecryptRTP(dec, buf[:n], &hdr)
				if err != nil {
					continue
				}
				payload = plain[offset:]
			}
			p.got <- capturedPacket{hdr, append([]byte(nil), payload...)}
		}
	}()
	return p
}

func (p *peer) description(t *testing.T) *Description {
	t.Helper()
	return &Description{Addr: netip.MustParseAddr("127.0.0.1"), Port: p.conn.LocalAddr().(*net.UDPAddr).Port, Direction: "sendrecv"}
}

func newTestSession(t *testing.T) *Session {
	t.Helper()
	s, err := NewPorts(net.IPv4(127, 0, 0, 1), 33000, 33999).Open(testLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func sendRTP(t *testing.T, conn *net.UDPConn, to int, h rtp.Header, payload []byte) {
	t.Helper()
	h.Version = 2
	raw, err := (&rtp.Packet{Header: h, Payload: payload}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: to}); err != nil {
		t.Fatal(err)
	}
}

func TestPortsAreEvenAndPaired(t *testing.T) {
	ports := NewPorts(net.IPv4(127, 0, 0, 1), 33101, 33106)
	var sessions []*Session
	defer func() {
		for _, s := range sessions {
			s.Close()
		}
	}()
	for range 2 {
		s, err := ports.Open(testLog())
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
		if s.Port()%2 != 0 {
			t.Errorf("RTP port %d is odd", s.Port())
		}
	}
	if _, err := ports.Open(testLog()); err != ErrNoPorts {
		t.Errorf("with the range used up, got %v", err)
	}
}

func TestSessionReceivesAudioAndFillsGaps(t *testing.T) {
	s := newTestSession(t)
	p := newPeer(t)
	var mu sync.Mutex
	total := 0
	s.OnAudio = func(pcm []int16) {
		mu.Lock()
		total += len(pcm)
		mu.Unlock()
	}
	if err := s.Configure(p.description(t), netip.MustParseAddr("127.0.0.1"), &Negotiated{Codec: Codec{CodecPCMU, 0}}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	payload := make([]byte, PhoneFrame)
	// Frame 3 is lost, a late copy of frame 1 arrives after that, and frame 4 shows how much time has gone by.
	for _, f := range []uint16{1, 2, 4, 1} {
		sendRTP(t, p.conn, s.Port(), rtp.Header{SequenceNumber: f, Timestamp: uint32(f) * PhoneFrame, SSRC: 7, PayloadType: 0}, payload)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, _, _ := s.Stats(); r == 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if total != 4*PhoneFrame {
		t.Errorf("got %d samples, want %d: three packets and one filled gap", total, 4*PhoneFrame)
	}
}

// Finding a call's port must not be enough to talk into it, or to redirect what it sends.
func TestSessionRefusesStrangers(t *testing.T) {
	s := newTestSession(t)
	p := newPeer(t)
	heard := make(chan int, 16)
	s.OnAudio = func(pcm []int16) { heard <- len(pcm) }
	// The call's peer is somewhere else, so the local socket here is a stranger.
	d := &Description{Addr: netip.MustParseAddr("192.0.2.77"), Port: 4000, Direction: "sendrecv"}
	if err := s.Configure(d, netip.MustParseAddr("192.0.2.77"), &Negotiated{Codec: Codec{CodecPCMU, 0}}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	sendRTP(t, p.conn, s.Port(), rtp.Header{SequenceNumber: 1, Timestamp: 160, SSRC: 7, PayloadType: 0}, make([]byte, PhoneFrame))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, rejected := s.Stats(); rejected > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r, _, rejected := s.Stats(); r != 0 || rejected != 1 {
		t.Errorf("received %d, rejected %d", r, rejected)
	}
	select {
	case <-heard:
		t.Error("a stranger's audio got through")
	default:
	}
}

func TestSessionFollowsNAT(t *testing.T) {
	s := newTestSession(t)
	p := newPeer(t)
	// The SDP names a port nothing is listening on, as a phone behind NAT would. Its first packet shows the real one.
	d := &Description{Addr: netip.MustParseAddr("127.0.0.1"), Port: 9, Direction: "sendrecv"}
	if err := s.Configure(d, netip.MustParseAddr("127.0.0.1"), &Negotiated{Codec: Codec{CodecPCMU, 0}}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	sendRTP(t, p.conn, s.Port(), rtp.Header{SequenceNumber: 1, Timestamp: 160, SSRC: 7, PayloadType: 0}, make([]byte, PhoneFrame))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !s.GotPacket() {
		time.Sleep(5 * time.Millisecond)
	}
	stop := make(chan struct{})
	defer close(stop)
	go s.RunSender(stop, func(dst []int16) { clear(dst) })
	select {
	case <-p.got:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing came back to the address the phone sent from")
	}
}

func TestSessionSendsAFrameEvery20ms(t *testing.T) {
	s := newTestSession(t)
	p := newPeer(t)
	if err := s.Configure(p.description(t), netip.MustParseAddr("127.0.0.1"), &Negotiated{Codec: Codec{CodecPCMU, 0}}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go s.RunSender(stop, func(dst []int16) { clear(dst) })
	start := time.Now()
	var first, last capturedPacket
	const want = 50
	for i := range want {
		select {
		case pkt := <-p.got:
			if i == 0 {
				first = pkt
			}
			last = pkt
			if len(pkt.payload) != PhoneFrame {
				t.Fatalf("payload of %d bytes", len(pkt.payload))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("packets stopped")
		}
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 1400*time.Millisecond {
		t.Errorf("%d frames took %v", want, d)
	}
	if got := uint16(last.hdr.SequenceNumber - first.hdr.SequenceNumber); got != want-1 {
		t.Errorf("sequence went up by %d", got)
	}
	if got := last.hdr.Timestamp - first.hdr.Timestamp; got != (want-1)*PhoneFrame {
		t.Errorf("timestamp went up by %d", got)
	}
}

func TestSessionSendsDTMF(t *testing.T) {
	s := newTestSession(t)
	p := newPeer(t)
	dtmf := Codec{dtmfName, 101}
	if err := s.Configure(p.description(t), netip.MustParseAddr("127.0.0.1"), &Negotiated{Codec: Codec{CodecPCMU, 0}, DTMF: &dtmf}); err != nil {
		t.Fatal(err)
	}
	s.QueueDTMF("1w#")
	stop := make(chan struct{})
	defer close(stop)
	go s.RunSender(stop, func(dst []int16) { clear(dst) })
	var r DTMFReceiver
	var digits []byte
	ends := map[uint32]int{}
	markers := 0
	var timestamps []uint32
	deadline := time.After(5 * time.Second)
	for !(len(digits) == 2 && ends[timestamps[1]] >= dtmfEndRepeats) {
		select {
		case pkt := <-p.got:
			if pkt.hdr.PayloadType != 101 {
				continue
			}
			if pkt.hdr.Marker {
				markers++
			}
			if d := r.Receive(pkt.hdr.Timestamp, pkt.payload); d != 0 {
				digits = append(digits, d)
				timestamps = append(timestamps, pkt.hdr.Timestamp)
			}
			if ev, ok := ParseDTMF(pkt.payload); ok && ev.End {
				ends[pkt.hdr.Timestamp]++
			}
		case <-deadline:
			t.Fatalf("got digits %q, ends %v", digits, ends)
		}
	}
	if string(digits) != "1#" {
		t.Errorf("got %q", digits)
	}
	if markers != 2 {
		t.Errorf("%d marker bits, want one per digit", markers)
	}
	if ends[timestamps[0]] != dtmfEndRepeats {
		t.Errorf("%d end packets, want %d", ends[timestamps[0]], dtmfEndRepeats)
	}
	// The pause is half a second, which is 4000 timestamp units at the phone's rate.
	if gap := timestamps[1] - timestamps[0]; gap < 4000 {
		t.Errorf("the digits are only %d apart", gap)
	}
}

// SRTP checked against the library directly: what a peer sends with its key is heard, and tampering is not.
func TestSessionSRTP(t *testing.T) {
	s := newTestSession(t)
	p := newPeer(t)
	local, err := newCrypto(1, cryptoSuites[0])
	if err != nil {
		t.Fatal(err)
	}
	remote, err := newCrypto(1, cryptoSuites[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Configure(p.description(t), netip.MustParseAddr("127.0.0.1"), &Negotiated{Codec: Codec{CodecPCMU, 0}, Local: &local, Remote: &remote}); err != nil {
		t.Fatal(err)
	}
	if !s.Encrypted() {
		t.Error("the session does not say it is encrypted")
	}
	heard := make(chan []int16, 16)
	s.OnAudio = func(pcm []int16) { heard <- append([]int16(nil), pcm...) }
	s.Start()
	sendCtx, err := newSRTPContext(&remote)
	if err != nil {
		t.Fatal(err)
	}
	p.recv, err = newSRTPContext(&local)
	if err != nil {
		t.Fatal(err)
	}
	send := func(seq uint16, tamper bool) {
		hdr := rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * PhoneFrame, SSRC: 9, PayloadType: 0}
		raw, err := (&rtp.Packet{Header: hdr, Payload: make([]byte, PhoneFrame)}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		out, err := sendCtx.EncryptRTP(nil, raw, &hdr)
		if err != nil {
			t.Fatal(err)
		}
		if tamper {
			out[20] ^= 0xff
		}
		if _, err := p.conn.WriteToUDP(out, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.Port()}); err != nil {
			t.Fatal(err)
		}
	}
	send(1, false)
	select {
	case <-heard:
	case <-time.After(2 * time.Second):
		t.Fatal("a good packet was not heard")
	}
	send(2, true)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, rejected := s.Stats(); rejected == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, rejected := s.Stats(); rejected != 1 {
		t.Errorf("rejected %d packets, want the tampered one", rejected)
	}
	stop := make(chan struct{})
	defer close(stop)
	go s.RunSender(stop, func(dst []int16) { clear(dst) })
	select {
	case pkt := <-p.got:
		if len(pkt.payload) != PhoneFrame {
			t.Errorf("decrypted %d bytes", len(pkt.payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("what the session sent could not be decrypted by the peer's key")
	}
}

func TestDialtones(t *testing.T) {
	if NewDialtone(DialtoneDisabled, RoomRate) != nil {
		t.Error("a disabled tone is not nil")
	}
	for name, c := range map[string]struct {
		d    Dialtone
		freq float64
	}{"ETSI": {DialtoneETSI, 425}, "US": {DialtoneUS, 440}, "UK": {DialtoneUK, 400}, "JP": {DialtoneJP, 400}, "AU": {DialtoneAU, 425}, "auto": {DialtoneAuto, 425}, "unknown": {Dialtone(99), 425}} {
		tone := NewDialtone(c.d, RoomRate)
		pcm := make([]int16, RoomRate/4) // A quarter of a second, which is in the first burst of them all.
		tone.Read(pcm)
		if v := goertzel(pcm, c.freq, RoomRate); v < 2000 {
			t.Errorf("%s: %.0f at %.0f Hz", name, v, c.freq)
		}
	}
	// Off periods are silent.
	tone := NewDialtone(DialtoneETSI, RoomRate)
	pcm := make([]int16, RoomRate*2)
	tone.Read(pcm)
	for _, v := range pcm[RoomRate+100:] {
		if v != 0 {
			t.Fatal("the off period is not silent")
		}
	}
}
