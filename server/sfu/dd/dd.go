// Package dd reads and writes the dependency descriptor RTP header extension of the AV1 RTP specification. It says, for
// every packet, which frame the packet belongs to, which spatial and temporal layer that frame is in, and which earlier
// frames it needs. Browsers send it for VP9 and AV1 SVC, and it is what lets a server choose layers frame by frame
// without understanding the codec.
//
// A descriptor cannot be read on its own. Most of what it says is an index into a table of templates, and that table, the
// structure, is sent only on key frames.
package dd

import (
	"errors"
	"fmt"
	"slices"
)

// URI names the header extension in SDP.
const URI = "https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension"

// Limits from the specification.
const (
	MaxSpatialLayers  = 4
	MaxTemporalLayers = 8
	MaxDecodeTargets  = 32
	MaxTemplates      = 64
)

// DTI is a decode target indication: what one frame means to one decode target.
type DTI uint8

// Decode target indications.
const (
	// NotPresent means the frame is not part of the target, and forwarding it for that target wastes bandwidth.
	NotPresent DTI = iota
	// Discardable means the target can do without the frame, and no later frame in the target needs it.
	Discardable
	// Switch means the target can be joined at this frame.
	Switch
	// Required means the target needs the frame, and the frame is not a way in.
	Required
)

// Template is one row of a structure, and also what a frame resolves to once its custom fields apply.
type Template struct {
	Spatial    int
	Temporal   int
	DTIs       []DTI
	FrameDiffs []int
	ChainDiffs []int
}

// Clone returns a copy that shares nothing with t.
func (t Template) Clone() Template {
	t.DTIs = slices.Clone(t.DTIs)
	t.FrameDiffs = slices.Clone(t.FrameDiffs)
	t.ChainDiffs = slices.Clone(t.ChainDiffs)
	return t
}

// Resolution is the size of one spatial layer.
type Resolution struct {
	Width, Height int
}

// Structure is the template table a key frame carries.
type Structure struct {
	// ID is template_id_offset. A frame's template index is relative to it, so two structures can be told apart.
	ID            int
	DecodeTargets int
	Chains        int
	// TargetChain says which chain protects each decode target. Empty when there are no chains.
	TargetChain []int
	Templates   []Template
	// Resolutions has one entry per spatial layer, or none.
	Resolutions []Resolution
}

// Descriptor is one packet's dependency descriptor, resolved through its structure.
type Descriptor struct {
	FirstPacket bool
	LastPacket  bool
	FrameNumber uint16
	// Frame holds the frame's layers and dependencies. It may share slices with a structure template, so treat it as
	// read-only.
	Frame Template
	// Attached is the structure this packet carries, or nil.
	Attached *Structure
	// ActiveTargets has a bit for each decode target the publisher is producing, or is nil if this packet does not say.
	// A structure implies all of them.
	ActiveTargets *uint32
}

// Errors from Read.
var (
	// ErrNoStructure means the packet needs a structure and none has been seen yet. This is normal before the first key
	// frame reaches this server.
	ErrNoStructure = errors.New("dependency descriptor: no structure yet")
	// ErrBadTemplate means the packet names a template its structure does not have, usually because the key frame that
	// brought the structure in use was lost.
	ErrBadTemplate = errors.New("dependency descriptor: template is not in the structure")
	errMalformed   = errors.New("dependency descriptor: malformed")
)

