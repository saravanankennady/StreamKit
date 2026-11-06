package sfu

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/prometheus/client_golang/prometheus"

	"elnova-streamkit.local/server/metrics"
)

const (
	keyframeRequestInterval = 500 * time.Millisecond
	// costWindow is how many one-second readings decide what a layer costs. Keyframes arrive in bursts, largest when a
	// stream starts or a shared screen changes, and a burst says nothing about what the layer needs from then on.
	costWindow     = 5
	costMinSamples = 3
	// A layer counts as stopped after this much silence, or several of its usual gaps if that is longer.
	// Publishers drop layers abruptly when their bandwidth estimate falls, and subscribers on that layer see
	// frozen video until they are moved, so this needs to be short. But a static screen share can go a long
	// time between packets without having stopped.
	minLayerSilence    = 600 * time.Millisecond
	maxLayerSilence    = 4 * time.Second
	layerSilenceFactor = 4
	// How quickly the record of a layer's longest gap forgets an old value, per second.
	gapDecay = 0.8
	// A subscriber count above this fans out on several goroutines.
	parallelFanoutThreshold = 16
	parallelFanoutChunk     = 8
)

// Packet is one RTP packet on its way to subscribers. Its memory belongs to the receive loop and must
// not be retained or modified after Forward returns.
type Packet struct {
	Header        *rtp.Header
	Payload       []byte
	Layer         int32
	KeyframeStart bool
	// Resumed is the first packet after a mute, with a gap behind it.
	Resumed bool
	// Selection says how a subscriber chooses from this stream. With SelectDescriptor, SVC holds what the packet's
	// descriptor said, or nil if it could not be read. With SelectVP9, VP9 holds its payload descriptor.
	Selection Selection
	SVC       *SVCFrame
	VP9       *codecs.VP9Packet
}

// ReceiverParams configures a Receiver.
type ReceiverParams struct {
	Kind    webrtc.RTPCodecType
	TrackID string
	Codec   webrtc.RTPCodecParameters
	// IsSVC says all of the track's spatial and temporal layers share one RTP stream.
	IsSVC     bool
	HeaderExt HeaderExtensionIDs
	// RequestKeyframe asks the publisher for a keyframe on one SSRC.
	RequestKeyframe func(ssrc uint32)
	// OnClosed runs once, after the last layer's stream has ended.
	OnClosed func()
}

// HeaderExtensionIDs holds the extension IDs negotiated with one side of a forwarded stream. Zero means
// not negotiated.
type HeaderExtensionIDs struct {
	AudioLevel           uint8
	DependencyDescriptor uint8
	PlayoutDelay         uint8
}

type layerState struct {
	index   int32
	ssrc    uint32
	bytes   atomic.Uint64
	packets atomic.Uint64
	lost    atomic.Uint64
	// lastSeen is a unix time in nanoseconds.
	lastSeen       atomic.Int64
	bitrate        atomic.Int64
	lastBytes      uint64
	readings       [costWindow]int64
	readingCount   int
	cost           atomic.Int64
	lastKeyRequest atomic.Int64
	lastSeq        uint16
	seqStarted     bool
	keyframes      atomic.Uint64
	// longestGap tracks the largest recent silence between packets, in nanoseconds.
	longestGap atomic.Int64
	// frames counts marker bits, each ending a video frame. fps is the rate over the last second.
	frames     atomic.Uint64
	lastFrames uint64
	fps        atomic.Int32
	// gated says the packets just before this one were dropped for a mute. Only the layer's read loop touches it.
	gated bool
	// sr is the newest sender report for this stream.
	sr atomic.Pointer[senderReport]
}

// isActive reports whether packets are still flowing, given this layer's own usual pace.
func (l *layerState) isActive(now int64) bool {
	seen := l.lastSeen.Load()
	if seen == 0 {
		return false
	}
	return now-seen < layerSilenceLimit(l.longestGap.Load())
}

func layerSilenceLimit(longestGap int64) int64 {
	limit := longestGap * layerSilenceFactor
	return max(int64(minLayerSilence), min(limit, int64(maxLayerSilence)))
}

// Receiver owns the incoming streams of one published track, one per simulcast layer, and hands their
// packets to subscribers' DownTracks.
type Receiver struct {
	params ReceiverParams
	mime   string

	mu     sync.Mutex
	layers [MaxLayers]*layerState
	active int
	closed bool

	down atomic.Pointer[[]*DownTrack]
	// svc is set when one stream carries several layers.
	svc *svcStream

	muted        atomic.Bool
	audio        audioLevelTracker
	inPackets    prometheus.Counter
	inBytes      prometheus.Counter
	inLost       prometheus.Counter
	stopStats    chan struct{}
	stopStatsOne sync.Once
}

