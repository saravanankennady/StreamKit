package sfu

import (
	"github.com/pion/rtp"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

const clock90k = 90000

func TestRewriterContinuousAcrossSwitch(t *testing.T) {
	w := rewriter{clockRate: clock90k}
	base := time.Now().UnixNano()

	w.rebase(1000, 500000, base)
	for i := uint16(0); i < 5; i++ {
		seq, ts, ok := w.rewrite(1000+i, 500000+uint32(i)*3000, base)
		if !ok || seq != 1000+i || ts != 500000+uint32(i)*3000 {
			t.Fatalf("first stream should pass unchanged: %d %d %v", seq, ts, ok)
		}
	}

	// Switch 100ms later to a layer with unrelated numbering.
	later := base + int64(100*time.Millisecond)
	w.rebase(60000, 42, later)
	seq, ts, ok := w.rewrite(60000, 42, later)
	if !ok {
		t.Fatal("keyframe at the switch point was dropped")
	}
	if seq != 1005 {
		t.Fatalf("seq = %d, want 1005 (no gap, so the receiver sends no NACK)", seq)
	}
	wantTS := uint32(500000 + 4*3000 + 9000) // 100ms at 90kHz past the last timestamp
	if ts != wantTS {
		t.Fatalf("ts = %d, want %d", ts, wantTS)
	}

	// Packets from before the switch never reached this subscriber's decoder.
	if _, _, ok := w.rewrite(59999, 41, later); ok {
		t.Fatal("packet from before the switch should be dropped")
	}

	// A late packet within the new stream keeps its relative position.
	seq2, _, ok := w.rewrite(60002, 42, later)
	if !ok || seq2 != 1007 {
		t.Fatalf("seq = %d, ok = %v", seq2, ok)
	}
	seq1, _, ok := w.rewrite(60001, 42, later)
	if !ok || seq1 != 1006 {
		t.Fatalf("reordered packet: seq = %d, ok = %v", seq1, ok)
	}
}

func TestRewriterSequenceWrap(t *testing.T) {
	w := rewriter{clockRate: clock90k}
	now := time.Now().UnixNano()
	w.rebase(65534, 1000, now)
	for i, want := range []uint16{65534, 65535, 0, 1} {
		seq, _, ok := w.rewrite(want, 1000, now)
		if !ok || seq != want {
			t.Fatalf("step %d: seq = %d, ok = %v", i, seq, ok)
		}
	}
	if int16(w.lastSeq-1) != 0 {
		t.Fatalf("lastSeq = %d", w.lastSeq)
	}
	// Switch right after the wrap.
	w.rebase(100, 2000, now)
	seq, _, ok := w.rewrite(100, 2000, now)
	if !ok || seq != 2 {
		t.Fatalf("seq = %d, ok = %v", seq, ok)
	}
}

func TestRewriterTimestampNeverStalls(t *testing.T) {
	w := rewriter{clockRate: clock90k}
	now := time.Now().UnixNano()
	w.rebase(1, 1000, now)
	w.rewrite(1, 1000, now)
	w.rebase(50, 7777, now) // same instant
	_, ts, _ := w.rewrite(50, 7777, now)
	if ts != 1001 {
		t.Fatalf("ts = %d, want 1001: a new frame must not repeat the previous timestamp", ts)
	}
}

func TestMatchCodec(t *testing.T) {
	vp8 := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/VP8", ClockRate: clock90k}, PayloadType: 96}
	h264a := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/H264", SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, PayloadType: 102}
	h264b := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/H264", SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"}, PayloadType: 104}
	opus := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/opus"}, PayloadType: 111}
	red := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/red"}, PayloadType: 63}

	got, _, ok := matchCodec(vp8, []webrtc.RTPCodecParameters{h264a, vp8})
	if !ok || got.PayloadType != 96 {
		t.Fatalf("vp8: %+v, %v", got, ok)
	}
	want := h264a
	want.SDPFmtpLine = "packetization-mode=1;profile-level-id=42e034" // same profile, other level
	got, _, ok = matchCodec(want, []webrtc.RTPCodecParameters{h264b, h264a})
	if !ok || got.PayloadType != 102 {
		t.Fatalf("h264: %+v, %v", got, ok)
	}
	if _, _, ok := matchCodec(vp8, []webrtc.RTPCodecParameters{h264a}); ok {
		t.Fatal("no vp8 offered, should fail")
	}

	got, strip, ok := matchCodec(red, []webrtc.RTPCodecParameters{opus, red})
	if !ok || strip || got.PayloadType != 63 {
		t.Fatalf("red kept: %+v %v %v", got, strip, ok)
	}
	got, strip, ok = matchCodec(red, []webrtc.RTPCodecParameters{opus})
	if !ok || !strip || got.PayloadType != 111 {
		t.Fatalf("red fallback: %+v %v %v", got, strip, ok)
	}
}

