// Package loadgen holds a real WebRTC participant and the load generator built on it. Publishers send pre-encoded VP8
// and Opus. Subscribers really receive, decrypt and count every packet. The streamkit CLI uses the participant for
// `room join`, and the SDK tests use that command as a real peer.
package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

// Frames is pre-encoded media, sent in a loop at its real rate.
type Frames struct {
	Data [][]byte
	Each time.Duration
}

// LoadIVF reads a VP8 file.
func LoadIVF(path string) (*Frames, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseIVF(f)
}

// ParseIVF reads VP8 frames.
func ParseIVF(r io.Reader) (*Frames, error) {
	reader, header, err := ivfreader.NewWith(r)
	if err != nil {
		return nil, err
	}
	out := &Frames{Each: time.Second * time.Duration(header.TimebaseNumerator) / time.Duration(header.TimebaseDenominator)}
	for {
		frame, _, err := reader.ParseNextFrame()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out.Data = append(out.Data, frame)
	}
}

// LoadOgg reads an Opus file.
func LoadOgg(path string) (*Frames, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseOgg(f)
}

// ParseOgg reads Opus pages. Each page must hold one 20 ms frame.
func ParseOgg(r io.Reader) (*Frames, error) {
	reader, _, err := oggreader.NewWith(r)
	if err != nil {
		return nil, err
	}
	out := &Frames{Each: 20 * time.Millisecond}
	for {
		page, _, err := reader.ParseNextPage()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out.Data = append(out.Data, page)
	}
}

// Stats counts what subscribers really received.
type Stats struct {
	connected, failed, tracks atomic.Int64
	packets, bytes, gaps      atomic.Int64
	mu                        sync.Mutex
	firstMedia                []time.Duration
	connectTimes              []time.Duration
}

func (s *Stats) noteFirstMedia(d time.Duration) {
	s.mu.Lock()
	s.firstMedia = append(s.firstMedia, d)
	s.mu.Unlock()
}

func (s *Stats) noteConnect(d time.Duration) {
	s.mu.Lock()
	s.connectTimes = append(s.connectTimes, d)
	s.mu.Unlock()
}

// Hooks tell a caller what a Client sees. Any of them may be nil. They run on the client's own goroutines, so they
// should not block.
type Hooks struct {
	// Signal sees every message from the server, before the client acts on it.
	Signal func(*pb.SignalResponse)
	// Connected fires once, when the subscriber connection comes up.
	Connected func()
	// Track fires when a remote track arrives.
	Track func(*webrtc.TrackRemote)
	// Packet fires for each RTP packet received.
	Packet func(track *webrtc.TrackRemote, size int)
	// DataChannel fires when a data channel opens. Packets sent before then are lost.
	DataChannel func(label string)
	// Data fires for each data packet received.
	Data func(label string, packet *pb.DataPacket)
}

// Client is one real participant: a signalling WebSocket and two peer connections, as a browser has.
type Client struct {
	URL, Token, Identity string
	Video, Audio         *Frames
	// Publish sends Video and Audio once joined.
	Publish bool
	// NoAutoSubscribe asks the server not to subscribe this client to everything.
	NoAutoSubscribe bool
	Stats           *Stats
	Hooks           Hooks

	start time.Time
	ws    *websocket.Conn
	wsMu  sync.Mutex
	pub   *webrtc.PeerConnection
	sub   *webrtc.PeerConnection
	sent  sync.Once
	done  chan struct{}
}

func (c *Client) send(req *pb.SignalRequest) {
	data, err := proto.Marshal(req)
	if err != nil {
		return
	}
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.ws.WriteMessage(websocket.BinaryMessage, data)
}

func newPC() (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
}

// describeRefusal says why a server refused a WebSocket. The response body is the only place it explains.
func describeRefusal(res *http.Response, err error) string {
	if res == nil {
		return err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	return fmt.Sprintf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
}

// Run joins and stays until ctx ends or the server closes the session.
func (c *Client) Run(ctx context.Context) error {
	c.start = time.Now()
	c.done = make(chan struct{})
	if c.Stats == nil {
		c.Stats = &Stats{}
	}
	u, err := url.Parse(strings.TrimRight(c.URL, "/") + "/rtc")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("access_token", c.Token)
	if c.NoAutoSubscribe {
		q.Set("auto_subscribe", "0")
	} else {
		q.Set("auto_subscribe", "1")
	}
	q.Set("protocol", "16")
	q.Set("sdk", "go")
	u.RawQuery = q.Encode()
	ws, res, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("could not open the signalling connection: %s", describeRefusal(res, err))
	}
	c.ws = ws
	defer c.ws.Close()

	if c.pub, err = newPC(); err != nil {
		return err
	}
	defer c.pub.Close()
	if c.sub, err = newPC(); err != nil {
		return err
	}
	defer c.sub.Close()

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
	c.sub.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			c.sent.Do(func() {
				c.Stats.connected.Add(1)
				c.Stats.noteConnect(time.Since(c.start))
				if c.Hooks.Connected != nil {
					c.Hooks.Connected()
				}
			})
		}
	})
	c.sub.OnTrack(c.receive)
	c.sub.OnDataChannel(c.receiveData)

	go c.pingLoop(ctx)
	return c.readLoop(ctx)
}

