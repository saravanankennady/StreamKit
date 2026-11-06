package sfu

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// allocation tuning.
const (
	allocateInterval = 500 * time.Millisecond
	// A higher layer must fit this much headroom before we move up, which avoids flapping.
	upgradeHeadroom = 1.15
	// A layer that has not been measured yet is assumed to cost this much.
	assumedLayerBitrate = 150_000
	// Nothing flows on a connection whose only track is paused, and the estimate cannot rise with nothing to measure.
	// So a paused track gets to flow again for a while, less often each time the estimate still says no.
	pauseRetryFirst = 5 * time.Second
	pauseRetryMax   = 30 * time.Second
	pauseRetryFor   = 3 * time.Second
	// A track starts on its lowest layer, and stays there for at least this long, and until browser reports show no queue on
	// the path. A top layer's first keyframe can be hundreds of kilobytes, far more than a thin link's queue holds, and it
	// never finishes arriving if most of it is dropped. A low layer gets a picture on screen whatever the link is like, and
	// on a good link the top follows within a few seconds.
	startHold    = 1500 * time.Millisecond
	startHoldMax = 6 * time.Second
	// A track's streams begin as its first packets arrive, so its first layer is usually known just after it is added.
	startRecheck = 150 * time.Millisecond
)

type pauseRetry struct {
	next, until time.Time
	backoff     time.Duration
}

// BandwidthEstimate reports the estimated capacity toward a subscriber in bits per second.
type BandwidthEstimate func() int64

// noLimit is the budget when nothing suggests the connection is full.
const noLimit = int64(1) << 62

// Allocator shares a subscriber's estimated bandwidth among its tracks, choosing each track's layer.
type Allocator struct {
	estimate BandwidthEstimate
	// AllowPause lets video stop entirely when the bandwidth cannot carry even its lowest layer.
	allowPause atomic.Bool
	maxBitrate atomic.Int64

	mu       sync.Mutex
	tracks   []*DownTrack
	paused   map[*DownTrack]bool
	retries  map[*DownTrack]*pauseRetry
	capacity capacity
	limit    int64

	onPauseChange func(d *DownTrack, paused bool)
	stop          chan struct{}
	stopOnce      sync.Once
}

// NewAllocator starts an allocator. estimate may be nil, in which case bandwidth is unconstrained.
func NewAllocator(estimate BandwidthEstimate, onPauseChange func(*DownTrack, bool)) *Allocator {
	a := &Allocator{
		estimate:      estimate,
		paused:        map[*DownTrack]bool{},
		retries:       map[*DownTrack]*pauseRetry{},
		onPauseChange: onPauseChange,
		stop:          make(chan struct{}),
	}
	go a.loop()
	return a
}

// SetAllowPause controls whether video may be paused under congestion.
func (a *Allocator) SetAllowPause(v bool) { a.allowPause.Store(v) }

// Add registers a track and picks its first layer right away.
func (a *Allocator) Add(d *DownTrack) {
	a.mu.Lock()
	a.tracks = append(a.tracks, d)
	a.mu.Unlock()
	if d.IsVideo() {
		d.SetTarget(LayerLow)
		time.AfterFunc(startRecheck, a.Reallocate)
	}
	a.Reallocate()
}

// Remove unregisters a track.
func (a *Allocator) Remove(d *DownTrack) {
	a.mu.Lock()
	a.tracks = slices.DeleteFunc(a.tracks, func(x *DownTrack) bool { return x == d })
	delete(a.paused, d)
	delete(a.retries, d)
	a.mu.Unlock()
	a.Reallocate()
}

// Close stops the allocator.
func (a *Allocator) Close() { a.stopOnce.Do(func() { close(a.stop) }) }

func (a *Allocator) loop() {
	t := time.NewTicker(allocateInterval)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.Reallocate()
		}
	}
}

// Reallocate recomputes every track's layer.
func (a *Allocator) Reallocate() { a.reallocateAt(time.Now()) }

