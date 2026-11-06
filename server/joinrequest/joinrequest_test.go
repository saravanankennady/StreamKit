package joinrequest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

func wrap(t *testing.T, join *pb.JoinRequest, zip bool) string {
	t.Helper()
	contents, err := proto.Marshal(join)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &pb.WrappedJoinRequest{Compression: pb.WrappedJoinRequest_NONE, JoinRequest: contents}
	if zip {
		wrapped.Compression = pb.WrappedJoinRequest_GZIP
		wrapped.JoinRequest = gzipped(t, contents)
	}
	raw, err := proto.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func gzipped(t *testing.T, contents []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecode(t *testing.T) {
	yes := true
	join := &pb.JoinRequest{
		Metadata:           "m",
		Reconnect:          true,
		ParticipantSid:     "PA_x",
		ReconnectReason:    pb.ReconnectReason_RR_SIGNAL_DISCONNECTED,
		ConnectionSettings: &pb.ConnectionSettings{AutoSubscribe: true, AutoSubscribeDataTrack: &yes},
		PublisherOffer:     &pb.SessionDescription{Type: "offer", Sdp: "v=0", Id: 1},
		AddTrackRequests:   []*pb.AddTrackRequest{{Cid: "c"}},
	}
	for _, zip := range []bool{false, true} {
		got, err := Decode(wrap(t, join, zip))
		if err != nil {
			t.Fatalf("gzip %v: %v", zip, err)
		}
		if !proto.Equal(got, join) {
			t.Errorf("gzip %v: got %v", zip, got)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	huge := []byte(strings.Repeat("x", maxBytes+1))
	over := func(zip bool) string {
		w := &pb.WrappedJoinRequest{JoinRequest: huge}
		if zip {
			w.Compression, w.JoinRequest = pb.WrappedJoinRequest_GZIP, gzipped(t, huge)
		}
		raw, err := proto.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		return base64.URLEncoding.EncodeToString(raw)
	}
	notWrapped := base64.URLEncoding.EncodeToString([]byte{0xff, 0xff, 0xff})
	notGzip, err := proto.Marshal(&pb.WrappedJoinRequest{Compression: pb.WrappedJoinRequest_GZIP, JoinRequest: []byte("plain")})
	if err != nil {
		t.Fatal(err)
	}
	notJoin, err := proto.Marshal(&pb.WrappedJoinRequest{JoinRequest: []byte{0xff, 0xff}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, in string
		want     error
	}{
		{"missing", "", ErrMissing},
		{"not base64", "!!!", ErrEncoding},
		{"standard base64 alphabet", "+/+/", ErrEncoding},
		{"not a wrapped request", notWrapped, ErrWrapped},
		{"claims gzip and is not", base64.URLEncoding.EncodeToString(notGzip), ErrGzip},
		{"not a join request", base64.URLEncoding.EncodeToString(notJoin), ErrContents},
		{"over the limit", over(false), ErrTooLarge},
		{"over the limit once decompressed", over(true), ErrTooLarge},
	}
	for _, c := range cases {
		if _, err := Decode(c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %v", c.name, err, c.want)
		}
	}
}