func simulcastCandidate(prio uint32, current, max int32) candidate {
	return candidate{
		priority: prio, current: current, max: max,
		bitrates:  [MaxLayers]int64{150_000, 500_000, 1_500_000},
		available: [MaxLayers]bool{true, true, true},
	}
}

func layersOf(p []plan) []int32 {
	out := make([]int32, len(p))
	for i, x := range p {
		out[i] = x.layer
	}
	return out
}

func TestAllocatePicksHighestThatFits(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		want   int32
	}{
		{"plenty", 10_000_000, LayerHigh},
		{"medium only", 800_000, LayerMedium},
		{"low only", 300_000, LayerLow},
		{"nothing fits", 50_000, LayerNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := allocate([]candidate{simulcastCandidate(0, LayerNone, LayerHigh)}, c.budget, true)
			if got[0].layer != c.want {
				t.Fatalf("layer = %d, want %d", got[0].layer, c.want)
			}
		})
	}
}

func TestAllocateRespectsSubscriberMax(t *testing.T) {
	got := allocate([]candidate{simulcastCandidate(0, LayerNone, LayerLow)}, 10_000_000, true)
	if got[0].layer != LayerLow {
		t.Fatalf("layer = %d, want LOW", got[0].layer)
	}
}

func TestAllocatePriorityGoesFirst(t *testing.T) {
	high := simulcastCandidate(1, LayerNone, LayerHigh)
	low := simulcastCandidate(5, LayerNone, LayerHigh)
	// Enough for both base layers, and to raise one track to medium.
	got := allocate([]candidate{low, high}, 800_000, true)
	if layersOf(got)[0] != LayerMedium || layersOf(got)[1] != LayerLow {
		t.Fatalf("layers = %v, want the priority 1 track upgraded first", layersOf(got))
	}
}

func TestAllocatePausesLowestPriorityUnderPressure(t *testing.T) {
	high := simulcastCandidate(1, LayerNone, LayerHigh)
	low := simulcastCandidate(5, LayerNone, LayerHigh)
	got := allocate([]candidate{low, high}, 200_000, true)
	if layersOf(got)[0] != LayerLow || layersOf(got)[1] != LayerNone {
		t.Fatalf("layers = %v, want only the priority 1 track kept", layersOf(got))
	}
}

func TestAllocateKeepsBaseLayerWhenPauseDisallowed(t *testing.T) {
	got := allocate([]candidate{simulcastCandidate(0, LayerNone, LayerHigh)}, 1000, false)
	if got[0].layer != LayerLow {
		t.Fatalf("layer = %d, want LOW when pausing is not allowed", got[0].layer)
	}
}

func TestAllocateHeadroomPreventsFlapping(t *testing.T) {
	// Exactly the cost of high: moving up needs headroom, staying does not.
	if got := allocate([]candidate{simulcastCandidate(0, LayerMedium, LayerHigh)}, 1_500_000, true); got[0].layer != LayerMedium {
		t.Fatalf("upgrade took %d without headroom", got[0].layer)
	}
	if got := allocate([]candidate{simulcastCandidate(0, LayerHigh, LayerHigh)}, 1_500_000, true); got[0].layer != LayerHigh {
		t.Fatalf("holding the current layer gave %d", got[0].layer)
	}
}

func TestAllocateDisabledAndNotFlowing(t *testing.T) {
	disabled := simulcastCandidate(0, LayerNone, LayerHigh)
	disabled.disabled = true
	silent := candidate{max: LayerHigh, current: LayerNone}
	got := allocate([]candidate{disabled, silent}, 10_000_000, true)
	if layersOf(got)[0] != LayerNone {
		t.Fatal("a disabled track must not forward")
	}
	if layersOf(got)[1] != LayerHigh {
		t.Fatal("a silent track should aim at its wanted layer, ready for when media starts")
	}
}

