package sfu

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/server/sfu/dd"
)

var (
	vp9Codec = webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"},
		PayloadType:        98,
	}
	svcExt = []webrtc.RTPHeaderExtensionParameter{{URI: DependencyDescriptorURI, ID: 7}}
)

const (
	dtiNo = dd.NotPresent
	dtiD  = dd.Discardable
	dtiS  = dd.Switch
)

// l3t3KeyStructure is what Chrome sends for VP9 L3T3_KEY, from a capture (GAP-022).
func l3t3KeyStructure() *dd.Structure {
	row := func(s, t int, dtis []dd.DTI, diffs, chains []int) dd.Template {
		return dd.Template{Spatial: s, Temporal: t, DTIs: dtis, FrameDiffs: diffs, ChainDiffs: chains}
	}
	return &dd.Structure{
		DecodeTargets: 9, Chains: 3,
		TargetChain: []int{0, 0, 0, 1, 1, 1, 2, 2, 2},
		Resolutions: []dd.Resolution{{Width: 320, Height: 180}, {Width: 640, Height: 360}, {Width: 1280, Height: 720}},
		Templates: []dd.Template{
			row(0, 0, []dd.DTI{dtiS, dtiS, dtiS, dtiS, dtiS, dtiS, dtiS, dtiS, dtiS}, nil, []int{0, 0, 0}),
			row(0, 0, []dd.DTI{dtiS, dtiS, dtiS, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo}, []int{12}, []int{12, 11, 10}),
			row(0, 1, []dd.DTI{dtiNo, dtiD, dtiS, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo}, []int{6}, []int{6, 5, 4}),
			row(0, 2, []dd.DTI{dtiNo, dtiNo, dtiD, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo}, []int{3}, []int{3, 2, 1}),
			row(0, 2, []dd.DTI{dtiNo, dtiNo, dtiD, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo}, []int{3}, []int{9, 8, 7}),
			row(1, 0, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiS, dtiS, dtiS, dtiS, dtiS, dtiS}, []int{1}, []int{1, 1, 1}),
			row(1, 0, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiS, dtiS, dtiS, dtiNo, dtiNo, dtiNo}, []int{12}, []int{1, 12, 11}),
			row(1, 1, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiD, dtiS, dtiNo, dtiNo, dtiNo}, []int{6}, []int{7, 6, 5}),
			row(1, 2, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiD, dtiNo, dtiNo, dtiNo}, []int{3}, []int{4, 3, 2}),
			row(1, 2, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiD, dtiNo, dtiNo, dtiNo}, []int{3}, []int{10, 9, 8}),
			row(2, 0, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiS, dtiS, dtiS}, []int{1}, []int{2, 1, 1}),
			row(2, 0, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiS, dtiS, dtiS}, []int{12}, []int{2, 1, 12}),
			row(2, 1, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiD, dtiS}, []int{6}, []int{8, 7, 6}),
			row(2, 2, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiD}, []int{3}, []int{5, 4, 3}),
			row(2, 2, []dd.DTI{dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiNo, dtiD}, []int{3}, []int{11, 10, 9}),
		},
	}
}

// svcEncoder produces the stream an L3T3_KEY encoder does, as real descriptor bytes through a real parser.
type svcEncoder struct {
	s       *dd.Structure
	parser  svcParser
	seq     uint16
	frame   uint16
	picture int
	ts      uint32
}

func newSVCEncoder() *svcEncoder {
	return &svcEncoder{s: l3t3KeyStructure(), seq: 1000, frame: 500, ts: 90000}
}

// templates says which row each layer of each picture uses. A group of pictures is T0, T2, T1, T2.
func (e *svcEncoder) template(spatial, picture int, key bool) int {
	if key {
		return []int{0, 5, 10}[spatial]
	}
	switch picture % 4 {
	case 0:
		return []int{1, 6, 11}[spatial]
	case 1:
		return []int{3, 8, 13}[spatial]
	case 2:
		return []int{2, 7, 12}[spatial]
	}
	return []int{4, 9, 14}[spatial]
}

type svcPacket struct {
	spatial, temporal int
	seq               uint16
	ts                uint32
	frame             *SVCFrame
	marker            bool
	ext               []byte
}

