package sfu

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/prometheus/client_golang/prometheus"

	"elnova-streamkit.local/server/metrics"
)

// rewriter holds the per-subscriber arithmetic that makes several source streams look like one
// continuous stream. It does no I/O, so it is unit-testable on its own.
type rewriter struct {
	clockRate uint32
	started   bool
	seqOffset uint16
	tsOffset  uint32
	lastSeq   uint16
	lastTS    uint32
	lastWall  int64
	// Packets older than the switch point belong to frames this subscriber never received.
	switchSeq uint16
	// switchTS is the source timestamp of the first packet since the last switch.
	switchTS uint32

	// For a stream packets are filtered out of: the highest source sequence number seen, and where the ones passed over
	// were to go if they turn up.
	highestIn uint16
	missing   [missingSlots]missingSlot
}

// missingSlots is how many packets lost from a filtered stream can still take their place when they arrive late.
const missingSlots = 64

type missingSlot struct {
	seq  uint16
	out  uint16
	used bool
}

// seqOrder says how a packet of a filtered stream relates to what has been seen.
type seqOrder uint8

const (
	// seqNext follows the last one directly, so nothing depends on where it would have gone.
	seqNext seqOrder = iota
	// seqAfterGap has packets missing just before it.
	seqAfterGap
	// seqLate is one of those missing packets.
	seqLate
	// seqRepeated has no place left: it has been seen, or it is older than can be tracked.
	seqRepeated
)

// startFiltered prepares a stream's first packet, or a resume of the same stream. Timestamps carry on as they were,
// because a stream only ever has one timeline, and only sequence numbers need to close over what was dropped.
func (w *rewriter) startFiltered(seq uint16, ts uint32, now int64) {
	if !w.started {
		w.rebase(seq, ts, now)
	} else {
		w.seqOffset = seq - (w.lastSeq + 1)
		w.switchSeq = seq
	}
	w.highestIn = seq - 1
	w.missing = [missingSlots]missingSlot{}
}

// place finds where a packet of a filtered stream would go.
func (w *rewriter) place(seq uint16) (out uint16, order seqOrder) {
	diff := int16(seq - w.highestIn)
	switch {
	case diff == 1:
		w.highestIn = seq
		return seq - w.seqOffset, seqNext
	case diff > 1:
		from := w.highestIn + 1
		if diff > missingSlots {
			from = seq - missingSlots
		}
		for m := from; m != seq; m++ {
			w.missing[m%missingSlots] = missingSlot{seq: m, out: m - w.seqOffset, used: true}
		}
		w.highestIn = seq
		return seq - w.seqOffset, seqAfterGap
	}
	slot := &w.missing[seq%missingSlots]
	if !slot.used || slot.seq != seq {
		return 0, seqRepeated
	}
	slot.used = false
	return slot.out, seqLate
}

// dropNext closes the sequence over a packet that came in order and will not be sent, so the subscriber never asks for it.
func (w *rewriter) dropNext() { w.seqOffset++ }

// emitted records a packet going out in a slot from place.
func (w *rewriter) emitted(out uint16, ts uint32, now int64) {
	if int16(out-w.lastSeq) > 0 {
		w.lastSeq = out
		w.lastTS = ts
		w.lastWall = now
	}
}

// rebase prepares to forward a new source stream beginning at seq and ts, at wall-clock time now in
// nanoseconds, with no knowledge of how its timestamps relate to the old stream's.
func (w *rewriter) rebase(seq uint16, ts uint32, now int64) {
	w.switchSeq, w.switchTS = seq, ts
	if !w.started {
		w.started = true
		w.seqOffset, w.tsOffset = 0, 0
		w.lastSeq, w.lastTS = seq-1, ts
		w.lastWall = now
		return
	}
	w.seqOffset = seq - (w.lastSeq + 1)
	w.tsOffset = ts - w.wallClockNext(now)
}

// rebaseMapped is rebase for when sender reports say what the new stream's first timestamp should become. It reports
// whether that was used.
func (w *rewriter) rebaseMapped(seq uint16, ts, mapped uint32, now int64) bool {
	if !w.started {
		w.rebase(seq, ts, now)
		return false
	}
	next, used := chooseSwitchTimestamp(mapped, w.lastTS, w.wallClockNext(now), w.clockRate)
	w.switchSeq, w.switchTS = seq, ts
	w.seqOffset = seq - (w.lastSeq + 1)
	w.tsOffset = ts - next
	return used
}