// NewReceiver creates a Receiver with no layers. Call AddLayer as streams arrive.
func NewReceiver(params ReceiverParams) *Receiver {
	r := &Receiver{params: params, mime: params.Codec.MimeType, stopStats: make(chan struct{})}
	empty := []*DownTrack{}
	r.down.Store(&empty)
	kind := params.Kind.String()
	r.inPackets = metrics.Packets.WithLabelValues("in", kind)
	r.inBytes = metrics.Bytes.WithLabelValues("in", kind)
	r.inLost = metrics.PacketsLost.WithLabelValues(kind)
	if params.IsSVC {
		r.svc = newSVCStream(r, params.HeaderExt.DependencyDescriptor)
	}
	go r.statsLoop()
	return r
}

// selectsWithin reports whether subscribers choose layers from within one stream, rather than choosing among streams.
func (r *Receiver) selectsWithin() bool { return r.svc != nil && r.svc.selection() != SelectStream }

// Kind returns the media kind.
func (r *Receiver) Kind() webrtc.RTPCodecType { return r.params.Kind }

// Codec returns the codec the publisher sends.
func (r *Receiver) Codec() webrtc.RTPCodecParameters { return r.params.Codec }

// TrackID returns the server-assigned track ID.
func (r *Receiver) TrackID() string { return r.params.TrackID }

// AddLayer starts reading one incoming stream. read returns one raw RTP packet per call.
func (r *Receiver) AddLayer(index int32, ssrc uint32, read func([]byte) (int, error)) {
	if index < 0 || index >= MaxLayers {
		index = LayerHigh
	}
	layer := &layerState{index: index, ssrc: ssrc}
	r.mu.Lock()
	if r.closed || r.layers[index] != nil && r.layers[index].ssrc == ssrc {
		r.mu.Unlock()
		return
	}
	r.layers[index] = layer
	r.active++
	r.mu.Unlock()
	go r.readLoop(layer, read)
}

// HasLayer reports whether a layer has an incoming stream.
func (r *Receiver) HasLayer(index int32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return index >= 0 && index < MaxLayers && r.layers[index] != nil
}

func (r *Receiver) readLoop(l *layerState, read func([]byte) (int, error)) {
	defer r.layerEnded()
	buf := make([]byte, 1500)
	var pkt rtp.Packet
	isVideo := r.params.Kind == webrtc.RTPCodecTypeVideo
	audioLevelID := r.params.HeaderExt.AudioLevel
	for {
		n, err := read(buf)
		if err != nil {
			return
		}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		now := time.Now().UnixNano()
		if prev := l.lastSeen.Swap(now); prev != 0 {
			// A gap past the limit was the layer stopping and restarting, not its usual pace.
			if gap := now - prev; gap > l.longestGap.Load() && gap < int64(maxLayerSilence) {
				l.longestGap.Store(gap)
			}
		}
		l.packets.Add(1)
		size := uint64(len(pkt.Payload))
		l.bytes.Add(size)
		r.inPackets.Inc()
		r.inBytes.Add(float64(size))

		late := false
		if l.seqStarted {
			if gap := pkt.SequenceNumber - l.lastSeq; gap > 0 && gap < 0x8000 {
				if gap > 1 {
					l.lost.Add(uint64(gap - 1))
					r.inLost.Add(float64(gap - 1))
				}
				l.lastSeq = pkt.SequenceNumber
			} else {
				late = true
			}
		} else {
			l.lastSeq = pkt.SequenceNumber
			l.seqStarted = true
		}

		p := Packet{Header: &pkt.Header, Payload: pkt.Payload, Layer: l.index}
		if isVideo {
			if pkt.Marker {
				l.frames.Add(1)
			}
			// Browsers probe bandwidth by resending old packets, including keyframes. Forwarding cannot start from
			// one: the packets after it were already dropped, so the viewer's NACKs for them could never be answered.
			p.KeyframeStart = !late && IsKeyframeStart(r.mime, pkt.Payload)
			if p.KeyframeStart {
				l.keyframes.Add(1)
			}
			if r.svc != nil {
				r.svc.observe(&pkt, &p, size, r.mime)
			}
		} else if audioLevelID != 0 {
			if ext := pkt.Header.GetExtension(audioLevelID); len(ext) > 0 {
				r.audio.observe(ext[0]&0x7f, ext[0]&0x80 != 0)
			}
		}
		if !isVideo {
			if r.muted.Load() {
				l.gated = true
				continue
			}
			p.Resumed, l.gated = l.gated, false
		}
		r.fanout(&p)
	}
}

// SetMuted says whether a track's owner is muted. Muted audio stops here, whatever the publisher keeps sending, so a
// client told to mute that ignores it is not heard either. Video keeps flowing: a camera is stopped by its client, and
// the keyframe it starts with must not be lost to a mute message that arrives a moment after it.
func (r *Receiver) SetMuted(muted bool) { r.muted.Store(muted) }

