package sip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

const (
	signalWriteTimeout = 10 * time.Second
	signalPingInterval = 5 * time.Second
	joinTimeout        = 20 * time.Second
	publisherChannel   = "_reliable"
	lossyChannel       = "_lossy"
	// maxRPCPayload bounds what a call answers an RPC with. Headers are limited elsewhere, so this is a backstop.
	maxRPCPayload = 15 * 1024
	dataQueue     = 256
)

// RPC methods a SIP participant answers. Agent frameworks call these by name.
const (
	rpcGetHeaders = "lk.sip.GetRemoteHeaders"
	rpcEndCall    = "lk.sip.EndCall"
)

// roomEvents is what a room client tells its call.
type roomEvents struct {
	// Track is a remote audio track, once there is something to read from it.
	Track func(identity string, track *webrtc.TrackRemote)
	// OtherSIP means another SIP participant is in the room.
	OtherSIP func()
	// Digit is a DTMF digit someone in the room sent, to go to the phone.
	Digit func(digit string, code uint32)
	// RPC is a method call from someone in the room. Its answer is a JSON payload or an error message.
	RPC func(method, payload string) (string, string, uint32)
	// Ended means the room connection is over. It runs once, and not when this side closed it.
	Ended func(reason pb.DisconnectReason, err error)
}

// roomClient is a SIP participant's connection to a room. It is an ordinary client: it joins through signalling and connects with
// WebRTC, so a room and its other participants need nothing special for a phone call, and the SFU knows nothing about SIP.
type roomClient struct {
	log      *slog.Logger
	wsURL    string
	token    string
	loopback bool
	events   roomEvents

	ws   *websocket.Conn
	wsMu sync.Mutex
	pub  *webrtc.PeerConnection
	sub  *webrtc.PeerConnection

	track    *webrtc.TrackLocalStaticSample
	reliable *webrtc.DataChannel

	mu            sync.Mutex
	joined        chan struct{}
	joinOnce      sync.Once
	sid           string
	identity      string
	roomSID       string
	attributes    map[string]string
	subscribed    map[string]bool
	remoteCands   map[pb.SignalTarget][]webrtc.ICECandidateInit
	remoteSet     map[pb.SignalTarget]bool
	dataOpen      chan struct{}
	dataOpenOnce  sync.Once
	connected     chan struct{}
	connectedOnce sync.Once

	outgoing chan *pb.DataPacket

	done     chan struct{}
	doneOnce sync.Once
	closing  bool
	workers  sync.WaitGroup
	endErr   error
}

func newRoomClient(log *slog.Logger, wsURL, token string, loopback bool, events roomEvents) *roomClient {
	return &roomClient{
		log: log, wsURL: wsURL, token: token, loopback: loopback, events: events,
		joined: make(chan struct{}), done: make(chan struct{}), dataOpen: make(chan struct{}), connected: make(chan struct{}), outgoing: make(chan *pb.DataPacket, dataQueue),
		attributes: map[string]string{}, subscribed: map[string]bool{},
		remoteCands: map[pb.SignalTarget][]webrtc.ICECandidateInit{}, remoteSet: map[pb.SignalTarget]bool{},
	}
}

func newRoomPC(loopback bool) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(loopback)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	me := &webrtc.MediaEngine{}
	// Opus only. A publisher sending RED is reduced to plain Opus for this subscriber by the server.
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{SDPSemantics: webrtc.SDPSemanticsUnifiedPlan})
}