// wallClockNext advances the timestamp by the time that really passed, so players keep correct timing.
func (w *rewriter) wallClockNext(now int64) uint32 {
	elapsed := now - w.lastWall
	if elapsed < 0 {
		elapsed = 0
	}
	delta := uint32(elapsed * int64(w.clockRate) / int64(time.Second))
	if delta == 0 {
		delta = 1
	}
	return w.lastTS + delta
}

// skip closes up the sequence over a packet that will not be forwarded, so the subscriber does not ask for it. A
// retransmission arriving from before this point could no longer be numbered correctly, so those count as too old.
func (w *rewriter) skip(seq uint16) {
	if int16(seq-w.seqOffset-w.lastSeq) <= 0 {
		return
	}
	w.dropNext()
	w.switchSeq = seq + 1
}

// rewrite maps a source packet's sequence number and timestamp. ok is false for a packet from before
// the last switch.
func (w *rewriter) rewrite(seq uint16, ts uint32, now int64) (outSeq uint16, outTS uint32, ok bool) {
	if int16(seq-w.switchSeq) < 0 {
		return 0, 0, false
	}
	outSeq = seq - w.seqOffset
	outTS = ts - w.tsOffset
	if int16(outSeq-w.lastSeq) > 0 {
		w.lastSeq = outSeq
		w.lastTS = outTS
		w.lastWall = now
	}
	return outSeq, outTS, true
}

// DownTrack forwards one track to one subscriber. It implements webrtc.TrackLocal.
type DownTrack struct {
	id       string
	streamID string
	receiver *Receiver
	isVideo  bool
	want     webrtc.RTPCodecParameters

	mu           sync.Mutex
	bound        bool
	closed       bool
	writer       webrtc.TrackLocalWriter
	ssrc         uint32
	pt           webrtc.PayloadType
	ext          HeaderExtensionIDs
	playoutDelay []byte
	stripRED     bool
	current      int32
	// lastLayer is the last stream forwarded. It outlives current through a pause, and the timestamps it left behind
	// still say where the output clock is.
	lastLayer   int32
	seq         rewriter
	vp8         vp8Munger
	isVP8       bool
	scratch     [1500]byte
	forwarded   bool
	unsupported bool
	resend      resender

	clock     outputClock
	srSent    bool
	srLastRTP uint32
	srMarkers [srMarkerCount]srMarker
	srNext    int

	// Choosing layers from within a stream. Which one is in use depends on what the publisher sends.
	svc svcSelector
	vp9 vp9Selector

	// target is the layer the allocator wants; maxLayer is what the subscriber asked for.
	target   atomic.Int32
	maxLayer atomic.Int32
	// fps is the frame rate the subscriber asked for, with 0 for no limit, and maxTemporal what that comes to.
	fps         atomic.Uint32
	maxTemporal atomic.Int32
	disabled    atomic.Bool
	priority    atomic.Uint32
	// allocPaused means the allocator has no bandwidth for this track.
	allocPaused atomic.Bool
	// startedAt is when the first packet was forwarded, as a unix time in nanoseconds.
	startedAt atomic.Int64
	// temporalBudget is the highest temporal layer that fits what the allocator thinks the link can carry. temporal, under
	// mu, is the highest now forwarded of a VP8 stream.
	temporalBudget atomic.Int32
	temporal       int32
	// linkShort means the allocator has found this subscriber's link short of what it wants. keyframeAsks, under mu, is how
	// its requests for keyframes are being spaced out.
	linkShort    atomic.Bool
	keyframeAsks keyframeSpacing

	packets atomic.Uint64
	bytes   atomic.Uint64
	nacks   atomic.Uint64
	plis    atomic.Uint64
	rtt     atomic.Int64 // nanoseconds
	jitter  atomic.Int64 // nanoseconds
	loss    atomic.Uint32

	// gaps counts sequence numbers skipped because those packets never reached this server. Guarded by mu.
	gaps uint64
	// reported is where the last receiver report left off. Guarded by mu.
	reported struct {
		valid bool
		seq   uint32
		// ownLost is the most packets ever lost on the way to this viewer, as far as reports have shown.
		ownLost int64
	}

	outPackets prometheus.Counter
	outBytes   prometheus.Counter

	onBound       func()
	onFirstPacket func()
	onClosed      func()
	onUnsupported func()
	closeOnce     sync.Once
}

