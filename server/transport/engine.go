// Package transport creates the WebRTC peer connections the SFU runs on: one for receiving from a
// participant and one for sending to them, sharing a single UDP and TCP port across all participants.
package transport

import (
	"fmt"
	"net"
	"strings"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/sfu"
)

const redFmtp = "111/111"

const udpBufferBytes = 4 << 20

// Engine holds the shared sockets and builds peer connections.
type Engine struct {
	cfg     config.RTC
	setting webrtc.SettingEngine
	udp     ice.UDPMux
	tcp     ice.TCPMux
	tcpLn   net.Listener
}

// New opens the media ports.
func New(cfg config.RTC) (*Engine, error) {
	if cfg.NodeIP != "" {
		// A loopback socket would be advertised under the node's address as well. ICE keeps the first of two identical
		// candidates, which is the loopback one, so media from other machines has nowhere to go.
		cfg.IncludeLoopback = false
	}
	e := &Engine{cfg: cfg}

	networks := []ice.NetworkType{ice.NetworkTypeUDP4}
	types := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4}
	if cfg.EnableIPv6 {
		networks = append(networks, ice.NetworkTypeUDP6)
		types = append(types, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6)
	}

	udp, err := openUDP(cfg, networks)
	if err != nil {
		return nil, fmt.Errorf("listen on UDP port %d: %w", cfg.UDPPort, err)
	}
	e.udp = udp

	network := "tcp4"
	if cfg.EnableIPv6 {
		network = "tcp"
	}
	tcpLn, err := net.Listen(network, fmt.Sprintf(":%d", cfg.TCPPort))
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("listen on TCP port %d: %w", cfg.TCPPort, err)
	}
	e.tcpLn = tcpLn
	e.tcp = webrtc.NewICETCPMux(nil, tcpLn, 64)

	e.setting.SetICEUDPMux(udp)
	e.setting.SetICETCPMux(e.tcp)
	e.setting.SetNetworkTypes(types)
	e.setting.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)
	e.setting.SetInterfaceFilter(usableInterface)
	// A browser with no media permission hides its address, so what it reaches here from is a peer reflexive candidate. By
	// default this server waits a second before choosing one, which is a second without video on every ICE restart.
	e.setting.SetPrflxAcceptanceMinWait(0)
	if cfg.NodeIP != "" {
		// Publish only the given address: the host is behind 1:1 NAT or has several addresses.
		if err := e.setting.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{cfg.NodeIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

// openUDP opens the UDP media port. Normally that is one socket for each local address, each advertised as itself. With NodeIP
// set, every address is advertised as that one, so ICE sees them as the same candidate, and only listens on the first. A
// machine with more than one address, such as a container on two networks, would then lose what arrives on the others, which
// is everything if that is where the port is published. A client that sends no candidates of its own, such as a WHIP publisher
// that does not wait for them, has no other way in. So in that case, one socket listens on all of them.
func openUDP(cfg config.RTC, networks []ice.NetworkType) (ice.UDPMux, error) {
	if cfg.NodeIP != "" && !cfg.EnableIPv6 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.UDPPort})
		if err != nil {
			return nil, err
		}
		_ = conn.SetReadBuffer(udpBufferBytes)
		_ = conn.SetWriteBuffer(udpBufferBytes)
		return ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}), nil
	}
	opts := []ice.UDPMuxFromPortOption{
		ice.UDPMuxFromPortWithNetworks(networks...),
		ice.UDPMuxFromPortWithInterfaceFilter(usableInterface),
		ice.UDPMuxFromPortWithReadBufferSize(udpBufferBytes),
		ice.UDPMuxFromPortWithWriteBufferSize(udpBufferBytes),
	}
	if cfg.IncludeLoopback {
		opts = append(opts, ice.UDPMuxFromPortWithLoopback())
	}
	return ice.NewMultiUDPMuxFromPort(cfg.UDPPort, opts...)
}

// usableInterface skips the bridges and tunnels container runtimes create. A browser can rarely reach
// those, and each extra candidate slows connectivity checks.
func usableInterface(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "calico"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// Close releases the media ports.
func (e *Engine) Close() {
	if e.udp != nil {
		_ = e.udp.Close()
	}
	if e.tcp != nil {
		_ = e.tcp.Close()
	}
	if e.tcpLn != nil {
		_ = e.tcpLn.Close()
	}
}

func videoFeedback() []webrtc.RTCPFeedback {
	return []webrtc.RTCPFeedback{
		{Type: "goog-remb"},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: webrtc.TypeRTCPFBTransportCC},
	}
}

type codecSpec struct {
	mime string
	fmtp string
	pt   webrtc.PayloadType
	rtx  webrtc.PayloadType
}

