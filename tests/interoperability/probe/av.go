package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
)

func avMedia() (*webrtc.MediaEngine, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	return me, nil
}

// ntpTime converts a wall-clock time to the 64-bit NTP format of a sender report.
func ntpTime(t time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	return secs<<32 | frac
}

func ntpSeconds(ntp uint64) float64 { return float64(ntp) / (1 << 32) }

// publishedStream is a controlled publisher track: its RTP timestamps and sender reports are exact, and each payload
// carries the timestamp the publisher gave it, so a subscriber can recover the server's offset per packet.
type publishedStream struct {
	track   *webrtc.TrackLocalStaticRTP
	sender  *webrtc.RTPSender
	clock   uint32
	base    uint32
	ssrc    uint32
	payload func(ts uint32) []byte
	seq     uint16
}

// payloadOffset says where the embedded timestamp sits. The VP8 one is a key frame's RTP descriptor, frame tag, start
// code and size, so every frame can start a stream. The Opus one is a TOC byte.
const (
	vp8EmbedOffset  = 1 + 10
	opusEmbedOffset = 1
)

func vp8Payload(ts uint32) []byte {
	p := make([]byte, 400)
	p[0] = 0x10                               // start of partition 0, no extensions
	copy(p[1:], []byte{0x10, 0x02, 0x00})     // key frame, shown, first partition size
	copy(p[4:], []byte{0x9d, 0x01, 0x2a})     // start code
	binary.LittleEndian.PutUint16(p[7:], 320) // width
	binary.LittleEndian.PutUint16(p[9:], 180) // height
	binary.BigEndian.PutUint32(p[vp8EmbedOffset:], ts)
	return p
}

func opusPayload(ts uint32) []byte {
	p := make([]byte, 60)
	p[0] = 0xfc // one 20 ms CELT frame
	binary.BigEndian.PutUint32(p[opusEmbedOffset:], ts)
	return p
}

type srSample struct {
	at                    time.Time
	ntp                   uint64
	rtp                   uint32
	errMs                 float64
	hasErr                bool
	sinceFirstPublisherSR time.Duration
}

// watchedStream is what the subscriber saw of one track.
type watchedStream struct {
	mu          sync.Mutex
	clock       uint32
	kind        string
	firstAt     time.Time
	packets     int
	gaps        int
	lastSeq     uint16
	started     bool
	offset      uint32
	offsetKnown bool
	offsets     map[uint32]int
	firstOffset uint32
	reports     []srSample
	cnames      map[string]bool
	sdesMids    map[string]bool
	ssrc        uint32
	msid        string
}

type avResult struct {
	Audio              avStreamResult `json:"audio"`
	Video              avStreamResult `json:"video"`
	PublisherFirstSRMs float64        `json:"publisher_first_sr_ms"`
	PublisherSRCount   int            `json:"publisher_sr_count"`
	VideoDelayMs       float64        `json:"video_delay_ms"`
	NTPOffsetSeconds   float64        `json:"ntp_offset_seconds"`
	WithholdMs         float64        `json:"withhold_ms"`
}

type avStreamResult struct {
	Packets                int      `json:"packets"`
	Gaps                   int      `json:"gaps"`
	FirstPacketMs          float64  `json:"first_packet_ms"`
	Reports                int      `json:"reports"`
	ReportsBeforePublisher int      `json:"reports_before_publisher"`
	FirstReportAfterMs     float64  `json:"first_report_after_publisher_ms"`
	MaxAbsErrorMs          float64  `json:"max_abs_error_ms"`
	MeanErrorMs            float64  `json:"mean_error_ms"`
	MedianIntervalMs       float64  `json:"median_interval_ms"`
	NTPMinusLocalSeconds   float64  `json:"ntp_minus_local_seconds"`
	TSOffsets              int      `json:"ts_offsets"`
	FirstTSOffset          uint32   `json:"first_ts_offset"`
	CNames                 []string `json:"cnames"`
	SDESMids               []string `json:"sdes_mids"`
	MSID                   string   `json:"msid"`
}

