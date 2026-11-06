package sip

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"elnova-streamkit.local/sip/rules"
)

const (
	defaultInvitesPerSecond = 20
	defaultInviteBurst      = 40
	defaultBanSeconds       = 600
	defaultBanAfter         = 20
	// A carrier named in an allow-list gets this many times the budget, because all its calls may come from one address.
	knownSourceFactor = 20
	failWindow        = time.Minute
	guardSweepEvery   = time.Minute
	// guardMaxSources bounds the memory a flood from many addresses can use. Past this, the idlest are forgotten.
	guardMaxSources = 100000
	// maxConnsPerSource and maxConns bound TCP and TLS connections, which cost more than a datagram.
	maxConnsPerSource = 32
	maxConns          = 8192
	refreshKnownEvery = 15 * time.Second
	// firstReadLimit is how long a new connection gets to send anything, which includes a TLS handshake. Connecting and then sending
	// nothing is cheap to do from many places, and an honest peer has a reason to connect.
	firstReadLimit = 10 * time.Second
)

// Parts of User-Agent headers that only scanning tools send. Dropping them without an answer gives such a tool nothing to go on. Real
// equipment does not send these, and something that lies about this is still stopped by everything else here.
var scannerAgents = []string{"friendly-scanner", "sipvicious", "sipcli", "sip-scan", "sundayddr", "pplsip", "vaxsipuseragent"}

func isScanner(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	for _, s := range scannerAgents {
		if strings.Contains(ua, s) {
			return true
		}
	}
	return false
}

type sourceState struct {
	limiter   *rate.Limiter
	fails     int
	failStart time.Time
	bannedTil time.Time
	lastSeen  time.Time
}

// guard is the first thing every packet meets. A phone-facing port is scanned from the moment it exists, and what is here means a
// scan costs a map lookup, not a database query.
type guard struct {
	perSecond rate.Limit
	burst     int
	banFor    time.Duration
	banAfter  int

	mu      sync.Mutex
	sources map[netip.Addr]*sourceState
	swept   time.Time
	known   atomic.Pointer[[]netip.Prefix]

	dropped atomic.Int64
	banned  atomic.Int64
}

func newGuard(perSecond float64, burst, banSeconds, banAfter int) *guard {
	if perSecond <= 0 {
		perSecond = defaultInvitesPerSecond
	}
	if burst <= 0 {
		burst = defaultInviteBurst
	}
	if banSeconds == 0 {
		banSeconds = defaultBanSeconds
	}
	if banAfter <= 0 {
		banAfter = defaultBanAfter
	}
	return &guard{perSecond: rate.Limit(perSecond), burst: burst, banFor: time.Duration(banSeconds) * time.Second, banAfter: banAfter, sources: map[netip.Addr]*sourceState{}, swept: time.Now()}
}

// isKnown reports whether an address is on a trunk's allow-list.
func (g *guard) isKnown(ip netip.Addr) bool {
	list := g.known.Load()
	if list == nil {
		return false
	}
	for _, p := range *list {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (g *guard) setKnown(masks []string) {
	list := make([]netip.Prefix, 0, len(masks))
	for _, m := range masks {
		if p, ok := rules.ParseMask(m); ok {
			list = append(list, p)
		}
	}
	g.known.Store(&list)
}

// refreshKnown keeps the list of carriers up to date until ctx ends.
func (g *guard) refreshKnown(ctx context.Context, load func(context.Context) ([]string, error), onError func(error)) {
	t := time.NewTicker(refreshKnownEvery)
	defer t.Stop()
	for {
		masks, err := load(ctx)
		if err == nil {
			g.setKnown(masks)
		} else if ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (g *guard) state(ip netip.Addr, now time.Time) *sourceState {
	s := g.sources[ip]
	if s == nil {
		limit, burst := g.perSecond, g.burst
		if g.isKnown(ip) {
			limit, burst = limit*knownSourceFactor, burst*knownSourceFactor
		}
		s = &sourceState{limiter: rate.NewLimiter(limit, burst)}
		g.sources[ip] = s
	}
	s.lastSeen = now
	g.sweep(now)
	return s
}

func (g *guard) sweep(now time.Time) {
	if now.Sub(g.swept) < guardSweepEvery && len(g.sources) < guardMaxSources {
		return
	}
	g.swept = now
	for ip, s := range g.sources {
		if now.After(s.bannedTil) && now.Sub(s.lastSeen) > guardSweepEvery {
			delete(g.sources, ip)
		}
	}
	if len(g.sources) >= guardMaxSources {
		// Every entry is fresh, which is a flood from a great many addresses. Bans are what must survive.
		for ip, s := range g.sources {
			if now.After(s.bannedTil) {
				delete(g.sources, ip)
			}
		}
	}
}

// allow says whether to look at a packet from an address at all.
func (g *guard) allow(ip netip.Addr, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state(ip, now)
	if now.Before(s.bannedTil) || !s.limiter.AllowN(now, 1) {
		g.dropped.Add(1)
		return false
	}
	return true
}

// failed counts a refused request, and bans a source that keeps getting refused. A carrier on an allow-list is never banned for this,
// because one misconfigured number could otherwise cut off all its calls.
func (g *guard) failed(ip netip.Addr, now time.Time) {
	if g.banFor < 0 || g.isKnown(ip) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state(ip, now)
	if now.Sub(s.failStart) > failWindow {
		s.failStart, s.fails = now, 0
	}
	s.fails++
	if s.fails >= g.banAfter && now.After(s.bannedTil) {
		s.bannedTil = now.Add(g.banFor)
		g.banned.Add(1)
	}
}

// parseSource reads the IP out of an address like the ones sockets give.
func parseSource(addr string) netip.Addr {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

// limitListener applies the guard to TCP and TLS connections, and limits how many there can be.
type limitListener struct {
	net.Listener
	g     *guard
	mu    sync.Mutex
	count map[netip.Addr]int
	total int
}

func newLimitListener(l net.Listener, g *guard) *limitListener {
	return &limitListener{Listener: l, g: g, count: map[netip.Addr]int{}}
}

// Accept returns the next connection that is allowed.
func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := parseSource(c.RemoteAddr().String())
		now := time.Now()
		if !ip.IsValid() || !l.g.allow(ip, now) {
			_ = c.Close()
			continue
		}
		l.mu.Lock()
		if l.total >= maxConns || l.count[ip] >= maxConnsPerSource {
			l.mu.Unlock()
			l.g.dropped.Add(1)
			_ = c.Close()
			continue
		}
		l.count[ip]++
		l.total++
		l.mu.Unlock()
		_ = c.SetReadDeadline(now.Add(firstReadLimit))
		return &limitConn{Conn: c, l: l, ip: ip}, nil
	}
}

type limitConn struct {
	net.Conn
	l     *limitListener
	ip    netip.Addr
	once  sync.Once
	first atomic.Bool
}

// Read lifts the first deadline as soon as there is something to read. After that a connection can stay quiet for as long as a call
// does: requests in a call come back along it.
func (c *limitConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.first.CompareAndSwap(false, true) {
		_ = c.Conn.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (c *limitConn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		c.l.count[c.ip]--
		if c.l.count[c.ip] <= 0 {
			delete(c.l.count, c.ip)
		}
		c.l.total--
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}
