package datatrack

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func fromHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Bytes captured from the official client, on its way to the reference (GAP-011).
const (
	singlePacketFrame = "1c00" + "0001" + "0000" + "0000" + "13b8a7c5" + "0002" + "0208" + "00000000000003e8" + "0101"
	firstOfThree      = "1400" + "0001" + "0001" + "0001" + "13b8a7c5" + "0002" + "0208" + "00000000000003e9" + "0202"
	middleOfThree     = "0400" + "0001" + "0002" + "0001" + "13b8a7c5" + "0002" + "0208" + "00000000000003e9" + "0202"
	lastOfThree       = "0c00" + "0001" + "0003" + "0001" + "13b8a7c5" + "0002" + "0208" + "00000000000003e9" + "0202"
)

func TestParseCapturedPackets(t *testing.T) {
	cases := []struct {
		name         string
		hex          string
		start, final bool
		sequence     uint16
		frame        uint16
	}{
		{"one packet", singlePacketFrame, true, true, 0, 0},
		{"first of three", firstOfThree, true, false, 1, 1},
		{"middle of three", middleOfThree, false, false, 2, 1},
		{"last of three", lastOfThree, false, true, 3, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := Parse(fromHex(t, c.hex))
			if err != nil {
				t.Fatal(err)
			}
			if h.Start != c.start || h.Final != c.final || h.Sequence != c.sequence || h.Frame != c.frame {
				t.Errorf("%+v", h)
			}
			if h.Handle != 1 || h.Version != 0 || h.Timestamp != 0x13b8a7c5 {
				t.Errorf("%+v", h)
			}
			if h.Size != 24 {
				t.Errorf("header plus extension is %d bytes, and 24 were observed", h.Size)
			}
		})
	}
}

func TestParseWithoutExtensions(t *testing.T) {
	h, err := Parse(fromHex(t, "1800"+"002a"+"0007"+"0009"+"000000ff"+"cafe"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Handle != 42 || h.Sequence != 7 || h.Frame != 9 || h.Timestamp != 255 || h.Size != HeaderSize {
		t.Errorf("%+v", h)
	}
}

func TestParseRejectsWhatDoesNotFit(t *testing.T) {
	cases := []struct {
		name string
		hex  string
		want error
	}{
		{"under 12 bytes", "1800000100000000000000", ErrTooShort},
		{"handle 0", "1800" + "0000" + "0000" + "0000" + "00000000", ErrZeroHandle},
		{"no room for the extension size", "1c00" + "0001" + "0000" + "0000" + "00000000", ErrBadExtension},
		{"extensions run past the end", "1c00" + "0001" + "0000" + "0000" + "00000000" + "0100" + "0000", ErrBadExtension},
		{"an extension runs past the word count", "1c00" + "0001" + "0000" + "0000" + "00000000" + "0000" + "02ff", ErrBadExtension},
		{"an extension's size byte is past the word count", "1c00" + "0001" + "0000" + "0000" + "00000000" + "0000" + "0200" + "02", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(fromHex(t, c.hex))
			if c.want == nil {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestParseToleratesUnknownExtensionsAndPadding(t *testing.T) {
	// An extension with an ID nothing knows, then padding to the word boundary.
	h, err := Parse(fromHex(t, "1c00"+"0001"+"0000"+"0000"+"00000000"+"0001"+"7701ee"+"000000"+"beef"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != 20 {
		t.Errorf("size = %d", h.Size)
	}
}

// Whatever a client sends, the parser must answer without running off the buffer.
func TestParseNeverPanics(t *testing.T) {
	packet := fromHex(t, singlePacketFrame)
	for i := range packet {
		_, _ = Parse(packet[:i])
		for _, v := range []byte{0x00, 0x04, 0xff} {
			mangled := bytes.Clone(packet)
			mangled[i] = v
			_, _ = Parse(mangled)
		}
	}
}

func TestForwardGivesEachSubscriberItsOwnHandle(t *testing.T) {
	tr := NewTrack("DTR_a", "name", 1, "PA_a", "alice")
	var a, b []byte
	tr.Add(&Subscriber{ID: "x", Handle: 16, Send: func(p []byte) { a = bytes.Clone(p) }})
	tr.Add(&Subscriber{ID: "y", Handle: 0xf8, Send: func(p []byte) { b = bytes.Clone(p) }})

	packet := fromHex(t, firstOfThree)
	original := bytes.Clone(packet)
	tr.Forward(packet)
	if !bytes.Equal(packet, original) {
		t.Fatal("Forward changed the packet it was given")
	}

	// Byte-identical except the handle, as observed on the reference.
	want := bytes.Clone(original)
	want[2], want[3] = 0x00, 0x10
	if !bytes.Equal(a, want) {
		t.Errorf("first subscriber got %x, want %x", a, want)
	}
	want[2], want[3] = 0x00, 0xf8
	if !bytes.Equal(b, want) {
		t.Errorf("second subscriber got %x, want %x", b, want)
	}

	if !tr.Remove("x") || tr.Remove("x") {
		t.Fatal("Remove reports wrongly")
	}
	a = nil
	tr.Forward(packet)
	if a != nil {
		t.Fatal("sent to a removed subscriber")
	}
}

func TestAddReplacesASubscribersEntry(t *testing.T) {
	tr := NewTrack("DTR_a", "name", 1, "PA_a", "alice")
	tr.Add(&Subscriber{ID: "x", Handle: 1, Send: func([]byte) {}})
	tr.Add(&Subscriber{ID: "x", Handle: 2, Send: func([]byte) {}})
	subs := tr.Subscribers()
	if len(subs) != 1 || subs[0].Handle != 2 {
		t.Fatalf("%+v", subs)
	}
}
