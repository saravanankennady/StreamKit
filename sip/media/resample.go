package media

import "math"

const (
	// RoomRate is the sample rate on the room's side. FrameMs of it is one Opus frame.
	RoomRate = 48000
	// PhoneRate is the sample rate on the phone's side.
	PhoneRate = 8000
	// FrameMs is how long a frame is, on both sides.
	FrameMs = 20
	// RoomFrame and PhoneFrame are one frame's samples.
	RoomFrame  = RoomRate * FrameMs / 1000
	PhoneFrame = PhoneRate * FrameMs / 1000

	resampleRatio = RoomRate / PhoneRate
	// resampleTaps sets how sharp the filter is. This gives about 70 dB of rejection above 4.4 kHz, for 2.5 ms of delay each way.
	resampleTaps = 240
	// resampleCutoff sits just below the 4 kHz limit of the phone side.
	resampleCutoff = 3800.0
)

// A single low-pass filter serves both directions. Blackman-windowed sinc: there is nothing here that needs anything fancier.
var resampleFilter = func() [resampleTaps]float32 {
	var h [resampleTaps]float32
	fc := resampleCutoff / RoomRate
	mid := float64(resampleTaps-1) / 2
	var sum float64
	var raw [resampleTaps]float64
	for n := range raw {
		x := float64(n) - mid
		sinc := 2 * fc
		if x != 0 {
			sinc = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		a := 2 * math.Pi * float64(n) / float64(resampleTaps-1)
		window := 0.42 - 0.5*math.Cos(a) + 0.08*math.Cos(2*a)
		raw[n] = sinc * window
		sum += raw[n]
	}
	for n := range raw {
		h[n] = float32(raw[n] / sum)
	}
	return h
}()

func clip16(v float32) int16 {
	switch {
	case v >= 32767:
		return 32767
	case v <= -32768:
		return -32768
	case v >= 0:
		return int16(v + 0.5)
	}
	return int16(v - 0.5)
}

// Upsampler goes from the phone's rate to the room's.
type Upsampler struct {
	history [resampleTaps / resampleRatio]float32
}

// Process makes len(in)*6 samples in out. out must have room for them.
func (u *Upsampler) Process(in []int16, out []int16) {
	taps := len(u.history)
	o := 0
	for _, s := range in {
		copy(u.history[1:], u.history[:taps-1])
		u.history[0] = float32(s)
		for phase := range resampleRatio {
			var acc float32
			// Each output is the filter at one offset over the input with zeros stuffed between samples, which only
			// needs every sixth tap. Stuffing zeros divides the level by six, so this multiplies it back.
			for i := range taps {
				acc += u.history[i] * resampleFilter[phase+i*resampleRatio]
			}
			out[o] = clip16(acc * resampleRatio)
			o++
		}
	}
}

// Downsampler goes from the room's rate to the phone's.
type Downsampler struct {
	history [resampleTaps]float32
	pos     int
	phase   int
}

// Process keeps one sample in six of a filtered in, and returns how many it wrote to out. out must have room for len(in)/6+1.
func (d *Downsampler) Process(in []int16, out []int16) int {
	n := 0
	for _, s := range in {
		d.history[d.pos] = float32(s)
		d.pos = (d.pos + 1) % resampleTaps
		d.phase++
		if d.phase < resampleRatio {
			continue
		}
		d.phase = 0
		var acc float32
		idx := d.pos
		for i := range resampleTaps {
			acc += d.history[idx] * resampleFilter[i]
			idx++
			if idx == resampleTaps {
				idx = 0
			}
		}
		out[n] = clip16(acc)
		n++
	}
	return n
}
