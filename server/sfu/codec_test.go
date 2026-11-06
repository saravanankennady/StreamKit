package sfu

import (
	"bytes"
	"testing"
)

func TestKeyframeStartDetection(t *testing.T) {
	// VP8 descriptor: X=1, S=1, PID=0; I=1 with a 15-bit picture ID; then the payload header.
	vp8Key := []byte{0x90, 0x80, 0x81, 0x23, 0x10}
	vp8Inter := []byte{0x90, 0x80, 0x81, 0x23, 0x11}
	vp8Continuation := []byte{0x80, 0x80, 0x81, 0x23, 0x10}

	cases := []struct {
		name string
		mime string
		p    []byte
		want bool
	}{
		{"vp8 key", "video/VP8", vp8Key, true},
		{"vp8 inter", "video/VP8", vp8Inter, false},
		{"vp8 continuation", "video/vp8", vp8Continuation, false},
		{"vp8 empty", "video/VP8", nil, false},
		{"vp8 truncated", "video/VP8", []byte{0x90, 0x80}, false},

		{"h264 idr", "video/H264", []byte{0x65, 0x00}, true},
		{"h264 sps", "video/H264", []byte{0x67, 0x00}, true},
		{"h264 non-idr", "video/H264", []byte{0x41, 0x00}, false},
		{"h264 stap-a with sps", "video/H264", []byte{0x78, 0x00, 0x02, 0x67, 0x00}, true},
		{"h264 stap-a without key", "video/H264", []byte{0x78, 0x00, 0x02, 0x41, 0x00}, false},
		{"h264 fu-a idr start", "video/H264", []byte{0x7c, 0x85, 0x00}, true},
		{"h264 fu-a idr middle", "video/H264", []byte{0x7c, 0x05, 0x00}, false},
		{"h264 fu-a non-idr start", "video/H264", []byte{0x7c, 0x81, 0x00}, false},

		{"vp9 key", "video/VP9", []byte{0x08, 0x82}, true},
		{"vp9 inter", "video/VP9", []byte{0x48, 0x86}, false},
		{"vp9 not begin", "video/VP9", []byte{0x00, 0x82}, false},
		{"vp9 key base layer", "video/VP9", []byte{0x28, 0x00, 0x00, 0x82}, true},
		{"vp9 key upper spatial", "video/VP9", []byte{0x28, 0x02, 0x00, 0x82}, false},
		{"vp9 intra-only is not a key frame", "video/VP9", []byte{0x08, 0x86}, false},
		{"vp9 shows an existing frame", "video/VP9", []byte{0x08, 0x8a}, false},
		{"vp9 key in profile 3, with its reserved bit", "video/VP9", []byte{0x08, 0xb1}, true},
		{"vp9 with no frame header", "video/VP9", []byte{0x08}, false},

		{"av1 new sequence", "video/AV1", []byte{0x08}, true},
		{"av1 continuing", "video/AV1", []byte{0x00}, false},
		{"av1 first OBU continues an earlier one", "video/AV1", []byte{0x88}, false},

		{"h265 idr", "video/H265", []byte{19 << 1, 0, 0}, true},
		{"h265 trail", "video/H265", []byte{1 << 1, 0, 0}, false},
		{"h265 fu start idr", "video/H265", []byte{49 << 1, 0, 0x80 | 19}, true},
		{"h265 fu middle", "video/H265", []byte{49 << 1, 0, 19}, false},

		{"audio is never a keyframe", "audio/opus", []byte{0x65}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsKeyframeStart(c.mime, c.p); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestLayerForRID(t *testing.T) {
	if LayerForRID("q") != LayerLow || LayerForRID("h") != LayerMedium || LayerForRID("f") != LayerHigh {
		t.Fatal("rid mapping wrong")
	}
	if LayerForRID("") != LayerHigh {
		t.Fatal("a stream with no rid is the track's full quality")
	}
}

func TestRedPrimary(t *testing.T) {
	// One redundant block of 3 bytes (F=1, 4-byte header), then the primary block (F=0, 1-byte header).
	p := []byte{0x80 | 111, 0x00, 0x00, 0x03, 111, 0xaa, 0xbb, 0xcc, 0x01, 0x02}
	got, ok := redPrimary(p)
	if !ok || !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Fatalf("got %x, ok=%v", got, ok)
	}

	// No redundancy.
	got, ok = redPrimary([]byte{111, 0x09})
	if !ok || !bytes.Equal(got, []byte{0x09}) {
		t.Fatalf("got %x, ok=%v", got, ok)
	}

	for _, bad := range [][]byte{nil, {0x80 | 111, 0, 0}, {0x80 | 111, 0x00, 0x00, 0x05, 111, 0xaa}} {
		if _, ok := redPrimary(bad); ok {
			t.Fatalf("%x should be rejected", bad)
		}
	}
}

func TestVP8Munger(t *testing.T) {
	packet := func(pic uint16, tl0 uint8) []byte {
		return []byte{0x90, 0xc0, 0x80 | byte(pic>>8), byte(pic), tl0, 0x10}
	}
	var m vp8Munger

	first := packet(100, 7)
	d, ok := parseVP8Descriptor(first)
	if !ok || d.picID != 100 || d.tl0PicID != 7 || !d.picIDWide {
		t.Fatalf("parse: %+v, %v", d, ok)
	}
	m.onSwitch(d)
	m.apply(first, d)
	if m.needsCopy() {
		t.Fatal("no rewrite should be needed before the first switch")
	}

	// Switching to a layer whose counters are far away.
	next := packet(32000, 200)
	d, _ = parseVP8Descriptor(next)
	m.onSwitch(d)
	if !m.needsCopy() {
		t.Fatal("offsets expected after switch")
	}
	m.apply(next, d)
	got, _ := parseVP8Descriptor(next)
	if got.picID != 101 || got.tl0PicID != 8 {
		t.Fatalf("after switch: pic=%d tl0=%d, want 101 and 8", got.picID, got.tl0PicID)
	}

	// Later frames in the new layer stay continuous.
	later := packet(32001, 201)
	d, _ = parseVP8Descriptor(later)
	m.apply(later, d)
	got, _ = parseVP8Descriptor(later)
	if got.picID != 102 || got.tl0PicID != 9 {
		t.Fatalf("later: pic=%d tl0=%d", got.picID, got.tl0PicID)
	}
}

func TestVP8PictureIDWraps(t *testing.T) {
	m := vp8Munger{started: true, lastPic: 0x7fff, lastTL0: 255}
	p := []byte{0x90, 0xc0, 0x80, 0x05, 9, 0x10}
	d, _ := parseVP8Descriptor(p)
	m.onSwitch(d)
	m.apply(p, d)
	got, _ := parseVP8Descriptor(p)
	if got.picID != 0 || got.tl0PicID != 0 {
		t.Fatalf("wrap: pic=%d tl0=%d, want 0 and 0", got.picID, got.tl0PicID)
	}
}