func (r *Receiver) layerEnded() {
	r.mu.Lock()
	r.active--
	done := r.active == 0
	r.mu.Unlock()
	if done {
		r.Close()
	}
}

func (r *Receiver) fanout(p *Packet) {
	subscribers := *r.down.Load()
	if len(subscribers) < parallelFanoutThreshold {
		for _, d := range subscribers {
			d.Forward(p)
		}
		return
	}
	var wg sync.WaitGroup
	for start := 0; start < len(subscribers); start += parallelFanoutChunk {
		end := min(start+parallelFanoutChunk, len(subscribers))
		wg.Add(1)
		go func(chunk []*DownTrack) {
			defer wg.Done()
			for _, d := range chunk {
				d.Forward(p)
			}
		}(subscribers[start:end])
	}
	wg.Wait()
}

// AddDownTrack begins forwarding to d.
func (r *Receiver) AddDownTrack(d *DownTrack) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	old := *r.down.Load()
	next := make([]*DownTrack, 0, len(old)+1)
	next = append(next, old...)
	r.down.Store(ptr(append(next, d)))
}

// RemoveDownTrack stops forwarding to d.
func (r *Receiver) RemoveDownTrack(d *DownTrack) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := *r.down.Load()
	next := make([]*DownTrack, 0, len(old))
	for _, x := range old {
		if x != d {
			next = append(next, x)
		}
	}
	r.down.Store(&next)
}

// DownTracks returns the current subscribers.
func (r *Receiver) DownTracks() []*DownTrack { return *r.down.Load() }

func ptr[T any](v T) *T { return &v }

// RequestKeyframe asks the publisher for a keyframe on a layer, at a limited rate. A layer that does
// not exist falls back to the closest one that does.
func (r *Receiver) RequestKeyframe(layer int32) {
	l := r.closestLayer(layer)
	if l == nil || r.params.RequestKeyframe == nil {
		return
	}
	now := time.Now().UnixNano()
	last := l.lastKeyRequest.Load()
	if now-last < int64(keyframeRequestInterval) || !l.lastKeyRequest.CompareAndSwap(last, now) {
		return
	}
	metrics.PLIs.WithLabelValues().Inc()
	r.params.RequestKeyframe(l.ssrc)
}

func (r *Receiver) closestLayer(want int32) *layerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if want >= 0 && want < MaxLayers && r.layers[want] != nil {
		return r.layers[want]
	}
	for i := int32(MaxLayers) - 1; i >= 0; i-- {
		if r.layers[i] != nil {
			return r.layers[i]
		}
	}
	return nil
}

// SelectLayer returns the highest active layer at or below max, then falls back to the lowest active
// layer above it. It returns LayerNone when nothing is being received.
func (r *Receiver) SelectLayer(max int32) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UnixNano()
	active := func(l *layerState) bool { return l != nil && l.isActive(now) }
	if r.selectsWithin() {
		// One stream, so silence says nothing about any one layer. The publisher says which it is producing.
		flowing := false
		for _, l := range r.layers {
			flowing = flowing || active(l)
		}
		top := r.svc.topSpatial(now)
		if !flowing || top < 0 {
			return LayerNone
		}
		if max < 0 {
			return 0
		}
		return min(top, max)
	}
	for i := min(max, MaxLayers-1); i >= 0; i-- {
		if active(r.layers[i]) {
			return i
		}
	}
	for i := max + 1; i < MaxLayers; i++ {
		if i >= 0 && active(r.layers[i]) {
			return i
		}
	}
	return LayerNone
}

// recordReading adds a one-second bitrate reading. The cost is their median, so bursts do not count until they last.
func (l *layerState) recordReading(rate int64) {
	l.readings[l.readingCount%costWindow] = rate
	l.readingCount++
	if l.readingCount < costMinSamples {
		return
	}
	sorted := slices.Clone(l.readings[:min(l.readingCount, costWindow)])
	slices.Sort(sorted)
	l.cost.Store(sorted[len(sorted)/2])
}

// LayerCost returns what a layer needs in bits per second, or 0 until there are enough readings to say.
func (r *Receiver) LayerCost(layer int32) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if layer < 0 || layer >= MaxLayers {
		return 0
	}
	if r.selectsWithin() {
		return r.svc.stats[layer].state.cost.Load()
	}
	if r.layers[layer] == nil {
		return 0
	}
	return r.layers[layer].cost.Load()
}

// LayerBitrate returns the measured bitrate of a layer in bits per second.
func (r *Receiver) LayerBitrate(layer int32) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if layer < 0 || layer >= MaxLayers {
		return 0
	}
	if r.selectsWithin() {
		return r.svc.stats[layer].state.bitrate.Load()
	}
	if r.layers[layer] == nil {
		return 0
	}
	return r.layers[layer].bitrate.Load()
}

