package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestRateLimiterIsPerKey(t *testing.T) {
	l := NewRateLimiter(60, 2)
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("burst was not allowed")
	}
	if l.Allow("a") {
		t.Fatal("over the limit was allowed")
	}
	if !l.Allow("b") {
		t.Fatal("one key's usage limited another")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := ClientIP(r, false); got != "10.0.0.5" {
		t.Fatalf("spoofable header was trusted: %s", got)
	}
	// The last entry is the one the trusted proxy added. Whatever comes before it, the client could have sent.
	if got := ClientIP(r, true); got != "10.0.0.1" {
		t.Fatalf("got %s", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := ClientIP(r, true); got != "203.0.113.9" {
		t.Fatalf("got %s", got)
	}
	r.Header.Del("X-Forwarded-For")
	if got := ClientIP(r, true); got != "10.0.0.5" {
		t.Fatalf("no header: got %s", got)
	}
}

func TestAddressKeyCountsAnIPv6NetworkAsOne(t *testing.T) {
	if AddressKey("2001:db8:1:2:aaaa::1") != AddressKey("2001:db8:1:2:bbbb::2") {
		t.Fatal("two addresses in one /64 count separately")
	}
	if AddressKey("2001:db8:1:2::1") == AddressKey("2001:db8:1:3::1") {
		t.Fatal("two /64s count as one")
	}
	if AddressKey("203.0.113.9") == AddressKey("203.0.113.10") {
		t.Fatal("two IPv4 addresses count as one")
	}
	if AddressKey("not an address") != "not an address" {
		t.Fatal("anything else should count as itself")
	}
}
