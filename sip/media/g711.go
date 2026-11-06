// Package media is the audio side of a phone call: G.711 on the phone's side, Opus on the room's side, the resampling and mixing
// between them, RTP and SRTP on the wire, and DTMF.
package media

// G.711 companding, from the description in ITU-T G.711. The tables are built once, because decoding every packet is the
// hot path, and encoding takes a 14-bit or 13-bit range that is small enough to look up.

const (
	ulawBias = 0x84
	ulawClip = 32635
	alawClip = 32635
)

var (
	ulawDecode [256]int16
	alawDecode [256]int16
)

func init() {
	for i := range ulawDecode {
		ulawDecode[i] = decodeULaw(byte(i))
		alawDecode[i] = decodeALaw(byte(i))
	}
}

func decodeULaw(b byte) int16 {
	b = ^b
	sign := b & 0x80
	exponent := (b >> 4) & 0x07
	mantissa := int32(b & 0x0f)
	sample := ((mantissa << 3) + ulawBias) << exponent
	sample -= ulawBias
	if sign != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

func decodeALaw(b byte) int16 {
	b ^= 0x55
	sign := b & 0x80
	exponent := (b >> 4) & 0x07
	mantissa := int32(b & 0x0f)
	var sample int32
	if exponent == 0 {
		sample = (mantissa << 4) + 8
	} else {
		sample = ((mantissa << 4) + 0x108) << (exponent - 1)
	}
	if sign != 0 {
		return int16(sample)
	}
	return int16(-sample)
}

func encodeULaw(s int16) byte {
	v := int32(s)
	sign := byte(0)
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > ulawClip {
		v = ulawClip
	}
	v += ulawBias
	exponent := byte(7)
	for mask := int32(0x4000); exponent > 0 && v&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte((v >> (exponent + 3)) & 0x0f)
	return ^(sign | (exponent << 4) | mantissa)
}

func encodeALaw(s int16) byte {
	v := int32(s)
	sign := byte(0x80)
	if v < 0 {
		v = -v - 1
		sign = 0
	}
	if v > alawClip {
		v = alawClip
	}
	var out byte
	if v >= 256 {
		exponent := byte(7)
		for mask := int32(0x4000); exponent > 1 && v&mask == 0; mask >>= 1 {
			exponent--
		}
		mantissa := byte((v >> (exponent + 3)) & 0x0f)
		out = (exponent << 4) | mantissa
	} else {
		out = byte(v >> 4)
	}
	return (sign | out) ^ 0x55
}

// Codec is an audio codec on the phone's side.
type Codec struct {
	Name string
	PT   uint8
}

// The codecs a call can use. Both run at 8 kHz, one byte to a sample.
const (
	CodecPCMU = "PCMU"
	CodecPCMA = "PCMA"
)

// Decode expands a G.711 payload into dst, and returns how many samples that is.
func (c Codec) Decode(payload []byte, dst []int16) int {
	n := min(len(payload), len(dst))
	table := &ulawDecode
	if c.Name == CodecPCMA {
		table = &alawDecode
	}
	for i := 0; i < n; i++ {
		dst[i] = table[payload[i]]
	}
	return n
}

// Encode compresses samples into dst, and returns how many bytes that is.
func (c Codec) Encode(pcm []int16, dst []byte) int {
	n := min(len(pcm), len(dst))
	if c.Name == CodecPCMA {
		for i := 0; i < n; i++ {
			dst[i] = encodeALaw(pcm[i])
		}
		return n
	}
	for i := 0; i < n; i++ {
		dst[i] = encodeULaw(pcm[i])
	}
	return n
}