// join connects and joins the room, and returns once the server has answered. Publishing has started by then, though its media
// connection may still be coming up.
func (r *roomClient) join(ctx context.Context) error {
	u, err := url.Parse(strings.TrimRight(r.wsURL, "/") + "/rtc")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("access_token", r.token)
	q.Set("auto_subscribe", "0")
	q.Set("protocol", "16")
	q.Set("sdk", "go")
	u.RawQuery = q.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, NetDialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	ws, res, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if res != nil {
			return fmt.Errorf("the server refused the connection: HTTP %d", res.StatusCode)
		}
		return fmt.Errorf("could not reach the server: %w", err)
	}
	if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
		_ = ws.Close()
		return fmt.Errorf("the server refused the connection: HTTP %d", res.StatusCode)
	}
	r.wsMu.Lock()
	r.ws = ws
	r.wsMu.Unlock()
	if r.pub, err = newRoomPC(r.loopback); err != nil {
		_ = ws.Close()
		return err
	}
	if r.sub, err = newRoomPC(r.loopback); err != nil {
		_ = r.pub.Close()
		_ = ws.Close()
		return err
	}
	r.setupPC(r.pub, pb.SignalTarget_PUBLISHER)
	r.setupPC(r.sub, pb.SignalTarget_SUBSCRIBER)
	r.sub.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio || r.events.Track == nil {
			return
		}
		// A track's stream ID is the participant's SID, followed by the track's own ID, in this protocol.
		identity := track.StreamID()
		if i := strings.IndexByte(identity, '|'); i >= 0 {
			identity = identity[:i]
		}
		r.workers.Add(1)
		go func() {
			defer r.workers.Done()
			r.events.Track(identity, track)
		}()
	})
	r.sub.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnMessage(func(m webrtc.DataChannelMessage) { r.onData(m.Data) })
	})
	ordered := true
	r.reliable, err = r.pub.CreateDataChannel(publisherChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		r.end(err)
		return err
	}
	r.reliable.OnOpen(func() { r.dataOpenOnce.Do(func() { close(r.dataOpen) }) })
	unordered := false
	noRetransmit := uint16(0)
	if _, err := r.pub.CreateDataChannel(lossyChannel, &webrtc.DataChannelInit{Ordered: &unordered, MaxRetransmits: &noRetransmit}); err != nil {
		r.end(err)
		return err
	}
	r.workers.Add(3)
	go r.pingLoop()
	go r.readLoop()
	go r.dataLoop()
	timeout := time.NewTimer(joinTimeout)
	defer timeout.Stop()
	select {
	case <-r.joined:
	case <-r.done:
		return r.endErr
	case <-ctx.Done():
		r.close()
		return ctx.Err()
	case <-timeout.C:
		r.close()
		return errors.New("the server did not answer the join")
	}
	return r.publish()
}

func (r *roomClient) setupPC(pc *webrtc.PeerConnection, target pb.SignalTarget) {
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, err := json.Marshal(c.ToJSON())
		if err != nil {
			return
		}
		r.send(&pb.SignalRequest{Message: &pb.SignalRequest_Trickle{Trickle: &pb.TrickleRequest{CandidateInit: string(data), Target: target}}})
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			if target == pb.SignalTarget_PUBLISHER {
				r.connectedOnce.Do(func() { close(r.connected) })
			}
		case webrtc.PeerConnectionStateFailed:
			r.finish(pb.DisconnectReason_STATE_MISMATCH, errors.New("the media connection to the server failed"))
		}
	})
}

// publish adds the track the phone's audio goes out on, named after the participant the way other clients expect.
func (r *roomClient) publish() error {
	r.mu.Lock()
	identity := r.identity
	r.mu.Unlock()
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "sip-audio", identity)
	if err != nil {
		return err
	}
	sender, err := r.pub.AddTrack(track)
	if err != nil {
		return err
	}
	r.track = track
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()
	r.send(&pb.SignalRequest{Message: &pb.SignalRequest_AddTrack{AddTrack: &pb.AddTrackRequest{
		Cid: "sip-audio", Type: pb.TrackType_AUDIO, Source: pb.TrackSource_MICROPHONE, Name: identity, Stream: identity,
	}}})
	offer, err := r.pub.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := r.pub.SetLocalDescription(offer); err != nil {
		return err
	}
	r.send(&pb.SignalRequest{Message: &pb.SignalRequest_Offer{Offer: &pb.SessionDescription{Type: "offer", Sdp: offer.SDP, Id: 1}}})
	return nil
}

