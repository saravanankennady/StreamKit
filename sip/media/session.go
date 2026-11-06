package media

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// ErrNoPorts means every media port is in use.
var ErrNoPorts = errors.New("no media ports are free")

// Ports hands out pairs of UDP ports: RTP on an even one, RTCP on the next.
type Ports struct {
	ip       net.IP
	min, max int
}

// NewPorts creates a range. ip is the local address to bind, or nil for all of them.
func NewPorts(ip net.IP, minPort, maxPort int) *Ports {
	return &Ports{ip: ip, min: minPort, max: maxPort}
}

func (p *Ports) open() (*net.UDPConn, *net.UDPConn, error) {
	pairs := (p.max - p.min + 1) / 2
	if pairs <= 0 {
		return nil, nil, ErrNoPorts
	}
	// Starting somewhere random means a port is rarely used twice in a row, so a new call does not get the old call's late packets.
	n, err := rand.Int(rand.Reader, big.NewInt(int64(pairs)))
	if err != nil {
		return nil, nil, err
	}
	first := int(n.Int64())
	base := p.min + p.min%2
	for i := range pairs {
		port := base + 2*((first+i)%pairs)
		if port+1 > p.max {
			continue
		}
		rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: p.ip, Port: port})
		if err != nil {
			continue
		}
		rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: p.ip, Port: port + 1})
		if err != nil {
			_ = rtpConn.Close()
			continue
		}
		return rtpConn, rtcpConn, nil
	}
	return nil, nil, ErrNoPorts
}

const (
	maxPacket = 1500
	// maxAudioPerSecond is the most frames a second a call may push into the encoder. A phone sends 50. Anything that sends far more
	// is trying to use up this worker's CPU, and the extra packets are dropped.
	maxAudioPerSecond = 150
	dtmfFrames        = 8
	dtmfGapFrames     = 3
	dtmfEndRepeats    = 3
	dtmfPauseFrames   = 25
	maxGapSamples     = PhoneRate / 5
)

// Session is a call's RTP.
type Session struct {
	log      *slog.Logger
	rtp      *net.UDPConn
	rtcp     *net.UDPConn
	port     int
	ssrc     uint32
	closed   chan struct{}
	stopOnce sync.Once
	readers  sync.WaitGroup

	mu        sync.RWMutex
	remote    *net.UDPAddr
	sources   []netip.Addr
	latched   bool
	codec     Codec
	dtmf      *Codec
	sendCtx   *srtp.Context
	recvCtx   *srtp.Context
	localKey  *Crypto
	remoteKey *Crypto
	canSend   bool
	expected  bool // The peer is meant to be sending, so silence from it is a problem.

	writeMu sync.Mutex
	seq     uint16
	ts      uint32

	dtmfMu    sync.Mutex
	dtmfQueue []byte
	dtmfCur   int // Code of the event being sent, or -1.
	dtmfPos   int
	dtmfGap   int
	dtmfTS    uint32

	lastReceived atomic.Int64
	firstPacket  atomic.Bool
	received     atomic.Int64
	sent         atomic.Int64
	rejected     atomic.Int64
	rejectLogged atomic.Bool

	// OnAudio gets the audio of each packet, in order, with lost time already filled with silence. It runs on the reader.
	OnAudio func(pcm []int16)
	onDigit atomic.Pointer[func(digit byte)]

	dtmfIn    DTMFReceiver
	inited    bool
	lastSeq   uint16
	lastTS    uint32
	lastSSRC  uint32
	decodeBuf [maxPacket]int16
	silence   [maxGapSamples]int16
	window    time.Time
	inWindow  int
}

// Open takes a pair of ports and makes a session on them.
func (p *Ports) Open(log *slog.Logger) (*Session, error) {
	rtpConn, rtcpConn, err := p.open()
	if err != nil {
		return nil, err
	}
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	s := &Session{
		log: log, rtp: rtpConn, rtcp: rtcpConn, port: rtpConn.LocalAddr().(*net.UDPAddr).Port, closed: make(chan struct{}),
		ssrc: binary.BigEndian.Uint32(b[:4]), seq: binary.BigEndian.Uint16(b[4:6]), ts: binary.BigEndian.Uint32(b[6:10]), dtmfCur: -1,
	}
	s.lastReceived.Store(time.Now().UnixNano())
	return s, nil
}

// SetDigitHandler says who gets each DTMF digit, once each. It runs on the reader, so it must not block.
func (s *Session) SetDigitHandler(f func(digit byte)) { s.onDigit.Store(&f) }

