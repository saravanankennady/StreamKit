package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
)

func watchMedia() (*webrtc.MediaEngine, error) {
	me := &webrtc.MediaEngine{}
	feedback := []webrtc.RTCPFeedback{{Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	for _, c := range []struct {
		mime string
		fmtp string
		pt   webrtc.PayloadType
	}{
		{webrtc.MimeTypeVP8, "", 96},
		{webrtc.MimeTypeVP9, "profile-id=0", 98},
		{webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", 102},
		{webrtc.MimeTypeAV1, "", 35},
	} {
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: c.mime, ClockRate: 90000, SDPFmtpLine: c.fmtp, RTCPFeedback: feedback},
			PayloadType:        c.pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: ddURI}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	return me, nil
}

// ddURI is the dependency descriptor header extension.
const ddURI = "https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension"

// watchStep is one request the subscriber holds for a while.
type watchStep struct {
	quality pb.VideoQuality
	fps     uint32
	hold    time.Duration
	name    string
}

// parseSteps reads a schedule such as LOW:6s,HIGH:6s,HIGH@5:6s.
func parseSteps(s string) ([]watchStep, error) {
	var out []watchStep
	for _, part := range strings.Split(s, ",") {
		name, hold, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("bad step %q", part)
		}
		d, err := time.ParseDuration(hold)
		if err != nil {
			return nil, err
		}
		step := watchStep{hold: d, name: name}
		q, fps, hasFPS := strings.Cut(name, "@")
		v, ok := pb.VideoQuality_value[q]
		if !ok {
			return nil, fmt.Errorf("bad quality %q", q)
		}
		step.quality = pb.VideoQuality(v)
		if hasFPS {
			n, err := strconv.Atoi(fps)
			if err != nil {
				return nil, err
			}
			step.fps = uint32(n)
		}
		out = append(out, step)
	}
	return out, nil
}

// stepResult is what arrived during one request, after it had time to take effect.
type stepResult struct {
	Step             string         `json:"step"`
	Packets          int            `json:"packets"`
	Kbps             int            `json:"kbps"`
	Frames           map[string]int `json:"frames,omitempty"`
	Masks            map[string]int `json:"masks,omitempty"`
	StructurePackets int            `json:"structure_packets"`
	MaxExtensionSize int            `json:"max_extension_size"`
	SequenceGaps     int            `json:"sequence_gaps"`
	Timestamps       int            `json:"timestamps"`
	Markers          int            `json:"markers"`
	NoMarker         int            `json:"timestamps_without_marker"`
	MultipleMarkers  int            `json:"timestamps_with_several_markers"`
	MarkerNotOnTop   int            `json:"markers_not_on_top_layer"`
	MaxTSDeltaMs     float64        `json:"max_ts_delta_ms"`
	FramesPerSecond  float64        `json:"timestamps_per_second"`
	DDErrors         int            `json:"dd_errors"`
	NonMonotonicFN   int            `json:"non_monotonic_frame_numbers"`
	Padding          int            `json:"padding_packets"`
	// FirstMs says how long after the request each spatial layer was first seen.
	FirstMs map[string]int64 `json:"first_ms,omitempty"`
}

type packetRecord struct {
	at        time.Time
	seq       uint16
	ts        uint32
	marker    bool
	size      int
	hasLayers bool
	padding   bool
	ddErr     bool
	spatial   int
	temporal  int
	frame     uint64
	mask      int64
	structure bool
	extSize   int
}

type srRecord struct {
	At      float64 `json:"at_ms"`
	NTP     float64 `json:"ntp_s"`
	RTP     uint32  `json:"rtp"`
	Mapping float64 `json:"ntp_minus_rtp_s"`
	SSRC    uint32  `json:"ssrc"`
}

type watchResult struct {
	Codec         string       `json:"codec"`
	Steps         []stepResult `json:"steps"`
	VideoReports  []srRecord   `json:"video_reports"`
	AudioReports  []srRecord   `json:"audio_reports"`
	MappingSpread float64      `json:"video_mapping_spread_ms"`
	AudioSpread   float64      `json:"audio_mapping_spread_ms"`
	// One descriptor of each kind, as sent to this subscriber.
	KeyDescriptor   string `json:"key_descriptor_hex,omitempty"`
	PlainDescriptor string `json:"plain_descriptor_hex,omitempty"`
}

func runWatch(args []string) (any, error) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	var o clientOptions
	fs.StringVar(&o.url, "url", "ws://127.0.0.1:7880", "server address")
	fs.StringVar(&o.key, "key", "devkey", "API key")
	fs.StringVar(&o.secret, "secret", "secret", "API secret")
	fs.StringVar(&o.room, "room", "watch", "room")
	fs.StringVar(&o.identity, "identity", "watcher", "identity")
	schedule := fs.String("steps", "LOW:6s,HIGH:6s,MEDIUM:6s", "what to ask for, and for how long")
	settle := fs.Duration("settle", 1500*time.Millisecond, "how long a request gets to take effect before its window starts counting")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for a video track")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	steps, err := parseSteps(*schedule)
	if err != nil {
		return nil, err
	}
	o.media = watchMedia

	ctx, cancel := context.WithTimeout(context.Background(), *wait+time.Minute)
	defer cancel()
	c, err := dial(ctx, o)
	if err != nil {
		return nil, err
	}
	defer c.close()

	start := time.Now()
	var mu sync.Mutex
	var records []packetRecord
	result := watchResult{}
	videoSID := make(chan string, 1)

	c.sub.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		kind := track.Kind()
		ssrc := uint32(track.SSRC())
		clock := float64(track.Codec().ClockRate)
		go func() {
			for {
				pkts, _, err := receiver.ReadRTCP()
				if err != nil {
					return
				}
				for _, p := range pkts {
					sr, ok := p.(*rtcp.SenderReport)
					if !ok || sr.SSRC != ssrc {
						continue
					}
					rec := srRecord{
						At:      float64(time.Since(start).Milliseconds()),
						NTP:     ntpSeconds(sr.NTPTime),
						RTP:     sr.RTPTime,
						Mapping: ntpSeconds(sr.NTPTime) - float64(sr.RTPTime)/clock,
						SSRC:    sr.SSRC,
					}
					mu.Lock()
					if kind == webrtc.RTPCodecTypeVideo {
						result.VideoReports = append(result.VideoReports, rec)
					} else {
						result.AudioReports = append(result.AudioReports, rec)
					}
					mu.Unlock()
				}
			}
		}()
		if kind != webrtc.RTPCodecTypeVideo {
			buf := make([]byte, 1500)
			for {
				if _, _, err := track.Read(buf); err != nil {
					return
				}
			}
		}
		mu.Lock()
		result.Codec = track.Codec().MimeType
		mu.Unlock()
		select {
		case videoSID <- track.ID():
		default:
		}
		var ddID uint8
		for _, h := range receiver.GetParameters().HeaderExtensions {
			if h.URI == ddURI {
				ddID = uint8(h.ID)
			}
		}
		parser := newDDLogger()
		isVP9 := strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeVP9)
		var pkt rtp.Packet
		buf := make([]byte, 1500)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			if pkt.Unmarshal(buf[:n]) != nil {
				continue
			}
			rec := packetRecord{at: time.Now(), seq: pkt.SequenceNumber, ts: pkt.Timestamp, marker: pkt.Marker, size: n, mask: -1}
			if pkt.Padding && len(pkt.Payload) == 0 {
				rec.padding = true
			} else {
				var ext []byte
				if ddID != 0 {
					ext = pkt.GetExtension(ddID)
				}
				if len(ext) > 0 {
					parser.parse(ext, &rec)
				} else if isVP9 {
					parser.parseVP9(pkt.Payload, &rec)
				}
			}
			mu.Lock()
			records = append(records, rec)
			result.KeyDescriptor, result.PlainDescriptor = parser.KeyHex, parser.PlainHex
			mu.Unlock()
		}
	})

	var sid string
	select {
	case sid = <-videoSID:
	case <-time.After(*wait):
		return nil, errors.New("no video track arrived")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	type window struct {
		name  string
		begin time.Time
		till  time.Time
	}
	var windows []window
	for _, s := range steps {
		c.send(&pb.SignalRequest{Message: &pb.SignalRequest_TrackSetting{TrackSetting: &pb.UpdateTrackSettings{
			TrackSids: []string{sid},
			Quality:   s.quality,
			Fps:       s.fps,
		}}})
		begin := time.Now()
		time.Sleep(s.hold)
		windows = append(windows, window{s.name, begin, time.Now()})
	}

	mu.Lock()
	defer mu.Unlock()
	for _, w := range windows {
		result.Steps = append(result.Steps, summarize(w.name, records, w.begin, w.begin.Add(*settle), w.till))
	}
	result.MappingSpread = spread(result.VideoReports)
	result.AudioSpread = spread(result.AudioReports)
	return result, nil
}

