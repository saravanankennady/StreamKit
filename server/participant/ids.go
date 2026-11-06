package participant

import (
	"crypto/rand"
	"strings"

	"elnova-streamkit.local/pkg/pb"
)

const idAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// NewID returns a prefixed random identifier, such as "PA_K4dXw2qRt8mN". The alphabet leaves out
// characters that are easy to confuse. 12 characters carry about 70 bits of randomness.
func NewID(prefix string) string {
	const length = 12
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	var b strings.Builder
	b.Grow(len(prefix) + length)
	b.WriteString(prefix)
	for _, c := range raw {
		b.WriteByte(idAlphabet[int(c)%len(idAlphabet)])
	}
	return b.String()
}

// Prefixes of generated identifiers.
const (
	RoomPrefix        = "RM_"
	ParticipantPrefix = "PA_"
	DataTrackPrefix   = "DTR_"
)

// TrackIDPrefix encodes a track's kind and source in its ID. It makes logs readable and matches the
// format clients of this protocol display.
func TrackIDPrefix(kind pb.TrackType, source pb.TrackSource) string {
	k := "D"
	switch kind {
	case pb.TrackType_AUDIO:
		k = "A"
	case pb.TrackType_VIDEO:
		k = "V"
	}
	s := "U"
	switch source {
	case pb.TrackSource_CAMERA:
		s = "C"
	case pb.TrackSource_MICROPHONE:
		s = "M"
	case pb.TrackSource_SCREEN_SHARE:
		s = "S"
	case pb.TrackSource_SCREEN_SHARE_AUDIO:
		s = "s"
	}
	return "TR_" + k + s
}
