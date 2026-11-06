package sfu

import (
	"math"
	"testing"
	"time"

	"github.com/pion/rtcp"
)

// downTrackWithLayers returns a bound video DownTrack whose receiver has incoming streams at the given layers, each with
// an SSRC of layer + 1.
func downTrackWithLayers(t *testing.T, layers ...int32) (*DownTrack, *Receiver, *fakeWriter) {
	t.Helper()
	d, w := newVideoDownTrack(t)
	r := d.Receiver()
	for _, l := range layers {
		state := &layerState{index: l, ssrc: uint32(l + 1)}
		state.lastSeen.Store(time.Now().UnixNano())
		r.layers[l] = state
	}
	return d, r, w
}

// publisherNTP is an arbitrary point on a publisher's clock: its value has nothing to do with this machine's time.
const publisherNTP = uint64(4_000_000_000) << 32

func report(layer int32, ntp uint64, rtpTime uint32) *rtcp.SenderReport {
	return &rtcp.SenderReport{SSRC: uint32(layer + 1), NTPTime: ntp, RTPTime: rtpTime}
}

func TestReceiverKeepsTheNewestReportPerStream(t *testing.T) {
	_, r, _ := downTrackWithLayers(t, LayerLow, LayerHigh)
	now := time.Now()

	r.SetSenderReport(report(LayerLow, publisherNTP, 1000), now)
	if sr := r.report(LayerLow); sr == nil || sr.rtp != 1000 || sr.ntp != publisherNTP || !sr.at.Equal(now) {
		t.Fatalf("report: %+v", sr)
	}
	if sr := r.report(LayerHigh); sr != nil {
		t.Fatalf("a report for another stream landed here: %+v", sr)
	}

	// Older by NTP, then older by RTP. Both keep the newest.
	r.SetSenderReport(report(LayerLow, publisherNTP-1, 2000), now)
	if r.report(LayerLow).rtp != 1000 {
		t.Fatal("accepted a report with an older NTP time")
	}
	r.SetSenderReport(report(LayerLow, publisherNTP+1, 999), now)
	if r.report(LayerLow).rtp != 1000 {
		t.Fatal("accepted a report with an older RTP time")
	}

	// RTP time wraps at 32 bits, and that is not going backwards.
	r.SetSenderReport(report(LayerHigh, publisherNTP, math.MaxUint32-10), now)
	r.SetSenderReport(report(LayerHigh, publisherNTP+uint64(1)<<32, 5), now)
	if r.report(LayerHigh).rtp != 5 {
		t.Fatal("rejected a report across the RTP wrap")
	}
	r.SetSenderReport(report(LayerHigh, publisherNTP+uint64(2)<<32, math.MaxUint32-20), now)
	if r.report(LayerHigh).rtp != 5 {
		t.Fatal("accepted a report from before the wrap")
	}

	// Another SSRC, as a retransmission stream has, changes nothing.
	r.SetSenderReport(&rtcp.SenderReport{SSRC: 999, NTPTime: publisherNTP + uint64(9)<<32, RTPTime: 9}, now)
	if r.report(LayerHigh).rtp != 5 {
		t.Fatal("accepted a report for an unknown stream")
	}
}

func TestReceiverIgnoresReportsBeforeTheFirstPacket(t *testing.T) {
	_, r, _ := downTrackWithLayers(t, LayerLow)
	r.layers[LayerLow].lastSeen.Store(0)
	r.SetSenderReport(report(LayerLow, publisherNTP, 1000), time.Now())
	if r.report(LayerLow) != nil {
		t.Fatal("accepted a report with no stream to apply it to")
	}
}

// With the numbers from a publisher whose frames reach this server late, the report must still say what the publisher's
// report said, moved on by the time that passed.
func TestSenderReportKeepsThePublishersMapping(t *testing.T) {
	d, r, _ := downTrackWithLayers(t, LayerHigh)
	d.SetTarget(LayerHigh)
	now := time.Now()
	if sr := d.CreateSenderReport(now); sr != nil {
		t.Fatalf("a report before anything was forwarded: %+v", sr)
	}
	forward(d, LayerHigh, 100, 900_000, true, 1)
	if sr := d.CreateSenderReport(now); sr != nil {
		t.Fatalf("a report before the publisher's first: %+v", sr)
	}

	r.SetSenderReport(report(LayerHigh, publisherNTP, 900_000), now)
	later := now.Add(500 * time.Millisecond)
	sr := d.CreateSenderReport(later)
	if sr == nil {
		t.Fatal("no report")
	}
	if sr.SSRC != 7777 {
		t.Errorf("SSRC = %d: reports use the subscriber's", sr.SSRC)
	}
	if want := publisherNTP + uint64(1)<<31; sr.NTPTime != want {
		t.Errorf("NTP = %d, want %d: the report stays on the publisher's clock", sr.NTPTime, want)
	}
	if want := uint32(900_000 + 45_000); sr.RTPTime != want {
		t.Errorf("RTP = %d, want %d", sr.RTPTime, want)
	}
	if sr.PacketCount != 1 || sr.OctetCount == 0 {
		t.Errorf("counts: %d packets, %d bytes", sr.PacketCount, sr.OctetCount)
	}

	// Going backwards is never reported, even with a clock that does.
	if sr := d.CreateSenderReport(now); sr != nil {
		t.Errorf("a report that goes backwards: %+v", sr)
	}
	if sr := d.CreateSenderReport(later.Add(time.Second)); sr == nil {
		t.Error("no report a second later")
	}
}

