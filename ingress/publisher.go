package ingress

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
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

const (
	signalWriteTimeout = 10 * time.Second
	signalPingInterval = 5 * time.Second
	publishTimeout     = 20 * time.Second
)

// publisher is a participant of kind INGRESS: an ordinary client, in that it joins through signalling and connects with
// WebRTC. That gives all of the room's behaviour without the SFU knowing anything about ingest.
type publisher struct {
	log      *slog.Logger
	wsURL    string
	token    string
	loopback bool

	ws   *websocket.Conn
	wsMu sync.Mutex
	pc   *webrtc.PeerConnection

	mu        sync.Mutex
	joined    chan struct{}
	joinOnce  sync.Once
	tracks    map[string]*webrtc.TrackLocalStaticRTP
	answering chan struct{}
	pending   []string
	connected chan struct{}
	connOnce  sync.Once

	done     chan struct{}
	doneOnce sync.Once
	endErr   error
}

func newPublisher(log *slog.Logger, wsURL, token string, loopback bool) *publisher {
	return &publisher{
		log: log, wsURL: wsURL, token: token, loopback: loopback,
		joined: make(chan struct{}), connected: make(chan struct{}), done: make(chan struct{}),
		tracks: map[string]*webrtc.TrackLocalStaticRTP{},
	}
}

func newPublisherPC(loopback bool) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(loopback)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	me := &webrtc.MediaEngine{}
	// Only what the transcoder produces, so these are what gets negotiated.
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}},
		},
		PayloadType: 125,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(me, reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg)).
		NewPeerConnection(webrtc.Configuration{SDPSemantics: webrtc.SDPSemanticsUnifiedPlan})
}

// start joins the room. It returns once the server has answered the join, so a refusal shows up here.
func (p *publisher) start(ctx context.Context) error {
	u, err := url.Parse(strings.TrimRight(p.wsURL, "/") + "/rtc")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("access_token", p.token)
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
		return fmt.Errorf("the server refused the connection: HTTP %d", res.StatusCode)
	}
	p.ws = ws
	if p.pc, err = newPublisherPC(p.loopback); err != nil {
		_ = ws.Close()
		return err
	}
	p.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, err := json.Marshal(c.ToJSON())
		if err != nil {
			return
		}
		p.send(&pb.SignalRequest{Message: &pb.SignalRequest_Trickle{Trickle: &pb.TrickleRequest{CandidateInit: string(data), Target: pb.SignalTarget_PUBLISHER}}})
	})
	p.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			p.connOnce.Do(func() { close(p.connected) })
		case webrtc.PeerConnectionStateFailed:
			p.end(errors.New("the media connection to the server failed"))
		}
	})
	go p.pingLoop()
	go p.readLoop()
	select {
	case <-p.joined:
		return nil
	case <-p.done:
		return p.endErr
	case <-ctx.Done():
		p.close()
		return ctx.Err()
	case <-time.After(publishTimeout):
		p.close()
		return errors.New("the server did not answer the join")
	}
}

func (p *publisher) send(req *pb.SignalRequest) {
	data, err := proto.Marshal(req)
	if err != nil {
		return
	}
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	_ = p.ws.SetWriteDeadline(time.Now().Add(signalWriteTimeout))
	if err := p.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		p.log.Debug("could not send a signal message", "error", err)
	}
}

func (p *publisher) pingLoop() {
	t := time.NewTicker(signalPingInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			p.send(&pb.SignalRequest{Message: &pb.SignalRequest_PingReq{PingReq: &pb.Ping{Timestamp: time.Now().UnixMilli()}}})
		}
	}
}

func (p *publisher) end(err error) {
	p.doneOnce.Do(func() {
		p.endErr = err
		close(p.done)
		if p.pc != nil {
			_ = p.pc.Close()
		}
		if p.ws != nil {
			_ = p.ws.Close()
		}
	})
}

