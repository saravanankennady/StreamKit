package sfu

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// resendWriter counts what goes out again, as opposed to what is forwarded the first time.
type resendWriter struct {
	fakeWriter
	resent atomic.Int32
	last   atomic.Pointer[rtp.Packet]
}

func (w *resendWriter) Write(b []byte) (int, error) {
	var p rtp.Packet
	if err := p.Unmarshal(b); err != nil {
		return 0, err
	}
	w.last.Store(&p)
	w.resent.Add(1)
	return len(b), nil
}

func newResendTrack(t *testing.T) (*DownTrack, *resendWriter) {
	t.Helper()
	d, _ := newVideoDownTrack(t)
	w := &resendWriter{}
	d.writer = w
	d.SetTarget(LayerHigh)
	for i := range 100 {
		forward(d, LayerHigh, uint16(100+i), 1000, i == 0, 5)
	}
	if len(w.sent) != 100 {
		t.Fatalf("forwarded %d of 100", len(w.sent))
	}
	return d, w
}

func nackFor(seq uint16) []rtcp.Packet {
	return []rtcp.Packet{&rtcp.TransportLayerNack{Nacks: []rtcp.NackPair{{PacketID: seq}}}}
}

// age makes every remembered packet look as if it was sent long enough ago to send again.
func age(d *DownTrack) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.resend.history {
		d.resend.history[i].lastSent -= int64(time.Second)
	}
}

func TestNackIsAnsweredWithThePacketAsItWasSent(t *testing.T) {
	d, w := newResendTrack(t)
	want := w.sent[10]
	age(d)
	d.HandleRTCP(nackFor(want.hdr.SequenceNumber))
	if got := w.resent.Load(); got != 1 {
		t.Fatalf("sent again %d times", got)
	}
	got := w.last.Load()
	if got.SequenceNumber != want.hdr.SequenceNumber || got.SSRC != want.hdr.SSRC || got.Timestamp != want.hdr.Timestamp {
		t.Fatalf("resent %+v, want %+v", got.Header, want.hdr)
	}
	if string(got.Payload) != string(want.payload) {
		t.Fatal("payload differs from what was sent")
	}
}

func TestNackIsNotAnsweredTooSoonOrTooOften(t *testing.T) {
	d, w := newResendTrack(t)
	seq := w.sent[10].hdr.SequenceNumber
	d.HandleRTCP(nackFor(seq))
	d.HandleRTCP(nackFor(seq))
	if got := w.resent.Load(); got != 1 {
		t.Fatalf("answered %d times, want the first request answered and an immediate repeat ignored", got)
	}
	for range 10 {
		age(d)
		d.HandleRTCP(nackFor(seq))
	}
	if got := w.resent.Load(); got != maxResends {
		t.Fatalf("resent %d times, want at most %d", got, maxResends)
	}
}

func TestNackForAPacketNeverSentIsIgnored(t *testing.T) {
	d, w := newResendTrack(t)
	age(d)
	d.HandleRTCP(nackFor(w.sent[99].hdr.SequenceNumber + 50))
	if got := w.resent.Load(); got != 0 {
		t.Fatalf("sent something for a packet that does not exist, %d times", got)
	}
}

// After a keyframe request the browser decodes nothing until the next keyframe, so old packets are of no use to it.
func TestKeyframeRequestStopsRetransmissionUntilTheKeyframe(t *testing.T) {
	d, w := newResendTrack(t)
	age(d)
	d.HandleRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{}})
	d.HandleRTCP(nackFor(w.sent[10].hdr.SequenceNumber))
	if got := w.resent.Load(); got != 0 {
		t.Fatalf("sent old packets again after a keyframe request, %d times", got)
	}
	forward(d, LayerHigh, 200, 5000, true, 6)
	age(d)
	d.HandleRTCP(nackFor(w.sent[100].hdr.SequenceNumber))
	if got := w.resent.Load(); got != 1 {
		t.Fatalf("after the keyframe, sent again %d times, want 1", got)
	}
}

// Once a keyframe is on its way, nothing before it can matter, even though the browser may go on asking.
func TestNothingFromBeforeTheLatestKeyframeIsSentAgain(t *testing.T) {
	d, w := newResendTrack(t)
	old := w.sent[10].hdr.SequenceNumber
	forward(d, LayerHigh, 200, 5000, true, 6)
	forward(d, LayerHigh, 201, 5000, false, 6)
	age(d)
	d.HandleRTCP(nackFor(old))
	if got := w.resent.Load(); got != 0 {
		t.Fatalf("sent a packet from before the keyframe, %d times", got)
	}
	d.HandleRTCP(nackFor(w.sent[len(w.sent)-1].hdr.SequenceNumber))
	if got := w.resent.Load(); got != 1 {
		t.Fatalf("a packet after the keyframe was sent %d times, want 1", got)
	}
}

func TestLeavingALayerStopsRetransmittingIt(t *testing.T) {
	d, w := newResendTrack(t)
	age(d)
	d.SetTarget(LayerLow)
	d.HandleRTCP(nackFor(w.sent[10].hdr.SequenceNumber))
	if got := w.resent.Load(); got != 0 {
		t.Fatalf("kept retransmitting a layer the link could not carry, %d times", got)
	}
}

