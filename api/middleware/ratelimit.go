// Package middleware holds cross-cutting HTTP concerns for the REST API.
package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const limiterIdleAfter = 10 * time.Minute

type limiterEntry struct {
	limiter *rate.Limiter
	seen    time.Time
}

// RateLimiter applies a token bucket per key.
type RateLimiter struct {
	limit rate.Limit
	burst int

	mu      sync.Mutex
	entries map[string]*limiterEntry
	swept   time.Time
}

// NewRateLimiter allows perMinute requests per key, with bursts up to burst.
func NewRateLimiter(perMinute float64, burst int) *RateLimiter {
	return &RateLimiter{limit: rate.Limit(perMinute / 60), burst: burst, entries: map[string]*limiterEntry{}, swept: time.Now()}
}

// Allow reports whether a request under key may proceed.
func (l *RateLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > limiterIdleAfter {
		for k, e := range l.entries {
			if now.Sub(e.seen) > limiterIdleAfter {
				delete(l.entries, k)
			}
		}
		l.swept = now
	}
	e := l.entries[key]
	if e == nil {
		e = &limiterEntry{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.entries[key] = e
	}
	e.seen = now
	return e.limiter.Allow()
}

// ClientIP finds the caller's address. X-Forwarded-For is believed only when configured, because a client can set it to
// anything otherwise, and so dodge rate limits and forge audit logs. Even then only the last entry counts, which is the one
// the proxy in front of this server added. A proxy that overwrites the header leaves only that, and one that appends leaves
// whatever the client sent in front of it.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Values("X-Forwarded-For"); len(fwd) > 0 {
			last := fwd[len(fwd)-1]
			if i := strings.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if last = strings.TrimSpace(last); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// AddressKey is what a limit per address should count by. One IPv6 customer usually has a whole /64 to pick from.
func AddressKey(ip string) string {
	if addr, err := netip.ParseAddr(ip); err == nil && addr.Is6() && !addr.Is4In6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return ip
}