// WriteOpus sends a frame of the phone's audio into the room.
func (r *roomClient) WriteOpus(packet []byte, duration time.Duration) error {
	if r.track == nil {
		return nil
	}
	return r.track.WriteSample(media.Sample{Data: packet, Duration: duration})
}

func (r *roomClient) send(req *pb.SignalRequest) {
	data, err := proto.Marshal(req)
	if err != nil {
		return
	}
	r.wsMu.Lock()
	defer r.wsMu.Unlock()
	// Someone can hang up a call whose connection is not up yet.
	if r.ws == nil {
		return
	}
	_ = r.ws.SetWriteDeadline(time.Now().Add(signalWriteTimeout))
	if err := r.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		r.log.Debug("could not send a signal message", "error", err)
	}
}

func (r *roomClient) pingLoop() {
	defer r.workers.Done()
	t := time.NewTicker(signalPingInterval)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.send(&pb.SignalRequest{Message: &pb.SignalRequest_PingReq{PingReq: &pb.Ping{Timestamp: time.Now().UnixMilli()}}})
		}
	}
}

// SetAttributes changes attributes on this participant. Only the ones given change. An empty value removes one.
func (r *roomClient) SetAttributes(attrs map[string]string) {
	r.mu.Lock()
	for k, v := range attrs {
		if v == "" {
			delete(r.attributes, k)
		} else {
			r.attributes[k] = v
		}
	}
	r.mu.Unlock()
	r.send(&pb.SignalRequest{Message: &pb.SignalRequest_UpdateMetadata{UpdateMetadata: &pb.UpdateParticipantMetadata{Attributes: attrs}}})
}

// Attributes returns this participant's attributes as last seen.
func (r *roomClient) Attributes() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.attributes))
	for k, v := range r.attributes {
		out[k] = v
	}
	return out
}

// IDs returns the participant's SID and the room's, once joined.
func (r *roomClient) IDs() (participantSID, roomSID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sid, r.roomSID
}

// SendDigit tells the room about a digit the phone pressed.
func (r *roomClient) SendDigit(digit string, code uint32) {
	r.mu.Lock()
	identity := r.identity
	r.mu.Unlock()
	r.sendData(&pb.DataPacket{Kind: pb.DataPacket_RELIABLE, ParticipantIdentity: identity, Value: &pb.DataPacket_SipDtmf{SipDtmf: &pb.SipDTMF{Code: code, Digit: digit}}})
}

// sendData queues a packet. It never blocks, because digits come from the thread reading RTP. A queue that is full means the data
// channel has been down for a long time, and the packet is dropped.
func (r *roomClient) sendData(dp *pb.DataPacket) {
	select {
	case r.outgoing <- dp:
	default:
		r.log.Warn("dropped a data packet because the queue is full")
	}
}

func (r *roomClient) dataLoop() {
	defer r.workers.Done()
	select {
	case <-r.dataOpen:
	case <-r.done:
		return
	}
	for {
		select {
		case <-r.done:
			return
		case dp := <-r.outgoing:
			data, err := proto.Marshal(dp)
			if err != nil {
				continue
			}
			if err := r.reliable.Send(data); err != nil {
				r.log.Debug("could not send a data packet", "error", err)
			}
		}
	}
}

func (r *roomClient) onData(data []byte) {
	var dp pb.DataPacket
	if err := proto.Unmarshal(data, &dp); err != nil {
		return
	}
	switch v := dp.GetValue().(type) {
	case *pb.DataPacket_SipDtmf:
		if r.events.Digit != nil {
			r.events.Digit(v.SipDtmf.GetDigit(), v.SipDtmf.GetCode())
		}
	case *pb.DataPacket_RpcRequest:
		go r.answerRPC(dp.GetParticipantIdentity(), v.RpcRequest)
	}
}

