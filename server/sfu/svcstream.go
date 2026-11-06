package sfu

import (
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"elnova-streamkit.local/server/sfu/dd"
)

// Selection says how subscribers choose from a packet's stream.
type Selection uint8

const (
	// SelectStream is plain forwarding: a subscriber takes all of one stream or none of it. Simulcast, audio, and any codec
	// with one layer per stream work this way.
	SelectStream Selection = iota
	// SelectDescriptor chooses frame by frame using the dependency descriptor. Official clients always send one with VP9
	// and AV1 SVC.
	SelectDescriptor
	// SelectVP9 chooses using VP9's own payload descriptor, for a publisher that sends no dependency descriptor. It is less
	// exact: it cannot tell what an upper layer needs, so it forwards every lower layer.
	SelectVP9
)

// SVCFrame is what the receive loop learned from one packet's dependency descriptor. It is shared by all subscribers and
// must not be changed.
type SVCFrame struct {
	Descriptor *dd.Descriptor
	// Structure is the structure in force, and Targets its decode targets, highest first.
	Structure *dd.Structure
	Targets   []dd.Target
	// Frame is the frame number extended across wraps, and KeyFrame the frame that brought the structure in.
	Frame    uint64
	KeyFrame uint64
	// StructureUpdated marks a packet carrying a structure. ActiveMask holds which targets the publisher is producing now.
	StructureUpdated bool
	ActiveMask       uint32
	// Generation counts restarts of the stream. Nothing from before one can be used after it.
	Generation int
	// Complete says every packet of this frame has now arrived. A frame only counts as forwarded when it has, because
	// a frame referring to a part of one cannot be decoded.
	Complete bool
}

// Reasons a descriptor's packet cannot be used. Anything else is a broken descriptor.
var (
	errFrameBeforeStructure = errors.New("frame is older than the structure in force")
	errStructureNotFirst    = errors.New("a structure on a packet that does not start a frame")
)

// How long a frame record lasts, in frames. A frame with this many later ones has gone, so its record can be reused.
const svcFrameRecords = 256

// unwrap16 extends a 16-bit counter that can move backwards a little across wraps.
type unwrap16 struct {
	started bool
	ext     uint64
}

func (u *unwrap16) update(v uint16) uint64 {
	if !u.started {
		u.started = true
		// Far from zero, so that a stream's first values can still go backwards.
		u.ext = uint64(1)<<16 | uint64(v)
		return u.ext
	}
	diff := int16(v - uint16(u.ext))
	out := uint64(int64(u.ext) + int64(diff))
	if diff > 0 {
		u.ext = out
	}
	return out
}

// jump takes a value as new whatever it looks like, for a stream that has restarted and owes nothing to earlier numbers.
func (u *unwrap16) jump(v uint16) uint64 {
	u.started = true
	u.ext += uint64(v - uint16(u.ext))
	return u.ext
}

// frameRecord tracks which packets of a frame have arrived.
type frameRecord struct {
	frame    uint64
	used     bool
	first    uint64
	last     uint64
	hasFirst bool
	hasLast  bool
	packets  uint32
}

// frameTracker answers whether a frame is complete, given that packets can arrive twice or out of order.
type frameTracker struct {
	records [svcFrameRecords]frameRecord
	seen    [2048]uint64
}

func (t *frameTracker) add(extSeq, frame uint64, first, last bool) bool {
	r := &t.records[frame%svcFrameRecords]
	if !r.used || r.frame != frame {
		*r = frameRecord{frame: frame, used: true}
	}
	slot := &t.seen[extSeq%uint64(len(t.seen))]
	if *slot != extSeq {
		*slot = extSeq
		r.packets++
	}
	if first {
		r.hasFirst, r.first = true, extSeq
	}
	if last {
		r.hasLast, r.last = true, extSeq
	}
	return r.hasFirst && r.hasLast && r.last >= r.first && uint64(r.packets) == r.last-r.first+1
}

// svcParser follows the dependency descriptors of one incoming stream. It belongs to that stream's receive loop.
type svcParser struct {
	structure *dd.Structure
	targets   []dd.Target

	frames unwrap16
	seqs   unwrap16
	// keyFrame is the frame that brought in the structure in force. changeFrame only moves when a structure really
	// changes: a key frame that repeats one leaves late frames from before it usable.
	keyFrame    uint64
	changeFrame uint64

	hasMask bool
	mask    uint32
	maskSeq uint64

	tracker frameTracker

	// A publisher silent for this long may come back with numbers that have nothing to do with the old ones.
	lastPacket time.Time
	generation int

	onMaskChanged func(mask uint32, targets []dd.Target)
}

func (s *svcParser) parse(ext []byte, seq uint16) (*SVCFrame, error) {
	return s.parseAt(ext, seq, time.Now())
}