// A publisher's clock has nothing to do with this one. Reports on it must not break the round trip, which is why a report
// is looked up by what the subscriber quotes back.
func TestRoundTripTimeComesFromTheSavedReport(t *testing.T) {
	d, r, _ := downTrackWithLayers(t, LayerHigh)
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 100, 900_000, true, 1)
	sent := time.Now()
	oneHourAhead := publisherNTP + uint64(3600)<<32
	r.SetSenderReport(report(LayerHigh, oneHourAhead, 900_000), sent)
	sr := d.CreateSenderReport(sent)
	if sr == nil {
		t.Fatal("no report")
	}
	lsr := uint32(sr.NTPTime >> 16)
	// The subscriber held it for 50 ms, and it came back 150 ms after it went out, so the network took 100 ms.
	rr := rtcp.ReceptionReport{LastSenderReport: lsr, Delay: 50 * 65536 / 1000}
	rtt, ok := d.roundTripTime(rr, sent.Add(150*time.Millisecond))
	if !ok {
		t.Fatal("no round trip")
	}
	if rtt < 99*time.Millisecond || rtt > 101*time.Millisecond {
		t.Fatalf("rtt = %v, want about 100 ms", rtt)
	}
	if _, ok := d.roundTripTime(rtcp.ReceptionReport{}, sent); ok {
		t.Error("a subscriber that has seen no report has no round trip")
	}
	if _, ok := d.roundTripTime(rtcp.ReceptionReport{LastSenderReport: lsr + 1}, sent); ok {
		t.Error("a report that was never sent has no round trip")
	}
	if _, ok := d.roundTripTime(rtcp.ReceptionReport{LastSenderReport: lsr, Delay: 65536}, sent.Add(100*time.Millisecond)); ok {
		t.Error("a delay longer than the round trip is not a round trip")
	}
}

// Each simulcast layer has its own timestamp base and shares only the NTP clock. After a switch, the output must still
// follow the clock it was on, and the reports must agree with it.
func TestSwitchKeepsOneClock(t *testing.T) {
	d, r, w := downTrackWithLayers(t, LayerLow, LayerHigh)
	d.SetTarget(LayerLow)
	start := time.Now()
	r.SetSenderReport(report(LayerLow, publisherNTP, 1_000_000), start)
	r.SetSenderReport(report(LayerHigh, publisherNTP, 7_000_000), start)

	forward(d, LayerLow, 100, 1_000_000, true, 1)
	forward(d, LayerLow, 101, 1_003_000, false, 2)
	mapping := func(sr *rtcp.SenderReport) float64 {
		return float64(sr.NTPTime)/(1<<32) - float64(sr.RTPTime)/90000
	}
	before := d.CreateSenderReport(start.Add(40 * time.Millisecond))
	if before == nil {
		t.Fatal("no report before the switch")
	}

	// A key frame on the high layer, captured 100 ms after the low layer's first frame.
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 5000, 7_009_000, true, 2)
	if len(w.sent) != 3 {
		t.Fatalf("sent = %d", len(w.sent))
	}
	got := w.sent[2].hdr
	if want := uint32(1_009_000); got.Timestamp != want {
		t.Errorf("switched timestamp = %d, want %d: the frame was captured 100 ms after the first", got.Timestamp, want)
	}
	if got.SequenceNumber != 102 {
		t.Errorf("sequence = %d", got.SequenceNumber)
	}

	// The new layer's next report describes the output too, and on the same line.
	next := start.Add(time.Second)
	r.SetSenderReport(report(LayerHigh, publisherNTP+uint64(1)<<32, 7_090_000), next)
	after := d.CreateSenderReport(next.Add(10 * time.Millisecond))
	if after == nil {
		t.Fatal("no report after the switch")
	}
	if diff := math.Abs(mapping(after)-mapping(before)) * 1000; diff > 1 {
		t.Errorf("the mapping moved by %.2f ms across the switch", diff)
	}
}

