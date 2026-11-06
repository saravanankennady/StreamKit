package sfu

import (
	"github.com/pion/rtp/codecs"

	"elnova-streamkit.local/server/sfu/dd"
)

// decision is what became of a frame for one subscriber.
type decision uint8

const (
	// decisionUnknown means the frame has not been seen, and may yet arrive through loss recovery.
	decisionUnknown decision = iota
	// decisionMissing means it is too old to arrive now.
	decisionMissing
	decisionDropped
	// decisionPartial means it is being forwarded, and not all of its packets have arrived yet.
	decisionPartial
	decisionForwarded
)

const (
	frameDecisionWindow = 512
	// A frame this far behind the newest has had its chance to be retransmitted.
	frameRecoveryWindow = 80
)

type frameDecision struct {
	frame uint64
	d     decision
	set   bool
}

// frameDecisions remembers the fate of recent frames. A packet of a frame that has been decided gets the same answer, and
// a frame whose references were dropped must be dropped too.
type frameDecisions struct {
	entries [frameDecisionWindow]frameDecision
	base    uint64
	last    uint64
	started bool
}

func (c *frameDecisions) get(frame uint64) decision {
	if !c.started || frame < c.base {
		return decisionMissing
	}
	if frame > c.last {
		return decisionUnknown
	}
	if c.last-frame >= frameDecisionWindow {
		return decisionMissing
	}
	if e := c.entries[frame%frameDecisionWindow]; e.set && e.frame == frame {
		return e.d
	}
	if c.last-frame >= frameRecoveryWindow {
		return decisionMissing
	}
	return decisionUnknown
}

func (c *frameDecisions) put(frame uint64, d decision) {
	if !c.started {
		c.started, c.base, c.last = true, frame, frame
	}
	if frame < c.base {
		return
	}
	c.entries[frame%frameDecisionWindow] = frameDecision{frame: frame, d: d, set: true}
	if frame > c.last {
		c.last = frame
	}
}

// tooOld reports whether a frame is older than anything this cache can answer for: past its window, or from before this
// subscriber's first frame, which nothing it has received can depend on.
func (c *frameDecisions) tooOld(frame uint64) bool {
	return c.started && (frame < c.base || (frame < c.last && c.last-frame >= frameDecisionWindow))
}

// frameChain follows one chain: the frames a decode target can be reached through. While every frame in the chain has
// been forwarded, a decoder following it is in a good state.
type frameChain struct {
	active bool
	broken bool
	// pending holds frames the chain depends on that have not been decided yet. A late packet must not break a chain.
	pending []uint64
}

func (c *frameChain) onFrame(frame uint64, diff int, decisions *frameDecisions) {
	if !c.active {
		return
	}
	// A frame with no predecessor in the chain is how a chain becomes usable.
	if diff == 0 {
		c.broken = false
		c.pending = c.pending[:0]
		return
	}
	c.settlePending(decisions)
	if c.broken {
		return
	}
	previous := frame - uint64(diff)
	switch decisions.get(previous) {
	case decisionForwarded:
	case decisionUnknown, decisionPartial:
		c.pending = append(c.pending, previous)
	default:
		c.broken = true
		c.pending = c.pending[:0]
	}
}

func (c *frameChain) settlePending(decisions *frameDecisions) {
	kept := c.pending[:0]
	for _, frame := range c.pending {
		switch decisions.get(frame) {
		case decisionForwarded:
		case decisionUnknown, decisionPartial:
			kept = append(kept, frame)
		default:
			c.broken = true
		}
	}
	c.pending = kept
	if c.broken {
		c.pending = c.pending[:0]
	}
}

type decodeTarget struct {
	dd.Target
	chain  *frameChain
	active bool
}

func (t *decodeTarget) valid() bool { return t.chain == nil || !t.chain.broken }

// frameNumberWrapper keeps frame numbers moving forward for a subscriber, across a publisher restarting them. It moves only
// when a structure arrives: within a group of pictures, frame references are differences, and they would break.
type frameNumberWrapper struct {
	started bool
	offset  uint64
	last    uint64
}

func (w *frameNumberWrapper) next(frame uint64, atStructure bool) uint16 {
	if !w.started {
		w.started, w.last = true, frame
		return uint16(frame)
	}
	if frame <= w.last {
		return uint16(frame + w.offset)
	}
	if atStructure {
		next, last := uint16(frame+w.offset), uint16(w.last+w.offset)
		if diff := next - last; diff > 0x8000 || (diff == 0x8000 && next < last) {
			// A jump that a receiver would read as going backwards. Go far enough past it to be plainly forward.
			w.offset += uint64(65535 - diff + 6000)
		}
	}
	w.last = frame
	return uint16(frame + w.offset)
}