// frame makes the packets of one layer frame.
func (e *svcEncoder) layerFrame(t *testing.T, spatial, picture int, key bool, packets int, lastOfPicture bool) []svcPacket {
	t.Helper()
	tmpl := e.s.Templates[e.template(spatial, picture, key)]
	var out []svcPacket
	for i := range packets {
		d := &dd.Descriptor{FirstPacket: i == 0, LastPacket: i == packets-1, FrameNumber: e.frame, Frame: tmpl}
		if key && spatial == 0 && i == 0 {
			d.Attached = e.s
		}
		ext, err := dd.Write(d, e.s)
		if err != nil {
			t.Fatal(err)
		}
		f, err := e.parser.parse(ext, e.seq)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, svcPacket{spatial, tmpl.Temporal, e.seq, e.ts, f, lastOfPicture && i == packets-1, ext})
		e.seq++
	}
	e.frame++
	return out
}

// picture makes every layer of one picture, one packet per frame unless told otherwise.
func (e *svcEncoder) nextPicture(t *testing.T, key bool) []svcPacket {
	t.Helper()
	if key {
		e.picture = 0
	}
	var out []svcPacket
	for s := range 3 {
		out = append(out, e.layerFrame(t, s, e.picture, key, 1, s == 2)...)
	}
	e.picture++
	e.ts += 4500
	return out
}

func newSVCDownTrack(t *testing.T) (*DownTrack, *fakeWriter) {
	t.Helper()
	r := NewReceiver(ReceiverParams{
		Kind: webrtc.RTPCodecTypeVideo, Codec: vp9Codec, IsSVC: true,
		HeaderExt:       HeaderExtensionIDs{DependencyDescriptor: 5},
		RequestKeyframe: func(uint32) {},
	})
	t.Cleanup(r.Close)
	r.svc.mode.Store(int32(SelectDescriptor))
	stream := &layerState{index: LayerHigh, ssrc: 1}
	stream.lastSeen.Store(time.Now().UnixNano())
	r.layers[LayerHigh] = stream
	r.svc.onMaskChanged(511, dd.Targets(l3t3KeyStructure()))
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	w := &fakeWriter{}
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{vp9Codec}, ext: svcExt, w: w}); err != nil {
		t.Fatal(err)
	}
	return d, w
}

func send(d *DownTrack, packets []svcPacket) {
	for _, p := range packets {
		hdr := &rtp.Header{SequenceNumber: p.seq, Timestamp: p.ts, Marker: p.marker, SSRC: 1}
		_ = hdr.SetExtension(5, p.ext)
		d.Forward(&Packet{Header: hdr, Payload: []byte{0xaa, 0xbb}, Layer: LayerHigh, Selection: SelectDescriptor, SVC: p.frame})
	}
}

// layersSent reads back what went out, through the same parser a subscriber would use.
func layersSent(t *testing.T, sent []sentPacket) []string {
	t.Helper()
	var structure *dd.Structure
	var out []string
	for _, p := range sent {
		if p.hdr.Padding {
			out = append(out, "padding")
			continue
		}
		ext := p.hdr.GetExtension(7)
		if ext == nil {
			t.Fatalf("a packet with no descriptor: %+v", p.hdr)
		}
		d, err := dd.Read(ext, structure)
		if err != nil {
			t.Fatalf("subscriber could not read the descriptor: %v", err)
		}
		if d.Attached != nil {
			structure = d.Attached
		}
		out = append(out, fmt.Sprintf("S%dT%d", d.Frame.Spatial, d.Frame.Temporal))
	}
	return out
}

func assertContinuous(t *testing.T, sent []sentPacket) {
	t.Helper()
	for i := 1; i < len(sent); i++ {
		if sent[i].hdr.SequenceNumber != sent[i-1].hdr.SequenceNumber+1 {
			t.Fatalf("sequence gap at %d: %d then %d", i, sent[i-1].hdr.SequenceNumber, sent[i].hdr.SequenceNumber)
		}
	}
}

// A stream that never counted as started would keep to its lowest layer for ever.
func TestSVCCountsAsStartedOnceItForwards(t *testing.T) {
	d, _ := newSVCDownTrack(t)
	d.SetTarget(LayerHigh)
	if !d.startingAt(time.Now(), true) {
		t.Fatal("started before anything was forwarded")
	}
	send(d, newSVCEncoder().nextPicture(t, true))
	if d.startingAt(time.Now().Add(startHoldMax), true) {
		t.Fatal("still starting long after it began to forward")
	}
}

