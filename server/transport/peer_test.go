package transport

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
)

// These run real PeerConnections, because the bugs they guard against live in the state machine Pion enforces.

// sendingVideoOffer makes an offer from a connection knowing only the given codecs, or all of them.
func sendingVideoOffer(t *testing.T, only ...string) webrtc.SessionDescription {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if len(only) == 0 {
		if err := registerCodecs(me, true, false); err != nil {
			t.Fatal(err)
		}
	}
	for i, mime := range only {
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000},
			PayloadType:        webrtc.PayloadType(96 + i),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			t.Fatal(err)
		}
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer
}

// answerNames applies an offer to a publisher connection, runs prefer on its transceiver, and lists the answer's codecs.
func answerNames(t *testing.T, offer webrtc.SessionDescription, prefer func(*webrtc.RTPTransceiver)) []string {
	t.Helper()
	server, err := (&Engine{}).NewPublisherPC(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	prefer(server.GetTransceivers()[0])
	answer, err := server.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(answer.SDP, "\r\n") {
		if _, rest, ok := strings.Cut(line, "a=rtpmap:"); ok {
			_, codec, _ := strings.Cut(rest, " ")
			name, _, _ := strings.Cut(codec, "/")
			names = append(names, name)
		}
	}
	return names
}

// A browser offers VP8 first whatever the application asked for, then sends what the answer lists first.
func TestPreferCodecDecidesTheAnswerOrder(t *testing.T) {
	for _, want := range []string{"VP9", "H264", "AV1", "VP8"} {
		got := answerNames(t, sendingVideoOffer(t), func(tr *webrtc.RTPTransceiver) {
			if found, err := PreferCodec(tr, "video/"+strings.ToLower(want), false); err != nil || !found {
				t.Fatalf("PreferCodec(%s): found %v, error %v", want, found, err)
			}
		})
		if len(got) == 0 || got[0] != want {
			t.Errorf("asked for %s, the answer lists %v", want, got)
		}
		if !slices.Contains(got, "VP8") || !slices.Contains(got, "rtx") {
			t.Errorf("asked for %s: the others should stay available: %v", want, got)
		}
	}

	got := answerNames(t, sendingVideoOffer(t), func(tr *webrtc.RTPTransceiver) {
		if found, err := PreferCodec(tr, "video/vp9", true); err != nil || !found {
			t.Fatalf("found %v, error %v", found, err)
		}
	})
	if len(got) == 0 || !slices.Contains(got, "VP9") {
		t.Fatalf("exclusive answer lists %v", got)
	}
	for _, name := range got {
		if name != "VP9" && name != "rtx" {
			t.Errorf("exclusive answer lists %v", got)
			break
		}
	}
}

func TestPreferCodecLeavesAnAnswerAloneWhenTheOfferLacksIt(t *testing.T) {
	got := answerNames(t, sendingVideoOffer(t, webrtc.MimeTypeVP8), func(tr *webrtc.RTPTransceiver) {
		if found, err := PreferCodec(tr, "video/vp9", false); err != nil || found {
			t.Fatalf("found %v, error %v", found, err)
		}
	})
	if !slices.Contains(got, "VP8") {
		t.Fatalf("the offered codec should still be answered: %v", got)
	}
}

// singleHarness is a client on one connection with a server Peer, for the protocol where only the client offers.
type singleHarness struct {
	t            *testing.T
	server       *Peer
	client       *webrtc.PeerConnection
	requirements chan [2]uint32
	nextID       uint32
}

func newSingleHarness(t *testing.T, sections int) *singleHarness {
	t.Helper()
	serverPC, err := (&Engine{}).NewSinglePC(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	h := &singleHarness{t: t, requirements: make(chan [2]uint32, 16)}
	h.server = NewPeer(serverPC, pb.SignalTarget_PUBLISHER, Callbacks{
		Requirement: func(audio, video uint32) { h.requirements <- [2]uint32{audio, video} },
	}, slog.New(slog.DiscardHandler))
	t.Cleanup(h.server.Close)
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me, true, false); err != nil {
		t.Fatal(err)
	}
	h.client, err = webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.client.Close() })
	for range sections {
		h.addSection()
	}
	return h
}

func (h *singleHarness) addSection() {
	h.t.Helper()
	if _, err := h.client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		h.t.Fatal(err)
	}
}

// exchange makes an offer from the client and applies the server's answer.
func (h *singleHarness) exchange() *pb.SessionDescription {
	h.t.Helper()
	offer, err := h.client.CreateOffer(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.SetLocalDescription(offer); err != nil {
		h.t.Fatal(err)
	}
	h.nextID++
	answer, err := h.server.Answer(&pb.SessionDescription{Type: "offer", Sdp: offer.SDP, Id: h.nextID}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if answer.GetId() != h.nextID {
		h.t.Fatalf("answer id %d to offer id %d", answer.GetId(), h.nextID)
	}
	if err := h.client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.GetSdp()}); err != nil {
		h.t.Fatal(err)
	}
	return answer
}

