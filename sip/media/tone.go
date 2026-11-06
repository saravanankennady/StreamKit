package media

import "math"

// Tone makes a repeating cadence of one or two frequencies.
type Tone struct {
	freqs []float64
	on    int
	off   int
	rate  int
	pos   int
}

// NewTone creates a tone at a sample rate. onMs and offMs make the cadence, and an offMs of zero is continuous.
func NewTone(rate int, onMs, offMs int, freqs ...float64) *Tone {
	return &Tone{freqs: freqs, on: rate * onMs / 1000, off: rate * offMs / 1000, rate: rate}
}

// Read fills dst.
func (t *Tone) Read(dst []int16) {
	period := t.on + t.off
	amp := 8000.0 / float64(len(t.freqs))
	for i := range dst {
		if period > 0 && t.pos%period >= t.on {
			dst[i] = 0
		} else {
			var v float64
			for _, f := range t.freqs {
				v += math.Sin(2 * math.Pi * f * float64(t.pos) / float64(t.rate))
			}
			dst[i] = int16(amp * v)
		}
		t.pos++
	}
}

// Dialtone is the name of a national ringing tone, as the API gives it.
type Dialtone int32

// The tones, in the numbers the API uses for them.
const (
	DialtoneDisabled Dialtone = iota
	DialtoneETSI
	DialtoneAuto
	DialtoneUS
	DialtoneUK
	DialtoneJP
	DialtoneAU
)

// NewDialtone makes a ringing tone at a sample rate, or nil for none. Unknown names, and AUTO, get the European one, because a
// number's country is not worked out here.
func NewDialtone(d Dialtone, rate int) *Tone {
	switch d {
	case DialtoneDisabled:
		return nil
	case DialtoneUS:
		return NewTone(rate, 2000, 4000, 440, 480)
	case DialtoneUK:
		return NewTone(rate, 400, 200, 400, 450)
	case DialtoneJP:
		return NewTone(rate, 1000, 2000, 400)
	case DialtoneAU:
		return NewTone(rate, 400, 200, 425, 450)
	}
	return NewTone(rate, 1000, 4000, 425)
}