// DownTrackParams configures a DownTrack.
type DownTrackParams struct {
	// ID and StreamID appear in the SDP as the msid. Clients parse StreamID as "<participant>|<track>".
	ID            string
	StreamID      string
	Receiver      *Receiver
	OnBound       func()
	OnFirstPacket func()
	OnClosed      func()
	// OnUnsupported runs if the subscriber cannot receive the track's codec.
	OnUnsupported func()
	// MaxPlayoutDelay limits how long the subscriber may hold frames before showing them. Zero sets no limit.
	MaxPlayoutDelay time.Duration
}

// NewDownTrack creates a DownTrack. Add it to a PeerConnection, then to the receiver.
func NewDownTrack(p DownTrackParams) *DownTrack {
	d := &DownTrack{
		id:            p.ID,
		streamID:      p.StreamID,
		receiver:      p.Receiver,
		want:          p.Receiver.Codec(),
		isVideo:       p.Receiver.Kind() == webrtc.RTPCodecTypeVideo,
		current:       LayerNone,
		lastLayer:     LayerNone,
		onBound:       p.OnBound,
		onFirstPacket: p.OnFirstPacket,
		onClosed:      p.OnClosed,
		onUnsupported: p.OnUnsupported,
	}
	if p.MaxPlayoutDelay > 0 {
		d.playoutDelay = playoutDelayLimit(p.MaxPlayoutDelay)
	}
	d.isVP8 = strings.EqualFold(d.want.MimeType, webrtc.MimeTypeVP8)
	d.seq.clockRate = d.want.ClockRate
	d.maxLayer.Store(LayerHigh)
	d.maxTemporal.Store(maxTemporalLayer)
	d.target.Store(LayerNone)
	d.temporalBudget.Store(maxTemporalLayer)
	d.temporal = maxTemporalLayer
	kind := p.Receiver.Kind().String()
	d.outPackets = metrics.Packets.WithLabelValues("out", kind)
	d.outBytes = metrics.Bytes.WithLabelValues("out", kind)
	return d
}

// ID implements webrtc.TrackLocal.
func (d *DownTrack) ID() string { return d.id }

// RID implements webrtc.TrackLocal. Subscribers receive a single stream, never simulcast.
func (d *DownTrack) RID() string { return "" }

// StreamID implements webrtc.TrackLocal.
func (d *DownTrack) StreamID() string { return d.streamID }

// Kind implements webrtc.TrackLocal.
func (d *DownTrack) Kind() webrtc.RTPCodecType { return d.receiver.Kind() }

// Bind implements webrtc.TrackLocal. It runs when the subscriber's answer has been applied.
func (d *DownTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	offered := ctx.CodecParameters()
	codec, stripRED, ok := matchCodec(d.want, offered)
	if !ok {
		// Returning an error here would fail SetRemoteDescription for the whole connection, stopping every
		// other track this subscriber receives. Bind to anything, send nothing, and report it instead.
		d.mu.Lock()
		first := !d.unsupported
		d.unsupported = true
		d.mu.Unlock()
		if first && d.onUnsupported != nil {
			go d.onUnsupported()
		}
		if len(offered) > 0 {
			return offered[0], nil
		}
		return webrtc.RTPCodecParameters{}, nil
	}
	d.mu.Lock()
	d.bound = true
	d.writer = ctx.WriteStream()
	d.ssrc = uint32(ctx.SSRC())
	d.pt = codec.PayloadType
	d.stripRED = stripRED
	for _, h := range ctx.HeaderExtensions() {
		switch h.URI {
		case sdp.AudioLevelURI:
			d.ext.AudioLevel = uint8(h.ID)
		case DependencyDescriptorURI:
			d.ext.DependencyDescriptor = uint8(h.ID)
		case PlayoutDelayURI:
			d.ext.PlayoutDelay = uint8(h.ID)
		}
	}
	d.mu.Unlock()
	if d.isVideo {
		d.receiver.RequestKeyframe(d.target.Load())
	}
	if d.onBound != nil {
		d.onBound()
	}
	return codec, nil
}

// PlayoutDelayURI is the RTP header extension that limits how long a receiver may hold frames before showing them.
const PlayoutDelayURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

