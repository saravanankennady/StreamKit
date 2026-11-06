package transport

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/metrics"
)

const (
	// negotiateDelay gathers track changes that arrive close together into one offer.
	negotiateDelay = 25 * time.Millisecond
	offerRetryBase = 250 * time.Millisecond
	offerRetryMax  = 5 * time.Second
)

// Callbacks deliver a Peer's output to the signalling layer.
type Callbacks struct {
	Offer       func(sd *pb.SessionDescription)
	ICE         func(candidate string, target pb.SignalTarget)
	StateChange func(webrtc.PeerConnectionState)
	// Requirement is for a connection where only the client offers, and it makes Negotiate ask the client to offer instead
	// of offering. It says how many more sections of each kind the client should add. Both can be zero, which still means
	// offer again.
	Requirement func(audio, video uint32)
}

// Peer adds this server's negotiation rules on top of a PeerConnection: it queues ICE candidates that
// arrive before the remote description and serializes the offers it sends.
type Peer struct {
	pc     *webrtc.PeerConnection
	target pb.SignalTarget
	cb     Callbacks
	log    *slog.Logger

	mu            sync.Mutex
	pendingICE    []webrtc.ICECandidateInit
	haveRemote    bool
	awaitingReply bool
	wantOffer     bool
	restartICE    bool
	timer         *time.Timer
	offerID       uint32
	offerFailures int
	closed        bool
	// How many sections the client has been asked for since its last offer was answered.
	requestedAudio uint32
	requestedVideo uint32
}

// NewPeer wraps a PeerConnection. target says which of the participant's two connections this is.
func NewPeer(pc *webrtc.PeerConnection, target pb.SignalTarget, cb Callbacks, log *slog.Logger) *Peer {
	p := &Peer{pc: pc, target: target, cb: cb, log: log}
	metrics.PeerConns.Inc()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || cb.ICE == nil {
			return
		}
		data, err := json.Marshal(c.ToJSON())
		if err != nil {
			return
		}
		cb.ICE(string(data), target)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Debug("peer connection state", "target", target.String(), "state", s.String())
		if cb.StateChange != nil {
			cb.StateChange(s)
		}
	})
	return p
}

// PC returns the underlying PeerConnection.
func (p *Peer) PC() *webrtc.PeerConnection { return p.pc }

// AddICE applies a candidate from the remote side, holding it until the remote description is set.
func (p *Peer) AddICE(candidateJSON string) {
	var c webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &c); err != nil {
		p.log.Warn("bad ICE candidate", "error", err)
		return
	}
	p.mu.Lock()
	if !p.haveRemote {
		p.pendingICE = append(p.pendingICE, c)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	if err := p.pc.AddICECandidate(c); err != nil {
		p.log.Debug("could not add ICE candidate", "error", err)
	}
}

func (p *Peer) flushICE() {
	p.mu.Lock()
	p.haveRemote = true
	pending := p.pendingICE
	p.pendingICE = nil
	p.mu.Unlock()
	for _, c := range pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			p.log.Debug("could not add queued ICE candidate", "error", err)
		}
	}
}

// Answer applies a remote offer and creates the answer. Only the publisher connection receives offers. prepare may be
// nil. It runs between the two steps, which is the only point where the transceivers exist and the answer is still
// open to change.
func (p *Peer) Answer(offer *pb.SessionDescription, prepare func()) (*pb.SessionDescription, error) {
	err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()})
	if err != nil {
		return nil, err
	}
	p.flushICE()
	if prepare != nil {
		prepare()
	}
	// A client that lost a requirement can be asked again, because every answer starts the count over.
	p.mu.Lock()
	p.requestedAudio, p.requestedVideo = 0, 0
	p.mu.Unlock()
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return nil, err
	}
	return &pb.SessionDescription{Type: answer.Type.String(), Sdp: stableExtmaps(answer.SDP), Id: offer.GetId(), MidToTrackId: p.midToTrack()}, nil
}

// AnswerSent asks for any sections that the answer just sent could not cover. It does nothing on a connection that makes
// its own offers.
func (p *Peer) AnswerSent() {
	if p.cb.Requirement != nil {
		p.sendRequirement(false)
	}
}