// layerChoice is what a selector decided about one packet.
type layerChoice struct {
	selected bool
	marker   bool
	// ext is the dependency descriptor to send. With keepExt set, the packet's own is right as it stands.
	ext     []byte
	keepExt bool
	// spatial is the layer the subscriber is now on, once it changed.
	spatial int32
}

// svcSelector chooses frames for one subscriber, from one stream's dependency descriptors.
type svcSelector struct {
	decisions frameDecisions
	structure *dd.Structure
	targets   []decodeTarget
	chains    []frameChain
	keyFrame  uint64
	keyValid  bool
	wrapper   frameNumberWrapper
	// generation is the stream restart this subscriber last saw, and active the decode targets the publisher last had on.
	generation int
	active     uint32

	hasCurrent bool
	current    dd.Target
	mask       uint32
}

func (s *svcSelector) updateStructure(f *SVCFrame) {
	s.structure = f.Structure
	s.keyFrame = f.KeyFrame
	s.keyValid = true
	s.chains = make([]frameChain, f.Structure.Chains)
	for i := range s.chains {
		s.chains[i].broken = true
	}
	s.targets = make([]decodeTarget, 0, len(f.Targets))
	for _, t := range f.Targets {
		target := decodeTarget{Target: t}
		if f.Structure.Chains > 0 && t.Index < len(f.Structure.TargetChain) {
			if c := f.Structure.TargetChain[t.Index]; c < len(s.chains) {
				target.chain = &s.chains[c]
			}
		}
		s.targets = append(s.targets, target)
	}
	if s.hasCurrent {
		// The same layers under the new structure's numbering.
		s.mask = dd.MaskFor(f.Targets, s.current.Spatial, s.current.Temporal)
	}
}

func (s *svcSelector) updateActive(mask uint32) {
	wasActive := make([]bool, len(s.chains))
	for i := range s.chains {
		wasActive[i] = s.chains[i].active
		s.chains[i].active = false
	}
	for i := range s.targets {
		t := &s.targets[i]
		t.active = mask&(1<<t.Index) != 0
		if t.active && t.chain != nil {
			t.chain.active = true
		}
	}
	for i := range s.chains {
		// A chain that comes back has missed frames, and has to wait for a way in.
		if s.chains[i].active && !wasActive[i] {
			s.chains[i].broken = true
		}
	}
}

func (s *svcSelector) invalidate() {
	s.keyValid = false
	s.chains = nil
	s.targets = nil
}

// reset forgets everything but frame numbering. After a pause, what the subscriber's decoder holds is no use for what comes
// next, so it starts from a key frame like anyone else.
func (s *svcSelector) reset() {
	s.decisions = frameDecisions{}
	s.invalidate()
	s.hasCurrent = false
}

// locked reports whether the subscriber can be given the spatial layer it asked for right now, with no key frame.
func (s *svcSelector) locked(spatial int32) bool {
	if !s.hasCurrent || !s.keyValid {
		return false
	}
	for i := range s.targets {
		t := &s.targets[i]
		if t.active && int32(t.Spatial) == spatial && t.valid() {
			return true
		}
	}
	return false
}

// choose decides about one packet. wantSpatial and wantTemporal are the highest layers the subscriber is to get.
func (s *svcSelector) choose(f *SVCFrame, marker bool, wantSpatial, wantTemporal int32, writeExt bool) layerChoice {
	var none layerChoice
	if f == nil {
		return none
	}
	d := f.Descriptor
	if f.Generation != s.generation {
		// The stream started over, and its numbers mean nothing against what is remembered. The subscriber stays on its layer
		// if that turns out to exist.
		s.generation = f.Generation
		s.decisions = frameDecisions{}
		s.invalidate()
	}
	if !s.keyValid && !f.StructureUpdated {
		return none
	}
	if s.decisions.tooOld(f.Frame) {
		return none
	}
	already := s.decisions.get(f.Frame)
	if f.StructureUpdated && already != decisionDropped {
		s.updateStructure(f)
	}
	if f.KeyFrame != s.keyFrame {
		// The frame belongs to a key frame this subscriber never saw. There is no way to decode from here.
		s.decisions.put(f.Frame, decisionDropped)
		s.invalidate()
		return none
	}
	// The stream's current mask, because the packet that changed it may not have been one this subscriber looked at.
	if f.StructureUpdated || f.ActiveMask != s.active {
		s.active = f.ActiveMask
		s.updateActive(f.ActiveMask)
	}
	if already == decisionDropped {
		return none
	}

	if already != decisionPartial && already != decisionForwarded {
		if !s.decide(f, wantSpatial, wantTemporal) {
			s.decisions.put(f.Frame, decisionDropped)
			return none
		}
	}

	out := layerChoice{selected: true, spatial: int32(s.current.Spatial)}
	out.marker = marker || (d.LastPacket && s.current.Spatial == d.Frame.Spatial)
	number := s.wrapper.next(f.Frame, f.StructureUpdated)
	if writeExt {
		if d.Attached != nil && number == d.FrameNumber {
			out.keepExt = true
		} else {
			clone := *d
			clone.FrameNumber = number
			if d.Attached == nil {
				mask := s.mask
				clone.ActiveTargets = &mask
			}
			ext, err := dd.Write(&clone, s.structure)
			if err != nil {
				s.decisions.put(f.Frame, decisionDropped)
				return none
			}
			out.ext = ext
		}
	}
	if f.Complete {
		s.decisions.put(f.Frame, decisionForwarded)
	} else if already != decisionForwarded {
		s.decisions.put(f.Frame, decisionPartial)
	}
	return out
}