// NACKs are answered here and not by Pion, so what a filtered stream sends has to be remembered too.
func TestSVCPacketsCanBeSentAgain(t *testing.T) {
	d, _ := newSVCDownTrack(t)
	w := &resendWriter{}
	d.writer = w
	d.SetTarget(LayerHigh)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))
	send(d, e.nextPicture(t, false))
	if len(w.sent) == 0 {
		t.Fatal("nothing forwarded")
	}
	d.HandleRTCP(nackFor(w.sent[len(w.sent)-1].hdr.SequenceNumber))
	if got := w.resent.Load(); got != 1 {
		t.Fatalf("sent again %d times, want 1", got)
	}
}

// One group of pictures at each request gives what was seen on the wire from the reference (GAP-024).
func TestSVCForwardsWhatEachRequestAsksFor(t *testing.T) {
	cases := []struct {
		name              string
		spatial, temporal int32
		// The key picture is the exception: higher layers depend on lower ones there.
		key  []string
		rest []string
		mask uint32
	}{
		{"low", 0, 7, []string{"S0T0"}, []string{"S0T2", "S0T1", "S0T2", "S0T0"}, 7},
		{"medium", 1, 7, []string{"S0T0", "S1T0"}, []string{"S1T2", "S1T1", "S1T2", "S1T0"}, 63},
		{"high", 2, 7, []string{"S0T0", "S1T0", "S2T0"}, []string{"S2T2", "S2T1", "S2T2", "S2T0"}, 511},
		{"high at the lowest rate", 2, 0, []string{"S0T0", "S1T0", "S2T0"}, []string{"S2T0"}, 73},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, w := newSVCDownTrack(t)
			d.SetTarget(c.spatial)
			d.maxTemporal.Store(c.temporal)
			e := newSVCEncoder()
			send(d, e.nextPicture(t, true))
			for range 4 {
				send(d, e.nextPicture(t, false))
			}
			got := layersSent(t, w.sent)
			want := append(slices.Clone(c.key), c.rest...)
			if !slices.Equal(got, want) {
				t.Fatalf("forwarded %v, want %v", got, want)
			}
			assertContinuous(t, w.sent)
			if d.Current() != c.spatial {
				t.Errorf("current spatial = %d", d.Current())
			}
			// The last packet is an ordinary one, so it carries the mask.
			last, err := dd.Read(w.sent[len(w.sent)-1].hdr.GetExtension(7), l3t3KeyStructure())
			if err != nil {
				t.Fatal(err)
			}
			if last.ActiveTargets == nil || *last.ActiveTargets != c.mask {
				t.Errorf("active mask = %v, want %d", last.ActiveTargets, c.mask)
			}
		})
	}
}

// With L3T3_KEY a subscriber on one spatial layer drops the frames of the others, so any spatial move has to wait for a key
// frame. It holds what it has until then, rather than showing nothing (GAP-024).
func TestSVCSpatialChangeWaitsForAKeyFrame(t *testing.T) {
	for _, c := range []struct {
		name     string
		from, to int32
		held     string
		after    []string
	}{
		{"down", 2, 0, "S2T", []string{"S0T0"}},
		{"up", 0, 2, "S0T", []string{"S0T0", "S1T0", "S2T0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, w := newSVCDownTrack(t)
			d.SetTarget(c.from)
			e := newSVCEncoder()
			send(d, e.nextPicture(t, true))
			// Until the second T0 goes by, every layer can still be reached from the key frame.
			for range 4 {
				send(d, e.nextPicture(t, false))
			}
			if d.NeedsKeyframe() {
				t.Fatal("a subscriber getting what it asked for needs no key frame")
			}
			before := len(w.sent)

			d.SetTarget(c.to)
			for range 2 {
				send(d, e.nextPicture(t, false))
			}
			if got := len(w.sent) - before; got != 2 {
				t.Fatalf("%d packets until the key frame, want 2 of the old layer", got)
			}
			for _, name := range layersSent(t, w.sent)[before:] {
				if name[:3] != c.held {
					t.Fatalf("forwarded %s while waiting: should hold %s", name, c.held)
				}
			}
			if !d.NeedsKeyframe() {
				t.Fatal("it should be asking for a key frame")
			}
			if d.Current() != c.from {
				t.Fatalf("moved to %d before the key frame", d.Current())
			}

			before = len(w.sent)
			send(d, e.nextPicture(t, true))
			if got := layersSent(t, w.sent)[before:]; !slices.Equal(got, c.after) {
				t.Fatalf("key picture forwarded %v, want %v", got, c.after)
			}
			if d.Current() != c.to {
				t.Fatalf("current = %d after the key frame", d.Current())
			}
			if d.NeedsKeyframe() {
				t.Fatal("still asking for a key frame after one")
			}
			assertContinuous(t, w.sent)
		})
	}
}