func (h *singleHarness) addTrack(id string) *webrtc.RTPSender {
	h.t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, id, "stream-"+id)
	if err != nil {
		h.t.Fatal(err)
	}
	sender, err := h.server.PC().AddTrack(track)
	if err != nil {
		h.t.Fatal(err)
	}
	return sender
}

func (h *singleHarness) requirement() [2]uint32 {
	h.t.Helper()
	select {
	case r := <-h.requirements:
		return r
	case <-time.After(2 * time.Second):
		h.t.Fatal("no requirement")
		return [2]uint32{}
	}
}

func (h *singleHarness) noRequirement() {
	h.t.Helper()
	select {
	case r := <-h.requirements:
		h.t.Fatalf("unexpected requirement %v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNegotiatingOnOneConnectionAsksTheClientToOffer(t *testing.T) {
	h := newSingleHarness(t, 3)
	h.exchange()

	// A track goes into a section the client already offered, but it takes another offer to say so. An empty requirement is
	// how to ask for one.
	h.addTrack("a")
	h.server.Negotiate(false)
	if got := h.requirement(); got != [2]uint32{0, 0} {
		t.Fatalf("requirement = %v", got)
	}
	answer := h.exchange()
	if got := answer.GetMidToTrackId(); len(got) != 1 {
		t.Fatalf("the answer should map the track to its section: %v", got)
	}
	h.server.AnswerSent()
	h.noRequirement()

	// Two more fill the sections, and the one after has nowhere to go.
	h.addTrack("b")
	h.addTrack("c")
	h.addTrack("d")
	h.server.Negotiate(false)
	if got := h.requirement(); got != [2]uint32{0, 1} {
		t.Fatalf("requirement = %v", got)
	}

	// Asking again counts only what is new.
	h.server.Negotiate(false)
	if got := h.requirement(); got != [2]uint32{0, 0} {
		t.Fatalf("second requirement = %v", got)
	}

	// The client lost that message, and offers with no new section. The next answer starts the count over.
	answer = h.exchange()
	if got := answer.GetMidToTrackId(); len(got) != 3 {
		t.Fatalf("mapped %v", got)
	}
	h.server.AnswerSent()
	if got := h.requirement(); got != [2]uint32{0, 1} {
		t.Fatalf("a lost requirement should be sent again: %v", got)
	}

	// With the section added, every track has a place.
	h.addSection()
	answer = h.exchange()
	if got := answer.GetMidToTrackId(); len(got) != 4 {
		t.Fatalf("mapped %v", got)
	}
	h.server.AnswerSent()
	h.noRequirement()
}

func TestOneConnectionReusesASectionAfterATrackLeaves(t *testing.T) {
	h := newSingleHarness(t, 1)
	sender := h.addTrack("a")
	answer := h.exchange()
	var mid string
	for m, id := range answer.GetMidToTrackId() {
		if id != "a" {
			t.Fatalf("mapped %q", id)
		}
		mid = m
	}
	if mid == "" {
		t.Fatalf("a track added before any offer should get a section: %v", answer.GetMidToTrackId())
	}

	if err := h.server.PC().RemoveTrack(sender); err != nil {
		t.Fatal(err)
	}
	answer = h.exchange()
	if got := answer.GetMidToTrackId(); len(got) != 0 {
		t.Fatalf("still mapped: %v", got)
	}
	if !strings.Contains(answer.GetSdp(), "a=inactive") {
		t.Fatalf("the section should go inactive:\n%s", answer.GetSdp())
	}

	h.addTrack("b")
	answer = h.exchange()
	if got := answer.GetMidToTrackId()[mid]; got != "b" {
		t.Fatalf("section %s holds %q: %v", mid, got, answer.GetMidToTrackId())
	}
}

// A section the client publishes on is no place for a track going the other way.
func TestOneConnectionKeepsTracksOffSectionsTheClientPublishesOn(t *testing.T) {
	h := newSingleHarness(t, 0)
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "published", "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatal(err)
	}
	h.addTrack("subscribed")
	h.exchange()
	h.server.Negotiate(false)
	if got := h.requirement(); got != [2]uint32{0, 1} {
		t.Fatalf("the client's own section does not count, so it needs a new one: %v", got)
	}
}

type peerHarness struct {
	t      *testing.T
	server *Peer
	client *webrtc.PeerConnection
	offers chan *pb.SessionDescription
}

func newPeerHarness(t *testing.T) *peerHarness {
	t.Helper()
	serverPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	h := &peerHarness{t: t, client: clientPC, offers: make(chan *pb.SessionDescription, 8)}
	h.server = NewPeer(serverPC, pb.SignalTarget_SUBSCRIBER, Callbacks{
		Offer: func(sd *pb.SessionDescription) { h.offers <- sd },
		ICE: func(candidate string, _ pb.SignalTarget) {
			var init webrtc.ICECandidateInit
			if json.Unmarshal([]byte(candidate), &init) == nil {
				_ = clientPC.AddICECandidate(init)
			}
		},
	}, slog.New(slog.DiscardHandler))
	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		h.server.AddICE(string(data))
	})
	t.Cleanup(func() {
		h.server.Close()
		_ = clientPC.Close()
	})
	if _, err := serverPC.CreateDataChannel("_reliable", nil); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *peerHarness) nextOffer(within time.Duration) *pb.SessionDescription {
	h.t.Helper()
	select {
	case sd := <-h.offers:
		return sd
	case <-time.After(within):
		h.t.Fatalf("no offer within %v (signaling state %s)", within, h.server.PC().SignalingState())
		return nil
	}
}