func runAV(args []string) (any, error) {
	fs := flag.NewFlagSet("av", flag.ExitOnError)
	var o clientOptions
	fs.StringVar(&o.url, "url", "ws://127.0.0.1:7880", "server address")
	fs.StringVar(&o.key, "key", "devkey", "API key")
	fs.StringVar(&o.secret, "secret", "secret", "API secret")
	fs.StringVar(&o.room, "room", "av", "room")
	seconds := fs.Duration("for", 12*time.Second, "how long to run")
	videoDelay := fs.Duration("video-delay", 150*time.Millisecond, "how long after its capture time each video frame is sent")
	ntpOffset := fs.Duration("ntp-offset", 0, "how far the publisher's NTP clock is ahead of real time")
	withhold := fs.Duration("withhold-sr", 0, "how long the publisher sends no sender reports")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	o.media = avMedia

	ctx, cancel := context.WithTimeout(context.Background(), *seconds+30*time.Second)
	defer cancel()

	pubOpts, subOpts := o, o
	pubOpts.identity, subOpts.identity = "av-pub", "av-sub"
	subscriber, err := dial(ctx, subOpts)
	if err != nil {
		return nil, err
	}
	defer subscriber.close()

	start := time.Now()
	var streamsMu sync.Mutex
	streams := map[string]*watchedStream{}
	var firstPublisherSR time.Time
	var firstSRMu sync.Mutex

	// The publisher's clock. t is the capture time of a frame: its RTP timestamp and NTP time both come from it.
	const audioBase, videoBase = uint32(1_000_000), uint32(5_000_000)
	publisherNTP := func(t time.Time) uint64 { return ntpTime(t.Add(*ntpOffset)) }
	captureOf := func(clock, base, ts uint32) time.Time {
		ticks := int64(int32(ts - base))
		return start.Add(time.Duration(ticks) * time.Second / time.Duration(clock))
	}

	subscriber.sub.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		kind := track.Kind().String()
		w := &watchedStream{kind: kind, clock: track.Codec().ClockRate, offsets: map[uint32]int{}, cnames: map[string]bool{}, sdesMids: map[string]bool{}, ssrc: uint32(track.SSRC()), msid: track.StreamID() + " " + track.ID()}
		streamsMu.Lock()
		streams[kind] = w
		streamsMu.Unlock()
		go func() {
			for {
				pkts, _, err := receiver.ReadRTCP()
				if err != nil {
					return
				}
				now := time.Now()
				for _, p := range pkts {
					switch m := p.(type) {
					case *rtcp.SenderReport:
						if m.SSRC != w.ssrc {
							continue
						}
						w.mu.Lock()
						s := srSample{at: now, ntp: m.NTPTime, rtp: m.RTPTime}
						if w.offsetKnown {
							base := videoBase
							if kind == "audio" {
								base = audioBase
							}
							// The publisher's own timestamp for this report, and the NTP time its clock gives for that.
							original := m.RTPTime + w.offset
							want := publisherNTP(captureOf(w.clock, base, original))
							s.errMs = (ntpSeconds(m.NTPTime) - ntpSeconds(want)) * 1000
							s.hasErr = true
						}
						firstSRMu.Lock()
						if !firstPublisherSR.IsZero() {
							s.sinceFirstPublisherSR = now.Sub(firstPublisherSR)
						} else {
							s.sinceFirstPublisherSR = -1
						}
						firstSRMu.Unlock()
						w.reports = append(w.reports, s)
						w.mu.Unlock()
					case *rtcp.SourceDescription:
						for _, chunk := range m.Chunks {
							if chunk.Source != w.ssrc {
								continue
							}
							w.mu.Lock()
							for _, item := range chunk.Items {
								switch item.Type {
								case rtcp.SDESCNAME:
									w.cnames[item.Text] = true
								case rtcp.SDESType(15):
									w.sdesMids[item.Text] = true
								}
							}
							w.mu.Unlock()
						}
					}
				}
			}
		}()
		var pkt rtp.Packet
		buf := make([]byte, 1500)
		embed := vp8EmbedOffset
		if kind == "audio" {
			embed = opusEmbedOffset
		}
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			if pkt.Unmarshal(buf[:n]) != nil || len(pkt.Payload) < embed+4 {
				continue
			}
			original := binary.BigEndian.Uint32(pkt.Payload[embed:])
			w.mu.Lock()
			if w.firstAt.IsZero() {
				w.firstAt = time.Now()
			}
			if w.started {
				if gap := pkt.SequenceNumber - w.lastSeq; gap > 1 && gap < 0x8000 {
					w.gaps += int(gap - 1)
				}
			}
			w.started = true
			w.lastSeq = pkt.SequenceNumber
			w.packets++
			w.offset = original - pkt.Timestamp
			if !w.offsetKnown {
				w.firstOffset = w.offset
			}
			w.offsetKnown = true
			w.offsets[w.offset]++
			w.mu.Unlock()
		}
	})

	publisher, err := dial(ctx, pubOpts)
	if err != nil {
		return nil, err
	}
	defer publisher.close()
	select {
	case <-publisher.joined:
	case <-ctx.Done():
		return nil, errors.New("no join response")
	}

	add := func(mime string, clock uint32, kind pb.TrackType, source pb.TrackSource, id string, payload func(uint32) []byte, base uint32) (*publishedStream, error) {
		track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mime, ClockRate: clock}, id, "av-stream")
		if err != nil {
			return nil, err
		}
		tr, err := publisher.pub.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			return nil, err
		}
		req := &pb.AddTrackRequest{Cid: id, Name: strings.ToLower(source.String()), Type: kind, Source: source}
		if kind == pb.TrackType_VIDEO {
			req.Width, req.Height = 320, 180
		}
		publisher.send(&pb.SignalRequest{Message: &pb.SignalRequest_AddTrack{AddTrack: req}})
		return &publishedStream{track: track, sender: tr.Sender(), clock: clock, base: base, payload: payload}, nil
	}
	audio, err := add(webrtc.MimeTypeOpus, 48000, pb.TrackType_AUDIO, pb.TrackSource_MICROPHONE, "av-audio", opusPayload, audioBase)
	if err != nil {
		return nil, err
	}
	video, err := add(webrtc.MimeTypeVP8, 90000, pb.TrackType_VIDEO, pb.TrackSource_CAMERA, "av-video", vp8Payload, videoBase)
	if err != nil {
		return nil, err
	}
	for range 2 {
		select {
		case <-publisher.published:
		case <-ctx.Done():
			return nil, errors.New("no track_published")
		}
	}
	if err := publisher.sendOffer(); err != nil {
		return nil, err
	}

	var done sync.WaitGroup
	stop := make(chan struct{})
	pace := func(s *publishedStream, each, delay time.Duration) {
		defer done.Done()
		t := time.NewTicker(each)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			capture := time.Now().Add(-delay)
			ticks := capture.Sub(start) * time.Duration(s.clock) / time.Second
			ts := s.base + uint32(int64(ticks))
			s.seq++
			_ = s.track.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: s.seq, Timestamp: ts},
				Payload: s.payload(ts),
			})
		}
	}
	done.Add(3)
	go pace(audio, 20*time.Millisecond, 0)
	go pace(video, 50*time.Millisecond, *videoDelay)

	// Sender reports state the capture clock, as a browser's do: now, and the RTP timestamp that now would have.
	srCount := 0
	go func() {
		defer done.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			now := time.Now()
			if now.Sub(start) < *withhold {
				continue
			}
			firstSRMu.Lock()
			if firstPublisherSR.IsZero() {
				firstPublisherSR = now
			}
			firstSRMu.Unlock()
			srCount++
			for _, s := range []*publishedStream{audio, video} {
				params := s.sender.GetParameters()
				if len(params.Encodings) == 0 {
					continue
				}
				ticks := now.Sub(start) * time.Duration(s.clock) / time.Second
				_ = publisher.pub.WriteRTCP([]rtcp.Packet{&rtcp.SenderReport{
					SSRC:    uint32(params.Encodings[0].SSRC),
					NTPTime: publisherNTP(now),
					RTPTime: s.base + uint32(int64(ticks)),
				}})
			}
		}
	}()

	select {
	case <-time.After(*seconds):
	case <-ctx.Done():
	}
	close(stop)
	done.Wait()

	firstSRMu.Lock()
	first := firstPublisherSR
	firstSRMu.Unlock()
	streamsMu.Lock()
	defer streamsMu.Unlock()
	out := avResult{
		PublisherSRCount: srCount,
		VideoDelayMs:     float64(videoDelay.Milliseconds()),
		NTPOffsetSeconds: ntpOffset.Seconds(),
		WithholdMs:       float64(withhold.Milliseconds()),
	}
	if !first.IsZero() {
		out.PublisherFirstSRMs = float64(first.Sub(start).Milliseconds())
	}
	if s := streams["audio"]; s != nil {
		out.Audio = s.result(start)
	} else {
		return nil, fmt.Errorf("the subscriber got no audio track")
	}
	if s := streams["video"]; s != nil {
		out.Video = s.result(start)
	} else {
		return nil, fmt.Errorf("the subscriber got no video track")
	}
	return out, nil
}