// playoutDelayLimit encodes the extension's value: a minimum of zero and this maximum, in units of 10 ms.
func playoutDelayLimit(maximum time.Duration) []byte {
	units := min(maximum.Milliseconds()/10, 0xfff)
	return []byte{0, byte(units >> 8), byte(units)}
}

// DependencyDescriptorURI is the RTP header extension carrying AV1 and VP9 frame dependencies.
const DependencyDescriptorURI = "https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension"

// Unbind implements webrtc.TrackLocal.
func (d *DownTrack) Unbind(webrtc.TrackLocalContext) error {
	d.mu.Lock()
	d.bound = false
	d.writer = nil
	d.mu.Unlock()
	return nil
}

// matchCodec finds the negotiated codec that carries want. When the publisher sends RED and the
// subscriber did not negotiate it, it falls back to the primary codec with stripRED set.
func matchCodec(want webrtc.RTPCodecParameters, offered []webrtc.RTPCodecParameters) (webrtc.RTPCodecParameters, bool, bool) {
	for _, c := range offered {
		if codecEqual(want.RTPCodecCapability, c.RTPCodecCapability) {
			return c, false, true
		}
	}
	if strings.EqualFold(want.MimeType, MimeTypeRED) {
		for _, c := range offered {
			if strings.EqualFold(c.MimeType, webrtc.MimeTypeOpus) {
				return c, true, true
			}
		}
	}
	return webrtc.RTPCodecParameters{}, false, false
}

// MimeTypeRED is redundant audio (RFC 2198).
const MimeTypeRED = "audio/red"

func codecEqual(a, b webrtc.RTPCodecCapability) bool {
	if !strings.EqualFold(a.MimeType, b.MimeType) {
		return false
	}
	switch {
	case strings.EqualFold(a.MimeType, webrtc.MimeTypeH264):
		return fmtpValue(a.SDPFmtpLine, "packetization-mode") == fmtpValue(b.SDPFmtpLine, "packetization-mode") &&
			strings.EqualFold(profileIDC(a.SDPFmtpLine), profileIDC(b.SDPFmtpLine))
	case strings.EqualFold(a.MimeType, webrtc.MimeTypeVP9):
		return fmtpValueOr(a.SDPFmtpLine, "profile-id", "0") == fmtpValueOr(b.SDPFmtpLine, "profile-id", "0")
	}
	return true
}