func (h *peerHarness) noOffer(during time.Duration) {
	h.t.Helper()
	select {
	case sd := <-h.offers:
		h.t.Fatalf("unexpected offer, id %d", sd.GetId())
	case <-time.After(during):
	}
}

// answer does what a client does with an offer, returning its answer.
func (h *peerHarness) answer(offer *pb.SessionDescription) *pb.SessionDescription {
	h.t.Helper()
	if err := h.client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()}); err != nil {
		h.t.Fatal(err)
	}
	answer, err := h.client.CreateAnswer(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.SetLocalDescription(answer); err != nil {
		h.t.Fatal(err)
	}
	return &pb.SessionDescription{Type: "answer", Sdp: answer.SDP, Id: offer.GetId()}
}

func (h *peerHarness) addTrack(id string) {
	h.t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, id, "stream-"+id)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.server.PC().AddTrack(track); err != nil {
		h.t.Fatal(err)
	}
}

func (h *peerHarness) waitConnected() {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.server.PC().ConnectionState() == webrtc.PeerConnectionStateConnected &&
			h.client.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("not connected: server %s, client %s", h.server.PC().ConnectionState(), h.client.ConnectionState())
}

func ufrag(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(line, "a=ice-ufrag:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "a=ice-ufrag:"))
		}
	}
	return ""
}

func TestChangesDuringAnOutstandingOfferMergeIntoOneMoreOffer(t *testing.T) {
	h := newPeerHarness(t)
	h.server.Negotiate(false)
	first := h.nextOffer(2 * time.Second)

	h.addTrack("a")
	h.server.Negotiate(false)
	h.addTrack("b")
	h.server.Negotiate(false)
	h.noOffer(200 * time.Millisecond)

	if err := h.server.HandleAnswer(h.answer(first)); err != nil {
		t.Fatal(err)
	}
	second := h.nextOffer(2 * time.Second)
	if second.GetId() <= first.GetId() {
		t.Fatalf("offer IDs must increase: %d then %d", first.GetId(), second.GetId())
	}
	if got := strings.Count(second.GetSdp(), "m=audio"); got != 2 {
		t.Fatalf("the merged offer has %d audio sections, want 2", got)
	}
	if len(second.GetMidToTrackId()) != 2 {
		t.Fatalf("mid_to_track_id = %v", second.GetMidToTrackId())
	}
	h.noOffer(200 * time.Millisecond)
}