func (a *Allocator) reallocateAt(now time.Time) {
	a.mu.Lock()
	tracks := slices.Clone(a.tracks)
	a.mu.Unlock()

	var video []*DownTrack
	for _, d := range tracks {
		if d.IsVideo() {
			video = append(video, d)
		}
	}
	budget := a.budget(video, now)
	for _, d := range tracks {
		if !d.IsVideo() && budget < noLimit {
			budget -= d.Receiver().Stats().Bitrate
		}
	}

	plans := make(map[*DownTrack]plan, len(video))
	a.mu.Lock()
	calm := a.capacity.calm
	a.mu.Unlock()
	for _, p := range allocate(toCandidates(video, now, calm), budget, a.allowPause.Load()) {
		plans[p.track] = p
	}

	movedUp := false
	for _, d := range video {
		layer := plans[d].layer
		if temporal := plans[d].temporal; temporal != d.TemporalBudget() {
			movedUp = movedUp || temporal > d.TemporalBudget()
			d.SetTemporalBudget(temporal)
		}
		wouldPause := layer == LayerNone && !d.Disabled() && d.Receiver().SelectLayer(d.MaxLayer()) != LayerNone
		retrying := a.updateRetry(d, wouldPause, now)
		if retrying {
			layer = d.Receiver().SelectLayer(LayerLow)
		}
		previous := d.Target()
		d.SetTarget(layer)
		d.updateTemporal()
		// Before the subscriber's answer there is nowhere to forward a keyframe to, and asking now would use up the request
		// that binding makes.
		if layer != LayerNone && d.isBound() && (layer != previous || d.Current() != layer || d.NeedsKeyframe()) {
			d.Receiver().RequestKeyframe(layer)
		}
		if previous != LayerNone && layer > previous {
			movedUp = true
		}
		a.notePause(d, wouldPause && !retrying)
	}
	if movedUp {
		a.mu.Lock()
		a.capacity.tryingMore(now)
		a.mu.Unlock()
	}
}

// budget returns how much video this subscriber's connection can take now.
func (a *Allocator) budget(video []*DownTrack, now time.Time) int64 {
	if a.estimate != nil {
		return a.estimate()
	}
	var bytes, plis, weighted, rate uint64
	var loss float64
	var rtt time.Duration
	var allocated, wanted int64
	for _, d := range video {
		s := d.Stats()
		bytes += s.Bytes
		plis += s.PLIs
		// Loss is weighed by how much each track carries, so one thin track cannot speak for the connection.
		if r := uint64(max(d.Receiver().LayerBitrate(d.Current()), 0)); r > 0 {
			rate += r
			weighted += uint64(float64(r) * float64(s.Loss))
		}
		rtt = max(rtt, s.RTT)
		if t := d.Target(); t != LayerNone {
			allocated += int64(float64(d.Receiver().LayerCost(t)) * temporalShare(d.TemporalBudget()))
		}
		if best := d.Receiver().SelectLayer(d.MaxLayer()); best != LayerNone {
			wanted += d.Receiver().LayerCost(best)
		}
	}
	if rate > 0 {
		loss = float64(weighted) / float64(rate)
	}
	a.mu.Lock()
	a.capacity.allocated, a.capacity.wanted = allocated, wanted
	limit := a.capacity.update(now, bytes, plis, loss, rtt)
	a.limit = limit
	a.mu.Unlock()
	for _, d := range video {
		d.linkShort.Store(limit > 0)
	}
	budget := noLimit
	if limit > 0 {
		budget = limit
	}
	if m := a.maxBitrate.Load(); m > 0 {
		budget = min(budget, m)
	}
	return budget
}

// SetMaxBitrate caps what this subscriber is sent, in bits per second. Zero sets no cap.
func (a *Allocator) SetMaxBitrate(v int64) { a.maxBitrate.Store(v) }

// Limit returns what this subscriber's connection is judged able to carry, in bits per second, or zero if nothing
// suggests a limit.
func (a *Allocator) Limit() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limit
}

// updateRetry reports whether a track the budget would pause should flow for now anyway.
func (a *Allocator) updateRetry(d *DownTrack, wouldPause bool, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !wouldPause {
		delete(a.retries, d)
		return false
	}
	r := a.retries[d]
	if r == nil {
		a.retries[d] = &pauseRetry{next: now.Add(pauseRetryFirst), backoff: pauseRetryFirst}
		return false
	}
	if now.Before(r.until) {
		return true
	}
	if now.Before(r.next) {
		return false
	}
	r.until = now.Add(pauseRetryFor)
	r.backoff = min(r.backoff*2, pauseRetryMax)
	r.next = r.until.Add(r.backoff)
	return true
}

func (a *Allocator) notePause(d *DownTrack, paused bool) {
	a.mu.Lock()
	changed := a.paused[d] != paused
	a.paused[d] = paused
	a.mu.Unlock()
	if changed && a.onPauseChange != nil {
		a.onPauseChange(d, paused)
	}
}

