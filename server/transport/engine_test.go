package transport

import (
	"net"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/server/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Development mode turns on loopback candidates, and a node address replaces the address of every candidate. Together
// they made two identical candidates, ICE kept the one on the loopback socket, and media from any other machine went
// nowhere while signalling worked. Found running a server in a container with --dev and rtc.node_ip. On a single machine
// it cannot be seen by connecting, because loopback reaches this machine's own addresses too.
func TestNodeAddressLeavesNoLoopbackSocket(t *testing.T) {
	engine, err := New(config.RTC{UDPPort: freePort(t), TCPPort: freePort(t), NodeIP: "203.0.113.7", IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	// With a node address there is one socket on every address, so a container on two networks hears both. What must not
	// happen is a loopback candidate going out under the node's address, so loopback candidates are off.
	if engine.cfg.IncludeLoopback {
		t.Error("loopback candidates are on, and one would be advertised as the node's address")
	}
	for _, a := range engine.udp.GetListenAddresses() {
		if udp, ok := a.(*net.UDPAddr); ok && udp.Port != engine.cfg.UDPPort {
			t.Errorf("listening on %s, not the media port", a)
		}
	}
}

func TestHeaderExtensionsKeepOneOrder(t *testing.T) {
	sdp := "v=0\r\na=extmap-allow-mixed\r\nm=video 9 UDP 96\r\na=mid:1\r\na=extmap:10 urn:b\r\na=extmap:4 urn:a\r\na=recvonly\r\nm=audio 9 UDP 111\r\na=extmap:2 urn:d\r\na=extmap:1 urn:e\r\n"
	want := "v=0\r\na=extmap-allow-mixed\r\nm=video 9 UDP 96\r\na=mid:1\r\na=extmap:4 urn:a\r\na=extmap:10 urn:b\r\na=recvonly\r\nm=audio 9 UDP 111\r\na=extmap:1 urn:e\r\na=extmap:2 urn:d\r\n"
	if got := stableExtmaps(sdp); got != want {
		t.Fatalf("got %q", got)
	}
}

// Pion writes extensions in map order. Whatever order it picks, what goes to a browser has to come out the same every time.
func TestDescriptionsListExtensionsInOneOrder(t *testing.T) {
	engine, err := New(config.RTC{UDPPort: freePort(t), TCPPort: freePort(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	seen := map[string]bool{}
	for range 20 {
		pc, err := engine.NewPublisherPC(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
			t.Fatal(err)
		}
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(stableExtmaps(offer.SDP), "\r\n") {
			if strings.HasPrefix(l, "a=extmap:") {
				lines = append(lines, l)
			}
		}
		seen[strings.Join(lines, "|")] = true
		_ = pc.Close()
	}
	if len(seen) != 1 {
		t.Fatalf("%d different orders: %v", len(seen), seen)
	}
}

func TestLoopbackIsUsedWithoutANodeAddress(t *testing.T) {
	engine, err := New(config.RTC{UDPPort: freePort(t), TCPPort: freePort(t), IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	for _, a := range engine.udp.GetListenAddresses() {
		if udp, ok := a.(*net.UDPAddr); ok && udp.IP.IsLoopback() {
			return
		}
	}
	t.Error("development mode should listen on loopback")
}