func fmtpValue(line, key string) string {
	for _, part := range strings.Split(line, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func fmtpValueOr(line, key, def string) string {
	if v := fmtpValue(line, key); v != "" {
		return v
	}
	return def
}

// profileIDC returns the profile part of profile-level-id, ignoring the level. Decoders of a profile
// handle any level up to their own, and browsers offer several.
func profileIDC(line string) string {
	v := fmtpValue(line, "profile-level-id")
	if len(v) < 4 {
		return ""
	}
	return v[:4]
}

// Forward sends one packet if this subscriber should receive it. Safe for concurrent use: during a
// layer switch, two layers' receive loops call it at once.
func (d *DownTrack) Forward(p *Packet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.bound || d.closed || d.writer == nil || d.unsupported {
		return
	}
	now := time.Now().UnixNano()
	if d.isVideo && p.Selection != SelectStream {
		d.forwardFiltered(p, now)
		return
	}
	first := false
	if d.isVideo {
		target := d.target.Load()
		switch {
		case target == LayerNone:
			d.current = LayerNone
			return
		case p.Layer != d.current:
			if p.Layer != target || !p.KeyframeStart {
				return
			}
			d.switchTo(p, now)
			// A keyframe depends on nothing, so any temporal layer can start from it.
			d.temporal = d.maxTemporal.Load()
			if d.isVP8 {
				if desc, ok := parseVP8Descriptor(p.Payload); ok {
					d.vp8.onSwitch(desc)
				}
			}
			first = !d.forwarded
		}
		if d.isVP8 && !p.KeyframeStart {
			if desc, ok := parseVP8Descriptor(p.Payload); ok && d.dropsTemporalLayer(desc) {
				d.seq.skip(p.Header.SequenceNumber)
				d.vp8.skip(desc)
				return
			}
		}
	} else if !d.seq.started {
		d.current, d.lastLayer = p.Layer, p.Layer
		d.seq.rebase(p.Header.SequenceNumber, p.Header.Timestamp, now)
		first = true
	} else if p.Resumed {
		// What was dropped during the mute must not look like lost packets, or the viewer's connection reads as bad.
		d.seq.rebase(p.Header.SequenceNumber, p.Header.Timestamp, now)
	}

	previous := d.seq.lastSeq
	outSeq, outTS, ok := d.seq.rewrite(p.Header.SequenceNumber, p.Header.Timestamp, now)
	if !ok {
		return
	}
	if step := int16(outSeq - previous); step > 1 {
		d.gaps += uint64(step - 1)
	} else if step <= 0 && d.gaps > 0 {
		// A late packet, filling a gap counted earlier.
		d.gaps--
	}
	payload := p.Payload
	if d.stripRED {
		primary, ok := redPrimary(payload)
		if !ok {
			return
		}
		payload = primary
	}
	if d.isVP8 && len(payload) > 0 {
		if desc, ok := parseVP8Descriptor(payload); ok {
			if d.vp8.needsCopy() && len(payload) <= len(d.scratch) {
				n := copy(d.scratch[:], payload)
				payload = d.scratch[:n]
			}
			d.vp8.apply(payload, desc)
		}
	}

	hdr := rtp.Header{
		Version:        2,
		Marker:         p.Header.Marker,
		PayloadType:    uint8(d.pt),
		SequenceNumber: outSeq,
		Timestamp:      outTS,
		SSRC:           d.ssrc,
	}
	if src := d.receiver.params.HeaderExt.AudioLevel; d.ext.AudioLevel != 0 && src != 0 {
		if v := p.Header.GetExtension(src); v != nil {
			_ = hdr.SetExtension(d.ext.AudioLevel, v)
		}
	}
	if src := d.receiver.params.HeaderExt.DependencyDescriptor; d.ext.DependencyDescriptor != 0 && src != 0 {
		if v := p.Header.GetExtension(src); v != nil {
			_ = hdr.SetExtension(d.ext.DependencyDescriptor, v)
		}
	}

	if d.ext.PlayoutDelay != 0 && d.playoutDelay != nil {
		_ = hdr.SetExtension(d.ext.PlayoutDelay, d.playoutDelay)
	}

	if _, err := d.writer.WriteRTP(&hdr, payload); err != nil {
		return
	}
	if d.isVideo {
		if p.KeyframeStart {
			d.resend.keyframeSent(outSeq)
		}
		d.resend.remember(&hdr, payload, now)
	}
	d.packets.Add(1)
	size := uint64(len(payload))
	d.bytes.Add(size)
	d.outPackets.Inc()
	d.outBytes.Add(float64(size))
	if first && !d.forwarded {
		d.forwarded = true
		d.startedAt.Store(now)
		if d.onFirstPacket != nil {
			go d.onFirstPacket()
		}
	}
}

// forwardFiltered handles a stream that carries several layers, where each subscriber gets a selection of its packets.
// The sequence numbers are rewritten to close over what it does not get. Timestamps, the payload and its codec descriptor
// pass through as they are, and the dependency descriptor changes only in its frame number and active targets.
func (d *DownTrack) forwardFiltered(p *Packet, now int64) {
	target := d.target.Load()
	if target == LayerNone {
		if d.current != LayerNone {
			d.current = LayerNone
			d.svc.reset()
			d.vp9.reset()
		}
		return
	}
	seq := p.Header.SequenceNumber
	var choice layerChoice
	if p.Selection == SelectDescriptor {
		choice = d.svc.choose(p.SVC, p.Header.Marker, target, d.maxTemporal.Load(), d.ext.DependencyDescriptor != 0)
	} else {
		choice = d.vp9.choose(p.VP9, p.KeyframeStart, p.Header.Marker, target, d.maxTemporal.Load())
		choice.keepExt = true
	}
	if d.current == LayerNone {
		if !choice.selected {
			return
		}
		d.seq.startFiltered(seq, p.Header.Timestamp, now)
		d.lastLayer = p.Layer
	}
	out, order := d.seq.place(seq)
	if order == seqRepeated {
		return
	}
	if !choice.selected {
		// Sending the next number on, or a gap in the numbers, is how a subscriber would come to ask for this packet.
		if order == seqNext {
			d.seq.dropNext()
		} else {
			d.writePaddingSlot(out, now)
		}
		return
	}
	d.current = choice.spatial

	outTS := p.Header.Timestamp - d.seq.tsOffset
	hdr := rtp.Header{
		Version:        2,
		Marker:         choice.marker,
		PayloadType:    uint8(d.pt),
		SequenceNumber: out,
		Timestamp:      outTS,
		SSRC:           d.ssrc,
	}
	// First, because the first extension decides the header's format, and a descriptor with a structure needs the longer one.
	if id := d.ext.DependencyDescriptor; id != 0 {
		ext := choice.ext
		if choice.keepExt {
			if src := d.receiver.params.HeaderExt.DependencyDescriptor; src != 0 {
				ext = p.Header.GetExtension(src)
			}
		}
		if ext != nil {
			_ = hdr.SetExtension(id, ext)
		}
	}
	if d.ext.PlayoutDelay != 0 && d.playoutDelay != nil {
		_ = hdr.SetExtension(d.ext.PlayoutDelay, d.playoutDelay)
	}
	if _, err := d.writer.WriteRTP(&hdr, p.Payload); err != nil {
		return
	}
	d.seq.emitted(out, outTS, now)
	if p.KeyframeStart {
		d.resend.keyframeSent(out)
	}
	d.resend.remember(&hdr, p.Payload, now)
	d.packets.Add(1)
	size := uint64(len(p.Payload))
	d.bytes.Add(size)
	d.outPackets.Inc()
	d.outBytes.Add(float64(size))
	if !d.forwarded {
		d.forwarded = true
		d.startedAt.Store(now)
		if d.onFirstPacket != nil {
			go d.onFirstPacket()
		}
	}
}

// writePaddingSlot sends a packet with nothing in it to fill a sequence number that has to be used. A subscriber's
// receiver counts these as received and does not ask for them.
func (d *DownTrack) writePaddingSlot(out uint16, now int64) {
	hdr := rtp.Header{
		Version:        2,
		Padding:        true,
		PayloadType:    uint8(d.pt),
		SequenceNumber: out,
		Timestamp:      d.seq.lastTS,
		SSRC:           d.ssrc,
	}
	if _, err := d.writer.WriteRTP(&hdr, []byte{1}); err == nil {
		d.seq.emitted(out, d.seq.lastTS, now)
		d.resend.remember(&hdr, []byte{1}, now)
	}
}

// NeedsKeyframe reports whether this subscriber cannot be given what it asked for without a key frame. Packets alone can
// break that, so it has to be polled: a lost frame breaks a chain with no other sign.
func (d *DownTrack) NeedsKeyframe() bool {
	target := d.target.Load()
	if !d.isVideo || target == LayerNone || !d.receiver.selectsWithin() {
		return false
	}
	want := d.receiver.SelectLayer(target)
	if want == LayerNone {
		want = target
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.bound || d.closed {
		return false
	}
	if d.receiver.svc.selection() == SelectVP9 {
		return !d.vp9.locked(want)
	}
	return !d.svc.locked(want)
}

// SetMaxFPS limits the frame rate of a stream this subscriber gets layers from. Zero sets no limit. For other streams, it
// has no effect.
func (d *DownTrack) SetMaxFPS(fps uint32) {
	d.fps.Store(fps)
	d.updateTemporal()
}

// updateTemporal works out the highest temporal layer to forward: the frame rate asked for, turned into a temporal layer,
// and what the allocator says fits. Rates are measured, so this is done again as they change.
func (d *DownTrack) updateTemporal() {
	limit := d.temporalBudget.Load()
	if fps := d.fps.Load(); fps != 0 && d.receiver.svc != nil {
		spatial := d.target.Load()
		if spatial == LayerNone {
			spatial = d.maxLayer.Load()
		}
		limit = min(limit, d.receiver.svc.temporalForFPS(spatial, fps))
	}
	d.maxTemporal.Store(limit)
}

// switchTo starts forwarding p's stream. The first choice for its timestamps is what sender reports say, because the
// output then stays on one clock and the reports going to this subscriber stay true across the switch. Layers can have
// nothing in common otherwise: each has its own timestamp base.
func (d *DownTrack) switchTo(p *Packet, now int64) {
	ts := p.Header.Timestamp
	seq := p.Header.SequenceNumber
	used := false
	if out, target, ok := d.mappedTimestamp(p.Layer, ts); ok {
		used = d.seq.rebaseMapped(seq, ts, out, now)
		if used {
			d.clock = outputClock{known: true, ntp: target.ntp, rtp: target.rtp - d.seq.tsOffset}
		}
	} else {
		d.seq.rebase(seq, ts, now)
	}
	if !used {
		d.clock.known = false
	}
	d.current, d.lastLayer = p.Layer, p.Layer
}

// dropsTemporalLayer decides whether a VP8 frame is above the temporal layer this subscriber gets, for a stream it takes
// whole otherwise. Streams filtered within have their selectors for this. Coming down happens at the start of a frame.
// Going up has to wait for a frame that does not depend on what was skipped, or the picture breaks.
func (d *DownTrack) dropsTemporalLayer(desc vp8Descriptor) bool {
	if !desc.hasTID {
		return false
	}
	tid := int32(desc.tid)
	limit := d.maxTemporal.Load()
	if desc.start && desc.partitionID == 0 {
		switch {
		case d.temporal > limit:
			d.temporal = limit
		case d.temporal < limit && desc.sync && tid > d.temporal && tid <= limit:
			d.temporal = tid
		}
	}
	return tid > d.temporal
}

// HandleRTCP processes feedback the subscriber sent about this track.
func (d *DownTrack) HandleRTCP(pkts []rtcp.Packet) {
	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			d.plis.Add(1)
			d.onKeyframeRequest()
		case *rtcp.FullIntraRequest:
			metrics.FIRs.WithLabelValues().Inc()
			d.plis.Add(1)
			d.onKeyframeRequest()
		case *rtcp.TransportLayerNack:
			d.nacks.Add(1)
			metrics.NACKs.WithLabelValues().Inc()
			d.answerNack(p)
		case *rtcp.ReceiverReport:
			d.applyReceptionReports(p.Reports)
		case *rtcp.SenderReport:
			d.applyReceptionReports(p.Reports)
		}
	}
}

// onKeyframeRequest takes a subscriber's request for a keyframe. A new keyframe is coming, so the old packets it has given
// up on need not be sent again. On a link found to be short, repeated requests are passed on less and less often.
func (d *DownTrack) onKeyframeRequest() {
	now := time.Now().UnixNano()
	d.mu.Lock()
	ok := d.keyframeAsks.allow(now, d.linkShort.Load())
	if ok {
		d.resend.awaitingKeyframeSince = now
	}
	d.mu.Unlock()
	if ok {
		d.requestKeyframe()
	}
}

func (d *DownTrack) answerNack(nack *rtcp.TransportLayerNack) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.bound || d.closed || d.writer == nil {
		return
	}
	// Leaving this layer for a lower one means the link could not carry it. Its packets will soon be of no use.
	if target := d.target.Load(); target != LayerNone && target < d.current {
		return
	}
	d.resend.answer(nack.Nacks, time.Now().UnixNano(), resendInterval(time.Duration(d.rtt.Load())), func(b []byte) {
		if _, err := d.writer.Write(b); err == nil {
			d.bytes.Add(uint64(len(b)))
		}
	})
}