func (w *watchedStream) result(start time.Time) avStreamResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := avStreamResult{
		Packets:       w.packets,
		Gaps:          w.gaps,
		Reports:       len(w.reports),
		TSOffsets:     len(w.offsets),
		FirstTSOffset: w.firstOffset,
		MSID:          w.msid,
	}
	if !w.firstAt.IsZero() {
		r.FirstPacketMs = float64(w.firstAt.Sub(start).Milliseconds())
	}
	for name := range w.cnames {
		r.CNames = append(r.CNames, name)
	}
	for mid := range w.sdesMids {
		r.SDESMids = append(r.SDESMids, mid)
	}
	var sum float64
	count := 0
	var intervals []float64
	firstAfter := true
	for i, s := range w.reports {
		if s.sinceFirstPublisherSR < 0 {
			r.ReportsBeforePublisher++
		} else if firstAfter {
			r.FirstReportAfterMs = float64(s.sinceFirstPublisherSR.Milliseconds())
			firstAfter = false
		}
		if s.hasErr {
			sum += s.errMs
			count++
			r.MaxAbsErrorMs = math.Max(r.MaxAbsErrorMs, math.Abs(s.errMs))
		}
		if i > 0 {
			intervals = append(intervals, float64(s.at.Sub(w.reports[i-1].at).Milliseconds()))
		}
		r.NTPMinusLocalSeconds = ntpSeconds(s.ntp) - ntpSeconds(ntpTime(s.at))
	}
	if count > 0 {
		r.MeanErrorMs = sum / float64(count)
	}
	if len(intervals) > 0 {
		slices.Sort(intervals)
		r.MedianIntervalMs = intervals[len(intervals)/2]
	}
	return r
}
