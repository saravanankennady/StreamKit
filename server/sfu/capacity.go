package sfu

import (
	"math"
	"time"
)

const (
	// A browser reports loss about once a second. WiFi loses a few percent without being full, so this takes more.
	congestedLoss = 0.10
	// A queue building up on the path shows as round trips growing. Jitter alone does not grow them this far.
	congestedRTTRise = 200 * time.Millisecond
	// Bad readings must last this long to count as congestion, and cuts are at least this far apart.
	congestionHold = 2 * time.Second
	// A queue this long says the link is busy with what it has already, so nothing more should be tried yet.
	calmRTTRise = 50 * time.Millisecond
	// A browser that keeps asking for keyframes cannot decode what it receives. This many in a window count as congestion.
	// It is the first sign on a link too thin for the stream: a viewer that has never decoded a frame reports no loss
	// worth the name, because lost packets come back as retransmissions, and has no good round trip to compare against.
	congestedPLIs = 2
	pliWindow     = 2 * time.Second
	// After moving up a layer, one bad report is enough for this long. Nothing else has changed, so the move is the likely
	// cause, and every moment it stays too big the viewer sees a frozen picture.
	upgradeWatch = 4 * time.Second
	// How bad that one report has to be. A report taken during a keyframe's burst on a link with jitter can read 10% or more
	// while packets are only out of order, which is the WiFi case. A layer that does not fit reads 40% to 70%.
	moveFailedLoss = 0.25
	// With no bad readings for this long, the limit starts to rise. It doubles after each cut, so a link that keeps
	// proving too thin is not tried again every few seconds.
	recoveryAfter    = 3 * time.Second
	recoveryAfterMax = 30 * time.Second
	// After this long with no trouble, the link is trusted again.
	recoveryForgotten = time.Minute
	// How fast the limit rises, per second. A layer that does not fit is noticed and given up within a couple of seconds,
	// so this can be quick: when a link recovers, a viewer should not have to wait long to see it.
	capacityGrowth = 1.15
	// A limit this far above what is being sent no longer limits anything, so it goes away.
	limitDropsAt = 4
	// Leaves room for what was lost to be sent again.
	capacityMargin = 0.9
	minCapacity    = 150_000
	// A minimum round trip could be from a route that no longer exists, so it creeps up.
	baseRTTCreepPerSecond = time.Millisecond
)

// capacity judges what a subscriber's connection can carry from what its browser reports: packets lost, and round
// trips growing. There is no limit until those show congestion.
//
// The delay-based estimate from GCC is not used for this. On a link with room to spare and 8 ms of jitter, Pion's fell
// from 10 Mbps to 0.16 Mbps in 15 seconds, and video followed it down. That is normal WiFi.
type capacity struct {
	limit          int64
	sent           int64
	lastBytes      uint64
	lastAt         time.Time
	baseRTT        time.Duration
	congestedSince time.Time
	lastCut        time.Time
	lastBad        time.Time
	recovery       time.Duration
	lastPLIs       uint64
	windowPLIs     uint64
	lastWindowPLIs uint64
	pliWindowStart time.Time
	// allocated is what the chosen layers are known to cost, set by the allocator. A cut goes below it, or the allocator
	// could answer a cut by keeping the same layers. wanted is what the best layers wanted would cost.
	allocated  int64
	wanted     int64
	watchUntil time.Time
	// calm says browser reports have shown no queue on the path: at least two round trips, the latest close to the best.
	calm        bool
	lastRTT     time.Duration
	rttReadings int
	// rose says the limit has risen since the last cut. Cuts in a row are one bout of congestion, and only a limit that
	// rose and proved wrong again should make the next try wait longer.
	rose bool
}

// tryingMore says a layer was just moved up, so what comes next is the answer to whether it fits.
func (c *capacity) tryingMore(now time.Time) { c.watchUntil = now.Add(upgradeWatch) }