// Temporal layers are on one chain, so a change takes no key frame.
func TestSVCTemporalChangeNeedsNoKeyFrame(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerHigh)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))
	send(d, e.nextPicture(t, false))
	send(d, e.nextPicture(t, false))
	send(d, e.nextPicture(t, false))
	send(d, e.nextPicture(t, false)) // the next T0

	before := len(w.sent)
	d.maxTemporal.Store(0)
	for range 4 {
		send(d, e.nextPicture(t, false))
	}
	if got := layersSent(t, w.sent)[before:]; !slices.Equal(got, []string{"S2T0"}) {
		t.Fatalf("lowest rate forwarded %v", got)
	}
	if d.NeedsKeyframe() {
		t.Fatal("a temporal change should need no key frame")
	}

	before = len(w.sent)
	d.maxTemporal.Store(maxTemporalLayer)
	for range 4 {
		send(d, e.nextPicture(t, false))
	}
	if got := layersSent(t, w.sent)[before:]; !slices.Equal(got, []string{"S2T2", "S2T1", "S2T2", "S2T0"}) {
		t.Fatalf("full rate forwarded %v", got)
	}
	assertContinuous(t, w.sent)
}

// A publisher marks only the top layer's last packet, which a subscriber below it would never see (GAP-026).
func TestSVCMarksTheTopForwardedLayer(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerLow)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))
	send(d, e.nextPicture(t, false))
	for i, p := range w.sent {
		if !p.hdr.Marker {
			t.Errorf("packet %d has no marker", i)
		}
	}

	d2, w2 := newSVCDownTrack(t)
	d2.SetTarget(LayerHigh)
	e2 := newSVCEncoder()
	send(d2, e2.nextPicture(t, true))
	markers := []bool{w2.sent[0].hdr.Marker, w2.sent[1].hdr.Marker, w2.sent[2].hdr.Marker}
	if !slices.Equal(markers, []bool{false, false, true}) {
		t.Errorf("markers on the key picture: %v", markers)
	}
}

// Timestamps pass through: all layers of a picture share one, and that is what makes a switch seamless.
func TestSVCLeavesTimestampsAndPayloadAlone(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerHigh)
	e := newSVCEncoder()
	first := e.nextPicture(t, true)
	send(d, first)
	second := e.nextPicture(t, false)
	send(d, second)
	if w.sent[0].hdr.Timestamp != first[0].ts || w.sent[2].hdr.Timestamp != first[0].ts {
		t.Errorf("key picture timestamps: %d, %d", w.sent[0].hdr.Timestamp, w.sent[2].hdr.Timestamp)
	}
	if w.sent[3].hdr.Timestamp != second[0].ts {
		t.Errorf("timestamp = %d, want %d", w.sent[3].hdr.Timestamp, second[0].ts)
	}
	if w.sent[0].hdr.SSRC != 7777 || w.sent[0].hdr.PayloadType != 98 {
		t.Errorf("subscriber's SSRC and payload type not applied: %+v", w.sent[0].hdr)
	}
	if string(w.sent[0].payload) != "\xaa\xbb" {
		t.Errorf("payload: %x", w.sent[0].payload)
	}
}

// All packets of a frame get one answer, even if the request changes in the middle of it.
func TestSVCDecidesOncePerFrame(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerHigh)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))
	// S2T2 in three packets: the first goes out, then the request drops to S0.
	var frames []svcPacket
	frames = append(frames, e.layerFrame(t, 0, 1, false, 1, false)...)
	frames = append(frames, e.layerFrame(t, 1, 1, false, 1, false)...)
	top := e.layerFrame(t, 2, 1, false, 3, true)
	send(d, frames)
	before := len(w.sent)
	send(d, top[:1])
	d.SetTarget(LayerLow)
	send(d, top[1:])
	if got := len(w.sent) - before; got != 3 {
		t.Fatalf("%d of the frame's 3 packets went out", got)
	}
}

