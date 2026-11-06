// Package track models a track that a participant has published.
package track

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/sfu"
)

// Published is a track's server-side state: its metadata, which clients see, and its media receiver.
type Published struct {
	mu          sync.RWMutex
	info        *pb.TrackInfo
	cid         string
	owner       string
	receiver    *sfu.Receiver
	subscribers map[string]*sfu.DownTrack
	createdAt   time.Time
	notified    atomic.Bool
}

// New creates a published track with no media yet.
func New(ownerSID, cid string, info *pb.TrackInfo) *Published {
	return &Published{info: info, cid: cid, owner: ownerSID, subscribers: map[string]*sfu.DownTrack{}, createdAt: time.Now()}
}

// MarkSubscribedNotified reports whether this is the first call. A publisher is told once that its track has a
// subscriber.
func (t *Published) MarkSubscribedNotified() bool { return t.notified.CompareAndSwap(false, true) }

// SID returns the server-assigned track ID.
func (t *Published) SID() string { return t.info.GetSid() }

// CID returns the client-assigned track ID.
func (t *Published) CID() string { return t.cid }

// OwnerSID returns the publishing participant's ID.
func (t *Published) OwnerSID() string { return t.owner }

// Info returns a copy of the metadata.
func (t *Published) Info() *pb.TrackInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return proto.Clone(t.info).(*pb.TrackInfo)
}

// Update changes the metadata under the lock.
func (t *Published) Update(fn func(*pb.TrackInfo)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(t.info)
}

// Receiver returns the media receiver, or nil before the first packet arrives.
func (t *Published) Receiver() *sfu.Receiver {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.receiver
}

// SetReceiver attaches the media receiver.
func (t *Published) SetReceiver(r *sfu.Receiver) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.receiver = r
}

// Source returns the track's source.
func (t *Published) Source() pb.TrackSource {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info.GetSource()
}

// Kind returns audio or video.
func (t *Published) Kind() pb.TrackType {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info.GetType()
}

// RequestedCodec returns the mime type a client asked to publish this track with, or "" if it did not say. Once media
// is flowing this is what was negotiated, which may differ in case but not in codec.
func (t *Published) RequestedCodec() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if codecs := t.info.GetCodecs(); len(codecs) > 0 {
		return codecs[0].GetMimeType()
	}
	return ""
}

// ResolveLayerMode settles how the track's layers map to RTP streams, now that media shows what codec it is. A mode already
// decided stays. It returns the mode.
func (t *Published) ResolveLayerMode(oneStream bool) pb.VideoLayer_Mode {
	t.mu.Lock()
	defer t.mu.Unlock()
	codecs := t.info.GetCodecs()
	if len(codecs) == 0 {
		return pb.VideoLayer_MODE_UNUSED
	}
	if codecs[0].GetVideoLayerMode() == pb.VideoLayer_MODE_UNUSED {
		if t.info.GetType() != pb.TrackType_VIDEO {
			return pb.VideoLayer_MODE_UNUSED
		}
		if oneStream {
			codecs[0].VideoLayerMode = pb.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM
		} else {
			codecs[0].VideoLayerMode = pb.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM
		}
	}
	return codecs[0].GetVideoLayerMode()
}

// Muted reports the publisher's mute state.
func (t *Published) Muted() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info.GetMuted()
}

// AddSubscriber records a subscription by subscriber participant ID.
func (t *Published) AddSubscriber(participantSID string, d *sfu.DownTrack) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subscribers[participantSID] = d
}

// RemoveSubscriber forgets a subscription.
func (t *Published) RemoveSubscriber(participantSID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.subscribers, participantSID)
}

// SubscriberCount returns the number of active subscriptions.
func (t *Published) SubscriberCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.subscribers)
}

// MaxSubscribedLayer returns the highest layer any subscriber wants, or LayerNone when nobody wants
// video from this track. Publishers stop encoding what nobody watches.
func (t *Published) MaxSubscribedLayer() int32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	best := sfu.LayerNone
	for _, d := range t.subscribers {
		if d.Disabled() {
			continue
		}
		if l := d.MaxLayer(); l > best {
			best = l
		}
	}
	return best
}

// LayerForDimensions picks the lowest layer that covers the size a subscriber will display. Layers are
// described by the publisher. With no usable description, it returns the highest.
func (t *Published) LayerForDimensions(width, height uint32) int32 {
	t.mu.RLock()
	layers := slices.Clone(t.info.GetLayers())
	t.mu.RUnlock()
	if len(layers) == 0 || (width == 0 && height == 0) {
		return sfu.LayerHigh
	}
	slices.SortFunc(layers, func(a, b *pb.VideoLayer) int { return int(a.GetQuality()) - int(b.GetQuality()) })
	for _, l := range layers {
		// Comparing the shorter sides handles portrait and landscape video alike.
		if min(l.GetWidth(), l.GetHeight()) >= min(width, height) {
			return int32(l.GetQuality())
		}
	}
	return int32(layers[len(layers)-1].GetQuality())
}
