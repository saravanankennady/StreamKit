package sfu

import (
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	// How many sent packets are kept for answering NACKs, per track and subscriber. At 5 Mbps this is about 4 seconds,
	// far longer than a retransmission stays useful. A power of two.
	resendHistory = 2048
	// A browser asks again while a packet is on its way, so a packet is only sent again a few times, and not too soon.
	maxResends        = 3
	resendMaxInterval = 100 * time.Millisecond
	// Retransmissions may add at most this share to what is being forwarded. At 20% loss a stream needs about a quarter
	// more. Past that, they only fill a full link further and push out the packets that are still worth having.
	resendShare = 0.5
	// Saved up retransmission allowance is limited to this many bytes, enough for a keyframe of a small layer.
	resendBurstBytes = 64 * 1024
	// How long to hold off after a keyframe request for the keyframe to start arriving.
	resendKeyframeWait = 2 * time.Second
)

// sentRecord is one packet as it went out. buf is reused as the history wraps, so the history allocates little once full.
type sentRecord struct {
	buf      []byte
	seq      uint16
	used     bool
	lastSent int64
	resends  uint8
}

// resender answers a subscriber's NACKs from a history of what was sent. It is guarded by the DownTrack's mutex.
//
// Pion's responder answers every NACK. On a link that is already full, that makes a storm: a browser that has lost a
// thousand packets asks for all of them each round trip, what is sent again crowds out the keyframe that would end it,
// and the viewer stays frozen even at a low layer. So retransmissions here stop when they cannot help.
type resender struct {
	history []sentRecord
	// awaitingKeyframeSince is when the browser gave up on what came before the next keyframe, or zero if it has not. If
	// no keyframe comes after a while, retransmission starts again, since it is then the only way to make progress.
	awaitingKeyframeSince int64
	allowance             float64
	sent                  uint64
	// Nothing before the latest keyframe can matter any more, but a browser may go on asking for it until the keyframe
	// has all arrived. That is exactly when the link can least afford to send it.
	keyframeSeq uint16
	hasKeyframe bool
}

// keyframeSent notes the first packet of a keyframe.
func (r *resender) keyframeSent(seq uint16) {
	r.awaitingKeyframeSince = 0
	r.keyframeSeq, r.hasKeyframe = seq, true
}

// remember records a packet just sent.
func (r *resender) remember(hdr *rtp.Header, payload []byte, now int64) {
	if r.history == nil {
		r.history = make([]sentRecord, resendHistory)
	}
	size := hdr.MarshalSize() + len(payload)
	e := &r.history[hdr.SequenceNumber&(resendHistory-1)]
	if cap(e.buf) < size {
		e.buf = make([]byte, size)
	}
	e.buf = e.buf[:size]
	n, err := hdr.MarshalTo(e.buf)
	if err != nil {
		e.used = false
		return
	}
	copy(e.buf[n:], payload)
	e.seq, e.used, e.lastSent, e.resends = hdr.SequenceNumber, true, now, 0
	r.allowance = min(r.allowance+float64(size)*resendShare, resendBurstBytes)
}

// answer sends again whatever a NACK asks for that is still worth sending. write sends one marshalled packet.
func (r *resender) answer(pairs []rtcp.NackPair, now int64, interval time.Duration, write func([]byte)) {
	if r.history == nil || (r.awaitingKeyframeSince != 0 && now-r.awaitingKeyframeSince < int64(resendKeyframeWait)) {
		return
	}
	for _, pair := range pairs {
		pair.Range(func(seq uint16) bool {
			e := &r.history[seq&(resendHistory-1)]
			if !e.used || e.seq != seq || e.resends >= maxResends || (r.hasKeyframe && int16(seq-r.keyframeSeq) < 0) {
				return true
			}
			// The first request is news. A repeat may only mean the answer is still on its way.
			if e.resends > 0 && now-e.lastSent < int64(interval) {
				return true
			}
			if r.allowance < float64(len(e.buf)) {
				return false
			}
			r.allowance -= float64(len(e.buf))
			e.resends++
			e.lastSent = now
			r.sent++
			write(e.buf)
			return true
		})
	}
}

const (
	// The wait for the next keyframe doubles while requests keep coming, up to this.
	keyframeSpacingMax = 8 * time.Second
	// Requests this far apart are not the same trouble.
	keyframeSpacingForgotten = 10 * time.Second
)

// keyframeSpacing spaces out a subscriber's keyframe requests on a short link. A browser that has decoded nothing asks every
// 3 seconds. Each new keyframe takes longer than that to cross a thin link, sets the encoder refining from scratch so the
// stream costs more, and is dropped in turn, and the viewer stays frozen. Left a little longer, the keyframe already on its
// way can finish arriving by retransmission, and the stream's cost falls until the next one fits.
type keyframeSpacing struct {
	next, last int64
	wait       time.Duration
}

// allow says whether a request received now should go on to the publisher.
func (k *keyframeSpacing) allow(now int64, short bool) bool {
	if !short {
		k.wait = 0
		return true
	}
	if now < k.next {
		return false
	}
	if k.last != 0 && now-k.last < int64(keyframeSpacingForgotten) {
		k.wait = min(max(k.wait*2, time.Second), keyframeSpacingMax)
	} else {
		k.wait = 0
	}
	k.last, k.next = now, now+int64(k.wait)
	return true
}

// resendInterval is how long to leave a packet before sending it again: long enough to know the last try did not arrive.
func resendInterval(rtt time.Duration) time.Duration {
	if rtt <= 0 {
		return resendMaxInterval
	}
	return min(2*rtt, resendMaxInterval)
}