// Read parses an extension's bytes. current is the structure in force, which may be nil.
func Read(data []byte, current *Structure) (*Descriptor, error) {
	if len(data) < 3 {
		return nil, errShortBuffer
	}
	r := bitReader{data: data}
	d := &Descriptor{}
	var err error
	if d.FirstPacket, err = r.flag(); err != nil {
		return nil, err
	}
	if d.LastPacket, err = r.flag(); err != nil {
		return nil, err
	}
	templateID, err := r.bits(6)
	if err != nil {
		return nil, err
	}
	frameNumber, err := r.bits(16)
	if err != nil {
		return nil, err
	}
	d.FrameNumber = uint16(frameNumber)

	var maskPresent, customDTIs, customDiffs, customChains bool
	if len(data) > 3 {
		var structurePresent bool
		for _, field := range []*bool{&structurePresent, &maskPresent, &customDTIs, &customDiffs, &customChains} {
			if *field, err = r.flag(); err != nil {
				return nil, err
			}
		}
		if structurePresent {
			if d.Attached, err = readStructure(&r); err != nil {
				return nil, err
			}
			current = d.Attached
			all := uint32(uint64(1)<<current.DecodeTargets - 1)
			d.ActiveTargets = &all
		}
	}
	if current == nil {
		return nil, ErrNoStructure
	}
	if maskPresent {
		mask, err := r.bits(current.DecodeTargets)
		if err != nil {
			return nil, err
		}
		d.ActiveTargets = &mask
	}

	index := (int(templateID) + MaxTemplates - current.ID) % MaxTemplates
	if index >= len(current.Templates) {
		return nil, ErrBadTemplate
	}
	d.Frame = current.Templates[index]
	if customDTIs {
		d.Frame.DTIs = make([]DTI, current.DecodeTargets)
		for i := range d.Frame.DTIs {
			v, err := r.bits(2)
			if err != nil {
				return nil, err
			}
			d.Frame.DTIs[i] = DTI(v)
		}
	}
	if customDiffs {
		d.Frame.FrameDiffs = nil
		for {
			size, err := r.bits(2)
			if err != nil {
				return nil, err
			}
			if size == 0 {
				break
			}
			v, err := r.bits(int(size) * 4)
			if err != nil {
				return nil, err
			}
			d.Frame.FrameDiffs = append(d.Frame.FrameDiffs, int(v)+1)
		}
	}
	if customChains {
		d.Frame.ChainDiffs = make([]int, current.Chains)
		for i := range d.Frame.ChainDiffs {
			v, err := r.bits(8)
			if err != nil {
				return nil, err
			}
			d.Frame.ChainDiffs[i] = int(v)
		}
	}
	return d, nil
}

func readStructure(r *bitReader) (*Structure, error) {
	id, err := r.bits(6)
	if err != nil {
		return nil, err
	}
	targets, err := r.bits(5)
	if err != nil {
		return nil, err
	}
	s := &Structure{ID: int(id), DecodeTargets: int(targets) + 1}

	// Layers. Each template says how the next one's layers differ, so the table is always in layer order.
	spatial, temporal := 0, 0
	for {
		if len(s.Templates) == MaxTemplates {
			return nil, fmt.Errorf("%w: too many templates", errMalformed)
		}
		s.Templates = append(s.Templates, Template{Spatial: spatial, Temporal: temporal})
		next, err := r.bits(2)
		if err != nil {
			return nil, err
		}
		if next == 3 {
			break
		}
		switch next {
		case 1:
			temporal++
			if temporal >= MaxTemporalLayers {
				return nil, fmt.Errorf("%w: too many temporal layers", errMalformed)
			}
		case 2:
			spatial++
			temporal = 0
			if spatial >= MaxSpatialLayers {
				return nil, fmt.Errorf("%w: too many spatial layers", errMalformed)
			}
		}
	}

	for i := range s.Templates {
		s.Templates[i].DTIs = make([]DTI, s.DecodeTargets)
		for j := range s.Templates[i].DTIs {
			v, err := r.bits(2)
			if err != nil {
				return nil, err
			}
			s.Templates[i].DTIs[j] = DTI(v)
		}
	}

	for i := range s.Templates {
		for {
			follows, err := r.flag()
			if err != nil {
				return nil, err
			}
			if !follows {
				break
			}
			v, err := r.bits(4)
			if err != nil {
				return nil, err
			}
			s.Templates[i].FrameDiffs = append(s.Templates[i].FrameDiffs, int(v)+1)
		}
	}

	chains, err := r.nonSymmetric(uint32(s.DecodeTargets) + 1)
	if err != nil {
		return nil, err
	}
	s.Chains = int(chains)
	if s.Chains > 0 {
		s.TargetChain = make([]int, s.DecodeTargets)
		for i := range s.TargetChain {
			v, err := r.nonSymmetric(chains)
			if err != nil {
				return nil, err
			}
			s.TargetChain[i] = int(v)
		}
		for i := range s.Templates {
			s.Templates[i].ChainDiffs = make([]int, s.Chains)
			for j := range s.Templates[i].ChainDiffs {
				v, err := r.bits(4)
				if err != nil {
					return nil, err
				}
				s.Templates[i].ChainDiffs[j] = int(v)
			}
		}
	}

	resolutions, err := r.flag()
	if err != nil {
		return nil, err
	}
	if resolutions {
		for range spatial + 1 {
			width, err := r.bits(16)
			if err != nil {
				return nil, err
			}
			height, err := r.bits(16)
			if err != nil {
				return nil, err
			}
			s.Resolutions = append(s.Resolutions, Resolution{int(width) + 1, int(height) + 1})
		}
	}
	return s, nil
}

