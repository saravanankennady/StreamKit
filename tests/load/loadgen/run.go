package loadgen

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"elnova-streamkit.local/server/auth"
)

// Options is one load test.
type Options struct {
	URL, Key, Secret, Room  string
	Publishers, Subscribers int
	// VideoPublishers and AudioPublishers, when either is above zero, limit how many of the Publishers send that kind.
	// When both are zero, every publisher sends both.
	VideoPublishers, AudioPublishers int
	Duration, RampEvery              time.Duration
	Video, Audio                     *Frames
	MetricsToken                     string
}

var metricLine = regexp.MustCompile(`(?m)^([a-z_]+)(?:\{[^}]*\})? ([0-9.e+-]+)$`)

// serverSample reads the server's own view of itself.
type serverSample struct {
	at                                               time.Time
	cpu                                              float64
	rss                                              float64
	bytesIn, bytesOut, packetsOut, lost, nacks, plis float64
}

func sampleServer(ctx context.Context, o *Options) (serverSample, error) {
	base := strings.Replace(strings.Replace(o.URL, "ws://", "http://", 1), "wss://", "https://", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return serverSample{}, err
	}
	if o.MetricsToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.MetricsToken)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return serverSample{}, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return serverSample{}, err
	}
	s := serverSample{at: time.Now()}
	for _, line := range strings.Split(string(data), "\n") {
		m := metricLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, _ := strconv.ParseFloat(m[2], 64)
		switch m[1] {
		case "process_cpu_seconds_total":
			s.cpu = v
		case "process_resident_memory_bytes":
			s.rss = v
		case "streamkit_rtp_bytes_total":
			if strings.Contains(line, `direction="in"`) {
				s.bytesIn += v
			} else {
				s.bytesOut += v
			}
		case "streamkit_rtp_packets_total":
			if strings.Contains(line, `direction="out"`) {
				s.packetsOut += v
			}
		case "streamkit_rtp_packets_lost_total":
			s.lost += v
		case "streamkit_rtcp_nack_total":
			s.nacks += v
		case "streamkit_rtcp_pli_total":
			s.plis += v
		}
	}
	return s, nil
}

// Result is one run's outcome.
type Result struct {
	Publishers       int     `json:"publishers"`
	Subscribers      int     `json:"subscribers"`
	Clients          int     `json:"clients"`
	Connected        int64   `json:"connected"`
	Failed           int64   `json:"failed"`
	Tracks           int64   `json:"tracks_received"`
	ExpectedTracks   int64   `json:"tracks_expected"`
	ConnectP50Ms     int64   `json:"connect_p50_ms"`
	ConnectP95Ms     int64   `json:"connect_p95_ms"`
	FirstMediaP95Ms  int64   `json:"first_media_p95_ms"`
	ServerCores      float64 `json:"server_cores"`
	ServerRSSMB      float64 `json:"server_rss_mb"`
	ServerInMbps     float64 `json:"server_in_mbps"`
	ServerOutMbps    float64 `json:"server_out_mbps"`
	ReceivedMbps     float64 `json:"received_mbps"`
	ReceivedPackets  int64   `json:"received_packets"`
	SubscriberGaps   int64   `json:"subscriber_sequence_gaps"`
	LossPercent      float64 `json:"loss_percent_at_subscribers"`
	PublisherLost    float64 `json:"publisher_stream_packets_lost"`
	NACKs            float64 `json:"nacks"`
	KeyframeRequests float64 `json:"keyframe_requests"`
	Seconds          float64 `json:"measured_seconds"`
	// LastError is the last thing that went wrong with a client, if anything did.
	LastError string `json:"-"`
}