func TestAllocateSingleLayerAboveMax(t *testing.T) {
	// A non-simulcast stream sits at HIGH. A subscriber asking for LOW must still get something.
	c := candidate{max: LayerLow, current: LayerNone}
	c.available[LayerHigh] = true
	c.bitrates[LayerHigh] = 1_000_000
	got := allocate([]candidate{c}, 10_000_000, true)
	if got[0].layer != LayerHigh {
		t.Fatalf("layer = %d, want HIGH: it is the only stream", got[0].layer)
	}
}

func TestAudioLevelTracker(t *testing.T) {
	var a audioLevelTracker
	if _, active := a.sample(); active {
		t.Fatal("no packets means no speech")
	}
	for range 50 {
		a.observe(127, false)
	}
	if _, active := a.sample(); active {
		t.Fatal("silence is not speech")
	}
	for range 50 {
		a.observe(20, false)
	}
	level, active := a.sample()
	if !active || level < 0.09 || level > 0.11 {
		t.Fatalf("level = %v, active = %v; -20 dBov is 0.1 in linear", level, active)
	}
	// Brief noise under the activity ratio does not count.
	for range 10 {
		a.observe(20, false)
	}
	for range 90 {
		a.observe(127, false)
	}
	if _, active := a.sample(); active {
		t.Fatal("a short blip is not speech")
	}
}

func TestLayerSilenceLimit(t *testing.T) {
	ms := func(n int64) int64 { return n * int64(time.Millisecond) }
	// A camera at 30fps: the floor applies, so a stopped layer is noticed quickly.
	if got := layerSilenceLimit(ms(33)); got != int64(minLayerSilence) {
		t.Fatalf("camera limit = %d", got)
	}
	// A static screen share sending a frame a second must not look stopped between frames.
	if got := layerSilenceLimit(ms(1000)); got != ms(4000) {
		t.Fatalf("screen share limit = %d", got)
	}
	if got := layerSilenceLimit(ms(60000)); got != int64(maxLayerSilence) {
		t.Fatalf("limit is not capped: %d", got)
	}
}

func TestLayerIsActive(t *testing.T) {
	now := time.Now().UnixNano()
	l := &layerState{}
	if l.isActive(now) {
		t.Fatal("a layer with no packets is not active")
	}
	l.lastSeen.Store(now - int64(100*time.Millisecond))
	if !l.isActive(now) {
		t.Fatal("recent packet")
	}
	l.lastSeen.Store(now - int64(time.Second))
	if l.isActive(now) {
		t.Fatal("a camera layer silent for a second has stopped")
	}
	l.longestGap.Store(int64(500 * time.Millisecond))
	if !l.isActive(now) {
		t.Fatal("a slow layer silent for a second may just be slow")
	}
}

func TestLayerCostIgnoresBursts(t *testing.T) {
	const m = 1_000_000
	cases := []struct {
		name     string
		readings []int64
		want     int64
	}{
		{"too few readings to say", []int64{4 * m, m}, 0},
		{"the keyframe that starts a stream", []int64{4 * m, m, m}, m},
		{"a burst when a shared screen changes", []int64{m, m, 9 * m, m, m}, m},
		{"two bursts in a row", []int64{m, m, m, 9 * m, 9 * m}, m},
		{"a real rise, once it lasts", []int64{m, m, 5 * m, 5 * m, 5 * m}, 5 * m},
		{"old readings fall away", []int64{9 * m, 9 * m, 9 * m, 9 * m, 9 * m, m, m, m}, m},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := &layerState{}
			for _, r := range c.readings {
				l.recordReading(r)
			}
			if got := l.cost.Load(); got != c.want {
				t.Fatalf("cost = %d, want %d", got, c.want)
			}
		})
	}
}

