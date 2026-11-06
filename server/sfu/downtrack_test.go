package sfu

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type sentPacket struct {
	hdr     rtp.Header
	payload []byte
}

type fakeWriter struct {
	mu   sync.Mutex
	sent []sentPacket
}

func (w *fakeWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, sentPacket{*h, append([]byte(nil), payload...)})
	return len(payload), nil
}

func (w *fakeWriter) Write(b []byte) (int, error) { return len(b), nil }

type fakeContext struct {
	codecs []webrtc.RTPCodecParameters
	ext    []webrtc.RTPHeaderExtensionParameter
	w      *fakeWriter
}

func (c fakeContext) CodecParameters() []webrtc.RTPCodecParameters           { return c.codecs }
func (c fakeContext) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter { return c.ext }
func (c fakeContext) SSRC() webrtc.SSRC                                      { return 7777 }
func (c fakeContext) SSRCRetransmission() webrtc.SSRC                        { return 0 }
func (c fakeContext) SSRCForwardErrorCorrection() webrtc.SSRC                { return 0 }
func (c fakeContext) WriteStream() webrtc.TrackLocalWriter                   { return c.w }
func (c fakeContext) ID() string                                             { return "ctx" }
func (c fakeContext) RTCPReader() interceptor.RTCPReader                     { return nil }

var vp8Codec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
	PayloadType:        96,
}

func newVideoDownTrack(t *testing.T) (*DownTrack, *fakeWriter) {
	t.Helper()
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp8Codec, RequestKeyframe: func(uint32) {}})
	t.Cleanup(r.Close)
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	w := &fakeWriter{}
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{vp8Codec}, w: w}); err != nil {
		t.Fatal(err)
	}
	return d, w
}

// vp8Payload builds a payload with a 15-bit picture ID and TL0PICIDX. key marks a keyframe's first packet.
func vp8Payload(pic uint16, tl0 uint8, key bool) []byte {
	head := byte(0x11) // inter frame
	if key {
		head = 0x10
	}
	return []byte{0x90, 0xc0, 0x80 | byte(pic>>8), byte(pic), tl0, head, 0xab}
}

func forward(d *DownTrack, layer int32, seq uint16, ts uint32, key bool, pic uint16) {
	p := vp8Payload(pic, uint8(pic), key)
	d.Forward(&Packet{Header: &rtp.Header{SequenceNumber: seq, Timestamp: ts, SSRC: uint32(layer + 1)}, Payload: p, Layer: layer, KeyframeStart: key})
}

func TestDownTrackForwardsNothingBeforeKeyframe(t *testing.T) {
	d, w := newVideoDownTrack(t)
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 100, 1000, false, 5)
	forward(d, LayerHigh, 101, 1000, false, 5)
	if len(w.sent) != 0 {
		t.Fatalf("sent %d packets a decoder could not use", len(w.sent))
	}
	forward(d, LayerHigh, 102, 4000, true, 6)
	forward(d, LayerHigh, 103, 4000, false, 6)
	if len(w.sent) != 2 {
		t.Fatalf("sent = %d", len(w.sent))
	}
	got := w.sent[0].hdr
	if got.SSRC != 7777 || got.PayloadType != 96 {
		t.Fatalf("subscriber's SSRC and payload type not applied: %+v", got)
	}
}

func TestDownTrackIgnoresOtherLayers(t *testing.T) {
	d, w := newVideoDownTrack(t)
	d.SetTarget(LayerLow)
	forward(d, LayerHigh, 900, 1000, true, 1)
	forward(d, LayerMedium, 800, 1000, true, 1)
	if len(w.sent) != 0 {
		t.Fatal("forwarded a layer that was not selected")
	}
	forward(d, LayerLow, 100, 1000, true, 1)
	forward(d, LayerHigh, 901, 2000, false, 2)
	if len(w.sent) != 1 {
		t.Fatalf("sent = %d", len(w.sent))
	}
}