var videoCodecs = []codecSpec{
	{webrtc.MimeTypeVP8, "", 96, 97},
	{webrtc.MimeTypeVP9, "profile-id=0", 98, 99},
	{webrtc.MimeTypeVP9, "profile-id=1", 100, 101},
	{webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", 125, 107},
	{webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", 108, 109},
	{webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f", 116, 117},
	{webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f", 112, 113},
	{webrtc.MimeTypeAV1, "", 35, 36},
}

// registerCodecs fills a MediaEngine. The answer on a publisher connection lists codecs in registration
// order and browsers send the first one, so preferRED decides whether a client that offers RED uses it.
func registerCodecs(me *webrtc.MediaEngine, withRTX, preferRED bool) error {
	audio := []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}
	opus := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: "minptime=10;useinbandfec=1", RTCPFeedback: audio,
	}, PayloadType: 111}
	red := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: sfu.MimeTypeRED, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: redFmtp, RTCPFeedback: audio,
	}, PayloadType: 63}
	audioCodecs := []webrtc.RTPCodecParameters{opus, red}
	if preferRED {
		audioCodecs = []webrtc.RTPCodecParameters{red, opus}
	}
	for _, c := range audioCodecs {
		if err := me.RegisterCodec(c, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}
	for _, c := range videoCodecs {
		err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: c.mime, ClockRate: 90000, SDPFmtpLine: c.fmtp, RTCPFeedback: videoFeedback(),
			},
			PayloadType: c.pt,
		}, webrtc.RTPCodecTypeVideo)
		if err != nil {
			return err
		}
		if !withRTX {
			continue
		}
		err = me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", c.pt),
			},
			PayloadType: c.rtx,
		}, webrtc.RTPCodecTypeVideo)
		if err != nil {
			return err
		}
	}
	return nil
}

func registerExtensions(me *webrtc.MediaEngine, uris []string, kind webrtc.RTPCodecType) error {
	for _, uri := range uris {
		if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, kind); err != nil {
			return err
		}
	}
	return nil
}

// NewPublisherPC creates the connection that receives a participant's media. This server answers its
// offers.
func (e *Engine) NewPublisherPC(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me, true, true); err != nil {
		return nil, err
	}
	if err := registerExtensions(me, []string{sdp.SDESMidURI, sdp.AudioLevelURI, sdp.TransportCCURI}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := registerExtensions(me, []string{
		sdp.SDESMidURI, sdp.SDESRTPStreamIDURI, sdp.SDESRepairRTPStreamIDURI,
		sdp.TransportCCURI, sfu.DependencyDescriptorURI,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	reg := &interceptor.Registry{}
	generator, err := nack.NewGeneratorInterceptor()
	if err != nil {
		return nil, err
	}
	reg.Add(generator)
	reports, err := report.NewReceiverInterceptor()
	if err != nil {
		return nil, err
	}
	reg.Add(reports)
	// Publishers run their own bandwidth estimation from these reports.
	if err := webrtc.ConfigureTWCCSender(me, reg); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(e.setting),
	)
	return api.NewPeerConnection(cfg)
}

// MaxBitrate returns the configured limit on what one subscriber receives, in bits per second.
func (e *Engine) MaxBitrate() int { return e.cfg.MaxBitrate }

// NewSubscriberPC creates the connection that sends media to a participant. This server sends the offers on it.
//
// There is no bandwidth estimator here on purpose: Pion's GCC collapses on WiFi jitter. See sfu/capacity.go. There is no
// NACK responder either, because each DownTrack answers NACKs itself and knows when doing so cannot help. See
// sfu/retransmit.go.
func (e *Engine) NewSubscriberPC(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me, false, false); err != nil {
		return nil, err
	}
	if err := registerExtensions(me, []string{sdp.AudioLevelURI}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := registerExtensions(me, []string{sfu.DependencyDescriptorURI, sfu.PlayoutDelayURI}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	reg := &interceptor.Registry{}
	// No sender report interceptor. It states that a packet's capture time is the moment it passed through here, which
	// folds in every delay before this point, differently for audio and video. A browser believes that and lines the two
	// up wrongly. Reports are written from the publisher's own, by Participant.

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(e.setting),
	)
	return api.NewPeerConnection(cfg)
}

// NewSinglePC creates the one connection a client on the single peer connection protocol has, carrying media both ways. The
// client makes every offer. It holds what NewPublisherPC and NewSubscriberPC hold, for the same reasons, and with RTX
// negotiated on subscribed tracks too, because each section's codecs are the same both ways.
func (e *Engine) NewSinglePC(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me, true, true); err != nil {
		return nil, err
	}
	if err := registerExtensions(me, []string{sdp.SDESMidURI, sdp.AudioLevelURI, sdp.TransportCCURI}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := registerExtensions(me, []string{
		sdp.SDESMidURI, sdp.SDESRTPStreamIDURI, sdp.SDESRepairRTPStreamIDURI,
		sdp.TransportCCURI, sfu.DependencyDescriptorURI, sfu.PlayoutDelayURI,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	reg := &interceptor.Registry{}
	generator, err := nack.NewGeneratorInterceptor()
	if err != nil {
		return nil, err
	}
	reg.Add(generator)
	reports, err := report.NewReceiverInterceptor()
	if err != nil {
		return nil, err
	}
	reg.Add(reports)
	if err := webrtc.ConfigureTWCCSender(me, reg); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(e.setting),
	)
	return api.NewPeerConnection(cfg)
}

// EnabledPublishCodecs lists what publishers may send, for the join response.
func EnabledPublishCodecs() []string {
	return []string{webrtc.MimeTypeVP8, webrtc.MimeTypeH264, webrtc.MimeTypeVP9, webrtc.MimeTypeAV1, webrtc.MimeTypeOpus, sfu.MimeTypeRED}
}