func (c *Client) pingLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-t.C:
			c.send(&pb.SignalRequest{Message: &pb.SignalRequest_PingReq{PingReq: &pb.Ping{Timestamp: time.Now().UnixMilli()}}})
		}
	}
}

func (c *Client) receive(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	c.Stats.tracks.Add(1)
	if c.Hooks.Track != nil {
		c.Hooks.Track(track)
	}
	began := time.Now()
	var last uint16
	first := true
	buf := make([]byte, 1500)
	var pkt rtp.Packet
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if first {
			first = false
			c.Stats.noteFirstMedia(time.Since(began))
		} else if gap := pkt.SequenceNumber - last; gap > 1 && gap < 0x8000 {
			c.Stats.gaps.Add(int64(gap - 1))
		}
		last = pkt.SequenceNumber
		c.Stats.packets.Add(1)
		c.Stats.bytes.Add(int64(n))
		if c.Hooks.Packet != nil {
			c.Hooks.Packet(track, n)
		}
	}
}

func (c *Client) receiveData(dc *webrtc.DataChannel) {
	label := dc.Label()
	dc.OnOpen(func() {
		if c.Hooks.DataChannel != nil {
			c.Hooks.DataChannel(label)
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var packet pb.DataPacket
		if proto.Unmarshal(msg.Data, &packet) == nil && c.Hooks.Data != nil {
			c.Hooks.Data(label, &packet)
		}
	})
}

func (c *Client) readLoop(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			// Saying goodbye lets the server close the session now rather than when it times out.
			c.send(&pb.SignalRequest{Message: &pb.SignalRequest_Leave{Leave: &pb.LeaveRequest{Reason: pb.DisconnectReason_CLIENT_INITIATED}}})
		case <-c.done:
		}
		_ = c.ws.Close()
	}()
	defer close(c.done)
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var res pb.SignalResponse
		if err := proto.Unmarshal(data, &res); err != nil {
			continue
		}
		if c.Hooks.Signal != nil {
			c.Hooks.Signal(&res)
		}
		switch m := res.GetMessage().(type) {
		case *pb.SignalResponse_Join:
			if c.Publish {
				if err := c.startPublishing(ctx); err != nil {
					return err
				}
			}
		case *pb.SignalResponse_Offer:
			if err := c.answer(m.Offer); err != nil {
				return err
			}
		case *pb.SignalResponse_Answer:
			if err := c.pub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Answer.GetSdp()}); err != nil {
				return err
			}
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
		case *pb.SignalResponse_Leave:
			return &LeftError{Reason: m.Leave.GetReason()}
		}
	}
}

// LeftError means the server closed the session.
type LeftError struct{ Reason pb.DisconnectReason }

func (e *LeftError) Error() string { return "server closed the session: " + e.Reason.String() }

func (c *Client) answer(offer *pb.SessionDescription) error {
	if err := c.sub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()}); err != nil {
		return err
	}
	answer, err := c.sub.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := c.sub.SetLocalDescription(answer); err != nil {
		return err
	}
	c.send(&pb.SignalRequest{Message: &pb.SignalRequest_Answer{Answer: &pb.SessionDescription{Type: "answer", Sdp: answer.SDP, Id: offer.GetId()}}})
	return nil
}

func (c *Client) startPublishing(ctx context.Context) error {
	add := func(kind pb.TrackType, source pb.TrackSource, mime string, f *Frames) error {
		id := kind.String() + "-" + c.Identity
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: mime}, id, c.Identity)
		if err != nil {
			return err
		}
		if _, err := c.pub.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
			return err
		}
		req := &pb.AddTrackRequest{Cid: id, Name: source.String(), Type: kind, Source: source}
		if kind == pb.TrackType_VIDEO {
			req.Width, req.Height = 640, 360
		}
		c.send(&pb.SignalRequest{Message: &pb.SignalRequest_AddTrack{AddTrack: req}})
		go pace(ctx, track, f)
		return nil
	}
	if c.Video != nil {
		if err := add(pb.TrackType_VIDEO, pb.TrackSource_CAMERA, webrtc.MimeTypeVP8, c.Video); err != nil {
			return err
		}
	}
	if c.Audio != nil {
		if err := add(pb.TrackType_AUDIO, pb.TrackSource_MICROPHONE, webrtc.MimeTypeOpus, c.Audio); err != nil {
			return err
		}
	}
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

// pace sends frames in a loop at their real rate.
func pace(ctx context.Context, track *webrtc.TrackLocalStaticSample, f *Frames) {
	t := time.NewTicker(f.Each)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := track.WriteSample(media.Sample{Data: f.Data[i%len(f.Data)], Duration: f.Each}); err != nil {
				return
			}
			i++
		}
	}
}
