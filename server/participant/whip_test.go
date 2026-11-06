package participant

import (
	"errors"
	"strings"
	"testing"

	"elnova-streamkit.local/pkg/pb"
)

func TestParsePublishOffer(t *testing.T) {
	tracks, err := parsePublishOffer(testOffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want the audio and the sending video: %+v", len(tracks), tracks)
	}
	if tracks[0].kind != pb.TrackType_AUDIO || tracks[0].cid != "cid-audio" || tracks[0].mid != "1" {
		t.Errorf("audio: %+v", tracks[0])
	}
	if tracks[1].kind != pb.TrackType_VIDEO || tracks[1].cid != "cid-video" || tracks[1].mid != "2" {
		t.Errorf("video: %+v", tracks[1])
	}
}

const offerHead = "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"

func section(kind, mid string, attrs ...string) string {
	return "m=" + kind + " 9 UDP/TLS/RTP/SAVPF 96\r\nc=IN IP4 0.0.0.0\r\na=mid:" + mid + "\r\n" + strings.Join(attrs, "\r\n") + "\r\n"
}

func TestParsePublishOfferWithoutMsid(t *testing.T) {
	tracks, err := parsePublishOffer(offerHead + section("video", "0", "a=sendonly"))
	if err != nil {
		t.Fatal(err)
	}
	if tracks[0].cid != "whip-video-0" {
		t.Errorf("cid %q", tracks[0].cid)
	}
}

func TestParsePublishOfferSimulcast(t *testing.T) {
	tracks, err := parsePublishOffer(offerHead + section("video", "0", "a=sendonly", "a=rid:q send", "a=rid:f send"))
	if err != nil {
		t.Fatal(err)
	}
	req := requestForOffered(tracks[0])
	if len(req.Layers) != 2 || req.Layers[0].Quality != pb.VideoQuality_LOW || req.Layers[1].Quality != pb.VideoQuality_HIGH {
		t.Errorf("layers: %+v", req.Layers)
	}
}

func TestParsePublishOfferRefusals(t *testing.T) {
	for name, offer := range map[string]string{
		"not sdp":         "hello",
		"nothing to send": offerHead + section("video", "0", "a=recvonly"),
		"two videos":      offerHead + section("video", "0", "a=sendonly") + section("video", "1", "a=sendonly"),
		"two audios":      offerHead + section("audio", "0", "a=sendonly") + section("audio", "1", "a=sendonly"),
		"unknown rid":     offerHead + section("video", "0", "a=sendonly", "a=rid:hi send"),
	} {
		if _, err := parsePublishOffer(offer); !errors.Is(err, ErrBadOffer) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