func TestSwitchFallsBackWithoutReports(t *testing.T) {
	d, r, w := downTrackWithLayers(t, LayerLow, LayerHigh)
	d.SetTarget(LayerLow)
	start := time.Now()
	forward(d, LayerLow, 100, 1_000_000, true, 1)

	// Neither layer has reported, as happens in the first second of a stream. The switch still goes through.
	d.SetTarget(LayerHigh)
	forward(d, LayerHigh, 5000, 7_009_000, true, 2)
	if len(w.sent) != 2 {
		t.Fatalf("sent = %d", len(w.sent))
	}
	if ts := w.sent[1].hdr.Timestamp; int32(ts-w.sent[0].hdr.Timestamp) <= 0 || ts-w.sent[0].hdr.Timestamp > 90000 {
		t.Errorf("timestamp jumped: %d then %d", w.sent[0].hdr.Timestamp, ts)
	}
	if sr := d.CreateSenderReport(start); sr != nil {
		t.Errorf("a report with nothing to base it on: %+v", sr)
	}

	// A report from before the switch was made against another offset, so it says nothing about the output now.
	r.SetSenderReport(report(LayerHigh, publisherNTP, 7_000_000), start)
	if sr := d.CreateSenderReport(start); sr != nil {
		t.Errorf("a report from before the switch: %+v", sr)
	}
	r.SetSenderReport(report(LayerHigh, publisherNTP+uint64(1)<<32, 7_090_000), start.Add(time.Second))
	if sr := d.CreateSenderReport(start.Add(time.Second)); sr == nil {
		t.Error("no report from one after the switch")
	}
}

func TestNoReportsWhilePaused(t *testing.T) {
	d, r, _ := downTrackWithLayers(t, LayerHigh)
	d.SetTarget(LayerHigh)
	now := time.Now()
	forward(d, LayerHigh, 100, 900_000, true, 1)
	r.SetSenderReport(report(LayerHigh, publisherNTP, 900_000), now)
	if d.CreateSenderReport(now) == nil {
		t.Fatal("no report while forwarding")
	}
	d.SetTarget(LayerNone)
	forward(d, LayerHigh, 101, 903_000, false, 2)
	if sr := d.CreateSenderReport(now.Add(time.Second)); sr != nil {
		t.Errorf("a report for a paused track: %+v", sr)
	}
}

func TestChooseSwitchTimestamp(t *testing.T) {
	const clock = 90000
	const last, wall = uint32(1_000_000), uint32(1_009_000)
	cases := []struct {
		name   string
		mapped uint32
		want   uint32
		used   bool
	}{
		{"ahead of the last packet", 1_004_500, 1_004_500, true},
		{"a little behind: capture times differ between layers", 1_000_000 - 1000, last + 1, true},
		{"exactly the last timestamp must still move forward", last, last + 1, true},
		{"far behind: stale reports", 1_000_000 - 5*clock/10, wall, false},
		{"far ahead of the wall clock: not describing now", wall + 3*clock, wall, false},
		{"just under the limit", wall + 2*clock - 1, wall + 2*clock - 1, true},
	}
	for _, c := range cases {
		got, used := chooseSwitchTimestamp(c.mapped, last, wall, clock)
		if got != c.want || used != c.used {
			t.Errorf("%s: got %d, used %v; want %d, used %v", c.name, got, used, c.want, c.used)
		}
	}
}

func TestNTPArithmetic(t *testing.T) {
	if got := addNTP(publisherNTP, 1500*time.Millisecond); got != publisherNTP+uint64(3)<<31 {
		t.Errorf("addNTP: %d", got)
	}
	if got := ntpSince(publisherNTP+uint64(1)<<31, publisherNTP, 90000); got != 45000 {
		t.Errorf("half a second is %d ticks", got)
	}
	if got := ntpSince(publisherNTP, publisherNTP+uint64(2)<<32, 90000); got != -180000 {
		t.Errorf("minus two seconds is %d ticks", got)
	}
}

func TestSDESNamesTheStream(t *testing.T) {
	d, _, _ := downTrackWithLayers(t, LayerHigh)
	chunk := d.SourceDescription("3")
	if chunk.Source != 7777 || len(chunk.Items) != 2 {
		t.Fatalf("%+v", chunk)
	}
	if chunk.Items[0].Type != rtcp.SDESCNAME || chunk.Items[0].Text != "PA|TR" {
		t.Errorf("CNAME: %+v", chunk.Items[0])
	}
	if chunk.Items[1].Text != "3" {
		t.Errorf("mid: %+v", chunk.Items[1])
	}
}
