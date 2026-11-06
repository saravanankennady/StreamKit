package sfu

import (
	"time"

	"github.com/pion/rtcp"
)

// senderReport is the newest sender report accepted from a publisher for one stream. Browsers put a frame's capture time
// on this clock. That is what a viewer needs: anything measured at this server has already been through the publisher's
// encoder and network, and the two media are delayed differently.
type senderReport struct {
	ntp uint64
	rtp uint32
	// at is when the report reached this server.
	at time.Time
}

// addNTP moves an NTP timestamp by a duration. The low 32 bits are fractions of a second.
func addNTP(ntp uint64, d time.Duration) uint64 {
	return ntp + uint64(d/time.Second)<<32 + uint64(d%time.Second)<<32/uint64(time.Second)
}

// ntpSince returns a - b in the clock's ticks, for a clock of the given rate. It is signed, and exact to within a tick.
func ntpSince(a, b uint64, clockRate uint32) int64 {
	diff := int64(a - b)
	negative := diff < 0
	if negative {
		diff = -diff
	}
	// Whole seconds and the fraction separately, so that a large difference cannot overflow.
	ticks := (diff>>32)*int64(clockRate) + int64((uint64(diff)&0xffffffff)*uint64(clockRate)>>32)
	if negative {
		return -ticks
	}
	return ticks
}

// SetSenderReport records a publisher's report for the stream it names. Reports older than the newest one are ignored:
// their NTP or RTP time has gone backwards, as happens when a track is replaced with nothing and later restored. So is
// anything before the first packet, which nothing can be forwarded against yet.
func (r *Receiver) SetSenderReport(sr *rtcp.SenderReport, now time.Time) {
	r.mu.Lock()
	var layer *layerState
	for _, l := range r.layers {
		if l != nil && l.ssrc == sr.SSRC {
			layer = l
			break
		}
	}
	r.mu.Unlock()
	if layer == nil || layer.lastSeen.Load() == 0 {
		return
	}
	if prev := layer.sr.Load(); prev != nil && (sr.NTPTime < prev.ntp || int32(sr.RTPTime-prev.rtp) < 0) {
		return
	}
	layer.sr.Store(&senderReport{ntp: sr.NTPTime, rtp: sr.RTPTime, at: now})
}

// report returns the newest sender report for a stream, or nil.
func (r *Receiver) report(index int32) *senderReport {
	if index < 0 || index >= MaxLayers {
		return nil
	}
	r.mu.Lock()
	l := r.layers[index]
	r.mu.Unlock()
	if l == nil {
		return nil
	}
	return l.sr.Load()
}

// srMarkerCount is how many sent reports a subscriber's reply can refer to. A browser replies about once a second to the
// newest one it has, and reports go out every few seconds.
const srMarkerCount = 8

// srMarker remembers when a report went out. A receiver report names the last sender report it saw by the middle bits of
// its NTP time, and normally that could be compared with the local clock. These reports carry the publisher's clock, so
// it cannot.
type srMarker struct {
	lsr    uint32
	sentAt time.Time
}

// outputClock ties this subscriber's output timestamps to the publisher's NTP clock.
type outputClock struct {
	known bool
	ntp   uint64
	rtp   uint32
}

// reportValid says whether a report can describe the stream as it is now. One from before the last switch was made
// against another offset: after a track is replaced with nothing, RTP may not have kept up with the wall clock, and
// applying the new offset to an old report would jump time.
func (d *DownTrack) reportValid(sr *senderReport) bool {
	return sr != nil && int32(sr.rtp-d.seq.switchTS) >= 0
}

// CreateSenderReport returns a report for this subscriber, or nil if there is nothing true to say yet: no publisher
// report has arrived, or none since the last switch. Media flows regardless. A report built from arrival times would be
// worse than none, because a browser believes it and lines audio and video up wrongly.
//
// The report stays on the publisher's clock, moved forward by the time since that report arrived.
func (d *DownTrack) CreateSenderReport(now time.Time) *rtcp.SenderReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.bound || d.closed || d.unsupported || !d.seq.started || d.current == LayerNone {
		return nil
	}
	sr := d.receiver.report(d.lastLayer)
	if !d.reportValid(sr) {
		return nil
	}
	elapsed := max(now.Sub(sr.at), 0)
	rtpNow := sr.rtp - d.seq.tsOffset + uint32(int64(elapsed)*int64(d.seq.clockRate)/int64(time.Second))
	if d.srSent && int32(rtpNow-d.srLastRTP) < 0 {
		return nil
	}
	ntpNow := addNTP(sr.ntp, elapsed)
	d.srSent = true
	d.srLastRTP = rtpNow
	d.srMarkers[d.srNext%srMarkerCount] = srMarker{lsr: uint32(ntpNow >> 16), sentAt: now}
	d.srNext++
	d.clock = outputClock{known: true, ntp: sr.ntp, rtp: sr.rtp - d.seq.tsOffset}
	return &rtcp.SenderReport{
		SSRC:        d.ssrc,
		NTPTime:     ntpNow,
		RTPTime:     rtpNow,
		PacketCount: uint32(d.packets.Load()),
		OctetCount:  uint32(d.bytes.Load()),
	}
}

