package sfu

import (
	"io"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Chrome probes bandwidth by resending old packets. When one of those starts a keyframe, a viewer still waiting for
// a keyframe must not start from it: the packets after it have already been dropped, and cannot be sent again.
func TestViewerDoesNotStartFromAResentKeyframe(t *testing.T) {
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: vp8Codec, RequestKeyframe: func(uint32) {}})
	t.Cleanup(r.Close)
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	w := &fakeWriter{}
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{vp8Codec}, w: w}); err != nil {
		t.Fatal(err)
	}
	d.SetTarget(LayerHigh)
	r.AddDownTrack(d)

	stream := []struct {
		seq uint16
		key bool
	}{
		{500, false},
		{501, false},
		{499, true}, // the old keyframe, sent again
		{502, false},
		{503, true}, // the next real keyframe
		{504, false},
	}
	done := make(chan struct{})
	next := 0
	r.AddLayer(LayerHigh, 1, func(b []byte) (int, error) {
		if next == len(stream) {
			close(done)
			return 0, io.EOF
		}
		p := stream[next]
		next++
		pkt := rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: p.seq, Timestamp: uint32(p.seq) * 3000, SSRC: 1},
			Payload: vp8Payload(p.seq, uint8(p.seq), p.key),
		}
		data, err := pkt.Marshal()
		if err != nil {
			t.Error(err)
			return 0, io.EOF
		}
		return copy(b, data), nil
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was not read")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.sent) != 2 {
		t.Fatalf("sent %d packets, want the real keyframe and the one after it", len(w.sent))
	}
	if w.sent[0].payload[5] != 0x10 {
		t.Fatalf("the first packet sent is not a keyframe: %x", w.sent[0].payload)
	}
	if got := w.sent[1].hdr.SequenceNumber - w.sent[0].hdr.SequenceNumber; got != 1 {
		t.Fatalf("sequence numbers jump by %d", got)
	}
}

// Packets that never reached the server show up as lost in every viewer's reports. That is the publisher's network,
// and scoring the viewers for it would call everyone watching them weak too.
func TestLossBeforeTheServerIsNotCountedAgainstAViewer(t *testing.T) {
	opus := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2}, PayloadType: 111}
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeAudio, Codec: opus})
	t.Cleanup(r.Close)
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{opus}, w: &fakeWriter{}}); err != nil {
		t.Fatal(err)
	}
	forward := func(seq uint16) {
		d.Forward(&Packet{Header: &rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8}})
	}
	report := func(highest, totalLost uint32, fraction uint8) float32 {
		d.HandleRTCP([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 7777, LastSequenceNumber: highest, TotalLost: totalLost, FractionLost: fraction}}}})
		return d.Stats().Loss
	}

	for seq := uint16(100); seq < 200; seq++ {
		forward(seq)
	}
	if got := report(199, 0, 0); got != 0 {
		t.Fatalf("first report: %v", got)
	}

	// Every third packet of the next 300 never arrived here. The viewer got all that were sent, and reports the rest lost.
	for seq := uint16(200); seq < 500; seq++ {
		if seq%3 == 0 {
			continue
		}
		forward(seq)
	}
	if got := report(499, 100, 85); got != 0 {
		t.Fatalf("loss before the server was counted against the viewer: %v", got)
	}

	// The same again, and this time 60 of the 300 went missing on the way to the viewer as well.
	for seq := uint16(500); seq < 800; seq++ {
		if seq%3 == 0 {
			continue
		}
		forward(seq)
	}
	got := report(799, 260, 136)
	if got < 0.19 || got > 0.21 {
		t.Fatalf("loss on the way to the viewer: %v, want about 0.2", got)
	}

	// A packet arriving late fills the gap it left, so a viewer who then got it has lost nothing. The 100 skipped on the
	// way to 900 never reached the server either, and the viewer counts them.
	forward(900)
	forward(902)
	forward(901)
	for seq := uint16(903); seq < 1000; seq++ {
		forward(seq)
	}
	if got := report(999, 360, 0); got != 0 {
		t.Fatalf("after a late packet: %v", got)
	}

	// A report describes a moment ago. Here the server has counted gaps the viewer has not reached yet, which reads as
	// less lost than before, and then the next report catches up. Neither is loss on the way to the viewer.
	for seq := uint16(1000); seq <= 1300; seq++ {
		if seq%3 == 0 {
			continue
		}
		forward(seq)
	}
	if got := report(1150, 410, 42); got != 0 {
		t.Fatalf("a report behind what was forwarded: %v", got)
	}
	if got := report(1300, 460, 42); got != 0 {
		t.Fatalf("the report that catches up: %v", got)
	}
}

