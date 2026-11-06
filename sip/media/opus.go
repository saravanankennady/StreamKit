package media

import (
	"fmt"

	"github.com/thesyncim/gopus"
)

// Opus goes through this small layer so that the library behind it can change without touching anything else. See
// docs/deployment/sip.md for why this one: it is written in Go with no C and no tricks with memory, and it is checked against libopus.

const (
	opusBitrate    = 32000
	opusComplexity = 5
	// maxOpusSamples is the longest frame Opus allows, 120 ms at 48 kHz.
	maxOpusSamples = 5760
)

// OpusEncoder makes mono Opus frames at the room's rate. It is not safe for use by more than one goroutine.
type OpusEncoder struct {
	enc *gopus.Encoder
	buf []byte
}

// NewOpusEncoder creates an encoder set up for speech.
func NewOpusEncoder() (*OpusEncoder, error) {
	enc, err := gopus.NewEncoder(gopus.EncoderConfig{SampleRate: RoomRate, Channels: 1, Application: gopus.ApplicationVoIP})
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetBitrate(opusBitrate); err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetComplexity(opusComplexity); err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	return &OpusEncoder{enc: enc, buf: make([]byte, 1500)}, nil
}

// Encode compresses one frame of RoomFrame samples. What it returns is only good until the next call.
func (e *OpusEncoder) Encode(pcm []int16) (packet []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("opus encoder: %v", r)
		}
	}()
	n, err := e.enc.EncodeInt16(pcm, e.buf)
	if err != nil {
		return nil, err
	}
	return e.buf[:n], nil
}

// Close releases the encoder.
func (e *OpusEncoder) Close() {}

// OpusDecoder turns Opus packets, mono or stereo, into mono at the room's rate. It is not safe for use by more than one goroutine.
type OpusDecoder struct {
	dec *gopus.Decoder
	buf []int16
}

// NewOpusDecoder creates a decoder.
func NewOpusDecoder() (*OpusDecoder, error) {
	dec, err := gopus.NewDecoder(gopus.DefaultDecoderConfig(RoomRate, 1))
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}
	return &OpusDecoder{dec: dec, buf: make([]int16, maxOpusSamples)}, nil
}

// Decode expands one packet. What it returns is only good until the next call. These packets come from whoever is in the room, so a
// decoder that trips over one must not take the whole worker with it.
func (d *OpusDecoder) Decode(packet []byte) (pcm []int16, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("opus decoder: %v", r)
		}
	}()
	n, err := d.dec.DecodeInt16(packet, d.buf)
	if err != nil {
		return nil, err
	}
	return d.buf[:n], nil
}

// Close releases the decoder.
func (d *OpusDecoder) Close() {}