// A paused track that was the only one flowing leaves nothing to measure, so without a retry it would stay paused.
func TestAllocatorRetriesAPausedTrack(t *testing.T) {
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp8Codec, RequestKeyframe: func(uint32) {}})
	t.Cleanup(r.Close)
	var stop atomic.Bool
	t.Cleanup(func() { stop.Store(true) })
	r.AddLayer(LayerHigh, 1, func(b []byte) (int, error) {
		if stop.Load() {
			return 0, io.EOF
		}
		time.Sleep(5 * time.Millisecond)
		pkt := rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1}, Payload: vp8Payload(1, 1, false)}
		data, _ := pkt.Marshal()
		return copy(b, data), nil
	})
	for r.SelectLayer(LayerHigh) != LayerHigh {
		time.Sleep(time.Millisecond)
	}
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	var estimate atomic.Int64
	estimate.Store(1000)
	var paused atomic.Bool
	a := NewAllocator(func() int64 { return estimate.Load() }, func(_ *DownTrack, p bool) { paused.Store(p) })
	t.Cleanup(a.Close)
	a.SetAllowPause(true)
	a.mu.Lock()
	a.tracks = append(a.tracks, d)
	a.mu.Unlock()

	check := func(at time.Time, wantPaused bool) {
		t.Helper()
		a.reallocateAt(at)
		if paused.Load() != wantPaused {
			t.Fatalf("paused = %v at %v, want %v", paused.Load(), at.Sub(start), wantPaused)
		}
		if got := d.Target() == LayerNone; got != wantPaused {
			t.Fatalf("target = %d at %v, want paused %v", d.Target(), at.Sub(start), wantPaused)
		}
	}
	check(start, true)
	check(start.Add(pauseRetryFirst-time.Second), true)

	first := start.Add(pauseRetryFirst)
	check(first, false)
	check(first.Add(pauseRetryFor-time.Second), false)

	// The estimate still says no, so it pauses again, and waits longer this time.
	again := first.Add(pauseRetryFor)
	check(again, true)
	check(again.Add(pauseRetryFirst), true)
	second := again.Add(2 * pauseRetryFirst)
	check(second, false)
	check(second.Add(pauseRetryFor), true)

	// Once the estimate allows it, the track stays on with no more retries.
	estimate.Store(10_000_000)
	later := second.Add(time.Minute)
	check(later, false)
	estimate.Store(1000)
	check(later.Add(time.Second), true)
	check(later.Add(time.Second+pauseRetryFirst-time.Millisecond), true)
}

var start = time.Unix(1_000_000, 0)

// feed runs the capacity judge for a while, as the allocator does twice a second, sending at a fixed rate.
func feed(c *capacity, at *time.Time, bytes *uint64, seconds float64, bps int64, loss float64, rtt time.Duration) int64 {
	var limit int64
	for t := 0.0; t < seconds; t += 0.5 {
		*at = at.Add(500 * time.Millisecond)
		*bytes += uint64(bps / 16)
		limit = c.update(*at, *bytes, 0, loss, rtt)
	}
	return limit
}

func TestCapacityIgnoresWhatWiFiDoes(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	if got := feed(c, &at, &bytes, 30, 5_000_000, 0.02, 30*time.Millisecond); got != 0 {
		t.Fatalf("2%% loss set a limit of %d", got)
	}
	// Round trips moving around, and one bad report, are not congestion.
	for i := range 20 {
		rtt, loss := 20*time.Millisecond, 0.01
		if i%2 == 0 {
			rtt = 120 * time.Millisecond
		}
		if i == 7 {
			loss = 0.3
		}
		if got := feed(c, &at, &bytes, 0.5, 5_000_000, loss, rtt); got != 0 {
			t.Fatalf("reading %d set a limit of %d", i, got)
		}
	}
}

func TestCapacityFindsAFullLink(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 5, 2_500_000, 0, 20*time.Millisecond)
	// 2.5 Mbps into a 1 Mbps link: most of it is lost.
	got := feed(c, &at, &bytes, 3, 2_500_000, 0.6, 300*time.Millisecond)
	if got < 800_000 || got > 1_000_000 {
		t.Fatalf("limit = %d, want what arrived, with a margin", got)
	}
	// A big queue with no loss yet: round trips show it, and it goes on cutting while they stay up.
	c = &capacity{}
	at, bytes = start, uint64(0)
	feed(c, &at, &bytes, 5, 2_500_000, 0, 20*time.Millisecond)
	first := feed(c, &at, &bytes, 2.5, 2_500_000, 0, 400*time.Millisecond)
	if first == 0 {
		t.Fatal("no limit from round trips growing by 380 ms")
	}
	if second := feed(c, &at, &bytes, 3, 1_000_000, 0, 400*time.Millisecond); second >= first {
		t.Fatalf("limit went from %d to %d while still congested", first, second)
	}
}