// With no figures yet to compare against, a first report stands, unless what was missing here explains it.
func TestFirstReportCountsOnlyWhatTheServerCannotExplain(t *testing.T) {
	opus := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2}, PayloadType: 111}
	first := func(seqs []uint16, totalLost uint32, fraction uint8) float32 {
		t.Helper()
		r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeAudio, Codec: opus})
		t.Cleanup(r.Close)
		d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
		if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{opus}, w: &fakeWriter{}}); err != nil {
			t.Fatal(err)
		}
		for _, seq := range seqs {
			d.Forward(&Packet{Header: &rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8}})
		}
		d.HandleRTCP([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 7777, LastSequenceNumber: uint32(seqs[len(seqs)-1]), TotalLost: totalLost, FractionLost: fraction}}}})
		return d.Stats().Loss
	}
	if got := first([]uint16{100, 101, 103, 104, 106}, 2, 80); got != 0 {
		t.Fatalf("the two missing here were counted against the viewer: %v", got)
	}
	if got := first([]uint16{100, 101, 102, 103, 104, 105, 106}, 2, 72); got == 0 {
		t.Fatal("real loss on the way to the viewer was hidden")
	}
}

// A muted participant's audio stops at the server, whatever their client keeps sending. What was dropped must not look
// like loss to the viewer afterwards, or a mute would read as a bad connection on the viewer's side.
func TestMutedAudioIsNotForwarded(t *testing.T) {
	opus := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2}, PayloadType: 111}
	r := NewReceiver(ReceiverParams{Kind: webrtc.RTPCodecTypeAudio, Codec: opus})
	t.Cleanup(r.Close)
	d := NewDownTrack(DownTrackParams{ID: "TR", StreamID: "PA|TR", Receiver: r})
	w := &fakeWriter{}
	if _, err := d.Bind(fakeContext{codecs: []webrtc.RTPCodecParameters{opus}, w: w}); err != nil {
		t.Fatal(err)
	}
	r.AddDownTrack(d)

	sentCount := func() int {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.sent)
	}
	feed := make(chan uint16)
	r.AddLayer(LayerHigh, 1, func(b []byte) (int, error) {
		seq, ok := <-feed
		if !ok {
			return 0, io.EOF
		}
		pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq) * 960, SSRC: 1}, Payload: []byte{0xf8, 0xff, 0xfe}}
		data, err := pkt.Marshal()
		if err != nil {
			t.Error(err)
			return 0, io.EOF
		}
		return copy(b, data), nil
	})
	defer close(feed)
	send := func(from, to uint16) {
		t.Helper()
		for seq := from; seq <= to; seq++ {
			feed <- seq
		}
		// The read loop asks for the next packet only once it has dealt with this one.
		feed <- to + 1
	}

	send(100, 109)
	before := sentCount()
	if before == 0 {
		t.Fatal("audio was not being forwarded to begin with")
	}

	r.SetMuted(true)
	send(112, 160)
	muted := sentCount()
	if muted > before+2 {
		t.Fatalf("%d packets went out while muted", muted-before)
	}

	r.SetMuted(false)
	send(200, 205)
	if got := sentCount(); got <= muted {
		t.Fatal("audio did not come back after unmuting")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 1; i < len(w.sent); i++ {
		if gap := w.sent[i].hdr.SequenceNumber - w.sent[i-1].hdr.SequenceNumber; gap != 1 && gap > 3 {
			t.Fatalf("sequence numbers jump by %d at packet %d, which a viewer counts as loss", gap, i)
		}
	}
}