// spread is how far SR.NTP - SR.RTP/clock moved, in milliseconds, over the reports after the first two. A correct
// mapping barely moves, whatever happens to the stream.
func spread(reports []srRecord) float64 {
	if len(reports) < 2 {
		return 0
	}
	low, high := reports[0].Mapping, reports[0].Mapping
	for _, r := range reports {
		low = min(low, r.Mapping)
		high = max(high, r.Mapping)
	}
	return (high - low) * 1000
}

func summarize(name string, records []packetRecord, begin, from, till time.Time) stepResult {
	r := stepResult{Step: name, Frames: map[string]int{}, Masks: map[string]int{}, FirstMs: map[string]int64{}}
	byTS := map[uint32][]packetRecord{}
	var order []uint32
	var bytes int
	var previous *packetRecord
	seenFrame := map[uint64]bool{}
	var lastFrame uint16
	haveFrame := false
	for i := range records {
		p := &records[i]
		if !p.at.Before(begin) && p.at.Before(till) && p.hasLayers {
			name := fmt.Sprintf("S%d", p.spatial)
			if _, ok := r.FirstMs[name]; !ok {
				r.FirstMs[name] = p.at.Sub(begin).Milliseconds()
			}
		}
		if p.at.Before(from) || p.at.After(till) {
			continue
		}
		if previous != nil {
			if gap := p.seq - previous.seq; gap > 1 && gap < 0x8000 {
				r.SequenceGaps += int(gap - 1)
			}
		}
		previous = p
		if p.padding {
			r.Padding++
			continue
		}
		r.Packets++
		bytes += p.size
		if _, ok := byTS[p.ts]; !ok {
			order = append(order, p.ts)
		}
		byTS[p.ts] = append(byTS[p.ts], *p)
		if p.marker {
			r.Markers++
		}
		if p.ddErr {
			r.DDErrors++
		}
		if p.hasLayers {
			if p.mask >= 0 {
				r.Masks[strconv.FormatInt(p.mask, 10)]++
			}
			if p.structure {
				r.StructurePackets++
			}
			r.MaxExtensionSize = max(r.MaxExtensionSize, p.extSize)
			if !seenFrame[p.frame] {
				seenFrame[p.frame] = true
				r.Frames[fmt.Sprintf("S%dT%d", p.spatial, p.temporal)]++
				// A frame number only exists with a descriptor.
				if p.extSize > 0 {
					number := uint16(p.frame)
					if haveFrame && int16(number-lastFrame) <= 0 {
						r.NonMonotonicFN++
					}
					lastFrame = number
					haveFrame = true
				}
			}
		}
	}
	seconds := till.Sub(from).Seconds()
	if seconds > 0 {
		r.Kbps = int(float64(bytes*8) / seconds / 1000)
		r.FramesPerSecond = float64(len(order)) / seconds
	}
	r.Timestamps = len(order)
	var deltas []float64
	for i, ts := range order {
		if i > 0 {
			deltas = append(deltas, float64(int32(ts-order[i-1]))/90)
		}
		packets := byTS[ts]
		markers := 0
		top := -1
		for _, p := range packets {
			top = max(top, p.spatial)
		}
		for _, p := range packets {
			if !p.marker {
				continue
			}
			markers++
			if p.hasLayers && p.spatial != top {
				r.MarkerNotOnTop++
			}
		}
		switch {
		case markers == 0:
			r.NoMarker++
		case markers > 1:
			r.MultipleMarkers++
		}
	}
	if len(deltas) > 0 {
		r.MaxTSDeltaMs = slices.Max(deltas)
	}
	return r
}