// unmatched counts tracks waiting for an m-section: added, but with nowhere to go until the client offers one.
func (p *Peer) unmatched() (audio, video uint32) {
	for _, t := range p.pc.GetTransceivers() {
		if t.Mid() != "" {
			continue
		}
		if d := t.Direction(); d != webrtc.RTPTransceiverDirectionSendonly && d != webrtc.RTPTransceiverDirectionSendrecv {
			continue
		}
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			audio++
		} else {
			video++
		}
	}
	return audio, video
}

func (p *Peer) sendRequirement(force bool) {
	audio, video := p.unmatched()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	var newAudio, newVideo uint32
	if audio > p.requestedAudio {
		newAudio = audio - p.requestedAudio
	}
	if video > p.requestedVideo {
		newVideo = video - p.requestedVideo
	}
	p.requestedAudio += newAudio
	p.requestedVideo += newVideo
	p.mu.Unlock()
	if force || newAudio+newVideo != 0 {
		p.cb.Requirement(newAudio, newVideo)
	}
}

// PreferCodec puts one codec first in the answer for a transceiver, and with exclusive removes the others. A browser
// sends whichever codec the answer lists first, so this is what decides what it publishes. It reports whether the
// offer carries the codec at all.
func PreferCodec(tr *webrtc.RTPTransceiver, mime string, exclusive bool) (bool, error) {
	receiver := tr.Receiver()
	if receiver == nil {
		return false, nil
	}
	var first, rest []webrtc.RTPCodecParameters
	for _, c := range receiver.GetParameters().Codecs {
		if strings.EqualFold(c.MimeType, mime) {
			first = append(first, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(first) == 0 {
		return false, nil
	}
	if exclusive {
		// Only retransmission of the kept codec. Pion drops any RTX whose codec is no longer listed.
		for _, c := range rest {
			if strings.EqualFold(c.MimeType, webrtc.MimeTypeRTX) {
				first = append(first, c)
			}
		}
		return true, tr.SetCodecPreferences(first)
	}
	return true, tr.SetCodecPreferences(append(first, rest...))
}

// HandleAnswer applies the remote answer to an offer from this side. An answer to an older offer, or with no
// offer outstanding, is ignored: resuming clients can repeat an answer already applied.
func (p *Peer) HandleAnswer(sd *pb.SessionDescription) error {
	p.mu.Lock()
	if id := sd.GetId(); !p.awaitingReply || (id != 0 && id != p.offerID) {
		p.mu.Unlock()
		p.log.Debug("ignoring answer to an old offer", "id", id, "current", p.offerID)
		return nil
	}
	p.mu.Unlock()
	err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sd.GetSdp()})
	if err != nil {
		return err
	}
	p.flushICE()

	p.mu.Lock()
	p.awaitingReply = false
	again := p.wantOffer
	p.wantOffer = false
	p.mu.Unlock()
	if again {
		p.Negotiate(false)
	}
	return nil
}

// stableExtmaps puts each section's header extension lines in order of ID. Pion writes them in the order of a map, so each
// description it makes can list them differently. Chrome compares the list as it stands, and when it differs it recreates
// its video send streams, whatever the reason for the negotiation. That silences the top layer of a share for about three
// seconds, seen here after a signalling connection dropped and an ICE restart renegotiated with nothing else changed. This
// is applied to what goes to the browser, because Pion refuses a description that differs from the one it made.
func stableExtmaps(sdp string) string {
	lines := strings.Split(sdp, "\r\n")
	id := func(line string) int {
		n, _ := strconv.Atoi(strings.TrimPrefix(strings.Fields(line)[0], "a=extmap:"))
		return n
	}
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "a=extmap:") {
			continue
		}
		end := i
		for end < len(lines) && strings.HasPrefix(lines[end], "a=extmap:") {
			end++
		}
		slices.SortStableFunc(lines[i:end], func(a, b string) int { return id(a) - id(b) })
		i = end
	}
	return strings.Join(lines, "\r\n")
}