func TestDownTrackSwitchesOnlyAtKeyframeAndStaysContinuous(t *testing.T) {
	d, w := newVideoDownTrack(t)
	d.SetTarget(LayerLow)
	forward(d, LayerLow, 100, 1000, true, 10)
	forward(d, LayerLow, 101, 4000, false, 11)

	// Move up: the old layer keeps flowing until the new one has a keyframe, so video never freezes.
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 5000, 777000, false, 3000)
	forward(d, LayerLow, 102, 7000, false, 12)
	if d.Current() != LayerLow {
		t.Fatal("switched before a keyframe")
	}
	forward(d, LayerHigh, 5001, 780000, true, 3001)
	forward(d, LayerHigh, 5002, 783000, false, 3002)
	forward(d, LayerLow, 103, 10000, false, 13)
	if d.Current() != LayerHigh {
		t.Fatal("did not switch at the keyframe")
	}
	if len(w.sent) != 5 {
		t.Fatalf("sent = %d, want 5", len(w.sent))
	}

	for i := 1; i < len(w.sent); i++ {
		if w.sent[i].hdr.SequenceNumber != w.sent[i-1].hdr.SequenceNumber+1 {
			t.Fatalf("sequence gap at %d: %d then %d", i, w.sent[i-1].hdr.SequenceNumber, w.sent[i].hdr.SequenceNumber)
		}
	}
	if ts := w.sent[3].hdr.Timestamp; int32(ts-w.sent[2].hdr.Timestamp) <= 0 || ts-w.sent[2].hdr.Timestamp > 90000 {
		t.Fatalf("timestamp jumped across the switch: %d then %d", w.sent[2].hdr.Timestamp, ts)
	}
	for i := 1; i < len(w.sent); i++ {
		a, _ := parseVP8Descriptor(w.sent[i-1].payload)
		b, _ := parseVP8Descriptor(w.sent[i].payload)
		if b.picID != (a.picID+1)&0x7fff {
			t.Fatalf("picture ID not continuous at %d: %d then %d", i, a.picID, b.picID)
		}
	}
}

func TestDownTrackMungingLeavesTheSharedPayloadAlone(t *testing.T) {
	d, _ := newVideoDownTrack(t)
	d.SetTarget(LayerLow)
	forward(d, LayerLow, 100, 1000, true, 10)
	d.SetTarget(LayerHigh)

	shared := vp8Payload(3000, 200, true)
	original := append([]byte(nil), shared...)
	d.Forward(&Packet{Header: &rtp.Header{SequenceNumber: 500, Timestamp: 9000}, Payload: shared, Layer: LayerHigh, KeyframeStart: true})
	for i := range shared {
		if shared[i] != original[i] {
			t.Fatal("rewrote a payload other subscribers also read")
		}
	}
}

func TestDownTrackPausesAndResumesAtKeyframe(t *testing.T) {
	d, w := newVideoDownTrack(t)
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 100, 1000, true, 1)
	d.SetTarget(LayerNone)
	forward(d, LayerHigh, 101, 4000, false, 2)
	if len(w.sent) != 1 || d.Current() != LayerNone {
		t.Fatalf("still forwarding while paused: sent=%d current=%d", len(w.sent), d.Current())
	}
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 102, 7000, false, 3)
	if len(w.sent) != 1 {
		t.Fatal("resumed mid-GOP: the decoder has lost its references")
	}
	forward(d, LayerHigh, 103, 10000, true, 4)
	if len(w.sent) != 2 {
		t.Fatal("did not resume at the keyframe")
	}
	if w.sent[1].hdr.SequenceNumber != w.sent[0].hdr.SequenceNumber+1 {
		t.Fatal("packets skipped during the pause left a sequence gap")
	}
}