// decide works out whether a new frame goes to this subscriber, moving it between layers if the frame allows that.
func (s *svcSelector) decide(f *SVCFrame, wantSpatial, wantTemporal int32) bool {
	frame := &f.Descriptor.Frame
	if len(frame.ChainDiffs) != len(s.chains) {
		return false
	}
	for i := range s.chains {
		s.chains[i].onFrame(f.Frame, frame.ChainDiffs[i], &s.decisions)
	}

	var chosen *decodeTarget
	for i := range s.targets {
		t := &s.targets[i]
		if t.active && int32(t.Spatial) <= wantSpatial && int32(t.Temporal) <= wantTemporal && t.valid() {
			chosen = t
			break
		}
	}
	// What was asked for cannot be decoded yet. Stay on what the subscriber has until a key frame opens the way, rather
	// than show nothing.
	if chosen == nil && s.hasCurrent {
		for i := range s.targets {
			t := &s.targets[i]
			if t.Spatial == s.current.Spatial && t.Temporal == s.current.Temporal && t.active && t.valid() {
				chosen = t
				break
			}
		}
	}
	if chosen == nil || chosen.Index >= len(frame.DTIs) || frame.DTIs[chosen.Index] == dd.NotPresent {
		return false
	}
	// A reference that is merely unknown might still arrive.
	for _, diff := range frame.FrameDiffs {
		if diff != 0 && s.decisions.get(f.Frame-uint64(diff)) == decisionDropped {
			return false
		}
	}
	if !s.hasCurrent || s.current != chosen.Target {
		s.hasCurrent = true
		s.current = chosen.Target
		s.mask = dd.MaskFor(f.Targets, chosen.Spatial, chosen.Temporal)
	}
	return true
}

// vp9Selector chooses for a subscriber using VP9's own payload descriptor. Every frame of a lower spatial layer passes,
// because an upper one may be predicted from it, and nothing here says otherwise. That is plain L3T3 behaviour, and with
// L3T3_KEY it costs about half as much bandwidth again as the descriptor path.
type vp9Selector struct {
	valid    bool
	spatial  int32
	temporal int32
}

func (s *vp9Selector) reset() { s.valid = false }

// locked reports whether the subscriber is on the spatial layer it asked for.
func (s *vp9Selector) locked(spatial int32) bool { return s.valid && s.spatial == spatial }

func (s *vp9Selector) choose(p *codecs.VP9Packet, keyframe, marker bool, wantSpatial, wantTemporal int32) layerChoice {
	if p == nil {
		return layerChoice{}
	}
	sid, tid := int32(0), int32(0)
	if p.L {
		sid, tid = int32(p.SID), int32(p.TID)
	}
	// The layer this packet is judged against, which moves up as soon as this packet is a way in.
	spatial, temporal := s.spatial, s.temporal
	if !s.valid {
		if !keyframe {
			return layerChoice{}
		}
		s.valid = true
		s.spatial, s.temporal = sid, tid
		spatial, temporal = sid, tid
	} else {
		if s.temporal < wantTemporal {
			if tid > s.temporal && tid <= wantTemporal && p.U && p.B {
				s.temporal = tid
				temporal = tid
			}
		} else if s.temporal > wantTemporal && p.E {
			s.temporal = wantTemporal
		}
		if s.spatial < wantSpatial {
			if sid > s.spatial && sid <= wantSpatial && !p.P && p.B {
				s.spatial = sid
				spatial = sid
			}
		} else if s.spatial > wantSpatial && p.E {
			s.spatial = wantSpatial
		}
	}
	selected := sid < spatial || (sid == spatial && tid <= temporal)
	out := layerChoice{selected: selected, marker: marker, spatial: s.spatial}
	if p.E && sid == spatial && (p.P || wantSpatial <= s.spatial) {
		out.marker = true
	}
	return out
}