func percentile(v []time.Duration, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[min(int(float64(len(v))*p), len(v)-1)].Milliseconds()
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Run connects the clients, waits for everyone to receive everything, then measures for o.Duration.
func Run(parent context.Context, o Options) (*Result, error) {
	if o.Room == "" {
		o.Room = fmt.Sprintf("load-%d", time.Now().UnixNano())
	}
	total := o.Publishers + o.Subscribers
	st := &Stats{}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wg sync.WaitGroup
	var lastErr atomic.Value
	expected := int64(0)
	yes := true
	sends := func(limit, i int) bool { return (o.VideoPublishers == 0 && o.AudioPublishers == 0) || i < limit }
	for i := range total {
		c := &Client{URL: o.URL, Stats: st, Publish: i < o.Publishers}
		if c.Publish {
			c.Identity = fmt.Sprintf("pub-%d", i)
			if sends(o.VideoPublishers, i) {
				c.Video = o.Video
			}
			if sends(o.AudioPublishers, i) {
				c.Audio = o.Audio
			}
			// Each publisher's tracks reach everyone but itself.
			for _, f := range []*Frames{c.Video, c.Audio} {
				if f != nil {
					expected += int64(total - 1)
				}
			}
		} else {
			c.Identity = fmt.Sprintf("sub-%d", i)
		}
		token, err := auth.Mint(auth.Key{APIKey: o.Key, Secret: o.Secret}, &auth.Grants{
			Identity: c.Identity,
			Video:    &auth.VideoGrant{RoomJoin: true, Room: o.Room, CanPublish: &c.Publish, CanSubscribe: &yes},
		}, time.Hour)
		if err != nil {
			return nil, err
		}
		c.Token = token
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Run(ctx); err != nil && ctx.Err() == nil {
				st.failed.Add(1)
				lastErr.Store(err.Error())
			}
		}()
		sleep(ctx, o.RampEvery)
	}

	deadline := time.Now().Add(2*time.Minute + time.Duration(total)*100*time.Millisecond)
	for st.tracks.Load() < expected && time.Now().Before(deadline) && ctx.Err() == nil {
		sleep(ctx, 200*time.Millisecond)
	}
	// Let the transport settle before measuring.
	sleep(ctx, 3*time.Second)

	before, err := sampleServer(ctx, &o)
	if err != nil {
		return nil, fmt.Errorf("could not read server metrics: %w", err)
	}
	receivedBefore, packetsBefore, gapsBefore := st.bytes.Load(), st.packets.Load(), st.gaps.Load()
	sleep(ctx, o.Duration)
	if err := parent.Err(); err != nil {
		return nil, err
	}
	after, err := sampleServer(ctx, &o)
	if err != nil {
		return nil, fmt.Errorf("could not read server metrics: %w", err)
	}
	receivedBytes, packets, gaps := st.bytes.Load()-receivedBefore, st.packets.Load()-packetsBefore, st.gaps.Load()-gapsBefore

	seconds := after.at.Sub(before.at).Seconds()
	st.mu.Lock()
	r := &Result{
		Publishers: o.Publishers, Subscribers: o.Subscribers, Clients: total,
		Connected: st.connected.Load(), Failed: st.failed.Load(), Tracks: st.tracks.Load(), ExpectedTracks: expected,
		ConnectP50Ms:     percentile(st.connectTimes, 0.5),
		ConnectP95Ms:     percentile(st.connectTimes, 0.95),
		FirstMediaP95Ms:  percentile(st.firstMedia, 0.95),
		ServerCores:      (after.cpu - before.cpu) / seconds,
		ServerRSSMB:      after.rss / (1 << 20),
		ServerInMbps:     (after.bytesIn - before.bytesIn) * 8 / seconds / 1e6,
		ServerOutMbps:    (after.bytesOut - before.bytesOut) * 8 / seconds / 1e6,
		ReceivedMbps:     float64(receivedBytes) * 8 / seconds / 1e6,
		ReceivedPackets:  packets,
		SubscriberGaps:   gaps,
		PublisherLost:    after.lost - before.lost,
		NACKs:            after.nacks - before.nacks,
		KeyframeRequests: after.plis - before.plis,
		Seconds:          seconds,
	}
	st.mu.Unlock()
	if packets+gaps > 0 {
		r.LossPercent = float64(gaps) / float64(packets+gaps) * 100
	}
	if v := lastErr.Load(); v != nil {
		r.LastError = v.(string)
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return r, nil
}
