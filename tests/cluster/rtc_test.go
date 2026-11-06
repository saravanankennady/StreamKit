package cluster

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"elnova-streamkit.local/server/auth"
)

// behindProxy is what a trusted proxy in front of the server sends. A proxy that appends leaves whatever the client sent in
// front of the address it saw for itself.
func behindProxy(addresses string) http.Header {
	return http.Header{"X-Forwarded-For": []string{addresses}}
}

func joinToken(t *testing.T, identity, room string) string {
	t.Helper()
	return mint(t, devKey, &auth.Grants{Identity: identity, Video: &auth.VideoGrant{RoomJoin: true, Room: room}})
}

const tightConnects = `limits:
  rtc_connects_per_minute: 6
  rtc_connect_burst: 3
`

func TestOneAddressCannotFloodSignalling(t *testing.T) {
	c := newCluster(t, 14, 1, tightConnects)
	n := c.nodes[0]
	start := func(identity string, h http.Header) int {
		t.Helper()
		_, status, _ := dial(t, n, joinToken(t, identity, "flood"), nil, h)
		return status
	}

	// RemoteAddr is all there is without a trusted proxy, so everything here is one address.
	for i := range 3 {
		if status := start(fmt.Sprintf("a-%d", i), nil); status != http.StatusSwitchingProtocols {
			t.Fatalf("connection %d: %d", i, status)
		}
	}
	if status := start("a-over", nil); status != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", status)
	}
	// Saying it came from somewhere else is no help when no proxy is trusted.
	if status := start("a-spoofed", behindProxy("203.0.113.50")); status != http.StatusTooManyRequests {
		t.Fatalf("with a made-up address: %d", status)
	}
	// Clients ask this when a connection fails, so it answers the same way and they can tell why.
	token := joinToken(t, "a-asks", "flood")
	res, err := http.Get(n.http + "/rtc/validate?access_token=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("validate over the limit: %d", res.StatusCode)
	}
}

// A connection passed from one node to another is counted once, where it arrived. Counting it again would count it against
// the first node's address, which is every client that node passes on.
func TestAConnectionIsCountedOnce(t *testing.T) {
	c := newCluster(t, 14, 2, tightConnects)
	connect(t, c.nodes[0], joinToken(t, "first", "once"))
	holder, other := c.holding(t)
	// That took one from whichever node it reached. Fill the other node's allowance through it.
	for i := range 2 {
		s, status, body := dial(t, other, joinToken(t, fmt.Sprintf("through-%d", i), "once"), nil, nil)
		if s == nil {
			t.Fatalf("connection %d through the other node: %d %s", i, status, body)
		}
	}
	for i := range 2 {
		s, status, body := dial(t, holder, joinToken(t, fmt.Sprintf("direct-%d", i), "once"), nil, nil)
		if s == nil {
			t.Fatalf("connection %d to the node holding the room: %d %s", i, status, body)
		}
	}
}

func TestTheLimitHoldsBehindATrustedProxy(t *testing.T) {
	c := newCluster(t, 15, 1, tightConnects, trustProxy)
	n := c.nodes[0]
	start := func(identity, addresses string) int {
		t.Helper()
		_, status, _ := dial(t, n, joinToken(t, identity, "behind"), nil, behindProxy(addresses))
		return status
	}
	for i := range 3 {
		if status := start(fmt.Sprintf("a-%d", i), "203.0.113.1"); status != http.StatusSwitchingProtocols {
			t.Fatalf("connection %d: %d", i, status)
		}
	}
	if status := start("a-over", "203.0.113.1"); status != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", status)
	}
	// A client can put anything at the front. The proxy's own entry is the one at the end.
	if status := start("a-spoofed", "198.51.100.77, 203.0.113.1"); status != http.StatusTooManyRequests {
		t.Fatalf("with a made-up address in front: %d", status)
	}
	// Someone else behind the same proxy is not held back, or every client would share one limit.
	if status := start("b", "203.0.113.2"); status != http.StatusSwitchingProtocols {
		t.Fatalf("someone at another address: %d", status)
	}
}

func TestAClientReconnectingIsNotRefused(t *testing.T) {
	c := newCluster(t, 15, 1, tightConnects, trustProxy)
	n := c.nodes[0]
	office := behindProxy("203.0.113.9")

	// The office uses up what its address is allowed.
	sessions := []*session{}
	for i := range 3 {
		s, status, body := dial(t, n, joinToken(t, fmt.Sprintf("p-%d", i), "office"), nil, office)
		if s == nil {
			t.Fatalf("connection %d: %d %s", i, status, body)
		}
		s.join = s.next().GetJoin()
		sessions = append(sessions, s)
	}
	if _, status, _ := dial(t, n, joinToken(t, "too-many", "office"), nil, office); status != http.StatusTooManyRequests {
		t.Fatalf("the address should be out of new connections: %d", status)
	}

	resume := func(s *session, h http.Header) {
		t.Helper()
		identity := s.join.GetParticipant().GetIdentity()
		params := url.Values{"reconnect": {"1"}, "sid": {s.join.GetParticipant().GetSid()}}
		back, status, body := dial(t, n, joinToken(t, identity, "office"), params, h)
		if back == nil {
			t.Fatalf("%s could not reconnect: %d %s", identity, status, body)
		}
		if first := back.next(); first.GetReconnect() == nil {
			t.Fatalf("%s did not get the same session back: %v", identity, first)
		}
	}
	// A phone moves from Wi-Fi to mobile data, and comes back from a new address.
	_ = sessions[0].conn.Close()
	resume(sessions[0], behindProxy("198.51.100.20"))
	// The office's connection blips, and everyone comes back from the address that has nothing left.
	_ = sessions[1].conn.Close()
	resume(sessions[1], office)
	_ = sessions[2].conn.Close()
	resume(sessions[2], office)

	// Saying reconnect=1 with no session to show for it buys nothing.
	garbage := url.Values{"reconnect": {"1"}}
	if _, status, _ := dial(t, n, "not-a-token", garbage, office); status != http.StatusTooManyRequests {
		t.Fatalf("a reconnect with a bad token: %d", status)
	}
}

func TestAMeetingStopsAtItsCap(t *testing.T) {
	c := newCluster(t, 15, 1, "room:\n  meeting_max_participants: 2\n")
	n := c.nodes[0]
	m := startMeeting(t, n, "owner@example.com")
	connect(t, n, joinLink(t, n, m.hostCode, "Hana"))
	connect(t, n, joinLink(t, n, m.guestCode, "Gita"))
	_, status, body := dial(t, n, joinLink(t, n, m.guestCode, "One too many"), nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("over the cap: %d %s", status, body)
	}
}
