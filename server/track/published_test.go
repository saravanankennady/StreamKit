package track

import (
	"testing"

	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/sfu"
)

func threeLayers() *pb.TrackInfo {
	return &pb.TrackInfo{Sid: "TR_1", Layers: []*pb.VideoLayer{
		{Quality: pb.VideoQuality_HIGH, Width: 1280, Height: 720},
		{Quality: pb.VideoQuality_LOW, Width: 320, Height: 180},
		{Quality: pb.VideoQuality_MEDIUM, Width: 640, Height: 360},
	}}
}

func TestLayerForDimensions(t *testing.T) {
	tr := New("PA_1", "cid", threeLayers())
	cases := []struct {
		w, h uint32
		want int32
	}{
		{0, 0, sfu.LayerHigh},
		{160, 90, sfu.LayerLow},
		{320, 180, sfu.LayerLow},
		{321, 181, sfu.LayerMedium},
		{640, 360, sfu.LayerMedium},
		{1280, 720, sfu.LayerHigh},
		{3840, 2160, sfu.LayerHigh},
		{180, 320, sfu.LayerLow}, // portrait display of the same size
	}
	for _, c := range cases {
		if got := tr.LayerForDimensions(c.w, c.h); got != c.want {
			t.Errorf("%dx%d: got %d, want %d", c.w, c.h, got, c.want)
		}
	}
	if got := New("PA_1", "cid", &pb.TrackInfo{}).LayerForDimensions(640, 360); got != sfu.LayerHigh {
		t.Errorf("no layers: got %d", got)
	}
}

func TestMaxSubscribedLayer(t *testing.T) {
	r := sfu.NewReceiver(sfu.ReceiverParams{Kind: webrtc.RTPCodecTypeVideo, Codec: webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
	}})
	defer r.Close()
	tr := New("PA_1", "cid", threeLayers())
	if got := tr.MaxSubscribedLayer(); got != sfu.LayerNone {
		t.Fatalf("no subscribers: %d", got)
	}
	a := sfu.NewDownTrack(sfu.DownTrackParams{ID: "x", StreamID: "s", Receiver: r})
	b := sfu.NewDownTrack(sfu.DownTrackParams{ID: "x", StreamID: "s", Receiver: r})
	a.SetMaxLayer(sfu.LayerLow)
	b.SetMaxLayer(sfu.LayerMedium)
	tr.AddSubscriber("A", a)
	tr.AddSubscriber("B", b)
	if got := tr.MaxSubscribedLayer(); got != sfu.LayerMedium {
		t.Fatalf("got %d", got)
	}
	b.SetDisabled(true)
	if got := tr.MaxSubscribedLayer(); got != sfu.LayerLow {
		t.Fatalf("a paused subscriber must not count: %d", got)
	}
	tr.RemoveSubscriber("A")
	if got := tr.MaxSubscribedLayer(); got != sfu.LayerNone {
		t.Fatalf("got %d", got)
	}
}

func TestInfoReturnsCopy(t *testing.T) {
	tr := New("PA_1", "cid", &pb.TrackInfo{Sid: "TR_1"})
	i := tr.Info()
	i.Muted = true
	if tr.Muted() {
		t.Fatal("Info must return a copy")
	}
	tr.Update(func(i *pb.TrackInfo) { i.Muted = true })
	if !tr.Muted() {
		t.Fatal("Update did not apply")
	}
}