func (d *DownTrack) requestKeyframe() {
	if !d.isVideo {
		return
	}
	d.mu.Lock()
	layer := d.current
	d.mu.Unlock()
	if layer == LayerNone {
		layer = d.target.Load()
	}
	d.receiver.RequestKeyframe(layer)
}

// ownLoss works out how much of a report's loss happened on the way to this viewer, out of 256. A report also counts what
// never reached this server, and that says nothing about the viewer: without this, one person on a bad uplink makes
// everyone watching them read as having a bad connection too.
//
// It compares running totals, and only what goes past the highest so far counts. A report describes a moment ago and the
// gaps are counted now, so the difference rises and falls by itself as reports catch up, and none of that is loss.
func (d *DownTrack) ownLoss(r rtcp.ReceptionReport) uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	own := int64(r.TotalLost) - int64(d.gaps)
	lost := own - d.reported.ownLost
	expected := int64(r.LastSequenceNumber) - int64(d.reported.seq)
	first := !d.reported.valid
	d.reported.valid, d.reported.seq = true, r.LastSequenceNumber
	if lost > 0 {
		d.reported.ownLost = own
	}
	if first {
		// Nothing to measure from yet, so the report's own figure stands, unless what was missing here explains it.
		if own <= 0 {
			return 0
		}
		return uint32(r.FractionLost)
	}
	if lost <= 0 || expected <= 0 {
		return 0
	}
	return uint32(min(lost*256/expected, 255))
}