// Write encodes d using structure s, which must be the one in force or the one attached. A frame number or active target
// change has to re-encode the descriptor rather than patch it: whether a mask is present changes where everything after
// it sits.
func Write(d *Descriptor, s *Structure) ([]byte, error) {
	if s == nil {
		return nil, ErrNoStructure
	}
	if d.Attached != nil {
		s = d.Attached
	}
	if len(d.Frame.DTIs) != s.DecodeTargets || len(d.Frame.ChainDiffs) != s.Chains {
		return nil, fmt.Errorf("%w: frame does not fit the structure", errMalformed)
	}
	match, ok := bestTemplate(d, s)
	if !ok {
		return nil, fmt.Errorf("%w: no template for spatial %d temporal %d", errMalformed, d.Frame.Spatial, d.Frame.Temporal)
	}

	w := bitWriter{data: make([]byte, 0, 16)}
	w.flag(d.FirstPacket)
	w.flag(d.LastPacket)
	w.bits(uint32((match.index+s.ID)%MaxTemplates), 6)
	w.bits(uint32(d.FrameNumber), 16)

	all := uint32(uint64(1)<<s.DecodeTargets - 1)
	writeMask := d.ActiveTargets != nil && (d.Attached == nil || *d.ActiveTargets != all)
	if d.Attached == nil && !writeMask && !match.dtis && !match.diffs && !match.chains {
		return w.data, nil
	}

	w.flag(d.Attached != nil)
	w.flag(writeMask)
	w.flag(match.dtis)
	w.flag(match.diffs)
	w.flag(match.chains)
	if d.Attached != nil {
		if err := writeStructure(&w, s); err != nil {
			return nil, err
		}
	}
	if writeMask {
		w.bits(*d.ActiveTargets, s.DecodeTargets)
	}
	if match.dtis {
		for _, v := range d.Frame.DTIs {
			w.bits(uint32(v), 2)
		}
	}
	if match.diffs {
		for _, diff := range d.Frame.FrameDiffs {
			switch {
			case diff <= 1<<4:
				w.bits(1, 2)
				w.bits(uint32(diff-1), 4)
			case diff <= 1<<8:
				w.bits(2, 2)
				w.bits(uint32(diff-1), 8)
			default:
				w.bits(3, 2)
				w.bits(uint32(diff-1), 12)
			}
		}
		w.bits(0, 2)
	}
	if match.chains {
		for i := range s.Chains {
			w.bits(uint32(d.Frame.ChainDiffs[i]), 8)
		}
	}
	return w.data, nil
}

type templateMatch struct {
	index               int
	dtis, diffs, chains bool
	extraBits           int
}

// bestTemplate finds the template of the frame's layers that needs the fewest custom fields. Only the templates of those
// layers qualify, because the template is the only thing that says what the layers are.
func bestTemplate(d *Descriptor, s *Structure) (templateMatch, bool) {
	best := templateMatch{index: -1}
	for i, t := range s.Templates {
		if t.Spatial != d.Frame.Spatial || t.Temporal != d.Frame.Temporal {
			continue
		}
		m := templateMatch{
			index:  i,
			dtis:   !slices.Equal(d.Frame.DTIs, t.DTIs),
			diffs:  !slices.Equal(d.Frame.FrameDiffs, t.FrameDiffs),
			chains: !slices.Equal(d.Frame.ChainDiffs, t.ChainDiffs),
		}
		if m.dtis {
			m.extraBits += 2 * s.DecodeTargets
		}
		if m.diffs {
			m.extraBits += 2
			for _, diff := range d.Frame.FrameDiffs {
				switch {
				case diff <= 1<<4:
					m.extraBits += 6
				case diff <= 1<<8:
					m.extraBits += 10
				default:
					m.extraBits += 14
				}
			}
		}
		if m.chains {
			m.extraBits += 8 * s.Chains
		}
		if best.index < 0 || m.extraBits < best.extraBits {
			best = m
		}
	}
	return best, best.index >= 0
}