func TestCapacityRecovers(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 5, 2_500_000, 0, 20*time.Millisecond)
	cut := feed(c, &at, &bytes, 3, 2_500_000, 0.6, 20*time.Millisecond)
	if got := feed(c, &at, &bytes, 2.5, 800_000, 0, 20*time.Millisecond); got != cut {
		t.Fatalf("limit moved to %d too soon after congestion, from %d", got, cut)
	}
	if got := feed(c, &at, &bytes, 4, 800_000, 0, 20*time.Millisecond); got <= cut {
		t.Fatalf("limit stayed at %d with no more congestion", got)
	}
	if got := feed(c, &at, &bytes, 60, 800_000, 0, 20*time.Millisecond); got != 0 {
		t.Fatalf("limit %d is still there long after it stopped mattering", got)
	}
}

// A viewer on a link too thin for the stream has not decoded anything yet, so keyframe requests are all it can say.
func TestCapacityTreatsRepeatedKeyframeRequestsAsCongestion(t *testing.T) {
	run := func(perSecond float64) int64 {
		c := &capacity{}
		at, bytes := start, uint64(0)
		var plis uint64
		var pending float64
		var limit int64
		for range 12 {
			at = at.Add(500 * time.Millisecond)
			bytes += 2_500_000 / 16
			pending += perSecond / 2
			plis += uint64(pending)
			pending -= float64(uint64(pending))
			limit = c.update(at, bytes, plis, 0, 20*time.Millisecond)
		}
		return limit
	}
	if got := run(1.5); got == 0 || got >= 2_500_000 {
		t.Fatalf("limit = %d with a browser that cannot decode, want less than what is sent", got)
	}
	if got := run(0.2); got != 0 {
		t.Fatalf("limit = %d from the odd keyframe request", got)
	}
}

func TestCapacityWaitsLongerAfterEachCut(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 2, 2_500_000, 0, 20*time.Millisecond)
	for _, wait := range []time.Duration{recoveryAfter, 2 * recoveryAfter, 4 * recoveryAfter} {
		cut := feed(c, &at, &bytes, 3, 2_500_000, 0.6, 20*time.Millisecond)
		seconds := wait.Seconds() - 1
		if got := feed(c, &at, &bytes, seconds, 800_000, 0, 20*time.Millisecond); got != cut {
			t.Fatalf("limit moved from %d to %d before %v had passed", cut, got, wait)
		}
		if got := feed(c, &at, &bytes, 3, 800_000, 0, 20*time.Millisecond); got <= cut {
			t.Fatalf("limit stayed at %d after %v", got, wait)
		}
	}
}

// After moving up a layer, nothing else has changed, so one bad report is enough. Waiting for more leaves the viewer frozen.
func TestCapacityAnswersAFailedMoveUpAtOnce(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 3, 400_000, 0, 20*time.Millisecond)
	c.allocated = 2_000_000
	c.tryingMore(at)
	if got := feed(c, &at, &bytes, 1, 2_000_000, 0, 20*time.Millisecond); got != 0 {
		t.Fatalf("limit = %d from a clean report after the move", got)
	}
	got := feed(c, &at, &bytes, 0.5, 2_000_000, 0.3, 20*time.Millisecond)
	if got == 0 || got >= 2_000_000 {
		t.Fatalf("limit = %d after one bad report during a move up, want less than the %d now allocated", got, 2_000_000)
	}

	// During a keyframe's burst on a link with jitter, a report can read more than 10% with nothing lost, only out of order.
	c = &capacity{}
	at, bytes = start, uint64(0)
	feed(c, &at, &bytes, 3, 400_000, 0, 20*time.Millisecond)
	c.allocated = 2_000_000
	c.tryingMore(at)
	if got := feed(c, &at, &bytes, 0.5, 2_000_000, 0.15, 20*time.Millisecond); got != 0 {
		t.Fatalf("limit = %d from one report of 15%%, which jitter alone can give", got)
	}

	// Without a move up, the same report counts for nothing, as WiFi produces those.
	c = &capacity{}
	at, bytes = start, uint64(0)
	feed(c, &at, &bytes, 3, 2_000_000, 0, 20*time.Millisecond)
	c.allocated = 2_000_000
	if got := feed(c, &at, &bytes, 0.5, 2_000_000, 0.3, 20*time.Millisecond); got != 0 {
		t.Fatalf("limit = %d from one bad report with nothing changed", got)
	}
}