// close leaves the room, telling the server, so others see the departure at once.
func (p *publisher) close() {
	if p.ws != nil {
		p.send(&pb.SignalRequest{Message: &pb.SignalRequest_Leave{Leave: &pb.LeaveRequest{Reason: pb.DisconnectReason_CLIENT_INITIATED}}})
	}
	p.end(nil)
}

func (p *publisher) readLoop() {
	for {
		_, data, err := p.ws.ReadMessage()
		if err != nil {
			p.end(errors.New("lost the connection to the server"))
			return
		}
		var res pb.SignalResponse
		if proto.Unmarshal(data, &res) != nil {
			continue
		}
		switch m := res.GetMessage().(type) {
		case *pb.SignalResponse_Join:
			p.joinOnce.Do(func() { close(p.joined) })
		case *pb.SignalResponse_Answer:
			if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Answer.GetSdp()}); err != nil {
				p.end(fmt.Errorf("the server's answer could not be used: %w", err))
				return
			}
			p.negotiated()
		case *pb.SignalResponse_Trickle:
			var c webrtc.ICECandidateInit
			if json.Unmarshal([]byte(m.Trickle.GetCandidateInit()), &c) == nil && m.Trickle.GetTarget() == pb.SignalTarget_PUBLISHER {
				_ = p.pc.AddICECandidate(c)
			}
		case *pb.SignalResponse_Leave:
			p.end(fmt.Errorf("the server ended the session: %s", m.Leave.GetReason()))
			return
		case *pb.SignalResponse_RequestResponse:
			if m.RequestResponse.GetReason() != pb.RequestResponse_OK {
				p.end(fmt.Errorf("the server refused a request: %s", m.RequestResponse.GetMessage()))
				return
			}
		}
	}
}

// publish adds a track. Tracks are added as their first packet arrives, since until then nobody knows whether a source
// has audio, video, or both. Only one offer may be open at a time, so one that is not answered yet makes this wait.
func (p *publisher) publish(kind string, width, height int) (*webrtc.TrackLocalStaticRTP, error) {
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	req := &pb.AddTrackRequest{Cid: "ingress-" + kind, Type: pb.TrackType_AUDIO, Source: pb.TrackSource_MICROPHONE, Name: "microphone", Stream: "ingress"}
	if kind == "video" {
		capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}
		req.Type, req.Source, req.Name = pb.TrackType_VIDEO, pb.TrackSource_CAMERA, "camera"
		req.Width, req.Height = uint32(width), uint32(height)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(capability, req.Cid, "ingress")
	if err != nil {
		return nil, err
	}
	sender, err := p.pc.AddTrack(track)
	if err != nil {
		return nil, err
	}
	go func() {
		// RTCP has to be read for NACK responses to work. A PLI gets no special answer: the transcoder cannot be told to
		// make a keyframe, so subscribers wait for the next one, at most two seconds away.
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()
	p.send(&pb.SignalRequest{Message: &pb.SignalRequest_AddTrack{AddTrack: req}})
	p.mu.Lock()
	p.tracks[kind] = track
	p.mu.Unlock()
	if err := p.negotiate(); err != nil {
		return nil, err
	}
	return track, nil
}

func (p *publisher) negotiate() error {
	p.mu.Lock()
	if p.answering != nil {
		// An offer is out. The answer triggers another, which will include this track.
		p.pending = append(p.pending, "again")
		p.mu.Unlock()
		return nil
	}
	p.answering = make(chan struct{})
	p.mu.Unlock()
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	p.send(&pb.SignalRequest{Message: &pb.SignalRequest_Offer{Offer: &pb.SessionDescription{Type: "offer", Sdp: offer.SDP, Id: 1}}})
	return nil
}

func (p *publisher) negotiated() {
	p.mu.Lock()
	if p.answering != nil {
		close(p.answering)
		p.answering = nil
	}
	again := len(p.pending) > 0
	p.pending = nil
	p.mu.Unlock()
	if again {
		if err := p.negotiate(); err != nil {
			p.end(fmt.Errorf("could not renegotiate: %w", err))
		}
	}
}
