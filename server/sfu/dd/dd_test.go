package dd

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
)

const (
	dash = NotPresent
	disc = Discardable
	swch = Switch
)

// l3t3key is the structure Chrome sends for VP9 L3T3_KEY, as observed on the wire (GAP-022).
func l3t3key() *Structure {
	row := func(s, t int, dtis []DTI, diffs, chains []int) Template {
		return Template{Spatial: s, Temporal: t, DTIs: dtis, FrameDiffs: diffs, ChainDiffs: chains}
	}
	return &Structure{
		ID: 0, DecodeTargets: 9, Chains: 3,
		TargetChain: []int{0, 0, 0, 1, 1, 1, 2, 2, 2},
		Resolutions: []Resolution{{320, 180}, {640, 360}, {1280, 720}},
		Templates: []Template{
			row(0, 0, []DTI{swch, swch, swch, swch, swch, swch, swch, swch, swch}, nil, []int{0, 0, 0}),
			row(0, 0, []DTI{swch, swch, swch, dash, dash, dash, dash, dash, dash}, []int{12}, []int{12, 11, 10}),
			row(0, 1, []DTI{dash, disc, swch, dash, dash, dash, dash, dash, dash}, []int{6}, []int{6, 5, 4}),
			row(0, 2, []DTI{dash, dash, disc, dash, dash, dash, dash, dash, dash}, []int{3}, []int{3, 2, 1}),
			row(0, 2, []DTI{dash, dash, disc, dash, dash, dash, dash, dash, dash}, []int{3}, []int{9, 8, 7}),
			row(1, 0, []DTI{dash, dash, dash, swch, swch, swch, swch, swch, swch}, []int{1}, []int{1, 1, 1}),
			row(1, 0, []DTI{dash, dash, dash, swch, swch, swch, dash, dash, dash}, []int{12}, []int{1, 12, 11}),
			row(1, 1, []DTI{dash, dash, dash, dash, disc, swch, dash, dash, dash}, []int{6}, []int{7, 6, 5}),
			row(1, 2, []DTI{dash, dash, dash, dash, dash, disc, dash, dash, dash}, []int{3}, []int{4, 3, 2}),
			row(1, 2, []DTI{dash, dash, dash, dash, dash, disc, dash, dash, dash}, []int{3}, []int{10, 9, 8}),
			row(2, 0, []DTI{dash, dash, dash, dash, dash, dash, swch, swch, swch}, []int{1}, []int{2, 1, 1}),
			row(2, 0, []DTI{dash, dash, dash, dash, dash, dash, swch, swch, swch}, []int{12}, []int{2, 1, 12}),
			row(2, 1, []DTI{dash, dash, dash, dash, dash, dash, dash, disc, swch}, []int{6}, []int{8, 7, 6}),
			row(2, 2, []DTI{dash, dash, dash, dash, dash, dash, dash, dash, disc}, []int{3}, []int{5, 4, 3}),
			row(2, 2, []DTI{dash, dash, dash, dash, dash, dash, dash, dash, disc}, []int{3}, []int{11, 10, 9}),
		},
	}
}

// Descriptors as a subscriber received them from the reference, publisher Chrome 156 with L3T3_KEY: the first packet of a
// key frame, then an ordinary packet with the mask the reference added. The structure parts of VP9's and AV1's are the same.
const (
	capturedVP9Key   = "8000c780081485214eaaaaa8000600004000100002aa80a8000600004000100002a000a800060000400016d549241b5524906d54923157e001974ca864330e222396eca8655304224390eca87753013f00b3027f016704ff02cf"
	capturedVP9Plain = "4000c747fc"
	capturedAV1Key   = "c000c480081485214eaaaaa8000600004000100002aa80a8000600004000100002a000a800060000400016d549241b5524906d54923157e001974ca864330e222396eca8655304224390eca87753013f00b3027f016704ff02cf"
	capturedAV1Plain = "8500c547fc"
)