func (s *svcParser) parseAt(ext []byte, seq uint16, now time.Time) (*SVCFrame, error) {
	restarted := !s.lastPacket.IsZero() && now.Sub(s.lastPacket) > svcRestartGap
	s.lastPacket = now
	if restarted {
		s.structure, s.targets = nil, nil
		s.keyFrame, s.changeFrame = 0, 0
		s.hasMask, s.maskSeq = false, 0
		s.tracker = frameTracker{}
		s.generation++
	}
	d, err := dd.Read(ext, s.structure)
	if err != nil {
		return nil, err
	}
	var frame uint64
	if restarted {
		frame = s.frames.jump(d.FrameNumber)
	} else {
		frame = s.frames.update(d.FrameNumber)
	}
	extSeq := s.seqs.update(seq)
	if frame < s.changeFrame {
		return nil, errFrameBeforeStructure
	}
	out := &SVCFrame{Descriptor: d, Frame: frame, Generation: s.generation}
	out.Complete = s.tracker.add(extSeq, frame, d.FirstPacket, d.LastPacket)
	if d.Attached != nil {
		if !d.FirstPacket {
			return nil, errStructureNotFirst
		}
		if frame < s.keyFrame {
			// A key frame arriving late must not put the old structure back.
			return nil, errFrameBeforeStructure
		}
		if s.structure == nil || s.structure.ID != d.Attached.ID {
			s.changeFrame = frame
		}
		s.structure = d.Attached
		s.targets = dd.Targets(d.Attached)
		s.keyFrame = frame
		out.StructureUpdated = true
	}
	if m := d.ActiveTargets; m != nil && (!s.hasMask || extSeq > s.maskSeq) {
		changed := !s.hasMask || *m != s.mask
		s.hasMask, s.mask, s.maskSeq = true, *m, extSeq
		if changed && s.onMaskChanged != nil {
			s.onMaskChanged(*m, s.targets)
		}
	}
	out.Structure = s.structure
	out.Targets = s.targets
	out.KeyFrame = s.keyFrame
	out.ActiveMask = s.mask
	return out, nil
}

const (
	// How long after a stream starts its first key frame is still asked for. Nothing can be selected from a stream until a
	// structure arrives, and there is no knowing which layers exist either.
	seedKeyframeFor = 30 * time.Second
	svcRestartGap   = 30 * time.Second
	// maxTemporalLayer means no limit on temporal layers.
	maxTemporalLayer = 7
)

// svcLayerStats holds what one spatial layer costs and how often its frames arrive.
type svcLayerStats struct {
	state layerState
	// frames counts frames by temporal layer, and rate is the last second's count.
	frames     [maxTemporalLayer + 1]atomic.Uint64
	lastFrames [maxTemporalLayer + 1]uint64
	rate       [maxTemporalLayer + 1]atomic.Int32
	// lastSeen is when a packet of this spatial layer last arrived, for a stream with no active mask to say.
	lastSeen atomic.Int64
}

// svcStream is a Receiver's view of the one RTP stream that carries all of a track's layers.
type svcStream struct {
	r     *Receiver
	ddID  uint8
	mode  atomic.Int32
	began bool
	// parser is only touched by the receive loop.
	parser svcParser
	// Which layers the publisher is producing now. -1 until known.
	maxSpatial  atomic.Int32
	maxTemporal atomic.Int32
	stats       [MaxLayers]svcLayerStats
	startedAt   atomic.Int64
	hasKeyframe atomic.Bool
}

func newSVCStream(r *Receiver, ddID uint8) *svcStream {
	s := &svcStream{r: r, ddID: ddID}
	s.maxSpatial.Store(-1)
	s.maxTemporal.Store(-1)
	s.parser.onMaskChanged = s.onMaskChanged
	return s
}

func (s *svcStream) onMaskChanged(mask uint32, targets []dd.Target) {
	spatial, temporal := int32(-1), int32(-1)
	for _, t := range targets {
		if mask&(1<<t.Index) != 0 {
			spatial = max(spatial, int32(t.Spatial))
			temporal = max(temporal, int32(t.Temporal))
		}
	}
	s.maxSpatial.Store(spatial)
	s.maxTemporal.Store(temporal)
}

// selection returns how subscribers choose from this stream, once a first packet has said.
func (s *svcStream) selection() Selection { return Selection(s.mode.Load()) }

