package media

import "encoding/binary"

// DTMF over RTP, from RFC 4733.

// dtmfDigits maps a digit to its event code, in the order the RFC numbers them.
const dtmfDigits = "0123456789*#ABCD"

// DigitCode gives the event code for a digit, or -1.
func DigitCode(digit byte) int {
	switch {
	case digit >= '0' && digit <= '9':
		return int(digit - '0')
	case digit == '*':
		return 10
	case digit == '#':
		return 11
	case digit >= 'A' && digit <= 'D':
		return 12 + int(digit-'A')
	case digit >= 'a' && digit <= 'd':
		return 12 + int(digit-'a')
	}
	return -1
}

// CodeDigit gives the digit for an event code, or 0 for a code that is not a digit.
func CodeDigit(code uint8) byte {
	if int(code) < len(dtmfDigits) {
		return dtmfDigits[code]
	}
	return 0
}

const dtmfVolume = 10

// DTMFPayload builds an event payload. duration is in timestamp units.
func DTMFPayload(code uint8, end bool, duration uint16) []byte {
	p := make([]byte, 4)
	p[0] = code
	p[1] = dtmfVolume
	if end {
		p[1] |= 0x80
	}
	binary.BigEndian.PutUint16(p[2:], duration)
	return p
}

// DTMFEvent is an event read from a packet.
type DTMFEvent struct {
	Code     uint8
	End      bool
	Duration uint16
}

// ParseDTMF reads an event payload.
func ParseDTMF(p []byte) (DTMFEvent, bool) {
	if len(p) < 4 {
		return DTMFEvent{}, false
	}
	return DTMFEvent{Code: p[0], End: p[1]&0x80 != 0, Duration: binary.BigEndian.Uint16(p[2:])}, true
}

// DTMFReceiver turns a stream of event packets into digits. An event is sent many times, and carriers reuse timestamps and lose
// marker bits, so a digit is identified by its timestamp and code together. Pressing the same digit twice gives a new timestamp.
type DTMFReceiver struct {
	lastTS   uint32
	lastCode uint8
	seen     bool
}

// Receive returns a digit the first time an event shows up, and 0 for every repeat of it.
func (r *DTMFReceiver) Receive(ts uint32, payload []byte) byte {
	ev, ok := ParseDTMF(payload)
	if !ok {
		return 0
	}
	digit := CodeDigit(ev.Code)
	if digit == 0 {
		return 0
	}
	if r.seen && r.lastTS == ts && r.lastCode == ev.Code {
		return 0
	}
	r.seen, r.lastTS, r.lastCode = true, ts, ev.Code
	return digit
}