// answerRPC acknowledges a request and then answers it, as the protocol has it. Only the methods a phone call has exist.
func (r *roomClient) answerRPC(from string, req *pb.RpcRequest) {
	r.mu.Lock()
	identity := r.identity
	r.mu.Unlock()
	to := []string{from}
	r.sendData(&pb.DataPacket{Kind: pb.DataPacket_RELIABLE, ParticipantIdentity: identity, DestinationIdentities: to,
		Value: &pb.DataPacket_RpcAck{RpcAck: &pb.RpcAck{RequestId: req.GetId()}}})
	res := &pb.RpcResponse{RequestId: req.GetId()}
	if r.events.RPC == nil {
		res.Value = &pb.RpcResponse_Error{Error: &pb.RpcError{Code: 1400, Message: "Method not supported at destination"}}
	} else {
		payload, message, code := r.events.RPC(req.GetMethod(), req.GetPayload())
		switch {
		case code != 0:
			res.Value = &pb.RpcResponse_Error{Error: &pb.RpcError{Code: code, Message: message}}
		case len(payload) > maxRPCPayload:
			res.Value = &pb.RpcResponse_Error{Error: &pb.RpcError{Code: 1402, Message: "Response payload too large"}}
		default:
			res.Value = &pb.RpcResponse_Payload{Payload: payload}
		}
	}
	r.sendData(&pb.DataPacket{Kind: pb.DataPacket_RELIABLE, ParticipantIdentity: identity, DestinationIdentities: to, Value: &pb.DataPacket_RpcResponse{RpcResponse: res}})
}

func (r *roomClient) readLoop() {
	defer r.workers.Done()
	for {
		_, data, err := r.ws.ReadMessage()
		if err != nil {
			r.finish(pb.DisconnectReason_UNKNOWN_REASON, errors.New("lost the connection to the server"))
			return
		}
		var res pb.SignalResponse
		if proto.Unmarshal(data, &res) != nil {
			continue
		}
		switch m := res.GetMessage().(type) {
		case *pb.SignalResponse_Join:
			r.onJoin(m.Join)
		case *pb.SignalResponse_Answer:
			if err := r.pub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Answer.GetSdp()}); err != nil {
				r.finish(pb.DisconnectReason_STATE_MISMATCH, fmt.Errorf("the server's answer could not be used: %w", err))
				return
			}
			r.flushCandidates(pb.SignalTarget_PUBLISHER)
		case *pb.SignalResponse_Offer:
			r.onOffer(m.Offer)
		case *pb.SignalResponse_Trickle:
			var c webrtc.ICECandidateInit
			if json.Unmarshal([]byte(m.Trickle.GetCandidateInit()), &c) == nil {
				r.addCandidate(m.Trickle.GetTarget(), c)
			}
		case *pb.SignalResponse_Update:
			r.onParticipants(m.Update.GetParticipants())
		case *pb.SignalResponse_Leave:
			r.finish(m.Leave.GetReason(), nil)
			return
		case *pb.SignalResponse_RequestResponse:
			if m.RequestResponse.GetReason() != pb.RequestResponse_OK {
				r.log.Warn("the server refused a request", "reason", m.RequestResponse.GetReason().String(), "message", m.RequestResponse.GetMessage())
			}
		}
	}
}

func (r *roomClient) onJoin(j *pb.JoinResponse) {
	r.mu.Lock()
	r.sid = j.GetParticipant().GetSid()
	r.identity = j.GetParticipant().GetIdentity()
	r.roomSID = j.GetRoom().GetSid()
	for k, v := range j.GetParticipant().GetAttributes() {
		r.attributes[k] = v
	}
	r.mu.Unlock()
	r.joinOnce.Do(func() { close(r.joined) })
	r.onParticipants(j.GetOtherParticipants())
}

