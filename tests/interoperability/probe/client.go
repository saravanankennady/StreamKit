package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
)

// client is the smallest possible participant: a signalling connection and the two peer connections, with nothing
// hidden from the caller. It reports itself as a JS client, because the reference sends padding to Go ones at the start.
type client struct {
	identity string
	ws       *websocket.Conn
	wsMu     sync.Mutex
	pub      *webrtc.PeerConnection
	sub      *webrtc.PeerConnection

	// joined closes when the join response has arrived.
	joined   chan struct{}
	joinOnce sync.Once
	join     *pb.JoinResponse

	published chan *pb.TrackInfo
	updates   chan []*pb.ParticipantInfo
	closed    chan struct{}
	closeOnce sync.Once
}

type clientOptions struct {
	url, key, secret string
	room, identity   string
	media            func() (*webrtc.MediaEngine, error)
}

func newPeerConnection(media func() (*webrtc.MediaEngine, error), withReports bool) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	me, err := media()
	if err != nil {
		return nil, err
	}
	// The publisher side has no interceptors: the probe writes its own sender reports, and they must be the only ones.
	reg := &interceptor.Registry{}
	if withReports {
		receiver, err := report.NewReceiverInterceptor()
		if err != nil {
			return nil, err
		}
		reg.Add(receiver)
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg)).NewPeerConnection(webrtc.Configuration{})
}

func dial(ctx context.Context, o clientOptions) (*client, error) {
	yes := true
	token, err := auth.Mint(auth.Key{APIKey: o.key, Secret: o.secret}, &auth.Grants{
		Identity: o.identity,
		Video:    &auth.VideoGrant{RoomJoin: true, Room: o.room, CanPublish: &yes, CanSubscribe: &yes},
	}, time.Hour)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(o.url + "/rtc")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("access_token", token)
	q.Set("auto_subscribe", "1")
	q.Set("protocol", "16")
	q.Set("sdk", "js")
	u.RawQuery = q.Encode()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", o.identity, err)
	}
	c := &client{
		identity:  o.identity,
		ws:        ws,
		joined:    make(chan struct{}),
		published: make(chan *pb.TrackInfo, 16),
		updates:   make(chan []*pb.ParticipantInfo, 256),
		closed:    make(chan struct{}),
	}
	if c.pub, err = newPeerConnection(o.media, false); err != nil {
		return nil, err
	}
	if c.sub, err = newPeerConnection(o.media, true); err != nil {
		return nil, err
	}
	trickle := func(pc *webrtc.PeerConnection, target pb.SignalTarget) {
		pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
			if ic == nil {
				return
			}
			data, _ := json.Marshal(ic.ToJSON())
			c.send(&pb.SignalRequest{Message: &pb.SignalRequest_Trickle{Trickle: &pb.TrickleRequest{CandidateInit: string(data), Target: target}}})
		})
	}
	trickle(c.pub, pb.SignalTarget_PUBLISHER)
	trickle(c.sub, pb.SignalTarget_SUBSCRIBER)
	go c.pingLoop()
	go c.readLoop()
	return c, nil
}

func (c *client) send(req *pb.SignalRequest) {
	data, err := proto.Marshal(req)
	if err != nil {
		return
	}
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.ws.WriteMessage(websocket.BinaryMessage, data)
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.send(&pb.SignalRequest{Message: &pb.SignalRequest_Leave{Leave: &pb.LeaveRequest{}}})
		_ = c.ws.Close()
		_ = c.pub.Close()
		_ = c.sub.Close()
	})
}

func (c *client) pingLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			c.send(&pb.SignalRequest{Message: &pb.SignalRequest_PingReq{PingReq: &pb.Ping{Timestamp: time.Now().UnixMilli()}}})
		}
	}
}

func (c *client) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var res pb.SignalResponse
		if proto.Unmarshal(data, &res) != nil {
			continue
		}
		switch m := res.GetMessage().(type) {
		case *pb.SignalResponse_Join:
			c.join = m.Join
			c.joinOnce.Do(func() { close(c.joined) })
		case *pb.SignalResponse_Offer:
			c.answer(m.Offer)
		case *pb.SignalResponse_Answer:
			_ = c.pub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Answer.GetSdp()})
		case *pb.SignalResponse_Trickle:
			var init webrtc.ICECandidateInit
			if json.Unmarshal([]byte(m.Trickle.GetCandidateInit()), &init) != nil {
				continue
			}
			pc := c.pub
			if m.Trickle.GetTarget() == pb.SignalTarget_SUBSCRIBER {
				pc = c.sub
			}
			_ = pc.AddICECandidate(init)
		case *pb.SignalResponse_TrackPublished:
			select {
			case c.published <- m.TrackPublished.GetTrack():
			default:
			}
		case *pb.SignalResponse_Update:
			select {
			case c.updates <- m.Update.GetParticipants():
			default:
			}
		}
	}
}

func (c *client) answer(offer *pb.SessionDescription) {
	if err := c.sub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()}); err != nil {
		return
	}
	answer, err := c.sub.CreateAnswer(nil)
	if err != nil {
		return
	}
	if err := c.sub.SetLocalDescription(answer); err != nil {
		return
	}
	c.send(&pb.SignalRequest{Message: &pb.SignalRequest_Answer{Answer: &pb.SessionDescription{Type: "answer", Sdp: answer.SDP, Id: offer.GetId()}}})
}

// sendOffer offers what the publisher connection now holds.
func (c *client) sendOffer() error {
	offer, err := c.pub.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := c.pub.SetLocalDescription(offer); err != nil {
		return err
	}
	c.send(&pb.SignalRequest{Message: &pb.SignalRequest_Offer{Offer: &pb.SessionDescription{Type: "offer", Sdp: offer.SDP, Id: 1}}})
	return nil
}
