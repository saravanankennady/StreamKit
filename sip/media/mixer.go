package media

import "sync"

const (
	// mixHold is how much audio an input gathers before it starts to play, to ride out uneven arrival.
	mixHold = 2 * RoomFrame
	// mixLimit is the most an input may hold. Past this it goes back to mixHold: the audio it drops is old, and
	// keeping it would delay this talker by that much for the rest of the call.
	mixLimit = 6 * RoomFrame
)

// Mixer sums the audio of everyone in a room into one stream at the room's rate, for the phone to hear.
type Mixer struct {
	mu     sync.Mutex
	inputs map[*MixInput]bool
	sum    []int32
}

// NewMixer creates a mixer.
func NewMixer() *Mixer { return &Mixer{inputs: map[*MixInput]bool{}, sum: make([]int32, RoomFrame)} }

// MixInput is one talker.
type MixInput struct {
	m       *Mixer
	buf     []int16
	started bool
}

// Add creates an input. Remove it when the talker goes.
func (m *Mixer) Add() *MixInput {
	in := &MixInput{m: m}
	m.mu.Lock()
	m.inputs[in] = true
	m.mu.Unlock()
	return in
}

// Remove drops an input.
func (m *Mixer) Remove(in *MixInput) {
	m.mu.Lock()
	delete(m.inputs, in)
	m.mu.Unlock()
}

// Write adds samples to an input.
func (in *MixInput) Write(pcm []int16) {
	in.m.mu.Lock()
	defer in.m.mu.Unlock()
	in.buf = append(in.buf, pcm...)
	if len(in.buf) > mixLimit {
		in.buf = append(in.buf[:0], in.buf[len(in.buf)-mixHold:]...)
	}
}

// Read fills dst, which must hold RoomFrame samples, with the next frame of the mix. Silence if nobody is talking.
func (m *Mixer) Read(dst []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.sum)
	for in := range m.inputs {
		if !in.started {
			if len(in.buf) < mixHold {
				continue
			}
			in.started = true
		}
		n := min(len(in.buf), RoomFrame)
		for i := range n {
			m.sum[i] += int32(in.buf[i])
		}
		in.buf = append(in.buf[:0], in.buf[n:]...)
		if n < RoomFrame {
			// Ran dry. Wait for enough again, rather than playing the next trickle in pieces.
			in.started = false
		}
	}
	for i, v := range m.sum {
		switch {
		case v > 32767:
			dst[i] = 32767
		case v < -32768:
			dst[i] = -32768
		default:
			dst[i] = int16(v)
		}
	}
}