func writeStructure(w *bitWriter, s *Structure) error {
	if s.ID < 0 || s.ID >= MaxTemplates || s.DecodeTargets < 1 || s.DecodeTargets > MaxDecodeTargets ||
		len(s.Templates) == 0 || len(s.Templates) > MaxTemplates {
		return fmt.Errorf("%w: structure out of range", errMalformed)
	}
	if s.Templates[0].Spatial != 0 || s.Templates[0].Temporal != 0 {
		return fmt.Errorf("%w: the first template must be in the base layer", errMalformed)
	}
	w.bits(uint32(s.ID), 6)
	w.bits(uint32(s.DecodeTargets-1), 5)
	for i := 1; i < len(s.Templates); i++ {
		prev, next := s.Templates[i-1], s.Templates[i]
		switch {
		case next.Spatial == prev.Spatial && next.Temporal == prev.Temporal:
			w.bits(0, 2)
		case next.Spatial == prev.Spatial && next.Temporal == prev.Temporal+1:
			w.bits(1, 2)
		case next.Spatial == prev.Spatial+1 && next.Temporal == 0:
			w.bits(2, 2)
		default:
			return fmt.Errorf("%w: templates out of layer order", errMalformed)
		}
	}
	w.bits(3, 2)
	for _, t := range s.Templates {
		if len(t.DTIs) != s.DecodeTargets {
			return fmt.Errorf("%w: template has %d indications for %d targets", errMalformed, len(t.DTIs), s.DecodeTargets)
		}
		for _, v := range t.DTIs {
			w.bits(uint32(v), 2)
		}
	}
	for _, t := range s.Templates {
		for _, diff := range t.FrameDiffs {
			w.flag(true)
			w.bits(uint32(diff-1), 4)
		}
		w.flag(false)
	}
	w.nonSymmetric(uint32(s.Chains), uint32(s.DecodeTargets)+1)
	if s.Chains > 0 {
		for _, chain := range s.TargetChain {
			w.nonSymmetric(uint32(chain), uint32(s.Chains))
		}
		for _, t := range s.Templates {
			if len(t.ChainDiffs) != s.Chains {
				return fmt.Errorf("%w: template has %d chain diffs for %d chains", errMalformed, len(t.ChainDiffs), s.Chains)
			}
			for _, diff := range t.ChainDiffs {
				w.bits(uint32(diff), 4)
			}
		}
	}
	w.flag(len(s.Resolutions) > 0)
	for _, res := range s.Resolutions {
		w.bits(uint32(res.Width-1), 16)
		w.bits(uint32(res.Height-1), 16)
	}
	return nil
}

// Target is one decode target and the highest layers it includes.
type Target struct {
	Index    int
	Spatial  int
	Temporal int
}

// Targets lists a structure's decode targets, highest first. A target's layers come from the templates: the highest
// spatial and temporal layer of any frame that is part of it.
func Targets(s *Structure) []Target {
	out := make([]Target, 0, s.DecodeTargets)
	for i := range s.DecodeTargets {
		t := Target{Index: i}
		for _, tmpl := range s.Templates {
			if i < len(tmpl.DTIs) && tmpl.DTIs[i] != NotPresent {
				t.Spatial = max(t.Spatial, tmpl.Spatial)
				t.Temporal = max(t.Temporal, tmpl.Temporal)
			}
		}
		out = append(out, t)
	}
	slices.SortStableFunc(out, func(a, b Target) int {
		if a.Spatial != b.Spatial {
			return b.Spatial - a.Spatial
		}
		return b.Temporal - a.Temporal
	})
	return out
}

// MaskFor returns the active target bits of everything at or below the given layers.
func MaskFor(targets []Target, spatial, temporal int) uint32 {
	var mask uint32
	for _, t := range targets {
		if t.Spatial <= spatial && t.Temporal <= temporal {
			mask |= 1 << t.Index
		}
	}
	return mask
}