func (d *DownTrack) applyReceptionReports(reports []rtcp.ReceptionReport) {
	d.mu.Lock()
	ssrc := d.ssrc
	clock := d.seq.clockRate
	d.mu.Unlock()
	for _, r := range reports {
		if r.SSRC != ssrc {
			continue
		}
		loss := d.ownLoss(r)
		d.loss.Store(loss)
		metrics.SubscriberLoss.Observe(float64(loss) / 256)
		if clock > 0 {
			j := time.Duration(r.Jitter) * time.Second / time.Duration(clock)
			d.jitter.Store(int64(j))
			metrics.SubscriberJitter.Observe(j.Seconds())
		}
		if rtt, ok := d.roundTripTime(r, time.Now()); ok {
			d.rtt.Store(int64(rtt))
			metrics.SubscriberRTT.Observe(rtt.Seconds())
		}
	}
}

// SetMaxLayer sets the highest layer the subscriber wants.
func (d *DownTrack) SetMaxLayer(l int32) { d.maxLayer.Store(l) }

// MaxLayer returns the highest layer the subscriber wants.
func (d *DownTrack) MaxLayer() int32 { return d.maxLayer.Load() }

// SetDisabled pauses or resumes forwarding at the subscriber's request.
func (d *DownTrack) SetDisabled(v bool) { d.disabled.Store(v) }

