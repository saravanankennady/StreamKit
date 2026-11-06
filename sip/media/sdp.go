package media

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"
)

// Reasons a call is refused for over its media. People search carrier logs for these, so they stay the same words as the reference.
var (
	ErrNoCodec    = errors.New("No Acceptable Codec")
	ErrNoCrypto   = errors.New("No Acceptable Encryption Profile")
	ErrInvalidSDP = errors.New("Invalid SDP")
)

// Encryption says what a call requires of its media.
type Encryption int

// Encryption settings, matching the API's.
const (
	EncryptDisable Encryption = iota
	EncryptAllow
	EncryptRequire
)

const dtmfName = "telephone-event"

// CryptoSuite is an SDES profile.
type CryptoSuite struct {
	Name    string
	KeyLen  int
	SaltLen int
}

// The SDES profiles offered, best first.
var cryptoSuites = []CryptoSuite{
	{"AES_CM_128_HMAC_SHA1_80", 16, 14},
	{"AES_CM_128_HMAC_SHA1_32", 16, 14},
}

func findSuite(name string) (CryptoSuite, bool) {
	for _, s := range cryptoSuites {
		if s.Name == name {
			return s, true
		}
	}
	return CryptoSuite{}, false
}

// Crypto is one a=crypto line.
type Crypto struct {
	Tag   int
	Suite CryptoSuite
	// Key is the master key followed by the master salt.
	Key []byte
}

func (c Crypto) line() string {
	return fmt.Sprintf("a=crypto:%d %s inline:%s", c.Tag, c.Suite.Name, base64.StdEncoding.EncodeToString(c.Key))
}

func newCrypto(tag int, suite CryptoSuite) (Crypto, error) {
	key := make([]byte, suite.KeyLen+suite.SaltLen)
	if _, err := rand.Read(key); err != nil {
		return Crypto{}, err
	}
	return Crypto{Tag: tag, Suite: suite, Key: key}, nil
}

// parseCrypto reads a value of a=crypto. A line with a profile we do not use, or a form we do not take, comes back false.
func parseCrypto(value string) (Crypto, bool) {
	fields := strings.Fields(value)
	if len(fields) < 3 {
		return Crypto{}, false
	}
	tag, err := strconv.Atoi(fields[0])
	if err != nil {
		return Crypto{}, false
	}
	suite, ok := findSuite(fields[1])
	if !ok {
		return Crypto{}, false
	}
	keyParam, ok := strings.CutPrefix(fields[2], "inline:")
	if !ok {
		return Crypto{}, false
	}
	// A lifetime or a master key index can follow, after a bar. Keys shared with a key index are not used here.
	b64, rest, _ := strings.Cut(keyParam, "|")
	if strings.Contains(rest, ":") {
		return Crypto{}, false
	}
	if len(fields) > 3 {
		// Session parameters change how SRTP behaves, such as UNENCRYPTED_SRTP, so a line that has them is skipped.
		return Crypto{}, false
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if key, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return Crypto{}, false
		}
	}
	if len(key) != suite.KeyLen+suite.SaltLen {
		return Crypto{}, false
	}
	return Crypto{Tag: tag, Suite: suite, Key: key}, true
}

// Description is what a peer's SDP says about its audio.
type Description struct {
	Addr      netip.Addr
	Port      int
	Codecs    []Codec
	DTMF      *Codec
	Cryptos   []Crypto
	Secure    bool // The media line said RTP/SAVP.
	Direction string
	// Others holds the media lines that are not the audio we use. An answer has to mention each, in order, turned off.
	Others []otherMedia
	// AudioIndex is where the audio line was among them.
	AudioIndex int
}

type otherMedia struct {
	Name    string
	Proto   string
	Formats []string
}

// Hold reports whether the peer has stopped sending or has asked not to be sent to.
func (d *Description) Hold() bool {
	return d.Direction == "inactive" || d.Direction == "sendonly" || !d.Addr.IsValid() || d.Addr.IsUnspecified()
}

func directionOf(attrs []sdp.Attribute) string {
	for _, a := range attrs {
		switch a.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return a.Key
		}
	}
	return ""
}