// A packet lost in the publisher's stream leaves the output with the same gap, so the subscriber asks for it, and when the
// retransmission arrives it takes the place it would have had. Dropping a packet right after a gap cannot close the numbers
// up, because that would move the missing packets' places, so it sends padding instead.
func TestSVCLossAndDroppedPacketsKeepTheirPlaces(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerLow)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true)) // S0 goes out, S1 and S2 close up

	p1 := e.nextPicture(t, false) // S0T2, S1T2, S2T2
	lost := p1[0]
	p2 := e.nextPicture(t, false) // S0T1, S1T1, S2T1
	send(d, p1[1:])               // S0T2 is lost, so S1T2 comes after a gap, and S2T2 is next to it
	send(d, p2[:1])
	if len(w.sent) != 3 {
		t.Fatalf("sent = %d, want the key frame, padding and S0T1", len(w.sent))
	}
	if !w.sent[1].hdr.Padding {
		t.Fatalf("a dropped packet after a gap should leave padding in its place: %+v", w.sent[1].hdr)
	}
	// One sequence number between the key frame and the padding is the lost packet's.
	if gap := w.sent[1].hdr.SequenceNumber - w.sent[0].hdr.SequenceNumber; gap != 2 {
		t.Fatalf("padding is %d after the key frame", gap)
	}

	send(d, []svcPacket{lost})
	if len(w.sent) != 4 {
		t.Fatalf("the late packet was not forwarded: sent = %d", len(w.sent))
	}
	if got, want := w.sent[3].hdr.SequenceNumber, w.sent[0].hdr.SequenceNumber+1; got != want {
		t.Fatalf("the late packet took %d, want %d", got, want)
	}

	// The same packet again has no place to go.
	send(d, []svcPacket{lost})
	if len(w.sent) != 4 {
		t.Fatal("a repeated packet was forwarded")
	}
}

// A frame that has not arrived yet must not break a chain, and one that never arrives must.
func TestSVCChainSurvivesALateFrameButNotALostOne(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerLow)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))

	// S0T0 of the next group is the next frame in chain 0. Lose it.
	send(d, e.nextPicture(t, false))
	send(d, e.nextPicture(t, false))
	send(d, e.nextPicture(t, false))
	lostGroup := e.nextPicture(t, false)
	send(d, lostGroup[1:])
	for range 3 {
		send(d, e.nextPicture(t, false))
	}
	if d.NeedsKeyframe() {
		t.Fatal("a frame that may still arrive should not break the chain")
	}
	sent := len(w.sent)
	send(d, lostGroup[:1])
	if len(w.sent) != sent+1 {
		t.Fatal("the late frame was not forwarded")
	}

	// Lost for good: past the recovery window, the chain gives up and asks for a key frame.
	send(d, e.nextPicture(t, false))
	lostAgain := e.nextPicture(t, false)
	_ = lostAgain
	for i := 0; i < 60 && !d.NeedsKeyframe(); i++ {
		// Skip the S0 frames of every T0, so the chain's next frame is never decided.
		pic := e.nextPicture(t, false)
		if e.picture%4 == 1 {
			pic = pic[1:]
		}
		send(d, pic)
	}
	if !d.NeedsKeyframe() {
		t.Fatal("a chain with a frame gone for good should ask for a key frame")
	}
	before := len(w.sent)
	send(d, e.nextPicture(t, true))
	if len(w.sent) == before {
		t.Fatal("the key frame should restore forwarding")
	}
	if d.NeedsKeyframe() {
		t.Fatal("still asking after a key frame")
	}
}

func TestSVCStartsOnlyAtAStructure(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerHigh)
	e := newSVCEncoder()
	key := e.nextPicture(t, true)
	// This subscriber arrives after the structure went by.
	send(d, key[1:])
	send(d, e.nextPicture(t, false))
	if len(w.sent) != 0 {
		t.Fatalf("forwarded %d packets with no structure", len(w.sent))
	}
	if !d.NeedsKeyframe() {
		t.Fatal("it should be asking for a key frame")
	}
	send(d, e.nextPicture(t, true))
	if len(w.sent) != 3 {
		t.Fatalf("sent = %d after a key picture", len(w.sent))
	}
}