func (r *Receiver) statsLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.stopStats:
			return
		case <-t.C:
		}
		r.mu.Lock()
		for _, l := range r.layers {
			if l == nil {
				continue
			}
			total := l.bytes.Load()
			rate := int64(total-l.lastBytes) * 8
			l.bitrate.Store(rate)
			l.recordReading(rate)
			l.lastBytes = total
			l.longestGap.Store(int64(float64(l.longestGap.Load()) * gapDecay))
			frames := l.frames.Load()
			l.fps.Store(int32(frames - l.lastFrames))
			l.lastFrames = frames
		}
		seedKeyframe := false
		if r.svc != nil {
			r.svc.updateStats()
			started := r.svc.startedAt.Load()
			seedKeyframe = r.selectsWithin() && !r.svc.hasKeyframe.Load() && time.Now().UnixNano()-started < int64(seedKeyframeFor)
		}
		r.mu.Unlock()
		if seedKeyframe {
			r.RequestKeyframe(LayerHigh)
		}
	}
}

// ReceiverStats is a point-in-time view of what a publisher is sending.
type ReceiverStats struct {
	Packets    uint64
	Bytes      uint64
	Lost       uint64
	Bitrate    int64
	Layers     int
	Keyframes  uint64
	LayerRates [MaxLayers]int64
	LayerFPS   [MaxLayers]int32
	// FPS is the frame rate of the highest layer now sending.
	FPS int32
}

// Stats returns totals across all layers.
func (r *Receiver) Stats() ReceiverStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s ReceiverStats
	for i, l := range r.layers {
		if l == nil {
			continue
		}
		s.Layers++
		s.Packets += l.packets.Load()
		s.Bytes += l.bytes.Load()
		s.Lost += l.lost.Load()
		s.Keyframes += l.keyframes.Load()
		rate := l.bitrate.Load()
		s.Bitrate += rate
		s.LayerRates[i] = rate
		s.LayerFPS[i] = l.fps.Load()
		if s.LayerFPS[i] > 0 {
			s.FPS = s.LayerFPS[i]
		}
	}
	if r.selectsWithin() {
		// Monitoring shows layers, and for this stream they are inside it.
		s.Layers = int(r.svc.maxSpatial.Load()) + 1
		for i := range s.LayerRates {
			s.LayerRates[i] = r.svc.stats[i].state.bitrate.Load()
			s.LayerFPS[i] = 0
			for t := range r.svc.stats[i].rate {
				s.LayerFPS[i] += r.svc.stats[i].rate[t].Load()
			}
		}
	}
	return s
}

// AudioLevel returns the speaking level for the last observation window and resets the window. active
// is false when the participant has not been speaking.
func (r *Receiver) AudioLevel() (level float32, active bool) { return r.audio.sample() }

// Close stops forwarding and closes every DownTrack. It is safe to call more than once.
func (r *Receiver) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	subscribers := *r.down.Load()
	empty := []*DownTrack{}
	r.down.Store(&empty)
	r.mu.Unlock()

	r.stopStatsOne.Do(func() { close(r.stopStats) })
	for _, d := range subscribers {
		d.Close()
	}
	if r.params.OnClosed != nil {
		r.params.OnClosed()
	}
}

// audioLevelTracker turns RFC 6464 per-packet levels into a speaking level per observation window.
type audioLevelTracker struct {
	mu          sync.Mutex
	packets     int
	active      int
	sumLinear   float64
	smoothed    float64
	wasSpeaking bool
}

const (
	// Levels are dBov: 0 is loudest, 127 is silence. Quieter than this does not count as speech.
	speakingThresholdDBov = 50
	minActiveRatio        = 0.35
	levelSmoothing        = 0.5
)

func (a *audioLevelTracker) observe(dBov byte, voiceActivity bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.packets++
	if dBov > speakingThresholdDBov {
		return
	}
	a.active++
	a.sumLinear += math.Pow(10, -float64(dBov)/20)
	_ = voiceActivity // browsers do not reliably set this bit, so the level alone decides
}

func (a *audioLevelTracker) sample() (float32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.packets == 0 {
		a.smoothed = 0
		return 0, false
	}
	ratio := float64(a.active) / float64(a.packets)
	var level float64
	if a.active > 0 {
		level = a.sumLinear / float64(a.active)
	}
	speaking := ratio >= minActiveRatio
	a.packets, a.active, a.sumLinear = 0, 0, 0
	if !speaking {
		a.smoothed = 0
		return 0, false
	}
	if a.smoothed == 0 {
		a.smoothed = level
	} else {
		a.smoothed += (level - a.smoothed) * levelSmoothing
	}
	return float32(a.smoothed), true
}
