// Package sfu implements selective forwarding: receiving RTP from publishers and rewriting it for
// each subscriber without transcoding.
package sfu

import (
	"strings"

	"github.com/pion/rtp/codecs"
)

// Layer indexes match the proto VideoQuality enum: LOW, MEDIUM, HIGH.
const (
	LayerLow    int32 = 0
	LayerMedium int32 = 1
	LayerHigh   int32 = 2
	MaxLayers         = 3
	// LayerNone means no layer is selected: a paused subscription, or nothing forwarded yet.
	LayerNone int32 = -1
)

// LayerForRID maps a simulcast RTP stream ID to a layer index.
func LayerForRID(rid string) int32 {
	switch rid {
	case "q":
		return LayerLow
	case "h":
		return LayerMedium
	case "f":
		return LayerHigh
	}
	return LayerHigh
}

// IsSVCCodec reports whether a codec carries several spatial layers in one RTP stream when asked to.
func IsSVCCodec(mime string) bool {
	return strings.EqualFold(mime, "video/vp9") || strings.EqualFold(mime, "video/av1")
}

// IsKeyframeStart reports whether an RTP payload is the first packet of a keyframe, which is the only
// safe point to begin decoding a stream. mime is compared without regard to case.
func IsKeyframeStart(mime string, payload []byte) bool {
	switch {
	case strings.EqualFold(mime, "video/vp8"):
		return vp8KeyframeStart(payload)
	case strings.EqualFold(mime, "video/vp9"):
		return vp9KeyframeStart(payload)
	case strings.EqualFold(mime, "video/h264"):
		return h264KeyframeStart(payload)
	case strings.EqualFold(mime, "video/h265"):
		return h265KeyframeStart(payload)
	case strings.EqualFold(mime, "video/av1"):
		return av1KeyframeStart(payload)
	}
	return false
}

func vp8KeyframeStart(p []byte) bool {
	d, ok := parseVP8Descriptor(p)
	if !ok || !d.start || d.partitionID != 0 || d.headerLen >= len(p) {
		return false
	}
	// The P bit of the VP8 payload header is 0 on keyframes.
	return p[d.headerLen]&0x01 == 0
}

// vp9KeyframeStart follows RFC 9628: a frame that begins a layer frame (B), is not inter-predicted (P
// clear), and is in the base spatial layer. That alone also fits an intra-only frame, so it looks into the frame's own
// header too.
func vp9KeyframeStart(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	b0 := p[0]
	interPicture := b0&0x40 != 0
	hasLayer := b0&0x20 != 0
	begin := b0&0x08 != 0
	if interPicture || !begin {
		return false
	}
	var desc codecs.VP9Packet
	payload, err := desc.Unmarshal(p)
	if err != nil {
		return false
	}
	if hasLayer && desc.SID != 0 {
		return false
	}
	return vp9HeaderIsKeyframe(payload)
}

// vp9HeaderIsKeyframe reads the start of a VP9 uncompressed header: a two-bit frame marker, two profile bits, a reserved
// bit in profile 3 only, then show_existing_frame, then frame_type, where 0 is a key frame.
func vp9HeaderIsKeyframe(h []byte) bool {
	if len(h) < 1 {
		return false
	}
	bits := h[0]
	if bits>>6 != 2 {
		return false
	}
	profile := (bits>>5)&1 | (bits>>4)&1<<1
	shift := 3
	if profile == 3 {
		shift = 2
	}
	showExisting := bits>>shift&1 != 0
	frameType := bits >> (shift - 1) & 1
	return !showExisting && frameType == 0
}

func h264KeyframeStart(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	isKey := func(nal byte) bool { return nal == 5 || nal == 7 }
	switch t := p[0] & 0x1f; t {
	case 24: // STAP-A
		for i := 1; i+2 < len(p); {
			size := int(p[i])<<8 | int(p[i+1])
			if isKey(p[i+2] & 0x1f) {
				return true
			}
			i += 2 + size
		}
		return false
	case 28: // FU-A
		return p[1]&0x80 != 0 && isKey(p[1]&0x1f)
	default:
		return isKey(t)
	}
}

func h265KeyframeStart(p []byte) bool {
	if len(p) < 3 {
		return false
	}
	isKey := func(t byte) bool { return (t >= 16 && t <= 21) || (t >= 32 && t <= 34) }
	switch t := (p[0] >> 1) & 0x3f; t {
	case 48: // aggregation packet
		for i := 2; i+2 < len(p); {
			size := int(p[i])<<8 | int(p[i+1])
			if isKey((p[i+2] >> 1) & 0x3f) {
				return true
			}
			i += 2 + size
		}
		return false
	case 49: // fragmentation unit
		return p[2]&0x80 != 0 && isKey(p[2]&0x3f)
	default:
		return isKey(t)
	}
}

// av1KeyframeStart reads the aggregation header. N marks the first packet of a new coded video sequence, and that always
// begins with a key frame, whose first OBU cannot be a continuation (Z) of something before it.
func av1KeyframeStart(p []byte) bool {
	return len(p) > 0 && p[0]&0x08 != 0 && p[0]&0x80 == 0
}

// redPrimary returns the primary (most recent) encoding from an RFC 2198 RED payload, for subscribers
// that did not negotiate RED.
func redPrimary(p []byte) ([]byte, bool) {
	offset := 0
	blockBytes := 0
	for {
		if offset >= len(p) {
			return nil, false
		}
		if p[offset]&0x80 == 0 { // F clear: the last header, one byte long, describes the primary block
			offset++
			break
		}
		if offset+4 > len(p) {
			return nil, false
		}
		blockBytes += int(p[offset+2]&0x03)<<8 | int(p[offset+3])
		offset += 4
	}
	start := offset + blockBytes
	if start > len(p) {
		return nil, false
	}
	return p[start:], true
}