func TestSVCPauseAndResume(t *testing.T) {
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerHigh)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))
	send(d, e.nextPicture(t, false))
	before := len(w.sent)

	d.SetTarget(LayerNone)
	send(d, e.nextPicture(t, false))
	if len(w.sent) != before {
		t.Fatal("forwarded while paused")
	}
	if d.Current() != LayerNone {
		t.Fatalf("current = %d", d.Current())
	}

	d.SetTarget(LayerHigh)
	send(d, e.nextPicture(t, false))
	if len(w.sent) != before {
		t.Fatal("a resume has to start at a key frame")
	}
	send(d, e.nextPicture(t, true))
	if got := len(w.sent) - before; got != 3 {
		t.Fatalf("sent %d of the key picture", got)
	}
	assertContinuous(t, w.sent)
	for i := 1; i < len(w.sent); i++ {
		if int32(w.sent[i].hdr.Timestamp-w.sent[i-1].hdr.Timestamp) < 0 {
			t.Fatalf("timestamp went backwards at %d", i)
		}
	}
}

// A publisher's mask saying a layer stopped moves what is available, not anything about packets (GAP-029).
func TestSVCAvailabilityComesFromTheMask(t *testing.T) {
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp9Codec, IsSVC: true, HeaderExt: HeaderExtensionIDs{DependencyDescriptor: 5}, RequestKeyframe: func(uint32) {}})
	t.Cleanup(r.Close)
	state := &layerState{index: LayerHigh, ssrc: 1}
	state.lastSeen.Store(time.Now().UnixNano())
	r.layers[LayerHigh] = state
	r.svc.mode.Store(int32(SelectDescriptor))
	if got := r.SelectLayer(LayerHigh); got != LayerNone {
		t.Fatalf("nothing is known before a structure: %d", got)
	}

	s := l3t3KeyStructure()
	targets := dd.Targets(s)
	r.svc.onMaskChanged(511, targets)
	for _, l := range []int32{LayerLow, LayerMedium, LayerHigh} {
		if got := r.SelectLayer(l); got != l {
			t.Errorf("SelectLayer(%d) = %d with all layers on", l, got)
		}
	}
	r.svc.onMaskChanged(63, targets) // the publisher dropped S2
	if got := r.SelectLayer(LayerHigh); got != LayerMedium {
		t.Errorf("SelectLayer(high) = %d with S2 off", got)
	}
	state.lastSeen.Store(time.Now().Add(-time.Minute).UnixNano())
	if got := r.SelectLayer(LayerHigh); got != LayerNone {
		t.Errorf("SelectLayer = %d with the stream silent", got)
	}
}

func TestSVCMeasuresCostAndFrameRatePerLayer(t *testing.T) {
	s := newSVCStream(nil, 5)
	e := newSVCEncoder()
	feed := func(pictures int) {
		for i := range pictures {
			for _, p := range e.nextPicture(t, i == 0) {
				s.countDescriptor(p.frame, 100)
			}
		}
		s.updateStats()
	}
	feed(20)
	// Five groups of four pictures. Each layer gets its own frames only, apart from the key picture.
	for layer, want := range []int64{(20 + 0) * 100 * 8, (20 + 1) * 100 * 8, (20 + 2) * 100 * 8} {
		if got := s.stats[layer].state.bitrate.Load(); got != want {
			t.Errorf("S%d cost %d, want %d", layer, got, want)
		}
	}
	// T0, T1 and T2 make 5, 5 and 10 frames in that second.
	if got := s.temporalForFPS(2, 5); got != 0 {
		t.Errorf("5 fps: T%d", got)
	}
	if got := s.temporalForFPS(2, 10); got != 1 {
		t.Errorf("10 fps: T%d", got)
	}
	if got := s.temporalForFPS(2, 15); got != 2 {
		t.Errorf("15 fps: T%d", got)
	}
	if got := s.temporalForFPS(2, 60); got != 2 {
		t.Errorf("60 fps: T%d", got)
	}
	if got := s.temporalForFPS(2, 0); got != maxTemporalLayer {
		t.Errorf("no limit: T%d", got)
	}
}