// A browser that has lost a lot asks for all of it. What goes out again must not be more than a share of what is forwarded.
func TestRetransmissionsAreHeldToAShareOfWhatIsForwarded(t *testing.T) {
	d, w := newResendTrack(t)
	age(d)
	var forwarded, resentBytes int
	for _, p := range w.sent {
		forwarded += p.hdr.MarshalSize() + len(p.payload)
	}
	// Twice over, as a browser would ask again.
	for range 2 {
		for _, p := range w.sent {
			d.HandleRTCP(nackFor(p.hdr.SequenceNumber))
		}
		age(d)
	}
	resentBytes = int(w.resent.Load()) * (w.sent[0].hdr.MarshalSize() + len(w.sent[0].payload))
	if limit := float64(forwarded) * resendShare; float64(resentBytes) > limit+float64(len(w.sent[0].payload)+w.sent[0].hdr.MarshalSize()) {
		t.Fatalf("resent %d bytes for %d forwarded", resentBytes, forwarded)
	}
	if w.resent.Load() == 0 {
		t.Fatal("nothing was sent again")
	}
}

// vp8Temporal builds a frame's first packet with a temporal layer, as Chrome's simulcast encoder sends it.
func vp8Temporal(pic uint16, tid uint8, sync bool) []byte {
	t := tid << 6
	if sync {
		t |= 0x20
	}
	return []byte{0x90, 0xa0, 0x80 | byte(pic>>8), byte(pic), t, 0x11, 0xab}
}

func forwardTemporal(d *DownTrack, seq uint16, pic uint16, tid uint8, sync bool) {
	d.Forward(&Packet{Header: &rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 3, Marker: true}, Payload: vp8Temporal(pic, tid, sync), Layer: LayerHigh})
}

func TestAThinLinkDropsTemporalLayersWithoutLeavingAGap(t *testing.T) {
	d, w := newVideoDownTrack(t)
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 100, 1000, true, 10)
	d.SetTemporalBudget(0)
	forwardTemporal(d, 101, 11, 2, false) // dropped
	forwardTemporal(d, 102, 12, 1, false) // dropped
	forwardTemporal(d, 103, 13, 0, false)
	forwardTemporal(d, 104, 14, 2, false) // dropped
	forwardTemporal(d, 105, 15, 0, false)
	if len(w.sent) != 3 {
		t.Fatalf("forwarded %d packets, want the keyframe and 2 base layer frames", len(w.sent))
	}
	for i := 1; i < len(w.sent); i++ {
		if w.sent[i].hdr.SequenceNumber != w.sent[i-1].hdr.SequenceNumber+1 {
			t.Fatalf("sequence gap: %d then %d. The browser would ask for what was dropped", w.sent[i-1].hdr.SequenceNumber, w.sent[i].hdr.SequenceNumber)
		}
		before, _ := parseVP8Descriptor(w.sent[i-1].payload)
		after, _ := parseVP8Descriptor(w.sent[i].payload)
		if after.picID != before.picID+1 {
			t.Fatalf("picture ID gap: %d then %d. Chrome would wait for the missing frame", before.picID, after.picID)
		}
	}
}

// A frame above the base layer can depend on earlier frames of its own layer, so a subscriber can only start on one
// that says it does not.
func TestTemporalLayerComesBackOnASyncFrame(t *testing.T) {
	d, w := newVideoDownTrack(t)
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 100, 1000, true, 10)
	d.SetTemporalBudget(0)
	forwardTemporal(d, 101, 11, 1, false)
	d.SetTemporalBudget(maxTemporalLayer)
	forwardTemporal(d, 102, 12, 1, false)
	if len(w.sent) != 1 {
		t.Fatalf("forwarded a frame that may depend on one the subscriber never got, %d packets", len(w.sent))
	}
	forwardTemporal(d, 103, 13, 1, true)
	forwardTemporal(d, 104, 14, 1, false)
	if len(w.sent) != 3 {
		t.Fatalf("forwarded %d packets after a sync frame, want 3", len(w.sent))
	}
}

func TestKeyframeRequestsAreSpacedOutOnAShortLink(t *testing.T) {
	var k keyframeSpacing
	second := int64(time.Second)
	// A browser that has decoded nothing asks every 3 seconds.
	var passed []int64
	for at := int64(0); at < 60; at += 3 {
		if k.allow(at*second+1, true) {
			passed = append(passed, at)
		}
	}
	if len(passed) >= 15 || len(passed) < 5 {
		t.Fatalf("passed on at %v", passed)
	}
	if last := passed[len(passed)-1] - passed[len(passed)-2]; last < 8 {
		t.Fatalf("still asking every %d s after a minute: %v", last, passed)
	}

	// Once they stop for a while, the next is new trouble.
	if !k.allow(90*second, true) {
		t.Fatal("a request long after the last was held back")
	}
	if k.wait != 0 {
		t.Fatalf("wait = %v after a long gap", k.wait)
	}

	// On a link with no sign of trouble, nothing is held back.
	var healthy keyframeSpacing
	for at := int64(0); at < 10; at++ {
		if !healthy.allow(at*second, false) {
			t.Fatal("held back a request on a link with no sign of trouble")
		}
	}
}

func TestResendIntervalFollowsTheRoundTrip(t *testing.T) {
	if got := resendInterval(20 * time.Millisecond); got != 40*time.Millisecond {
		t.Fatalf("interval = %v at 20 ms", got)
	}
	if got := resendInterval(500 * time.Millisecond); got != resendMaxInterval {
		t.Fatalf("interval = %v at 500 ms", got)
	}
	if got := resendInterval(0); got != resendMaxInterval {
		t.Fatalf("interval = %v with no round trip known", got)
	}
}