// Negotiate schedules an offer. Calls made before the previous offer is answered merge into one more offer,
// sent once that answer arrives. A requested ICE restart waits too: Pion accepts neither a second offer nor a
// rollback from have-local-offer, so the answer to the outstanding offer is the only way back to a state that
// can make a new one. See ResendPendingOffer for when that answer may never come on its own.
func (p *Peer) Negotiate(restartICE bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if p.cb.Requirement != nil {
		// Only the client offers here, so ask it to, gathering changes as an offer would.
		if p.timer == nil {
			p.timer = time.AfterFunc(negotiateDelay, func() {
				p.mu.Lock()
				p.timer = nil
				p.mu.Unlock()
				p.sendRequirement(true)
			})
		}
		return
	}
	if restartICE {
		p.restartICE = true
	}
	if p.awaitingReply {
		p.wantOffer = true
		return
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(negotiateDelay, p.sendOffer)
	}
}

// ResendPendingOffer repeats an unanswered offer. A client that lost its signalling connection may never have
// seen the offer, or its answer may have been lost. Either way, the same offer gets a fresh answer out of it,
// which unblocks whatever is waiting behind it. This uses the current local description, because candidates
// trickled on the old connection may have been lost with it.
func (p *Peer) ResendPendingOffer() {
	p.mu.Lock()
	pending := p.awaitingReply && !p.closed
	id := p.offerID
	p.mu.Unlock()
	if !pending || p.cb.Offer == nil {
		return
	}
	local := p.pc.LocalDescription()
	if local == nil || local.Type != webrtc.SDPTypeOffer {
		return
	}
	p.cb.Offer(&pb.SessionDescription{Type: local.Type.String(), Sdp: stableExtmaps(local.SDP), Id: id, MidToTrackId: p.midToTrack()})
}

func (p *Peer) sendOffer() {
	p.mu.Lock()
	p.timer = nil
	if p.closed || p.awaitingReply {
		p.mu.Unlock()
		return
	}
	restart := p.restartICE
	p.restartICE = false
	p.wantOffer = false // this offer carries every change made so far
	p.awaitingReply = true
	p.offerID++
	id := p.offerID
	p.mu.Unlock()

	var opts *webrtc.OfferOptions
	if restart {
		opts = &webrtc.OfferOptions{ICERestart: true}
	}
	offer, err := p.pc.CreateOffer(opts)
	if err == nil {
		err = p.pc.SetLocalDescription(offer)
	}
	if err != nil {
		p.retryOffer(err, restart)
		return
	}
	p.mu.Lock()
	p.offerFailures = 0
	p.mu.Unlock()
	if p.cb.Offer != nil {
		p.cb.Offer(&pb.SessionDescription{
			Type:         offer.Type.String(),
			Sdp:          stableExtmaps(offer.SDP),
			Id:           id,
			MidToTrackId: p.midToTrack(),
		})
	}
}

// retryOffer tries again later. Giving up would strand whatever the offer was carrying: a subscriber would
// stay connected and silently never receive the track.
func (p *Peer) retryOffer(cause error, restart bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.awaitingReply = false
	p.restartICE = p.restartICE || restart
	if p.closed {
		return
	}
	p.offerFailures++
	delay := min(offerRetryBase<<min(p.offerFailures-1, 6), offerRetryMax)
	p.log.Warn("could not create offer, will retry", "error", cause, "target", p.target.String(), "attempt", p.offerFailures, "delay", delay.String())
	if p.timer == nil {
		p.timer = time.AfterFunc(delay, p.sendOffer)
	}
}

func (p *Peer) midToTrack() map[string]string {
	out := map[string]string{}
	for _, t := range p.pc.GetTransceivers() {
		if s := t.Sender(); s != nil && s.Track() != nil && t.Mid() != "" {
			out[t.Mid()] = s.Track().ID()
		}
	}
	return out
}

// CurrentOfferID returns the last offer's ID, for tests and diagnostics.
func (p *Peer) CurrentOfferID() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.offerID
}

// Close shuts the connection down. It is safe to call more than once.
func (p *Peer) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
	metrics.PeerConns.Dec()
	if err := p.pc.Close(); err != nil {
		p.log.Debug("error closing peer connection", "error", err)
	}
}

// CloneSD copies a session description.
func CloneSD(sd *pb.SessionDescription) *pb.SessionDescription {
	return proto.Clone(sd).(*pb.SessionDescription)
}