// A publisher silent for a long time can come back with frame numbers lower than before. Without noticing, every frame would
// look older than the structure in force, and the stream would never start again.
func TestSVCStreamRestartsAfterASilence(t *testing.T) {
	e := newSVCEncoder()
	d, w := newSVCDownTrack(t)
	d.SetTarget(LayerLow)
	send(d, e.nextPicture(t, true))
	send(d, e.nextPicture(t, false))
	sent := len(w.sent)
	if sent != 2 {
		t.Fatalf("sent = %d", sent)
	}

	s := e.s
	key := &dd.Descriptor{FirstPacket: true, LastPacket: true, FrameNumber: 3, Frame: s.Templates[0], Attached: s}
	ext, err := dd.Write(key, s)
	if err != nil {
		t.Fatal(err)
	}

	// Frame 3 is far behind 500. A key frame with that number, arriving now, would be taken for a late one.
	if _, err := e.parser.parseAt(ext, e.seq, time.Now()); err == nil {
		t.Fatal("a key frame far behind the last one, with no silence before it, should not be accepted")
	}
	f, err := e.parser.parseAt(ext, e.seq+1, time.Now().Add(svcRestartGap+time.Second))
	if err != nil {
		t.Fatalf("after a silence: %v", err)
	}
	if f.Generation != 1 || !f.StructureUpdated {
		t.Fatalf("%+v", f)
	}
	hdr := &rtp.Header{SequenceNumber: e.seq + 1, Timestamp: e.ts, SSRC: 1}
	_ = hdr.SetExtension(5, ext)
	d.Forward(&Packet{Header: hdr, Payload: []byte{0xaa}, Layer: LayerHigh, Selection: SelectDescriptor, SVC: f})
	if len(w.sent) != sent+1 {
		t.Fatalf("the restarted stream was not forwarded: sent = %d", len(w.sent))
	}
	last := w.sent[len(w.sent)-1].hdr
	number, err := dd.Read(last.GetExtension(7), nil)
	if err != nil {
		t.Fatal(err)
	}
	// What a subscriber reads must still move forward from the frame numbers it has seen.
	if diff := number.FrameNumber - uint16(504); diff >= 0x8000 {
		t.Errorf("frame number %d reads as going backwards from about 504", number.FrameNumber)
	}
}

func TestFrameNumberWrapper(t *testing.T) {
	w := frameNumberWrapper{}
	start := uint64(70000)
	if got := w.next(start, false); got != uint16(start) {
		t.Errorf("first: %d", got)
	}
	if got := w.next(start+3, false); got != uint16(start+3) {
		t.Errorf("forward: %d", got)
	}
	if got := w.next(start+1, false); got != uint16(start+1) {
		t.Errorf("late: %d", got)
	}
	// Within a group of pictures nothing moves, because references are differences.
	if got := w.next(start+40003, false); got != uint16(start+40003) {
		t.Errorf("no structure, no offset: %d", got)
	}
	// With a structure, a jump a receiver would read as backwards goes further on instead.
	last := w.last
	got := w.next(last+40000, true)
	if diff := got - uint16(last); diff >= 0x8000 || diff == 0 {
		t.Errorf("a jump reads as %d forward", diff)
	}
}

func TestVP9SelectorFollowsThePayloadDescriptor(t *testing.T) {
	packet := func(sid, tid uint8, key, begin, end bool) (*codecs.VP9Packet, bool) {
		return &codecs.VP9Packet{L: true, SID: sid, TID: tid, B: begin, E: end, P: !key && sid == 0, U: true}, key && sid == 0 && begin
	}
	const top = maxTemporalLayer
	s := vp9Selector{}
	p, key := packet(1, 0, false, true, true)
	if s.choose(p, key, false, 2, top).selected {
		t.Fatal("started without a key frame")
	}
	p, key = packet(0, 0, true, true, true)
	if got := s.choose(p, key, false, 2, top); !got.selected || got.marker {
		t.Fatalf("key frame: %+v", got)
	}
	// The upper layers of the key picture are the way up, and each lower one still passes.
	p, key = packet(1, 0, true, true, true)
	if got := s.choose(p, key, false, 2, top); !got.selected {
		t.Fatalf("S1 of the key picture: %+v", got)
	}
	p, key = packet(2, 0, true, true, true)
	if got := s.choose(p, key, false, 2, top); !got.selected || !got.marker {
		t.Fatalf("S2 of the key picture should carry the marker: %+v", got)
	}
	if s.spatial != 2 {
		t.Fatalf("spatial = %d", s.spatial)
	}
	// A lower layer always passes: nothing says the upper one does not need it.
	p, key = packet(0, 2, false, true, true)
	if got := s.choose(p, key, false, 2, top); !got.selected {
		t.Fatalf("S0T2 on the way to S2: %+v", got)
	}

	// Down: it takes effect at the end of a frame.
	p, key = packet(2, 0, false, true, false)
	if got := s.choose(p, key, false, 0, top); !got.selected {
		t.Fatalf("mid-frame: %+v", got)
	}
	p, key = packet(2, 0, false, false, true)
	if got := s.choose(p, key, false, 0, top); !got.selected {
		t.Fatalf("end of the frame: %+v", got)
	}
	p, key = packet(1, 0, false, true, true)
	if got := s.choose(p, key, false, 0, top); got.selected {
		t.Fatalf("S1 after moving to S0: %+v", got)
	}
}