// ParseDescription reads an SDP offer or answer.
func ParseDescription(body []byte) (*Description, error) {
	var s sdp.SessionDescription
	if err := s.UnmarshalString(string(body)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidSDP, err)
	}
	d := &Description{AudioIndex: -1, Direction: directionOf(s.Attributes)}
	var session string
	if s.ConnectionInformation != nil && s.ConnectionInformation.Address != nil {
		session = s.ConnectionInformation.Address.Address
	}
	for i, m := range s.MediaDescriptions {
		proto := strings.Join(m.MediaName.Protos, "/")
		usable := m.MediaName.Media == "audio" && m.MediaName.Port.Value != 0 && d.AudioIndex < 0 && (proto == "RTP/AVP" || proto == "RTP/SAVP")
		if !usable {
			d.Others = append(d.Others, otherMedia{m.MediaName.Media, proto, m.MediaName.Formats})
			continue
		}
		d.AudioIndex = i
		d.Secure = proto == "RTP/SAVP"
		d.Port = m.MediaName.Port.Value
		address := session
		if m.ConnectionInformation != nil && m.ConnectionInformation.Address != nil {
			address = m.ConnectionInformation.Address.Address
		}
		if address != "" {
			if ip, err := netip.ParseAddr(address); err == nil {
				d.Addr = ip.Unmap()
			}
		}
		if dir := directionOf(m.Attributes); dir != "" {
			d.Direction = dir
		}
		maps := map[uint8]string{0: CodecPCMU + "/8000", 8: CodecPCMA + "/8000"}
		for _, a := range m.Attributes {
			switch a.Key {
			case "rtpmap":
				pt, rest, ok := strings.Cut(a.Value, " ")
				if !ok {
					continue
				}
				if n, err := strconv.ParseUint(pt, 10, 8); err == nil {
					maps[uint8(n)] = rest
				}
			case "crypto":
				if c, ok := parseCrypto(a.Value); ok {
					d.Cryptos = append(d.Cryptos, c)
				}
			}
		}
		for _, f := range m.MediaName.Formats {
			n, err := strconv.ParseUint(f, 10, 8)
			if err != nil {
				continue
			}
			name, rate, _ := strings.Cut(maps[uint8(n)], "/")
			rate, _, _ = strings.Cut(rate, "/")
			switch {
			case rate != "8000":
			case strings.EqualFold(name, CodecPCMU):
				d.Codecs = append(d.Codecs, Codec{CodecPCMU, uint8(n)})
			case strings.EqualFold(name, CodecPCMA):
				d.Codecs = append(d.Codecs, Codec{CodecPCMA, uint8(n)})
			case strings.EqualFold(name, dtmfName):
				d.DTMF = &Codec{dtmfName, uint8(n)}
			}
		}
	}
	if d.AudioIndex < 0 {
		return nil, fmt.Errorf("%w: no audio", ErrNoCodec)
	}
	if !d.Addr.IsValid() {
		return nil, fmt.Errorf("%w: no connection address", ErrInvalidSDP)
	}
	return d, nil
}

// Negotiated is what both sides of a call have agreed.
type Negotiated struct {
	Codec Codec
	DTMF  *Codec
	// Local and Remote are the SRTP keys for what this side sends and what it receives, or nil for plain RTP.
	Local, Remote *Crypto
}

// KeepKeys uses the local key from last time if the peer has kept its own.
func (n *Negotiated) KeepKeys(last *Negotiated) {
	if last == nil || n.Remote == nil || last.Remote == nil || last.Local == nil {
		return
	}
	if n.Remote.Suite.Name == last.Remote.Suite.Name && bytes.Equal(n.Remote.Key, last.Remote.Key) {
		n.Local, n.Remote = last.Local, last.Remote
	}
}

// Policy is what this side will accept.
type Policy struct {
	// Codecs are the names allowed, in order of preference when offering.
	Codecs     []string
	Encryption Encryption
}

// DefaultPolicy allows both G.711 variants and no encryption.
func DefaultPolicy() Policy { return Policy{Codecs: []string{CodecPCMU, CodecPCMA}} }

// Answer works out a response to an offer. The caller gets ErrNoCodec or ErrNoCrypto if nothing can be agreed.
func (p Policy) Answer(offer *Description) (*Negotiated, error) {
	n := &Negotiated{DTMF: offer.DTMF}
	found := false
	for _, c := range offer.Codecs {
		if slices.Contains(p.Codecs, c.Name) {
			n.Codec, found = c, true
			break
		}
	}
	if !found {
		return nil, ErrNoCodec
	}
	switch {
	case offer.Secure && p.Encryption == EncryptDisable:
		return nil, ErrNoCrypto
	case p.Encryption == EncryptRequire && len(offer.Cryptos) == 0:
		return nil, ErrNoCrypto
	case offer.Secure && len(offer.Cryptos) == 0:
		return nil, ErrNoCrypto
	case p.Encryption != EncryptDisable && len(offer.Cryptos) > 0:
		remote := offer.Cryptos[0]
		local, err := newCrypto(remote.Tag, remote.Suite)
		if err != nil {
			return nil, err
		}
		n.Local, n.Remote = &local, &remote
	}
	return n, nil
}

func addrType(ip netip.Addr) string {
	if ip.Is6() {
		return "IP6"
	}
	return "IP4"
}