func TestDownTrackBindSurvivesUnsupportedCodec(t *testing.T) {
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp8Codec})
	t.Cleanup(r.Close)
	notified := make(chan struct{})
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r, OnUnsupported: func() { close(notified) }})
	h264 := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, PayloadType: 102}
	w := &fakeWriter{}
	// An error from Bind fails SetRemoteDescription for every track on the connection.
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{h264}, w: w}); err != nil {
		t.Fatalf("Bind returned %v", err)
	}
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("OnUnsupported did not run")
	}
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 1, 1, true, 1)
	if len(w.sent) != 0 {
		t.Fatal("sent VP8 labelled as another codec")
	}
	if _, err := d.Bind(fakeContext{w: w}); err != nil {
		t.Fatalf("Bind with no codecs returned %v", err)
	}
}

func TestAudioDownTrackStripsREDWhenNotNegotiated(t *testing.T) {
	red := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: MimeTypeRED, ClockRate: 48000}, PayloadType: 63}
	opus := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000}, PayloadType: 111}
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeAudio, Codec: red})
	t.Cleanup(r.Close)
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	w := &fakeWriter{}
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{opus}, w: w}); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0x80 | 111, 0, 0, 2, 111, 0xaa, 0xbb, 0x01, 0x02, 0x03}
	d.Forward(&Packet{Header: &rtp.Header{SequenceNumber: 9, Timestamp: 960}, Payload: payload})
	if len(w.sent) != 1 || w.sent[0].hdr.PayloadType != 111 {
		t.Fatalf("sent = %+v", w.sent)
	}
	if got := w.sent[0].payload; len(got) != 3 || got[0] != 0x01 {
		t.Fatalf("payload = %x, want only the primary encoding", got)
	}
}

func TestFanoutParallelDeliversToAll(t *testing.T) {
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeAudio, Codec: webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000}, PayloadType: 111,
	}})
	t.Cleanup(r.Close)
	const n = parallelFanoutThreshold*3 + 1
	writers := make([]*fakeWriter, n)
	for i := range writers {
		d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
		writers[i] = &fakeWriter{}
		if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{r.Codec()}, w: writers[i]}); err != nil {
			t.Fatal(err)
		}
		r.AddDownTrack(d)
	}
	r.fanout(&Packet{Header: &rtp.Header{SequenceNumber: 1, Timestamp: 1}, Payload: []byte{1, 2, 3}})
	for i, w := range writers {
		if len(w.sent) != 1 {
			t.Fatalf("subscriber %d got %d packets", i, len(w.sent))
		}
	}
}

func TestDownTrackLimitsPlayoutDelayOnlyWhenAsked(t *testing.T) {
	const id = 9
	send := func(limit time.Duration) []byte {
		r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp8Codec, RequestKeyframe: func(uint32) {}})
		t.Cleanup(r.Close)
		d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r, MaxPlayoutDelay: limit})
		w := &fakeWriter{}
		ctx := fakeContext{codecs: []webrtc.RTPCodecParameters{vp8Codec}, w: w, ext: []webrtc.RTPHeaderExtensionParameter{{URI: PlayoutDelayURI, ID: id}}}
		if _, err := d.Bind(ctx); err != nil {
			t.Fatal(err)
		}
		d.SetTarget(LayerHigh)
		forward(d, LayerHigh, 100, 1000, true, 1)
		if len(w.sent) != 1 {
			t.Fatalf("sent %d packets", len(w.sent))
		}
		return w.sent[0].hdr.GetExtension(id)
	}
	if got := send(100 * time.Millisecond); !bytes.Equal(got, []byte{0, 0, 10}) {
		t.Fatalf("minimum 0 and maximum 100 ms encode as %x", got)
	}
	if got := send(5 * time.Second); !bytes.Equal(got, []byte{0, 0x01, 0xf4}) {
		t.Fatalf("maximum 5 s encodes as %x", got)
	}
	if got := send(0); got != nil {
		t.Fatalf("sent %x with no limit asked for", got)
	}
}