// candidate is the allocator's view of a track. Keeping allocate free of live objects makes the policy
// easy to test.
type candidate struct {
	track    *DownTrack
	priority uint32
	current  int32
	temporal int32
	// bitrates holds the measured cost per layer; available says which layers exist and are flowing.
	bitrates  [MaxLayers]int64
	available [MaxLayers]bool
	max       int32
	disabled  bool
}

func toCandidates(tracks []*DownTrack, now time.Time, calm bool) []candidate {
	out := make([]candidate, 0, len(tracks))
	for _, d := range tracks {
		c := candidate{track: d, priority: d.Priority(), current: d.Target(), temporal: d.TemporalBudget(), max: d.MaxLayer(), disabled: d.Disabled()}
		if d.startingAt(now, calm) {
			c.max = LayerLow
		}
		r := d.Receiver()
		for l := LayerHigh; l >= LayerLow; l-- {
			if r.SelectLayer(l) == l {
				c.available[l] = true
				c.bitrates[l] = r.LayerCost(l)
			}
		}
		out = append(out, c)
	}
	return out
}

type plan struct {
	track    *DownTrack
	layer    int32
	temporal int32
}

// temporalShare is roughly how much of a layer's bitrate its temporal layers up to this one take. A VP8 encoder gives its
// base layer about 60% and the next about 20%.
func temporalShare(layer int32) float64 {
	switch layer {
	case 0:
		return 0.6
	case 1:
		return 0.8
	}
	return 1
}

// temporalFor picks how many temporal layers fit when a whole spatial layer does not. A thin link gets a lower frame rate,
// not a frozen picture.
func temporalFor(budget, cost int64, current int32) int32 {
	for _, layer := range []int32{maxTemporalLayer, 1} {
		needed := float64(cost) * temporalShare(layer)
		if layer > current {
			needed *= upgradeHeadroom
		}
		if float64(budget) >= needed {
			return layer
		}
	}
	return 0
}

// allocate chooses a layer per candidate under a budget. Every track first gets its lowest usable layer,
// in priority order; then remaining bandwidth upgrades tracks, also in priority order.
func allocate(cands []candidate, budget int64, allowPause bool) []plan {
	order := slices.Clone(cands)
	slices.SortStableFunc(order, func(a, b candidate) int {
		return int(priorityRank(a.priority)) - int(priorityRank(b.priority))
	})

	chosen := make([]int32, len(order))
	temporal := make([]int32, len(order))
	for i, c := range order {
		chosen[i] = LayerNone
		temporal[i] = maxTemporalLayer
		if c.disabled {
			continue
		}
		low := lowestUsable(c)
		if low == LayerNone {
			// Nothing is flowing yet. Aim at the wanted layer so it forwards as soon as a stream starts.
			chosen[i] = c.max
			continue
		}
		cost := layerCost(c, low)
		if budget < cost && allowPause {
			continue
		}
		chosen[i] = low
		if budget < noLimit && c.bitrates[low] > 0 {
			temporal[i] = temporalFor(budget, cost, c.temporal)
		}
		budget -= cost
	}

	for i, c := range order {
		if chosen[i] == LayerNone || c.disabled || lowestUsable(c) == LayerNone {
			continue
		}
		current := chosen[i]
		for l := current + 1; l <= c.max && l < MaxLayers; l++ {
			if !c.available[l] {
				continue
			}
			extra := layerCost(c, l) - layerCost(c, current)
			needed := extra
			// Holding a layer already in use takes no headroom, only moving above it does.
			if l > c.current {
				needed = int64(float64(extra) * upgradeHeadroom)
			}
			if budget < needed {
				break
			}
			budget -= extra
			current = l
		}
		chosen[i] = current
	}

	out := make([]plan, len(order))
	for i, c := range order {
		out[i] = plan{track: c.track, layer: chosen[i], temporal: temporal[i]}
	}
	return out
}

func priorityRank(p uint32) uint32 {
	if p == 0 {
		return 1 << 16
	}
	return p
}

func lowestUsable(c candidate) int32 {
	for l := LayerLow; l < MaxLayers && l <= c.max; l++ {
		if c.available[l] {
			return l
		}
	}
	// The subscriber wants less than what exists: take the lowest that exists.
	for l := c.max + 1; l < MaxLayers; l++ {
		if l >= 0 && c.available[l] {
			return l
		}
	}
	return LayerNone
}

func layerCost(c candidate, l int32) int64 {
	if b := c.bitrates[l]; b > 0 {
		return b
	}
	return assumedLayerBitrate << l
}