// A cut that the current layers still fit under would change nothing, and the viewer stays frozen until a later one.
func TestCapacityCutGoesBelowWhatIsAllocated(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 3, 4_000_000, 0, 20*time.Millisecond)
	c.allocated = 1_500_000
	got := feed(c, &at, &bytes, 3, 4_000_000, 0.15, 20*time.Millisecond)
	if got == 0 || got >= 1_500_000 {
		t.Fatalf("limit = %d, want below the %d allocated", got, 1_500_000)
	}
}

// Cuts in a row are one bout of congestion. Only a limit that rose and proved wrong again makes the next try wait longer,
// or a viewer who sat through a long bad spell would wait a long time after it ended.
func TestCapacityCountsABoutOfCongestionOnce(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 2, 2_500_000, 0, 20*time.Millisecond)
	feed(c, &at, &bytes, 20, 2_500_000, 0.6, 20*time.Millisecond)
	if c.recovery != recoveryAfter {
		t.Fatalf("waiting %v after one long bout of congestion, want %v", c.recovery, recoveryAfter)
	}
}

// A limit that would let a better layer through must not go away just because a low layer is cheap, or the wait after a
// failed move up means nothing.
func TestCapacityKeepsALimitThatIsHoldingSomethingBack(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 3, 1_000_000, 0, 20*time.Millisecond)
	c.wanted = 3_000_000
	c.allocated = 1_000_000
	c.tryingMore(at)
	cut := feed(c, &at, &bytes, 0.5, 1_000_000, 0.3, 20*time.Millisecond)
	if cut == 0 {
		t.Fatal("no limit after a failed move up")
	}
	// A cheap layer now, 200 kbps. Four times that is well below the limit, but the top would still not fit.
	c.allocated = 200_000
	if got := feed(c, &at, &bytes, 3.5, 200_000, 0, 20*time.Millisecond); got == 0 {
		t.Fatal("the limit went away while it was still holding the top layer back")
	}
}

// What flows with no complaint fits, whatever the limit says. A layer costs more just after a move up, because a new keyframe
// starts the encoder refining again.
func TestCapacityRaisesALimitBelowWhatFlowsWithoutComplaint(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feed(c, &at, &bytes, 3, 1_000_000, 0, 20*time.Millisecond)
	c.allocated, c.wanted = 1_000_000, 3_000_000
	c.tryingMore(at)
	feed(c, &at, &bytes, 0.5, 1_000_000, 0.3, 20*time.Millisecond)
	c.allocated = 2_500_000
	if got := feed(c, &at, &bytes, 1, 2_500_000, 0, 20*time.Millisecond); got < 2_500_000 {
		t.Fatalf("limit = %d, below the %d flowing with no complaint", got, 2_500_000)
	}
}

func TestTemporalLayersFitAThinBudget(t *testing.T) {
	const cost = 1_000_000
	for _, c := range []struct {
		budget  int64
		current int32
		want    int32
	}{
		{cost, maxTemporalLayer, maxTemporalLayer},
		{cost * 8 / 10, maxTemporalLayer, 1},
		{cost / 2, maxTemporalLayer, 0},
		// Going back up takes room to spare, or it would flap.
		{cost, 1, 1},
		{cost * 8 / 10, 0, 0},
		{cost * 95 / 100, 0, 1},
		{cost * 12 / 10, 1, maxTemporalLayer},
	} {
		if got := temporalFor(c.budget, cost, c.current); got != c.want {
			t.Errorf("budget %d from %d: got %d, want %d", c.budget, c.current, got, c.want)
		}
	}
}

