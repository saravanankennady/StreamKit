package media

import (
	"sync"
	"sync/atomic"
	"time"
)

// ToRoom sends a frame of Opus into the room.
type ToRoom func(packet []byte, duration time.Duration) error

// Bridge joins a call's RTP to a room: what the phone sends goes to the room as Opus, and what the room says, mixed, goes to the phone.
type Bridge struct {
	session *Session
	mixer   *Mixer

	mu      sync.Mutex
	enc     *OpusEncoder
	up      Upsampler
	pending []int16
	room    ToRoom
	roomOut [RoomFrame]int16

	down     Downsampler
	mixBuf   [RoomFrame]int16
	override atomic.Pointer[Tone]
	stop     chan struct{}
	stopped  sync.Once
	running  sync.WaitGroup
}

// NewBridge creates a bridge for a session.
func NewBridge(s *Session) (*Bridge, error) {
	enc, err := NewOpusEncoder()
	if err != nil {
		return nil, err
	}
	b := &Bridge{session: s, mixer: NewMixer(), enc: enc, stop: make(chan struct{})}
	s.OnAudio = b.fromPhone
	return b, nil
}

// Mixer is where the room's audio goes.
func (b *Bridge) Mixer() *Mixer { return b.mixer }

// SetRoom says where the phone's audio goes. Until this is called it goes nowhere, which is what a caller entering a PIN needs.
func (b *Bridge) SetRoom(room ToRoom) {
	b.mu.Lock()
	b.room = room
	b.mu.Unlock()
}

// PlayToPhone plays a tone to the phone in place of the room, or goes back to the room when given nil.
func (b *Bridge) PlayToPhone(t *Tone) { b.override.Store(t) }

func (b *Bridge) fromPhone(pcm []int16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.room == nil || b.enc == nil {
		b.pending = b.pending[:0]
		return
	}
	b.pending = append(b.pending, pcm...)
	for len(b.pending) >= PhoneFrame {
		b.up.Process(b.pending[:PhoneFrame], b.roomOut[:])
		b.pending = append(b.pending[:0], b.pending[PhoneFrame:]...)
		packet, err := b.enc.Encode(b.roomOut[:])
		if err != nil {
			continue
		}
		_ = b.room(packet, FrameMs*time.Millisecond)
	}
}

func (b *Bridge) toPhone(dst []int16) {
	if t := b.override.Load(); t != nil {
		t.Read(dst)
		return
	}
	b.mixer.Read(b.mixBuf[:])
	b.down.Process(b.mixBuf[:], dst)
}

// Start sends to the phone until the bridge is closed.
func (b *Bridge) Start() {
	b.running.Add(1)
	go func() {
		defer b.running.Done()
		b.session.RunSender(b.stop, b.toPhone)
	}()
}

// PlayToRoom plays a tone into the room from now until the returned function is called. The phone's own audio takes over from there.
func (b *Bridge) PlayToRoom(t *Tone, room ToRoom) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pcm := make([]int16, RoomFrame)
		frame := FrameMs * time.Millisecond
		tick := time.NewTicker(frame)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-b.stop:
				return
			case <-tick.C:
			}
			t.Read(pcm)
			b.mu.Lock()
			if b.enc != nil {
				if packet, err := b.enc.Encode(pcm); err == nil {
					_ = room(packet, frame)
				}
			}
			b.mu.Unlock()
		}
	}()
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

// Close stops the bridge. The session is the caller's to close.
func (b *Bridge) Close() {
	b.stopped.Do(func() { close(b.stop) })
	b.running.Wait()
	b.mu.Lock()
	b.room = nil
	if b.enc != nil {
		b.enc.Close()
		b.enc = nil
	}
	b.mu.Unlock()
}