// update takes the totals so far of bytes sent and keyframe requests received, and the latest reports. It returns the
// limit in bits per second. Zero means no limit is known.
func (c *capacity) update(now time.Time, bytes, plis uint64, loss float64, rtt time.Duration) int64 {
	if c.lastAt.IsZero() {
		c.lastAt, c.lastBytes, c.lastPLIs, c.pliWindowStart = now, bytes, plis, now
		return c.limit
	}
	elapsed := now.Sub(c.lastAt)
	if elapsed <= 0 {
		return c.limit
	}
	rate := int64(float64(bytes-c.lastBytes) * 8 / elapsed.Seconds())
	c.lastAt, c.lastBytes = now, bytes
	if c.sent == 0 {
		c.sent = rate
	} else {
		c.sent = (c.sent + rate) / 2
	}

	if rtt > 0 && rtt != c.lastRTT {
		c.lastRTT = rtt
		c.rttReadings++
	}
	c.calm = c.rttReadings >= 2 && rtt-c.baseRTT < calmRTTRise && loss < congestedLoss/2
	if c.baseRTT > 0 {
		c.baseRTT += time.Duration(float64(baseRTTCreepPerSecond) * elapsed.Seconds())
	}
	if rtt > 0 && (c.baseRTT == 0 || rtt < c.baseRTT) {
		c.baseRTT = rtt
	}

	if now.Sub(c.pliWindowStart) >= pliWindow {
		c.pliWindowStart, c.lastWindowPLIs, c.windowPLIs = now, c.windowPLIs, 0
	}
	c.windowPLIs += plis - c.lastPLIs
	c.lastPLIs = plis

	cannotDecode := c.windowPLIs >= congestedPLIs || c.lastWindowPLIs >= congestedPLIs
	// Round trips are not enough on their own here: a burst from one keyframe can move a single reading.
	moveFailed := now.Before(c.watchUntil) && (loss >= moveFailedLoss || cannotDecode)
	if loss >= congestedLoss || (rtt > 0 && rtt-c.baseRTT >= congestedRTTRise) || cannotDecode {
		c.lastBad = now
		if c.congestedSince.IsZero() {
			c.congestedSince = now
		}
		held := now.Sub(c.congestedSince) >= congestionHold && now.Sub(c.lastCut) >= congestionHold
		if held || moveFailed {
			// What arrived is the best evidence of what fits.
			to := int64(float64(c.sent) * (1 - loss) * capacityMargin)
			if c.allocated > 0 {
				to = min(to, int64(float64(c.allocated)*capacityMargin))
			}
			c.cut(to)
			c.lastCut = now
			if c.rose || c.recovery == 0 {
				c.recovery = min(max(c.recovery*2, recoveryAfter), recoveryAfterMax)
				c.rose = false
			}
			c.watchUntil = time.Time{}
			// Those complaints were about what has just been given up. The next ones will be about what replaces it.
			c.congestedSince = time.Time{}
			c.windowPLIs, c.lastWindowPLIs = 0, 0
		}
		return c.limit
	}
	c.congestedSince = time.Time{}

	// What is flowing with no complaint fits, whatever the limit says. A layer's cost is often higher just after a move up,
	// because a new keyframe starts the encoder refining again, and that alone must not push the layer back down.
	if c.limit > 0 && c.allocated > c.limit {
		c.limit = c.allocated
	}
	if c.limit > 0 && now.Sub(c.lastBad) >= c.recovery {
		c.limit = int64(float64(c.limit) * math.Pow(capacityGrowth, elapsed.Seconds()))
		c.rose = true
		// A limit far above what is sent limits nothing, unless what it is holding back is more than that.
		if c.sent > 0 && c.limit >= limitDropsAt*c.sent && c.limit >= c.wanted {
			c.limit = 0
		}
	}
	// Only a long time with no trouble shows the link has really changed.
	if now.Sub(c.lastBad) >= recoveryForgotten {
		c.limit, c.recovery = 0, 0
	}
	return c.limit
}

func (c *capacity) cut(to int64) {
	to = max(to, minCapacity)
	if c.limit == 0 || to < c.limit {
		c.limit = to
	}
}