// flowingTrack builds a track on a receiver whose given layers are flowing, for the allocator to choose between.
func flowingTrack(t *testing.T, layers ...int32) *DownTrack {
	t.Helper()
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp8Codec, RequestKeyframe: func(uint32) {}})
	t.Cleanup(r.Close)
	var stop atomic.Bool
	t.Cleanup(func() { stop.Store(true) })
	for _, layer := range layers {
		ssrc := uint32(layer + 1)
		r.AddLayer(layer, ssrc, func(b []byte) (int, error) {
			if stop.Load() {
				return 0, io.EOF
			}
			time.Sleep(5 * time.Millisecond)
			pkt := rtp.Packet{Header: rtp.Header{Version: 2, SSRC: ssrc}, Payload: vp8Payload(1, 1, false)}
			data, _ := pkt.Marshal()
			return copy(b, data), nil
		})
		for r.SelectLayer(layer) != layer {
			time.Sleep(time.Millisecond)
		}
	}
	return NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
}

// A top layer's first keyframe can be far more than a thin link's queue holds. A low layer gets a picture up whatever the
// link is like, and on a good link the top follows.
func TestAllocatorStartsOnTheLowestLayer(t *testing.T) {
	d := flowingTrack(t, LayerLow, LayerHigh)
	a := NewAllocator(nil, nil)
	t.Cleanup(a.Close)
	a.Add(d)
	now := time.Now()
	a.reallocateAt(now)
	if got := d.Target(); got != LayerLow {
		t.Fatalf("target = %d before anything is forwarded, want the lowest layer", got)
	}
	d.startedAt.Store(now.UnixNano())
	a.reallocateAt(now.Add(startHold - time.Millisecond))
	if got := d.Target(); got != LayerLow {
		t.Fatalf("target = %d during the hold", got)
	}
	// Past the least it holds for, with no reports yet to say the path has no queue.
	a.reallocateAt(now.Add(startHold))
	if got := d.Target(); got != LayerLow {
		t.Fatalf("target = %d with no reports yet", got)
	}
	a.mu.Lock()
	a.capacity.calm = true
	a.mu.Unlock()
	if !d.startingAt(now.Add(startHold-time.Millisecond), true) || d.startingAt(now.Add(startHold), true) {
		t.Fatal("with calm reports, the hold should last exactly as long as its least")
	}
	// A browser that never reports cannot hold it there for ever.
	a.mu.Lock()
	a.capacity.calm = false
	a.mu.Unlock()
	a.reallocateAt(now.Add(startHoldMax))
	if got := d.Target(); got != LayerHigh {
		t.Fatalf("target = %d after the longest hold", got)
	}
}

// Round trips that have settled at their best say the path has no queue. One reading says nothing, and on a thin link the
// first ones go through the queue the first keyframe made.
func TestCapacityIsCalmOnlyOnceRoundTripsSettle(t *testing.T) {
	c := &capacity{}
	at, bytes := start, uint64(0)
	feedRTT := func(rtt time.Duration, loss float64) {
		at = at.Add(500 * time.Millisecond)
		bytes += 50_000
		c.update(at, bytes, 0, loss, rtt)
	}
	feedRTT(0, 0)
	feedRTT(20*time.Millisecond, 0)
	if c.calm {
		t.Fatal("calm from one reading")
	}
	feedRTT(22*time.Millisecond, 0)
	if !c.calm {
		t.Fatal("not calm with two readings close together")
	}
	feedRTT(300*time.Millisecond, 0)
	if c.calm {
		t.Fatal("calm with a queue on the path")
	}
	feedRTT(21*time.Millisecond, 0.08)
	if c.calm {
		t.Fatal("calm while losing packets")
	}
	feedRTT(21*time.Millisecond, 0)
	if !c.calm {
		t.Fatal("not calm once it has settled again")
	}
}

func TestAllocatorStartsASingleLayerTrackOnThatLayer(t *testing.T) {
	d := flowingTrack(t, LayerHigh)
	a := NewAllocator(nil, nil)
	t.Cleanup(a.Close)
	a.Add(d)
	a.reallocateAt(time.Now())
	if got := d.Target(); got != LayerHigh {
		t.Fatalf("target = %d, but only the top layer exists", got)
	}
}

func TestAllocatorDoesNotPauseUnlessAsked(t *testing.T) {
	a := NewAllocator(func() int64 { return 1000 }, nil)
	defer a.Close()
	if a.allowPause.Load() {
		t.Fatal("video pauses under congestion by default, and the reference server's does not")
	}
}