// SourceDescription returns what a compound RTCP packet says about this stream, as the reference does: the stream ID as
// CNAME, which is what ties a shared stream's tracks together, and the m-section in a private item.
func (d *DownTrack) SourceDescription(mid string) rtcp.SourceDescriptionChunk {
	d.mu.Lock()
	ssrc := d.ssrc
	d.mu.Unlock()
	return rtcp.SourceDescriptionChunk{
		Source: ssrc,
		Items: []rtcp.SourceDescriptionItem{
			{Type: rtcp.SDESCNAME, Text: d.streamID},
			{Type: sdesMid, Text: mid},
		},
	}
}

const sdesMid = rtcp.SDESType(15)

// roundTripTime computes the round trip from a reception report and the time the report it refers to went out (RFC 3550,
// section 6.4.1).
func (d *DownTrack) roundTripTime(r rtcp.ReceptionReport, now time.Time) (time.Duration, bool) {
	if r.LastSenderReport == 0 {
		return 0, false
	}
	d.mu.Lock()
	var sentAt time.Time
	for _, m := range d.srMarkers {
		if m.lsr == r.LastSenderReport && !m.sentAt.IsZero() {
			sentAt = m.sentAt
			break
		}
	}
	d.mu.Unlock()
	if sentAt.IsZero() {
		return 0, false
	}
	held := time.Duration(uint64(r.Delay) * uint64(time.Second) >> 16)
	rtt := now.Sub(sentAt) - held
	if rtt < 0 {
		return 0, false
	}
	return rtt, true
}

// How far apart the two ways of placing a switch may be before the sender reports are not believed. Reports from before
// a mute can be arbitrarily stale, and a layer that had stopped may report a time far from the present.
const (
	maxBehindLastTimestamp = 50 * time.Millisecond
	maxFromWallClock       = 2 * time.Second
)

// mappedTimestamp says which output timestamp a new stream's packet gets if the output is to stay on one clock across a
// switch. The packet's capture time comes from its own stream's report. The output clock then says where that time falls.
// It reports false if either side has no report, in which case only the wall clock is left to go on.
func (d *DownTrack) mappedTimestamp(index int32, ts uint32) (out uint32, target *senderReport, ok bool) {
	if d.lastLayer != LayerNone {
		if sr := d.receiver.report(d.lastLayer); d.reportValid(sr) {
			d.clock = outputClock{known: true, ntp: sr.ntp, rtp: sr.rtp - d.seq.tsOffset}
		}
	}
	target = d.receiver.report(index)
	if !d.clock.known || target == nil {
		return 0, nil, false
	}
	ticks := ntpSince(target.ntp, d.clock.ntp, d.seq.clockRate) + int64(int32(ts-target.rtp))
	return d.clock.rtp + uint32(ticks), target, true
}

// chooseSwitchTimestamp decides between the timestamp sender reports give, mapped, and the one the wall clock gives,
// wallClock. Timestamps must always move forward. A switch point a little behind the last packet is normal, because
// simulcast layers do not capture at exactly the same moment. One far behind, or far from the wall clock, means the
// reports are not describing this moment, so the wall clock wins and the reports take over from the next one.
func chooseSwitchTimestamp(mapped, last, wallClock, clockRate uint32) (uint32, bool) {
	behindLast := int64(int32(last - mapped))
	if behindLast > int64(maxBehindLastTimestamp)*int64(clockRate)/int64(time.Second) {
		return wallClock, false
	}
	fromWallClock := int64(int32(mapped - wallClock))
	limit := int64(maxFromWallClock) * int64(clockRate) / int64(time.Second)
	if fromWallClock > limit || fromWallClock < -limit {
		return wallClock, false
	}
	if behindLast >= 0 {
		return last + 1, true
	}
	return mapped, true
}
