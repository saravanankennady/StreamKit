// Package datatrack carries data tracks: lossy streams of small packets, with a 12-byte header, on their own data channel.
// The server only reads a packet's header, replaces the publisher's handle with the subscriber's, and sends it on. It does
// not reassemble, reorder or retransmit anything, and sequence numbers, frame numbers and timestamps pass through.
package datatrack

import (
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
)

// ChannelLabel names the data channel, on both of a client's connections.
const ChannelLabel = "_data_track"

// HeaderSize is the part of the header every packet has.
const HeaderSize = 12

const (
	handleOffset    = 2
	extensionOffset = HeaderSize
	extensionFlag   = 1 << 2
	startFlag       = 1 << 4
	finalFlag       = 1 << 3
)

// Errors from Parse. All of these come from client-controlled input, and the packet is dropped.
var (
	ErrTooShort     = errors.New("data track packet is shorter than its header")
	ErrBadExtension = errors.New("data track packet has an extension that runs past its end")
	ErrZeroHandle   = errors.New("data track packet has no handle")
)

// Header is what a packet says about itself.
type Header struct {
	Version   uint8
	Start     bool
	Final     bool
	Handle    uint16
	Sequence  uint16
	Frame     uint16
	Timestamp uint32
	// Size is the header's length, including extensions and their padding.
	Size int
}

// Parse reads a packet's header and checks that its extensions fit. It does not read what they say: unknown ones pass
// through with the rest of the packet.
func Parse(p []byte) (Header, error) {
	if len(p) < HeaderSize {
		return Header{}, ErrTooShort
	}
	h := Header{
		Version:   p[0] >> 5,
		Start:     p[0]&startFlag != 0,
		Final:     p[0]&finalFlag != 0,
		Handle:    binary.BigEndian.Uint16(p[handleOffset:]),
		Sequence:  binary.BigEndian.Uint16(p[4:]),
		Frame:     binary.BigEndian.Uint16(p[6:]),
		Timestamp: binary.BigEndian.Uint32(p[8:]),
		Size:      HeaderSize,
	}
	if h.Handle == 0 {
		return Header{}, ErrZeroHandle
	}
	if p[0]&extensionFlag == 0 {
		return h, nil
	}
	if len(p) < extensionOffset+2 {
		return Header{}, ErrBadExtension
	}
	// The size is in 32-bit words, minus one, and those words include the two bytes that say so.
	end := extensionOffset + (int(binary.BigEndian.Uint16(p[extensionOffset:]))+1)*4
	if end > len(p) {
		return Header{}, ErrBadExtension
	}
	for i := extensionOffset + 2; i < end; {
		if p[i] == 0 {
			// An ID of zero starts the padding.
			break
		}
		if i+2 > end || i+2+int(p[i+1]) > end {
			return Header{}, ErrBadExtension
		}
		i += 2 + int(p[i+1])
	}
	h.Size = end
	return h, nil
}

// SetHandle overwrites a packet's handle in place.
func SetHandle(p []byte, handle uint16) { binary.BigEndian.PutUint16(p[handleOffset:], handle) }

// Subscriber is one participant receiving a track.
type Subscriber struct {
	// ID identifies the subscribing participant.
	ID string
	// Handle is what this subscriber knows the track by. It has nothing to do with the publisher's handle.
	Handle uint16
	// Send delivers a packet. It must not block, and it must copy anything it keeps.
	Send func(packet []byte)
}

// Track is one published data track.
type Track struct {
	sid           string
	name          string
	handle        uint16
	ownerSID      string
	ownerIdentity string

	mu   sync.Mutex
	subs atomic.Pointer[[]*Subscriber]
}

// NewTrack creates a track with no subscribers.
func NewTrack(sid, name string, handle uint16, ownerSID, ownerIdentity string) *Track {
	t := &Track{sid: sid, name: name, handle: handle, ownerSID: ownerSID, ownerIdentity: ownerIdentity}
	empty := []*Subscriber{}
	t.subs.Store(&empty)
	return t
}

// SID returns the server-assigned ID.
func (t *Track) SID() string { return t.sid }

// Name returns the name, unique per publisher.
func (t *Track) Name() string { return t.name }

// Handle returns the publisher's handle.
func (t *Track) Handle() uint16 { return t.handle }

// OwnerSID returns the publishing participant's ID.
func (t *Track) OwnerSID() string { return t.ownerSID }

// OwnerIdentity returns the publishing participant's identity.
func (t *Track) OwnerIdentity() string { return t.ownerIdentity }

// Subscribers returns who receives this track now.
func (t *Track) Subscribers() []*Subscriber { return *t.subs.Load() }

// Add starts sending to a subscriber, replacing any earlier entry for it.
func (t *Track) Add(s *Subscriber) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := slices.DeleteFunc(slices.Clone(*t.subs.Load()), func(x *Subscriber) bool { return x.ID == s.ID })
	next = append(next, s)
	t.subs.Store(&next)
}

// Remove stops sending to a subscriber, and reports whether it was there.
func (t *Track) Remove(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := *t.subs.Load()
	next := slices.DeleteFunc(slices.Clone(old), func(x *Subscriber) bool { return x.ID == id })
	t.subs.Store(&next)
	return len(next) != len(old)
}

// Forward sends a packet to every subscriber, each under its own handle. The packet is left as it was. Each Send sees the
// same scratch copy, so what it keeps it has to copy.
func (t *Track) Forward(packet []byte) {
	subs := *t.subs.Load()
	if len(subs) == 0 {
		return
	}
	out := slices.Clone(packet)
	for _, s := range subs {
		SetHandle(out, s.Handle)
		s.Send(out)
	}
}