// Regression: a client's signalling can drop while an offer is unanswered, and resuming restarts ICE at that
// moment. Pion refuses a second offer from have-local-offer. The restart and any queued tracks used to be lost, which
// left that subscriber connected, with no media, until it left.
func TestICERestartWaitsForAnUnansweredOfferAndLosesNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		// clientAlreadyAnswered: the client saw the first offer, but its answer was lost with the connection.
		clientAlreadyAnswered bool
	}{
		{"the client never saw the offer", false},
		{"the client's answer was lost", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPeerHarness(t)
			h.server.Negotiate(false)
			first := h.nextOffer(2 * time.Second)
			if c.clientAlreadyAnswered {
				h.answer(first)
			}

			// A track arrives, and then the client resumes.
			h.addTrack("late")
			h.server.Negotiate(false)
			h.server.ResendPendingOffer()
			h.server.Negotiate(true)

			replay := h.nextOffer(2 * time.Second)
			if replay.GetId() != first.GetId() || ufrag(replay.GetSdp()) != ufrag(first.GetSdp()) {
				t.Fatalf("expected the same offer again: id %d then %d", first.GetId(), replay.GetId())
			}
			h.noOffer(100 * time.Millisecond)
			if err := h.server.HandleAnswer(h.answer(replay)); err != nil {
				t.Fatal(err)
			}

			second := h.nextOffer(2 * time.Second)
			if second.GetId() <= first.GetId() {
				t.Fatalf("offer IDs must increase: %d then %d", first.GetId(), second.GetId())
			}
			if ufrag(second.GetSdp()) == ufrag(first.GetSdp()) {
				t.Fatal("the next offer must restart ICE")
			}
			if !strings.Contains(second.GetSdp(), "m=audio") {
				t.Fatal("the track queued behind the unanswered offer was lost")
			}
			if err := h.server.HandleAnswer(h.answer(second)); err != nil {
				t.Fatal(err)
			}
			if got := h.server.PC().SignalingState(); got != webrtc.SignalingStateStable {
				t.Fatalf("state = %s", got)
			}
			h.waitConnected()
			h.noOffer(200 * time.Millisecond)
		})
	}
}

func TestResendWithNothingPendingDoesNothing(t *testing.T) {
	h := newPeerHarness(t)
	h.server.ResendPendingOffer()
	h.noOffer(100 * time.Millisecond)
	h.server.Negotiate(false)
	offer := h.nextOffer(2 * time.Second)
	if err := h.server.HandleAnswer(h.answer(offer)); err != nil {
		t.Fatal(err)
	}
	h.server.ResendPendingOffer()
	h.noOffer(100 * time.Millisecond)
}

func TestAnswerToAnOlderOfferIsIgnored(t *testing.T) {
	h := newPeerHarness(t)
	h.server.Negotiate(false)
	first := h.nextOffer(2 * time.Second)
	firstAnswer := h.answer(first)
	if err := h.server.HandleAnswer(firstAnswer); err != nil {
		t.Fatal(err)
	}
	h.addTrack("x")
	h.server.Negotiate(false)
	second := h.nextOffer(2 * time.Second)

	// A late copy of the first answer must not be applied to the second offer.
	if err := h.server.HandleAnswer(firstAnswer); err != nil {
		t.Fatalf("a stale answer should be ignored: %v", err)
	}
	if got := h.server.PC().SignalingState(); got != webrtc.SignalingStateHaveLocalOffer {
		t.Fatalf("a stale answer changed the state to %s", got)
	}
	if err := h.server.HandleAnswer(h.answer(second)); err != nil {
		t.Fatal(err)
	}
	h.waitConnected()
}

func TestAnAnswerWithNoOfferOutstandingIsIgnored(t *testing.T) {
	h := newPeerHarness(t)
	h.server.Negotiate(false)
	offer := h.nextOffer(2 * time.Second)
	answer := h.answer(offer)
	if err := h.server.HandleAnswer(answer); err != nil {
		t.Fatal(err)
	}
	// A client replays its last answer in SyncState after resuming.
	if err := h.server.HandleAnswer(answer); err != nil {
		t.Fatalf("a replayed answer should be ignored: %v", err)
	}
	if got := h.server.PC().SignalingState(); got != webrtc.SignalingStateStable {
		t.Fatalf("state = %s", got)
	}
}

// A failed offer used to be forgotten, so the change it carried never reached the client.
func TestAFailedOfferIsRetried(t *testing.T) {
	h := newPeerHarness(t)
	// With a remote offer pending, Pion refuses a local one, so the first attempts fail.
	if _, err := h.client.CreateDataChannel("x", nil); err != nil {
		t.Fatal(err)
	}
	remote, err := h.client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.SetLocalDescription(remote); err != nil {
		t.Fatal(err)
	}
	if err := h.server.PC().SetRemoteDescription(remote); err != nil {
		t.Fatal(err)
	}
	h.server.Negotiate(true)
	h.noOffer(300 * time.Millisecond)

	// Once the conflict clears, the offer goes out, with its ICE restart intact.
	before := ufrag(remote.SDP)
	answer, err := h.server.PC().CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.PC().SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	got := h.nextOffer(6 * time.Second)
	if u := ufrag(got.GetSdp()); u == "" || u == ufrag(answer.SDP) || u == before {
		t.Fatalf("the retried offer did not restart ICE: %q", u)
	}
}

func TestClosedPeerStopsNegotiating(t *testing.T) {
	h := newPeerHarness(t)
	h.server.Negotiate(false)
	h.server.Close()
	h.noOffer(200 * time.Millisecond)
	h.server.Negotiate(true)
	h.noOffer(100 * time.Millisecond)
}