// Port is the local RTP port.
func (s *Session) Port() int { return s.port }

func cryptoProfile(c *Crypto) (srtp.ProtectionProfile, error) {
	switch c.Suite.Name {
	case "AES_CM_128_HMAC_SHA1_80":
		return srtp.ProtectionProfileAes128CmHmacSha1_80, nil
	case "AES_CM_128_HMAC_SHA1_32":
		return srtp.ProtectionProfileAes128CmHmacSha1_32, nil
	}
	return 0, fmt.Errorf("unsupported crypto suite %s", c.Suite.Name)
}

func newSRTPContext(c *Crypto) (*srtp.Context, error) {
	profile, err := cryptoProfile(c)
	if err != nil {
		return nil, err
	}
	return srtp.CreateContext(c.Key[:c.Suite.KeyLen], c.Key[c.Suite.KeyLen:], profile)
}

// Configure says who the session talks to and how. trusted is the address the signalling came from. RTP is taken from that address
// or the one in the SDP, and from nowhere else, so nobody can talk into a call just by finding its port. Called again for a re-INVITE.
func (s *Session) Configure(d *Description, trusted netip.Addr, n *Negotiated) error {
	s.mu.RLock()
	sendCtx, recvCtx := s.sendCtx, s.recvCtx
	sameKeys := n.Local != nil && n.Local == s.localKey && n.Remote == s.remoteKey
	s.mu.RUnlock()
	switch {
	case sameKeys:
		// The stream carries on, and so do its counters.
	case n.Local != nil && n.Remote != nil:
		var err error
		if sendCtx, err = newSRTPContext(n.Local); err != nil {
			return err
		}
		if recvCtx, err = newSRTPContext(n.Remote); err != nil {
			return err
		}
	default:
		sendCtx, recvCtx = nil, nil
	}
	addr := d.Addr
	// An SDP address on a private network, from a peer signalling from a public one, is a phone behind NAT that does not know
	// its public address. Its signalling address is the better guess until its first packet says for sure.
	if trusted.IsValid() && !trusted.IsPrivate() && !trusted.IsLoopback() && (addr.IsPrivate() || addr.IsLinkLocalUnicast()) {
		addr = trusted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remote = &net.UDPAddr{IP: addr.AsSlice(), Port: d.Port}
	s.sources = []netip.Addr{addr}
	if d.Addr != addr && d.Addr.IsValid() {
		s.sources = append(s.sources, d.Addr)
	}
	if trusted.IsValid() && trusted != addr {
		s.sources = append(s.sources, trusted)
	}
	s.latched = false
	s.codec, s.dtmf = n.Codec, n.DTMF
	s.sendCtx, s.recvCtx = sendCtx, recvCtx
	s.localKey, s.remoteKey = n.Local, n.Remote
	reachable := d.Addr.IsValid() && !d.Addr.IsUnspecified()
	// What the peer says it does is the opposite of what this side may do. A peer on hold may still send music, or may send nothing,
	// so it is only expected to send when it says it both sends and receives.
	s.canSend = reachable && d.Direction != "sendonly" && d.Direction != "inactive"
	s.expected = reachable && (d.Direction == "sendrecv" || d.Direction == "")
	s.lastReceived.Store(time.Now().UnixNano())
	return nil
}

// Start begins reading.
func (s *Session) Start() {
	s.readers.Add(2)
	go s.readRTP()
	go s.drainRTCP()
}

// Close stops the session and frees its ports.
func (s *Session) Close() {
	s.stopOnce.Do(func() {
		close(s.closed)
		_ = s.rtp.Close()
		_ = s.rtcp.Close()
	})
	s.readers.Wait()
}

// IdleFor says how long it has been since the last packet, and false if the peer is not meant to be sending at all, such as when on
// hold.
func (s *Session) IdleFor() (time.Duration, bool) {
	s.mu.RLock()
	expected := s.expected
	s.mu.RUnlock()
	if !expected {
		return 0, false
	}
	return time.Since(time.Unix(0, s.lastReceived.Load())), true
}

// GotPacket reports whether any RTP has arrived yet.
func (s *Session) GotPacket() bool { return s.firstPacket.Load() }

// Stats gives packets received and sent, and packets refused because of who sent them or because they failed authentication.
func (s *Session) Stats() (received, sent, rejected int64) {
	return s.received.Load(), s.sent.Load(), s.rejected.Load()
}

// Codec returns the codec in use.
func (s *Session) Codec() Codec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.codec
}