// Disabled reports whether the subscriber paused this track.
func (d *DownTrack) Disabled() bool { return d.disabled.Load() }

// SetPriority sets the subscription priority: 1 is highest, 0 is unset.
func (d *DownTrack) SetPriority(v uint32) { d.priority.Store(v) }

// Priority returns the subscription priority.
func (d *DownTrack) Priority() uint32 { return d.priority.Load() }

// Receiver returns the source of this track.
func (d *DownTrack) Receiver() *Receiver { return d.receiver }

// IsVideo reports the media kind.
func (d *DownTrack) IsVideo() bool { return d.isVideo }

// SetTarget chooses the layer to forward, or LayerNone to pause.
func (d *DownTrack) SetTarget(layer int32) { d.target.Store(layer) }

// Target returns the chosen layer.
func (d *DownTrack) Target() int32 { return d.target.Load() }

// SetTemporalBudget sets the highest temporal layer that fits the link.
func (d *DownTrack) SetTemporalBudget(layer int32) {
	d.temporalBudget.Store(layer)
	d.updateTemporal()
}

// TemporalBudget returns the highest temporal layer that fits the link.
func (d *DownTrack) TemporalBudget() int32 { return d.temporalBudget.Load() }

// startingAt reports whether this track has yet to start, or only just has, so it should keep to its lowest layer. calm says
// whether browser reports have shown no queue on the path, and a silent browser cannot hold it there for ever.
func (d *DownTrack) startingAt(now time.Time, calm bool) bool {
	started := d.startedAt.Load()
	if started == 0 {
		return true
	}
	since := now.UnixNano() - started
	return since < int64(startHold) || (!calm && since < int64(startHoldMax))
}

func (d *DownTrack) isBound() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bound
}

// Current returns the layer now being forwarded.
func (d *DownTrack) Current() int32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

// DownTrackStats is a point-in-time view of what one subscriber receives.
type DownTrackStats struct {
	Packets uint64
	Bytes   uint64
	NACKs   uint64
	PLIs    uint64
	RTT     time.Duration
	Jitter  time.Duration
	// Loss is the fraction lost in the last receiver report, from 0 to 1.
	Loss float32
}

// Stats returns current counters.
func (d *DownTrack) Stats() DownTrackStats {
	return DownTrackStats{
		Packets: d.packets.Load(),
		Bytes:   d.bytes.Load(),
		NACKs:   d.nacks.Load(),
		PLIs:    d.plis.Load(),
		RTT:     time.Duration(d.rtt.Load()),
		Jitter:  time.Duration(d.jitter.Load()),
		Loss:    float32(d.loss.Load()) / 256,
	}
}

// Close stops forwarding. It is safe to call more than once.
func (d *DownTrack) Close() {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		d.receiver.RemoveDownTrack(d)
		if d.onClosed != nil {
			d.onClosed()
		}
	})
}