// observe fills in what subscribers need to choose from this packet. A stream that negotiated the descriptor but does not
// send one on its first packet is not going to: select by what the codec itself says, or if it says nothing, forward it all.
func (s *svcStream) observe(pkt *rtp.Packet, p *Packet, size uint64, mime string) {
	var ext []byte
	if s.ddID != 0 {
		ext = pkt.Header.GetExtension(s.ddID)
	}
	// A browser's bandwidth probes have nothing in them, and say nothing about how the stream is made.
	if !s.began && len(pkt.Payload) == 0 {
		return
	}
	if !s.began {
		s.began = true
		s.startedAt.Store(time.Now().UnixNano())
		switch {
		case len(ext) > 0:
			s.mode.Store(int32(SelectDescriptor))
		case strings.EqualFold(mime, "video/vp9"):
			s.mode.Store(int32(SelectVP9))
		default:
			s.mode.Store(int32(SelectStream))
		}
	}
	p.Selection = s.selection()
	switch p.Selection {
	case SelectDescriptor:
		if len(ext) == 0 {
			return
		}
		f, err := s.parser.parse(ext, pkt.SequenceNumber)
		if err != nil {
			return
		}
		p.SVC = f
		if f.StructureUpdated {
			s.hasKeyframe.Store(true)
		}
		s.countDescriptor(f, size)
	case SelectVP9:
		vp9 := &codecs.VP9Packet{}
		if _, err := vp9.Unmarshal(pkt.Payload); err != nil {
			return
		}
		p.VP9 = vp9
		if p.KeyframeStart {
			s.hasKeyframe.Store(true)
		}
		s.countVP9(vp9, size)
	}
}

// countDescriptor charges a packet to each spatial layer that needs it: every target that uses the frame, whichever layer
// the frame is in. So a layer's figure is what a subscriber to it would actually receive.
func (s *svcStream) countDescriptor(f *SVCFrame, size uint64) {
	frame := &f.Descriptor.Frame
	for spatial := range MaxLayers {
		// The top temporal target of this spatial layer, which is the first one in the list.
		for _, t := range f.Targets {
			if t.Spatial != spatial {
				continue
			}
			if t.Index < len(frame.DTIs) && frame.DTIs[t.Index] != dd.NotPresent {
				s.stats[spatial].state.bytes.Add(size)
			}
			break
		}
	}
	if frame.Spatial < MaxLayers && frame.Temporal <= maxTemporalLayer && f.Descriptor.FirstPacket {
		s.stats[frame.Spatial].frames[frame.Temporal].Add(1)
	}
}

// countVP9 charges a packet to its spatial layer and every one above it, because without a descriptor all lower layers
// get forwarded.
func (s *svcStream) countVP9(p *codecs.VP9Packet, size uint64) {
	spatial := int(p.SID)
	if !p.L {
		spatial = 0
	}
	if spatial >= MaxLayers {
		return
	}
	now := time.Now().UnixNano()
	s.stats[spatial].lastSeen.Store(now)
	for i := spatial; i < MaxLayers; i++ {
		s.stats[i].state.bytes.Add(size)
	}
	if p.B && p.TID <= maxTemporalLayer {
		s.stats[spatial].frames[p.TID].Add(1)
	}
}

// topSpatial returns the highest spatial layer being produced, or -1 if that is not known yet.
func (s *svcStream) topSpatial(now int64) int32 {
	if s.selection() == SelectVP9 {
		for i := int32(MaxLayers) - 1; i >= 0; i-- {
			if seen := s.stats[i].lastSeen.Load(); seen != 0 && now-seen < int64(minLayerSilence)*layerSilenceFactor {
				return i
			}
		}
		return -1
	}
	return s.maxSpatial.Load()
}

// updateStats turns the last second's counts into rates. It runs under the Receiver's lock, once a second.
func (s *svcStream) updateStats() {
	for i := range s.stats {
		l := &s.stats[i]
		total := l.state.bytes.Load()
		rate := int64(total-l.state.lastBytes) * 8
		l.state.bitrate.Store(rate)
		l.state.recordReading(rate)
		l.state.lastBytes = total
		for t := range l.frames {
			count := l.frames[t].Load()
			l.rate[t].Store(int32(count - l.lastFrames[t]))
			l.lastFrames[t] = count
		}
	}
}

// temporalForFPS picks the lowest temporal layer that gives at least about the wanted frame rate on a spatial layer,
// from the rates measured. It is a little generous, because a measured rate is never exactly a nominal one. With nothing
// measured yet, it leaves the rate alone.
func (s *svcStream) temporalForFPS(spatial int32, fps uint32) int32 {
	if fps == 0 || spatial < 0 || spatial >= MaxLayers {
		return maxTemporalLayer
	}
	want := float64(fps) * 0.9
	total := int32(0)
	best := int32(-1)
	for t := int32(0); t <= maxTemporalLayer; t++ {
		rate := s.stats[spatial].rate[t].Load()
		if rate == 0 {
			continue
		}
		total += rate
		best = t
		if float64(total) >= want {
			return t
		}
	}
	if best < 0 {
		return maxTemporalLayer
	}
	return best
}