// onParticipants subscribes to every audio track in the room as it appears, and to nothing else, and keeps this participant's own
// attributes up to date, because someone else may change them.
func (r *roomClient) onParticipants(list []*pb.ParticipantInfo) {
	var want []string
	otherSIP := false
	r.mu.Lock()
	for _, p := range list {
		if p.GetSid() == r.sid {
			r.attributes = make(map[string]string, len(p.GetAttributes()))
			for k, v := range p.GetAttributes() {
				r.attributes[k] = v
			}
			continue
		}
		if p.GetState() == pb.ParticipantInfo_DISCONNECTED {
			continue
		}
		if p.GetKind() == pb.ParticipantInfo_SIP {
			otherSIP = true
		}
		for _, t := range p.GetTracks() {
			if t.GetType() == pb.TrackType_AUDIO && !r.subscribed[t.GetSid()] {
				r.subscribed[t.GetSid()] = true
				want = append(want, t.GetSid())
			}
		}
	}
	r.mu.Unlock()
	if len(want) > 0 {
		r.send(&pb.SignalRequest{Message: &pb.SignalRequest_Subscription{Subscription: &pb.UpdateSubscription{TrackSids: want, Subscribe: true}}})
	}
	if otherSIP && r.events.OtherSIP != nil {
		r.events.OtherSIP()
	}
}

func (r *roomClient) onOffer(offer *pb.SessionDescription) {
	if err := r.sub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()}); err != nil {
		r.log.Warn("the server's offer could not be used", "error", err)
		return
	}
	r.flushCandidates(pb.SignalTarget_SUBSCRIBER)
	answer, err := r.sub.CreateAnswer(nil)
	if err != nil {
		r.log.Warn("could not answer the server's offer", "error", err)
		return
	}
	if err := r.sub.SetLocalDescription(answer); err != nil {
		r.log.Warn("could not answer the server's offer", "error", err)
		return
	}
	r.send(&pb.SignalRequest{Message: &pb.SignalRequest_Answer{Answer: &pb.SessionDescription{Type: "answer", Sdp: answer.SDP, Id: offer.GetId()}}})
}

func (r *roomClient) pcFor(target pb.SignalTarget) *webrtc.PeerConnection {
	if target == pb.SignalTarget_PUBLISHER {
		return r.pub
	}
	return r.sub
}

// addCandidate holds a candidate that arrives before the description it belongs with.
func (r *roomClient) addCandidate(target pb.SignalTarget, c webrtc.ICECandidateInit) {
	r.mu.Lock()
	if !r.remoteSet[target] {
		r.remoteCands[target] = append(r.remoteCands[target], c)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if err := r.pcFor(target).AddICECandidate(c); err != nil {
		r.log.Debug("could not add a candidate", "error", err)
	}
}

func (r *roomClient) flushCandidates(target pb.SignalTarget) {
	r.mu.Lock()
	held := r.remoteCands[target]
	r.remoteCands[target] = nil
	r.remoteSet[target] = true
	r.mu.Unlock()
	for _, c := range held {
		if err := r.pcFor(target).AddICECandidate(c); err != nil {
			r.log.Debug("could not add a candidate", "error", err)
		}
	}
}

// finish handles the room ending the connection by itself.
func (r *roomClient) finish(reason pb.DisconnectReason, err error) {
	r.mu.Lock()
	closing := r.closing
	r.mu.Unlock()
	r.end(err)
	if !closing && r.events.Ended != nil {
		go r.events.Ended(reason, err)
	}
}

func (r *roomClient) end(err error) {
	r.doneOnce.Do(func() {
		r.endErr = err
		close(r.done)
		if r.pub != nil {
			_ = r.pub.Close()
		}
		if r.sub != nil {
			_ = r.sub.Close()
		}
		r.wsMu.Lock()
		ws := r.ws
		r.wsMu.Unlock()
		if ws != nil {
			_ = ws.Close()
		}
	})
}

// close leaves the room, telling the server, so others see the departure at once. Nothing it does calls the Ended event.
func (r *roomClient) close() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.send(&pb.SignalRequest{Message: &pb.SignalRequest_Leave{Leave: &pb.LeaveRequest{Reason: pb.DisconnectReason_CLIENT_INITIATED}}})
	r.end(nil)
	r.workers.Wait()
}

// readTrack decodes a remote track until it ends. Its packets are passed to handle one at a time.
func readTrack(track *webrtc.TrackRemote, handle func(payload []byte)) {
	buf := make([]byte, 1500)
	var pkt rtp.Packet
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		if pkt.Unmarshal(buf[:n]) != nil {
			continue
		}
		handle(pkt.Payload)
	}
}