func TestCapturedDescriptorsReadAndWriteBack(t *testing.T) {
	for _, c := range []struct {
		name, key, plain string
		firstPacket      bool
		lastOfKey        bool
	}{
		{"vp9", capturedVP9Key, capturedVP9Plain, true, false},
		{"av1", capturedAV1Key, capturedAV1Plain, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			keyBytes, err := hex.DecodeString(c.key)
			if err != nil {
				t.Fatal(err)
			}
			d, err := Read(keyBytes, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d.Attached == nil {
				t.Fatal("no structure")
			}
			if !reflect.DeepEqual(d.Attached, l3t3key()) {
				t.Errorf("the structure on the wire is not the one in the analysis:\n got %+v\nwant %+v", d.Attached, l3t3key())
			}
			if d.Frame.Spatial != 0 || d.Frame.Temporal != 0 || !d.FirstPacket || d.LastPacket != c.lastOfKey {
				t.Errorf("frame: %+v", d)
			}
			if d.ActiveTargets == nil || *d.ActiveTargets != 511 {
				t.Errorf("active targets: %v", d.ActiveTargets)
			}
			written, err := Write(d, d.Attached)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(written, keyBytes) {
				t.Errorf("wrote back\n %x\nread\n %x", written, keyBytes)
			}

			plainBytes, err := hex.DecodeString(c.plain)
			if err != nil {
				t.Fatal(err)
			}
			p, err := Read(plainBytes, d.Attached)
			if err != nil {
				t.Fatal(err)
			}
			if p.ActiveTargets == nil || *p.ActiveTargets != 511 {
				t.Errorf("active targets: %v", p.ActiveTargets)
			}
			written, err = Write(p, d.Attached)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(written, plainBytes) {
				t.Errorf("wrote back %x, read %x", written, plainBytes)
			}
		})
	}
}