// Encrypted reports whether media is SRTP.
func (s *Session) Encrypted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sendCtx != nil
}

// CanSendDTMF reports whether the peer negotiated telephone events.
func (s *Session) CanSendDTMF() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dtmf != nil
}

// RTCP is not used for anything yet, but something has to hold the port: a peer sending to a closed one gets ICMP errors back.
func (s *Session) drainRTCP() {
	defer s.readers.Done()
	buf := make([]byte, maxPacket)
	for {
		if _, _, err := s.rtcp.ReadFromUDP(buf); err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
		}
	}
}

// knownSource reports whether a packet came from this call's peer.
func (s *Session) knownSource(from *net.UDPAddr) bool {
	ip, ok := netip.AddrFromSlice(from.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.sources {
		if a == ip {
			return true
		}
	}
	return false
}

// latch notes where the first real packet came from. That is where to send, too, which is what gets audio through a NAT.
func (s *Session) latch(from *net.UDPAddr) {
	s.mu.RLock()
	done := s.latched
	s.mu.RUnlock()
	if done {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.latched {
		s.latched = true
		s.remote = &net.UDPAddr{IP: append(net.IP(nil), from.IP...), Port: from.Port}
	}
}

func (s *Session) readRTP() {
	defer s.readers.Done()
	buf := make([]byte, maxPacket)
	dec := make([]byte, maxPacket)
	for {
		n, from, err := s.rtp.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.handle(buf[:n], dec, from)
	}
}

func (s *Session) reject(why string, from *net.UDPAddr) {
	s.rejected.Add(1)
	if s.rejectLogged.CompareAndSwap(false, true) {
		s.log.Warn("refused an RTP packet", "why", why, "from", from.String())
	}
}

func (s *Session) handle(pkt, dec []byte, from *net.UDPAddr) {
	s.mu.RLock()
	recvCtx := s.recvCtx
	codec := s.codec
	dtmf := s.dtmf
	configured := s.remote != nil
	s.mu.RUnlock()
	if !configured {
		return
	}
	// Who it came from is checked first, and everything else only after that.
	var hdr rtp.Header
	offset, err := hdr.Unmarshal(pkt)
	if err != nil || hdr.Version != 2 {
		return
	}
	if !s.knownSource(from) {
		s.reject("it did not come from this call's peer", from)
		return
	}
	payload := pkt[offset:]
	if recvCtx != nil {
		plain, err := recvCtx.DecryptRTP(dec, pkt, &hdr)
		if err != nil {
			s.reject("it failed SRTP authentication", from)
			return
		}
		payload = plain[offset:]
	}
	s.latch(from)
	s.lastReceived.Store(time.Now().UnixNano())
	s.firstPacket.Store(true)
	s.received.Add(1)
	if dtmf != nil && hdr.PayloadType == dtmf.PT {
		if digit := s.dtmfIn.Receive(hdr.Timestamp, payload); digit != 0 {
			if f := s.onDigit.Load(); f != nil {
				(*f)(digit)
			}
		}
		return
	}
	if hdr.PayloadType != codec.PT {
		return
	}
	s.handleAudio(&hdr, payload, codec)
}

func (s *Session) handleAudio(hdr *rtp.Header, payload []byte, codec Codec) {
	if s.OnAudio == nil || len(payload) == 0 {
		return
	}
	now := time.Now()
	if now.Sub(s.window) >= time.Second {
		s.window, s.inWindow = now, 0
	}
	s.inWindow++
	if s.inWindow > maxAudioPerSecond {
		return
	}
	if !s.inited || s.lastSSRC != hdr.SSRC {
		s.inited = true
		s.lastSSRC = hdr.SSRC
	} else {
		delta := int16(hdr.SequenceNumber - s.lastSeq)
		if delta <= 0 {
			return // Late or repeated. A frame already went out in its place.
		}
		if gap := int32(hdr.Timestamp - s.lastTS); gap > 0 {
			// Lost packets. The room's clock only moves with what it is given, so this keeps it honest about the time that passed.
			missing := int(gap) - len(payload)
			if missing > 0 && missing < maxGapSamples {
				s.OnAudio(s.silence[:missing])
			}
		}
	}
	s.lastSeq, s.lastTS = hdr.SequenceNumber, hdr.Timestamp
	n := codec.Decode(payload, s.decodeBuf[:])
	s.OnAudio(s.decodeBuf[:n])
}

func (s *Session) send(pt uint8, marker bool, ts uint32, payload []byte) {
	s.mu.RLock()
	remote := s.remote
	sendCtx := s.sendCtx
	canSend := s.canSend
	s.mu.RUnlock()
	if remote == nil || !canSend {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	hdr := rtp.Header{Version: 2, PayloadType: pt, Marker: marker, SequenceNumber: s.seq, Timestamp: ts, SSRC: s.ssrc}
	s.seq++
	raw, err := hdr.Marshal()
	if err != nil {
		return
	}
	raw = append(raw, payload...)
	if sendCtx != nil {
		out, err := sendCtx.EncryptRTP(make([]byte, 0, len(raw)+16), raw, &hdr)
		if err != nil {
			s.log.Warn("could not encrypt RTP", "error", err)
			return
		}
		raw = out
	}
	if _, err := s.rtp.WriteToUDP(raw, remote); err == nil {
		s.sent.Add(1)
	}
}

// QueueDTMF adds digits to send. A w is a half-second pause.
func (s *Session) QueueDTMF(digits string) {
	s.dtmfMu.Lock()
	s.dtmfQueue = append(s.dtmfQueue, digits...)
	s.dtmfMu.Unlock()
}

// DTMFPending reports whether digits are still waiting to go out.
func (s *Session) DTMFPending() bool {
	s.dtmfMu.Lock()
	defer s.dtmfMu.Unlock()
	return len(s.dtmfQueue) > 0 || s.dtmfCur >= 0 || s.dtmfGap > 0
}

// nextDTMF sends the next packet of a DTMF event if one is due, and reports whether audio should stay off for this frame.
func (s *Session) nextDTMF(pt *Codec) bool {
	s.dtmfMu.Lock()
	defer s.dtmfMu.Unlock()
	if s.dtmfGap > 0 {
		s.dtmfGap--
		return false
	}
	if s.dtmfCur < 0 {
		for len(s.dtmfQueue) > 0 {
			next := s.dtmfQueue[0]
			s.dtmfQueue = s.dtmfQueue[1:]
			if next == 'w' || next == 'W' {
				s.dtmfGap = dtmfPauseFrames
				return false
			}
			if code := DigitCode(next); code >= 0 {
				s.dtmfCur, s.dtmfPos, s.dtmfTS = code, 0, s.ts
				break
			}
		}
		if s.dtmfCur < 0 {
			return false
		}
	}
	if pt == nil {
		s.dtmfCur, s.dtmfQueue = -1, nil
		return false
	}
	s.dtmfPos++
	if s.dtmfPos <= dtmfFrames {
		end := s.dtmfPos == dtmfFrames
		duration := uint16(s.dtmfPos * PhoneFrame)
		s.send(pt.PT, s.dtmfPos == 1, s.dtmfTS, DTMFPayload(uint8(s.dtmfCur), end, duration))
		return true
	}
	// The last packet is repeated, since a lost end would leave a digit held down.
	s.send(pt.PT, false, s.dtmfTS, DTMFPayload(uint8(s.dtmfCur), true, uint16(dtmfFrames*PhoneFrame)))
	if s.dtmfPos >= dtmfFrames+dtmfEndRepeats-1 {
		s.dtmfCur, s.dtmfGap = -1, dtmfGapFrames
	}
	return true
}

// RunSender sends a frame every 20 ms until stop closes, or the session does. next fills a frame of PhoneFrame samples.
func (s *Session) RunSender(stop <-chan struct{}, next func(dst []int16)) {
	pcm := make([]int16, PhoneFrame)
	payload := make([]byte, PhoneFrame)
	frame := FrameMs * time.Millisecond
	due := time.Now().Add(frame)
	timer := time.NewTimer(frame)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.closed:
			return
		case <-timer.C:
		}
		s.mu.RLock()
		codec, dtmf := s.codec, s.dtmf
		s.mu.RUnlock()
		next(pcm)
		if !s.nextDTMF(dtmf) {
			n := codec.Encode(pcm, payload)
			s.send(codec.PT, false, s.ts, payload[:n])
		}
		s.ts += PhoneFrame
		due = due.Add(frame)
		wait := time.Until(due)
		if wait < -200*time.Millisecond {
			// Fell a long way behind, such as a machine that stalled. Bursting to catch up helps nobody.
			due = time.Now().Add(frame)
			wait = frame
		}
		if wait < 0 {
			wait = 0
		}
		timer.Reset(wait)
	}
}
