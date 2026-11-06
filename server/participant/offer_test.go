package participant

import (
	"testing"

	"elnova-streamkit.local/pkg/pb"
)

func TestRequestedVideoCodec(t *testing.T) {
	cases := []struct {
		name string
		req  *pb.AddTrackRequest
		want string
	}{
		{"nothing asked for", &pb.AddTrackRequest{Type: pb.TrackType_VIDEO, Cid: "a"}, ""},
		{"audio is not this code's concern", &pb.AddTrackRequest{Type: pb.TrackType_AUDIO, Cid: "a", SimulcastCodecs: []*pb.SimulcastCodec{{Codec: "opus", Cid: "a"}}}, ""},
		{"short name", &pb.AddTrackRequest{Type: pb.TrackType_VIDEO, Cid: "a", SimulcastCodecs: []*pb.SimulcastCodec{{Codec: "vp9", Cid: "a"}}}, "video/vp9"},
		{"full name", &pb.AddTrackRequest{Type: pb.TrackType_VIDEO, Cid: "a", SimulcastCodecs: []*pb.SimulcastCodec{{Codec: "video/AV1", Cid: "a"}}}, "video/AV1"},
		{"a backup codec listed first does not win", &pb.AddTrackRequest{Type: pb.TrackType_VIDEO, Cid: "a", SimulcastCodecs: []*pb.SimulcastCodec{{Codec: "vp8", Cid: "b"}, {Codec: "h264", Cid: "a"}}}, "video/h264"},
		{"no entry for this cid uses the first", &pb.AddTrackRequest{Type: pb.TrackType_VIDEO, Cid: "a", SimulcastCodecs: []*pb.SimulcastCodec{{Codec: "vp9"}, {Codec: "vp8"}}}, "video/vp9"},
	}
	for _, c := range cases {
		if got := requestedVideoCodec(c.req); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestVideoLayerMode(t *testing.T) {
	const svc, single = pb.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM, pb.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM
	if got := videoLayerMode(&pb.AddTrackRequest{Cid: "a"}, "video/vp9"); got != svc {
		t.Errorf("vp9 with no mode: %s", got)
	}
	if got := videoLayerMode(&pb.AddTrackRequest{Cid: "a"}, "video/AV1"); got != svc {
		t.Errorf("av1 with no mode: %s", got)
	}
	if got := videoLayerMode(&pb.AddTrackRequest{Cid: "a"}, "video/vp8"); got != single {
		t.Errorf("vp8 with no mode: %s", got)
	}
	if got := videoLayerMode(&pb.AddTrackRequest{Cid: "a"}, ""); got != pb.VideoLayer_MODE_UNUSED {
		t.Errorf("no codec named, so media has to say: %s", got)
	}
	// A client that says what it is sending is believed, as on the reference.
	req := &pb.AddTrackRequest{Cid: "a", SimulcastCodecs: []*pb.SimulcastCodec{{Cid: "a", Codec: "vp9", VideoLayerMode: single}}}
	if got := videoLayerMode(req, "video/vp9"); got != single {
		t.Errorf("explicit mode: %s", got)
	}
}

const testOffer = `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1 2 3
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
c=IN IP4 0.0.0.0
a=mid:0
m=audio 9 UDP/TLS/RTP/SAVPF 63 111
c=IN IP4 0.0.0.0
a=mid:1
a=sendonly
a=msid:stream-1 cid-audio
a=rtpmap:63 red/48000/2
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:2
a=sendonly
a=msid:stream-1 cid-video
a=rtpmap:96 VP8/90000
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:3
a=inactive
a=rtpmap:96 VP8/90000
`

const testAnswer = `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
c=IN IP4 0.0.0.0
a=mid:0
m=audio 9 UDP/TLS/RTP/SAVPF 63 111
c=IN IP4 0.0.0.0
a=mid:1
a=recvonly
a=rtpmap:63 red/48000/2
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:2
a=recvonly
a=rtpmap:96 VP8/90000
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:3
a=inactive
a=rtpmap:96 VP8/90000
`

func TestParseOfferedMedia(t *testing.T) {
	got := parseOfferedMedia(testOffer, testAnswer)
	if len(got) != 3 {
		t.Fatalf("got %d m-sections, want 3 (the data channel section is skipped): %+v", len(got), got)
	}
	audio, video, stopped := got[0], got[1], got[2]
	if audio.mid != "1" || audio.trackID != "cid-audio" || audio.kind != "audio" || !audio.sending {
		t.Errorf("audio: %+v", audio)
	}
	if audio.mime != "audio/red" {
		t.Errorf("mime = %q: the first codec in the answer is the one in use", audio.mime)
	}
	if video.mid != "2" || video.trackID != "cid-video" || video.mime != "video/VP8" || !video.sending {
		t.Errorf("video: %+v", video)
	}
	if stopped.mid != "3" || stopped.sending || stopped.trackID != "" {
		t.Errorf("inactive section: %+v", stopped)
	}
}

// Firefox offers the sections it bundles with port 0 and bundle-only. Taking those for rejected sections meant nothing
// Firefox published ever reached anyone.
func TestParseOfferedMediaUnderstandsBundleOnly(t *testing.T) {
	offer := `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
c=IN IP4 0.0.0.0
a=mid:0
m=video 0 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=bundle-only
a=mid:1
a=sendonly
a=msid:stream-1 firefox-own-id
a=rtpmap:96 VP8/90000
m=video 0 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:2
a=sendonly
a=msid:stream-1 rejected-id
a=rtpmap:96 VP8/90000
`
	got := parseOfferedMedia(offer, testAnswer)
	if len(got) != 2 {
		t.Fatalf("got %d m-sections, want 2: %+v", len(got), got)
	}
	if !got[0].sending || got[0].trackID != "firefox-own-id" {
		t.Errorf("a bundle-only section is in use: %+v", got[0])
	}
	if got[1].sending {
		t.Errorf("port 0 without bundle-only is a rejected section: %+v", got[1])
	}
}

func TestParseOfferedMediaRejectsGarbage(t *testing.T) {
	if got := parseOfferedMedia("not sdp", testAnswer); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewID("PA_")
		if len(id) != 15 || id[:3] != "PA_" {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
