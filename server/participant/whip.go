package participant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
)

// ErrBadOffer is returned for an offer that cannot be published as one participant's camera and microphone.
var ErrBadOffer = errors.New("the offer must send at most one audio and one video track, using rids q, h and f for simulcast")

const offerGatherTimeout = 5 * time.Second

// offeredPublish is one track a signalling-less publisher offers.
type offeredPublish struct {
	mid  string
	cid  string
	kind pb.TrackType
	rids []string
}

// parsePublishOffer reads the tracks out of an offer. Clients with signalling say what they publish in AddTrack
// requests. A WHIP client's offer is all there is, so it has to say everything.
func parsePublishOffer(offer string) ([]offeredPublish, error) {
	var desc sdp.SessionDescription
	if err := desc.Unmarshal([]byte(offer)); err != nil {
		return nil, ErrBadOffer
	}
	var out []offeredPublish
	audio, video := 0, 0
	for _, m := range desc.MediaDescriptions {
		kind := m.MediaName.Media
		if kind != "audio" && kind != "video" || m.MediaName.Port.Value == 0 {
			continue
		}
		_, sendOnly := m.Attribute("sendonly")
		_, sendRecv := m.Attribute("sendrecv")
		if !sendOnly && !sendRecv {
			continue
		}
		o := offeredPublish{kind: pb.TrackType_AUDIO}
		o.mid, _ = m.Attribute("mid")
		if kind == "video" {
			o.kind = pb.TrackType_VIDEO
			video++
		} else {
			audio++
		}
		if msid, ok := m.Attribute("msid"); ok {
			if parts := strings.Fields(msid); len(parts) >= 2 {
				o.cid = parts[1]
			}
		}
		if o.cid == "" {
			o.cid = "whip-" + kind + "-" + o.mid
		}
		for _, a := range m.Attributes {
			if a.Key != "rid" {
				continue
			}
			rid, direction, _ := strings.Cut(a.Value, " ")
			if !strings.HasPrefix(direction, "send") {
				continue
			}
			if rid != "q" && rid != "h" && rid != "f" {
				return nil, ErrBadOffer
			}
			o.rids = append(o.rids, rid)
		}
		out = append(out, o)
	}
	if len(out) == 0 || audio > 1 || video > 1 {
		return nil, ErrBadOffer
	}
	return out, nil
}

func requestForOffered(o offeredPublish) *pb.AddTrackRequest {
	req := &pb.AddTrackRequest{Cid: o.cid, Type: o.kind}
	if o.kind == pb.TrackType_AUDIO {
		req.Source, req.Name = pb.TrackSource_MICROPHONE, "microphone"
		return req
	}
	req.Source, req.Name = pb.TrackSource_CAMERA, "camera"
	for _, rid := range o.rids {
		quality := pb.VideoQuality_HIGH
		switch rid {
		case "q":
			quality = pb.VideoQuality_LOW
		case "h":
			quality = pb.VideoQuality_MEDIUM
		}
		req.Layers = append(req.Layers, &pb.VideoLayer{Quality: quality})
	}
	return req
}

// PublishOffer takes a publisher with no signalling connection, such as a WHIP client: it creates tracks from the
// offer's sections and answers it. The answer contains all of this server's candidates, since such a client may have
// no way to receive more later.
func (p *Participant) PublishOffer(ctx context.Context, offer string) (string, error) {
	tracks, err := parsePublishOffer(offer)
	if err != nil {
		return "", err
	}
	cids := make(map[string]string, len(tracks))
	for _, o := range tracks {
		p.handleAddTrack(requestForOffered(o))
		cids[o.mid] = o.cid
	}

	pc := p.pub.PC()
	gathered := webrtc.GatheringCompletePromise(pc)
	answer, err := p.pub.Answer(&pb.SessionDescription{Type: "offer", Sdp: offer}, nil)
	if err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(offerGatherTimeout):
		return "", errors.New("timed out gathering candidates")
	case <-ctx.Done():
		return "", ctx.Err()
	}

	media := parseOfferedMedia(offer, answer.GetSdp())
	for i := range media {
		media[i].trackID = cids[media[i].mid]
	}
	p.bindOfferedTracks(media)
	return pc.LocalDescription().SDP, nil
}
