package ingress

import (
	"encoding/binary"
	"io"
)

// FLV tag types, which are also RTMP message types.
const (
	tagAudio  = 8
	tagVideo  = 9
	tagScript = 18
)

// flvHeader says the stream may hold audio and video, followed by the first "previous tag size" of zero.
var flvHeader = []byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}

// writeFLVTag writes one tag and the size that follows it. RTMP messages and FLV tags carry the same bytes, so an RTMP
// stream is one small header away from a file ffmpeg can read.
func writeFLVTag(w io.Writer, kind byte, timestamp uint32, payload []byte) error {
	size := len(payload)
	head := [11]byte{
		kind,
		byte(size >> 16), byte(size >> 8), byte(size),
		byte(timestamp >> 16), byte(timestamp >> 8), byte(timestamp),
		byte(timestamp >> 24),
	}
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], uint32(size+len(head)))
	_, err := w.Write(tail[:])
	return err
}