func writeHeader(b *strings.Builder, ip netip.Addr) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	t := addrType(ip)
	fmt.Fprintf(b, "v=0\r\no=streamkit %s %s IN %s %s\r\ns=StreamKit\r\nc=IN %s %s\r\nt=0 0\r\n", now, now, t, ip, t, ip)
}

// BuildAnswer writes the SDP of an answer. direction is what this side is doing, which is the mirror image of the offer.
func BuildAnswer(ip netip.Addr, port int, offer *Description, n *Negotiated, direction string) []byte {
	var b strings.Builder
	writeHeader(&b, ip)
	other := 0
	total := len(offer.Others) + 1
	for i := range total {
		if i != offer.AudioIndex {
			o := offer.Others[other]
			other++
			fmt.Fprintf(&b, "m=%s 0 %s %s\r\n", o.Name, o.Proto, strings.Join(o.Formats, " "))
			continue
		}
		proto := "RTP/AVP"
		if offer.Secure {
			proto = "RTP/SAVP"
		}
		formats := []string{strconv.Itoa(int(n.Codec.PT))}
		if n.DTMF != nil {
			formats = append(formats, strconv.Itoa(int(n.DTMF.PT)))
		}
		fmt.Fprintf(&b, "m=audio %d %s %s\r\n", port, proto, strings.Join(formats, " "))
		fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\n", n.Codec.PT, n.Codec.Name)
		if n.DTMF != nil {
			fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\na=fmtp:%d 0-16\r\n", n.DTMF.PT, dtmfName, n.DTMF.PT)
		}
		fmt.Fprintf(&b, "a=ptime:%d\r\na=%s\r\n", FrameMs, direction)
		if n.Local != nil {
			b.WriteString(n.Local.line() + "\r\n")
		}
	}
	return []byte(b.String())
}

// MirrorDirection gives this side's direction, for a peer's. Saying what we do is required to be the opposite of what they do.
func MirrorDirection(peer string) string {
	switch peer {
	case "sendonly":
		return "recvonly"
	case "recvonly":
		return "sendonly"
	case "inactive":
		return "inactive"
	}
	return "sendrecv"
}

// Offer is an SDP offer this side made, kept for reading the answer.
type Offer struct {
	SDP     []byte
	Cryptos []Crypto
	Policy  Policy
}

const (
	offerDTMF = 101
)

// BuildOffer writes an offer for an outbound call.
func BuildOffer(ip netip.Addr, port int, p Policy) (*Offer, error) {
	var b strings.Builder
	writeHeader(&b, ip)
	proto := "RTP/AVP"
	if p.Encryption == EncryptRequire {
		proto = "RTP/SAVP"
	}
	formats := make([]string, 0, len(p.Codecs)+1)
	var maps []string
	for _, name := range p.Codecs {
		pt := 0
		if name == CodecPCMA {
			pt = 8
		}
		formats = append(formats, strconv.Itoa(pt))
		maps = append(maps, fmt.Sprintf("a=rtpmap:%d %s/8000", pt, name))
	}
	formats = append(formats, strconv.Itoa(offerDTMF))
	fmt.Fprintf(&b, "m=audio %d %s %s\r\n", port, proto, strings.Join(formats, " "))
	for _, m := range maps {
		b.WriteString(m + "\r\n")
	}
	fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\na=fmtp:%d 0-16\r\na=ptime:%d\r\na=sendrecv\r\n", offerDTMF, dtmfName, offerDTMF, FrameMs)
	o := &Offer{Policy: p}
	if p.Encryption != EncryptDisable {
		for i, suite := range cryptoSuites {
			c, err := newCrypto(i+1, suite)
			if err != nil {
				return nil, err
			}
			o.Cryptos = append(o.Cryptos, c)
			b.WriteString(c.line() + "\r\n")
		}
	}
	o.SDP = []byte(b.String())
	return o, nil
}

// Accept reads the answer to this offer.
func (o *Offer) Accept(answer *Description) (*Negotiated, error) {
	n := &Negotiated{DTMF: answer.DTMF}
	found := false
	for _, c := range answer.Codecs {
		if slices.Contains(o.Policy.Codecs, c.Name) {
			n.Codec, found = c, true
			break
		}
	}
	if !found {
		return nil, ErrNoCodec
	}
	if len(answer.Cryptos) == 0 {
		if o.Policy.Encryption == EncryptRequire || answer.Secure {
			return nil, ErrNoCrypto
		}
		return n, nil
	}
	remote := answer.Cryptos[0]
	for i := range o.Cryptos {
		if o.Cryptos[i].Tag == remote.Tag && o.Cryptos[i].Suite.Name == remote.Suite.Name {
			n.Local, n.Remote = &o.Cryptos[i], &remote
			return n, nil
		}
	}
	return nil, ErrNoCrypto
}
