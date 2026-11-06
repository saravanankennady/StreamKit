package sip

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/icholy/digest"
)

func TestDigestAcceptsAnAnswerFromAnIndependentClient(t *testing.T) {
	d := newDigestAuth("master")
	now := time.Now()
	header := d.challenge(now, "realm", "call-1", "ST_a", false)
	chal, err := digest.ParseChallenge(header)
	if err != nil {
		t.Fatalf("our challenge does not parse: %v: %s", err, header)
	}
	cred, err := digest.Digest(chal, digest.Options{Method: "INVITE", URI: "sip:+15550100@example.com", Username: "alice", Password: "s3cret", Count: 1, Cnonce: "abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.check(now, cred.String(), "INVITE", "realm", "alice", "s3cret", "call-1", "ST_a"); got != authOK {
		t.Fatalf("got %d for a correct answer: %s", got, cred.String())
	}
}

func answer(t *testing.T, challenge, user, pass string) string {
	t.Helper()
	chal, err := digest.ParseChallenge(challenge)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := digest.Digest(chal, digest.Options{Method: "INVITE", URI: "sip:a@b", Username: user, Password: pass, Count: 1, Cnonce: "c0ffee" + user + pass})
	if err != nil {
		t.Fatal(err)
	}
	return cred.String()
}

func TestDigestRefusals(t *testing.T) {
	d := newDigestAuth("master")
	now := time.Now()
	ch := func(call, trunk string) string { return d.challenge(now, "realm", call, trunk, false) }
	check := func(header, call, trunk string, at time.Time) authResult {
		return d.check(at, header, "INVITE", "realm", "alice", "s3cret", call, trunk)
	}
	if got := check("", "c", "t", now); got != authChallenge {
		t.Errorf("no answer: %d", got)
	}
	if got := check("not a digest header", "c", "t", now); got != authBad {
		t.Errorf("garbage: %d", got)
	}
	if got := check(answer(t, ch("c1", "t"), "alice", "wrong"), "c1", "t", now); got != authBad {
		t.Errorf("wrong password: %d", got)
	}
	if got := check(answer(t, ch("c2", "t"), "mallory", "s3cret"), "c2", "t", now); got != authBad {
		t.Errorf("wrong user: %d", got)
	}
	// A challenge for one call does not answer for another, and neither does one for another trunk.
	if got := check(answer(t, ch("c3", "t"), "alice", "s3cret"), "c4", "t", now); got != authBad {
		t.Errorf("another call: %d", got)
	}
	if got := check(answer(t, ch("c5", "t"), "alice", "s3cret"), "c5", "other", now); got != authBad {
		t.Errorf("another trunk: %d", got)
	}
	// A nonce this did not make.
	forged := newDigestAuth("another key").challenge(now, "realm", "c6", "t", false)
	if got := check(answer(t, forged, "alice", "s3cret"), "c6", "t", now); got != authBad {
		t.Errorf("forged nonce: %d", got)
	}
	// An old answer is asked again, not called wrong, since that is what stale is for.
	old := d.challenge(now.Add(-2*nonceLife), "realm", "c7", "t", false)
	if got := check(answer(t, old, "alice", "s3cret"), "c7", "t", now); got != authStale {
		t.Errorf("old nonce: %d", got)
	}
}

func TestDigestAnswerWorksOnce(t *testing.T) {
	d := newDigestAuth("master")
	now := time.Now()
	header := answer(t, d.challenge(now, "realm", "call", "t", false), "alice", "s3cret")
	if got := d.check(now, header, "INVITE", "realm", "alice", "s3cret", "call", "t"); got != authOK {
		t.Fatalf("first use: %d", got)
	}
	if got := d.check(now, header, "INVITE", "realm", "alice", "s3cret", "call", "t"); got != authStale {
		t.Fatalf("replay: %d", got)
	}
}

// The two ends must agree on the password, not on anything else: a client that leaves out qop still has to know it.
func TestDigestWithoutQOP(t *testing.T) {
	d := newDigestAuth("master")
	now := time.Now()
	nonce := d.nonce(now, "call", "t")
	response := md5Hex(md5Hex("alice:realm:s3cret") + ":" + nonce + ":" + md5Hex("INVITE:sip:a@b"))
	header := fmt.Sprintf(`Digest username="alice", realm="realm", nonce="%s", uri="sip:a@b", response="%s", algorithm=MD5`, nonce, response)
	if got := d.check(now, header, "INVITE", "realm", "alice", "s3cret", "call", "t"); got != authOK {
		t.Fatalf("got %d", got)
	}
}

func TestChallengeAsksForMD5AndQOP(t *testing.T) {
	d := newDigestAuth("master")
	header := d.challenge(time.Now(), "my realm", "call", "t", true)
	for _, want := range []string{"Digest ", `realm="my realm"`, "algorithm=MD5", "qop=", "stale=true"} {
		if !strings.Contains(header, want) {
			t.Errorf("challenge lacks %q: %s", want, header)
		}
	}
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestGuardLimitsASource(t *testing.T) {
	g := newGuard(10, 20, 600, 5)
	now := time.Now()
	allowed := 0
	for range 100 {
		if g.allow(addr("198.51.100.1"), now) {
			allowed++
		}
	}
	if allowed != 20 {
		t.Errorf("allowed %d in a burst, want 20", allowed)
	}
	// Someone else is not affected.
	if !g.allow(addr("198.51.100.2"), now) {
		t.Error("another source was held back")
	}
	if !g.allow(addr("198.51.100.1"), now.Add(time.Second)) {
		t.Error("the first source did not recover")
	}
}

func TestGuardBansAfterRepeatedFailures(t *testing.T) {
	g := newGuard(1000, 1000, 600, 5)
	now := time.Now()
	ip := addr("198.51.100.1")
	for range 4 {
		g.failed(ip, now)
	}
	if !g.allow(ip, now) {
		t.Fatal("banned too early")
	}
	g.failed(ip, now)
	if g.allow(ip, now) {
		t.Fatal("not banned")
	}
	if g.allow(ip, now.Add(599*time.Second)) {
		t.Fatal("the ban ended early")
	}
	if !g.allow(ip, now.Add(601*time.Second)) {
		t.Fatal("the ban did not end")
	}
}

func TestGuardFailuresAgeOut(t *testing.T) {
	g := newGuard(1000, 1000, 600, 5)
	now := time.Now()
	ip := addr("198.51.100.1")
	for i := range 20 {
		g.failed(ip, now.Add(time.Duration(i)*20*time.Second))
	}
	if !g.allow(ip, now.Add(400*time.Second)) {
		t.Error("slow failures over a long time got a source banned")
	}
}

// One misconfigured number at a carrier must not cut off everyone calling through it.
func TestGuardDoesNotBanKnownCarriers(t *testing.T) {
	g := newGuard(10, 20, 600, 5)
	g.setKnown([]string{"203.0.113.0/24", "not an address"})
	now := time.Now()
	ip := addr("203.0.113.9")
	for range 100 {
		g.failed(ip, now)
	}
	if !g.allow(ip, now) {
		t.Fatal("a carrier on an allow-list got banned")
	}
	allowed := 0
	for range 1000 {
		if g.allow(addr("203.0.113.10"), now) {
			allowed++
		}
	}
	if allowed != 20*knownSourceFactor {
		t.Errorf("a carrier got %d, want %d", allowed, 20*knownSourceFactor)
	}
}

func TestGuardBanCanBeTurnedOff(t *testing.T) {
	g := newGuard(1000, 1000, -1, 5)
	now := time.Now()
	ip := addr("198.51.100.1")
	for range 100 {
		g.failed(ip, now)
	}
	if !g.allow(ip, now) {
		t.Error("banned with bans off")
	}
}

func TestGuardMemoryIsBounded(t *testing.T) {
	g := newGuard(1000, 1000, 600, 5)
	now := time.Now()
	for i := range guardMaxSources + 5000 {
		b := [4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}
		g.allow(netip.AddrFrom4(b), now)
	}
	if n := len(g.sources); n > guardMaxSources+1 {
		t.Errorf("holds %d sources", n)
	}
}

func TestScannerAgents(t *testing.T) {
	for ua, want := range map[string]bool{
		"friendly-scanner":  true,
		"sipvicious":        true,
		"SIPVicious 0.3.0":  true,
		"sipcli/v1.8":       true,
		"PJSUA v2.15.1":     false,
		"Asterisk PBX 20.1": false,
		"Twilio Gateway":    false,
		"":                  false,
	} {
		if got := isScanner(ua); got != want {
			t.Errorf("%q: got %v", ua, got)
		}
	}
}

func TestParseSource(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1:5060":        "192.0.2.1",
		"[2001:db8::1]:5060":    "2001:db8::1",
		"192.0.2.1":             "192.0.2.1",
		"[::ffff:192.0.2.1]:80": "192.0.2.1",
	} {
		if got := parseSource(in); got != addr(want) {
			t.Errorf("%q: got %v", in, got)
		}
	}
	if parseSource("not an address").IsValid() {
		t.Error("garbage parsed")
	}
}

func TestLimitListener(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	limited := newLimitListener(l, newGuard(100000, 100000, -1, 5))
	accepted := make(chan net.Conn, maxConnsPerSource+8)
	go func() {
		for {
			c, err := limited.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for range maxConnsPerSource + 5 {
		c, err := net.Dial("tcp4", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	var got []net.Conn
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case c := <-accepted:
			got = append(got, c)
			if len(got) > maxConnsPerSource {
				break collect
			}
		case <-timeout:
			break collect
		}
	}
	if len(got) != maxConnsPerSource {
		t.Fatalf("accepted %d connections from one source, want %d", len(got), maxConnsPerSource)
	}
	// Closing one makes room for another.
	_ = got[0].Close()
	time.Sleep(50 * time.Millisecond)
	c, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("no room was made")
	}
	for _, c := range got[1:] {
		_ = c.Close()
	}
}
