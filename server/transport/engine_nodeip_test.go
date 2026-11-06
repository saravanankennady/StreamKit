package transport

import (
	"net"
	"testing"

	"github.com/pion/ice/v4"

	"elnova-streamkit.local/server/config"
)

func freePorts(t *testing.T) (int, int) {
	t.Helper()
	u, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return u.LocalAddr().(*net.UDPAddr).Port, l.Addr().(*net.TCPAddr).Port
}

// With one address advertised for all of them, what arrives on any local address has to reach ICE.
func TestOneAdvertisedAddressListensOnAllOfThem(t *testing.T) {
	udp, tcp := freePorts(t)
	e, err := New(config.RTC{UDPPort: udp, TCPPort: tcp, NodeIP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, ok := e.udp.(*ice.UDPMuxDefault); !ok {
		t.Fatalf("got a %T", e.udp)
	}
	// Whatever other addresses this machine has, a packet to loopback arrives at the same socket.
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udp})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if busy, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udp}); err == nil {
		_ = busy.Close()
		t.Fatal("loopback's port should be taken by the socket for all addresses")
	}
}

func TestWithoutOneAdvertisedAddressEachHasItsOwnSocket(t *testing.T) {
	udp, tcp := freePorts(t)
	e, err := New(config.RTC{UDPPort: udp, TCPPort: tcp, IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, ok := e.udp.(*ice.MultiUDPMuxDefault); !ok {
		t.Fatalf("got a %T", e.udp)
	}
}