func TestStructureRoundTrip(t *testing.T) {
	s := l3t3key()
	d := &Descriptor{FirstPacket: true, FrameNumber: 4242, Frame: s.Templates[0], Attached: s}
	data, err := Write(d, nil)
	if err == nil {
		t.Fatal("writing with no structure in force should fail")
	}
	if !errors.Is(err, ErrNoStructure) {
		t.Fatalf("error = %v", err)
	}
	data, err = Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	// The size observed on the wire for this structure.
	if len(data) != 90 {
		t.Errorf("a key frame with this structure is %d bytes, and 90 were observed", len(data))
	}
	got, err := Read(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FirstPacket || got.LastPacket || got.FrameNumber != 4242 {
		t.Errorf("mandatory fields: %+v", got)
	}
	if got.Attached == nil {
		t.Fatal("no structure read back")
	}
	if !reflect.DeepEqual(got.Attached, s) {
		t.Errorf("structure changed in the round trip:\n got %+v\nwant %+v", got.Attached, s)
	}
	if got.ActiveTargets == nil || *got.ActiveTargets != 511 {
		t.Errorf("a structure implies all targets active: %v", got.ActiveTargets)
	}
	if !reflect.DeepEqual(got.Frame.DTIs, s.Templates[0].DTIs) {
		t.Errorf("frame: %+v", got.Frame)
	}
}

func TestOrdinaryPacketNeedsTheStructure(t *testing.T) {
	s := l3t3key()
	d := &Descriptor{LastPacket: true, FrameNumber: 7, Frame: s.Templates[7]}
	data, err := Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 3 {
		t.Errorf("a packet with nothing custom is %d bytes, want 3", len(data))
	}
	if _, err := Read(data, nil); !errors.Is(err, ErrNoStructure) {
		t.Errorf("error = %v", err)
	}
	got, err := Read(data, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frame.Spatial != 1 || got.Frame.Temporal != 1 || !got.LastPacket || got.FirstPacket || got.FrameNumber != 7 {
		t.Errorf("read back %+v", got)
	}
	if got.ActiveTargets != nil {
		t.Errorf("no mask was written, but read %v", *got.ActiveTargets)
	}
}

func TestActiveMaskTakesFiveBytesAsObserved(t *testing.T) {
	s := l3t3key()
	mask := uint32(63)
	d := &Descriptor{FirstPacket: true, LastPacket: true, FrameNumber: 9, Frame: s.Templates[5], ActiveTargets: &mask}
	data, err := Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 5 {
		t.Errorf("%d bytes, and 5 were observed", len(data))
	}
	got, err := Read(data, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveTargets == nil || *got.ActiveTargets != 63 {
		t.Errorf("mask: %v", got.ActiveTargets)
	}
}

func TestStructureIDOffsetsTemplateIDs(t *testing.T) {
	s := l3t3key()
	s.ID = 60
	d := &Descriptor{FrameNumber: 1, Frame: s.Templates[10]}
	data, err := Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	if id := int(data[0] & 0x3f); id != 6 { // (10 + 60) mod 64
		t.Errorf("template ID = %d", id)
	}
	got, err := Read(data, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frame.Spatial != 2 || got.Frame.Temporal != 0 || !reflect.DeepEqual(got.Frame.FrameDiffs, []int{1}) {
		t.Errorf("frame: %+v", got.Frame)
	}
}

func TestCustomFieldsRoundTrip(t *testing.T) {
	s := l3t3key()
	frame := s.Templates[12].Clone()
	frame.DTIs[3] = Required
	frame.FrameDiffs = []int{1, 16, 17, 300, 4096}
	frame.ChainDiffs = []int{200, 7, 6}
	mask := uint32(73)
	d := &Descriptor{FirstPacket: true, FrameNumber: 65535, Frame: frame, ActiveTargets: &mask}
	data, err := Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(data, s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Frame.DTIs, frame.DTIs) {
		t.Errorf("DTIs: %v", got.Frame.DTIs)
	}
	if !reflect.DeepEqual(got.Frame.FrameDiffs, frame.FrameDiffs) {
		t.Errorf("frame diffs: %v", got.Frame.FrameDiffs)
	}
	if !reflect.DeepEqual(got.Frame.ChainDiffs, frame.ChainDiffs) {
		t.Errorf("chain diffs: %v", got.Frame.ChainDiffs)
	}
	if *got.ActiveTargets != 73 || got.FrameNumber != 65535 {
		t.Errorf("read back %+v", got)
	}
	// Reading custom fields must not write through to the structure's templates.
	if !reflect.DeepEqual(s.Templates[12].FrameDiffs, []int{6}) {
		t.Errorf("template changed: %v", s.Templates[12].FrameDiffs)
	}
}

func TestWriteChoosesTheCheapestTemplate(t *testing.T) {
	s := l3t3key()
	// S0T0 has two templates. A frame that matches the second must not take custom fields on the first.
	d := &Descriptor{FrameNumber: 1, Frame: s.Templates[1]}
	data, err := Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 3 || data[0]&0x3f != 1 {
		t.Errorf("%x", data)
	}
}

func TestStructureWithoutChainsOrResolutions(t *testing.T) {
	s := &Structure{
		ID: 3, DecodeTargets: 1,
		Templates: []Template{{DTIs: []DTI{Switch}}, {DTIs: []DTI{Discardable}, FrameDiffs: []int{1}}},
	}
	d := &Descriptor{FirstPacket: true, Frame: s.Templates[0], Attached: s}
	data, err := Write(d, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attached.Chains != 0 || got.Attached.TargetChain != nil || got.Attached.Resolutions != nil {
		t.Errorf("%+v", got.Attached)
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	s := l3t3key()
	if _, err := Read([]byte{1, 2}, s); err == nil {
		t.Error("two bytes should not parse")
	}
	bad := &Descriptor{Frame: s.Templates[0]}
	data, err := Write(bad, s)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = data[0]&0xc0 | 40 // a template this structure does not have
	if _, err := Read(data, s); !errors.Is(err, ErrBadTemplate) {
		t.Errorf("error = %v", err)
	}
	// Everything truncated, and everything mangled, must fail or parse: never panic.
	key, err := Write(&Descriptor{Frame: s.Templates[0], Attached: s}, s)
	if err != nil {
		t.Fatal(err)
	}
	for i := range key {
		_, _ = Read(key[:i], nil)
		_, _ = Read(key[:i], s)
		mangled := append([]byte{}, key...)
		mangled[i] ^= 0xff
		_, _ = Read(mangled, nil)
		_, _ = Read(mangled, s)
	}
}

func TestTargetsAreHighestFirst(t *testing.T) {
	targets := Targets(l3t3key())
	if len(targets) != 9 {
		t.Fatalf("%d targets", len(targets))
	}
	if targets[0] != (Target{Index: 8, Spatial: 2, Temporal: 2}) {
		t.Errorf("first: %+v", targets[0])
	}
	if targets[8] != (Target{Index: 0, Spatial: 0, Temporal: 0}) {
		t.Errorf("last: %+v", targets[8])
	}
}

// The active masks observed on the wire for each request (GAP-024, GAP-027).
func TestMaskForMatchesObservedMasks(t *testing.T) {
	targets := Targets(l3t3key())
	cases := []struct {
		spatial, temporal int
		want              uint32
	}{{0, 2, 7}, {1, 2, 63}, {2, 2, 511}, {2, 0, 73}}
	for _, c := range cases {
		if got := MaskFor(targets, c.spatial, c.temporal); got != c.want {
			t.Errorf("S%d T%d: mask %d, want %d", c.spatial, c.temporal, got, c.want)
		}
	}
}