// Without a descriptor on its first packet, a stream of a codec that has its own is selected by that one.
func TestStreamWithNoDescriptorFallsBack(t *testing.T) {
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp9Codec, IsSVC: true, HeaderExt: HeaderExtensionIDs{DependencyDescriptor: 5}, RequestKeyframe: func(uint32) {}})
	t.Cleanup(r.Close)
	pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: 1}, Payload: []byte{0x28, 0x00, 0x00, 0x82}}
	var p Packet
	r.svc.observe(pkt, &p, 4, "video/VP9")
	if p.Selection != SelectVP9 || p.VP9 == nil {
		t.Fatalf("selection = %d, VP9 = %v", p.Selection, p.VP9)
	}

	av1 := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1, ClockRate: 90000}}, IsSVC: true, HeaderExt: HeaderExtensionIDs{DependencyDescriptor: 5}, RequestKeyframe: func(uint32) {}})
	t.Cleanup(av1.Close)
	var q Packet
	av1.svc.observe(pkt, &q, 4, "video/AV1")
	if q.Selection != SelectStream {
		t.Fatalf("AV1 has nothing else to go on, so it forwards everything: selection = %d", q.Selection)
	}
}

func TestRewriterPlacesPacketsOfAFilteredStream(t *testing.T) {
	w := rewriter{clockRate: clock90k}
	now := time.Now().UnixNano()
	w.startFiltered(100, 1000, now)
	out, order := w.place(100)
	if out != 100 || order != seqNext {
		t.Fatalf("first: %d, %d", out, order)
	}
	w.emitted(out, 1000, now)
	// Dropped in order: the next one takes its number.
	if _, order = w.place(101); order != seqNext {
		t.Fatalf("order = %d", order)
	}
	w.dropNext()
	out, order = w.place(102)
	if out != 101 || order != seqNext {
		t.Fatalf("after a drop: %d, %d", out, order)
	}
	w.emitted(out, 1000, now)
	// A gap keeps places for what is missing, at the offset in force when it was passed.
	out, order = w.place(105)
	if out != 104 || order != seqAfterGap {
		t.Fatalf("after a gap: %d, %d", out, order)
	}
	w.emitted(out, 1000, now)
	if _, order = w.place(106); order != seqNext {
		t.Fatalf("order = %d", order)
	}
	w.dropNext()
	out, order = w.place(104)
	if out != 103 || order != seqLate {
		t.Fatalf("late: %d, %d", out, order)
	}
	out, order = w.place(103)
	if out != 102 || order != seqLate {
		t.Fatalf("late: %d, %d", out, order)
	}
	if _, order = w.place(103); order != seqRepeated {
		t.Fatalf("a repeat: %d", order)
	}
	if _, order = w.place(50); order != seqRepeated {
		t.Fatalf("far too old: %d", order)
	}
}

func TestSVCSendsReportsLikeAnyStream(t *testing.T) {
	d, w := newSVCDownTrack(t)
	r := d.Receiver()
	d.SetTarget(LayerLow)
	e := newSVCEncoder()
	send(d, e.nextPicture(t, true))
	if len(w.sent) != 1 {
		t.Fatalf("sent = %d", len(w.sent))
	}
	now := time.Now()
	r.SetSenderReport(&rtcp.SenderReport{SSRC: 1, NTPTime: publisherNTP, RTPTime: 90000}, now)
	sr := d.CreateSenderReport(now)
	if sr == nil {
		t.Fatal("no report for an SVC stream on a spatial layer other than the stream's index")
	}
	if sr.RTPTime != 90000 {
		t.Errorf("RTP = %d", sr.RTPTime)
	}
}
