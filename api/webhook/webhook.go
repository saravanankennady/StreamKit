// Package webhook delivers room events to customers' HTTP endpoints, with signatures and retries.
package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

const (
	maxAttempts    = 6
	attemptTimeout = 10 * time.Second
	pollInterval   = time.Second
	batchSize      = 50
	// A claimed delivery becomes available again after this, in case this node dies mid-send.
	claimLease = 2 * time.Minute
	// Signatures are valid this long, which limits replay.
	signatureTTL = 5 * time.Minute
)

// Backoff between attempts. The last one is about two minutes after the first failure's run of retries.
var backoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// Dispatcher sends queued deliveries.
type Dispatcher struct {
	store  *store.Store
	log    *slog.Logger
	client *http.Client
	wake   chan struct{}
}

// New creates a Dispatcher. Unless allowPrivate is set, it refuses to connect to private addresses.
// Checking at connect time, not when the URL is saved, also stops a domain that later resolves to an internal
// address.
func New(s *store.Store, log *slog.Logger, allowPrivate bool) *Dispatcher {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if IsPrivateIP(net.ParseIP(host)) {
				return fmt.Errorf("%w: %s", ErrPrivateAddress, host)
			}
			return nil
		}
	}
	return &Dispatcher{
		store: s,
		log:   log,
		wake:  make(chan struct{}, 1),
		client: &http.Client{
			Timeout:   attemptTimeout,
			Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: attemptTimeout},
			// A redirect could point anywhere, so a delivery only goes where it was configured to.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ErrPrivateAddress means a webhook URL resolved to an internal address.
var ErrPrivateAddress = errors.New("webhook URLs cannot point at private addresses")

// IsPrivateIP reports whether an address is on a private, loopback or link-local network.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, block := range nonPublicBlocks {
		if block.Contains(addr) {
			return true
		}
	}
	// An IPv6 address can carry an IPv4 one, for a gateway that may well deliver it, so that has to pass too.
	if v4, ok := embeddedIPv4(addr); ok {
		return IsPrivateIP(net.IP(v4.AsSlice()))
	}
	return false
}

// nonPublicBlocks are the ranges not on the public internet that net.IP has no method for: many clouds and networks use them
// internally.
var nonPublicBlocks = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",        // "This network".
		"100.64.0.0/10",    // Carrier-grade NAT, which many clouds use internally.
		"168.63.129.16/32", // Azure's host services, which is not in any private range.
		"192.0.0.0/24",     // IETF protocol assignments.
		"192.0.2.0/24",     // Documentation.
		"192.88.99.0/24",   // The old 6to4 relay.
		"198.18.0.0/15",    // Benchmarking, and in practice used for internal networks.
		"198.51.100.0/24",  // Documentation.
		"203.0.113.0/24",   // Documentation.
		"240.0.0.0/4",      // Reserved, and in practice used for internal networks.
		"::/128",           // Unspecified.
		"64:ff9b:1::/48",   // Local NAT64.
		"100::/64",         // Discard.
		"2001:db8::/32",    // Documentation.
		"fec0::/10",        // Deprecated site-local.
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// embeddedIPv4 finds an IPv4 address carried inside an IPv6 one: NAT64, 6to4, and the deprecated IPv4-compatible form.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	b := addr.As16()
	switch {
	case netip.MustParsePrefix("64:ff9b::/96").Contains(addr), netip.MustParsePrefix("::/96").Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case netip.MustParsePrefix("2002::/16").Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// Notify wakes the dispatcher right after queueing a delivery, without waiting for the next poll.
func (d *Dispatcher) Notify() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run sends deliveries until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
		for {
			due, err := d.store.ClaimDueDeliveries(ctx, batchSize, claimLease)
			if err != nil {
				if ctx.Err() == nil {
					d.log.Warn("could not claim webhook deliveries", "error", err)
				}
				break
			}
			if len(due) == 0 {
				break
			}
			for _, delivery := range due {
				d.attempt(ctx, delivery)
			}
		}
	}
}

func (d *Dispatcher) attempt(ctx context.Context, delivery store.Delivery) {
	status, responseStatus, lastError, next := d.send(ctx, delivery)
	if err := d.store.FinishDelivery(ctx, delivery.ID, status, responseStatus, lastError, next); err != nil && ctx.Err() == nil {
		d.log.Warn("could not record webhook delivery", "error", err, "delivery", delivery.ID)
	}
}

func (d *Dispatcher) send(ctx context.Context, delivery store.Delivery) (status string, responseStatus *int, lastError string, next time.Time) {
	retry := func(reason string, code *int) (string, *int, string, time.Time) {
		if delivery.Attempts >= maxAttempts {
			return "failed", code, reason, time.Now()
		}
		return "pending", code, reason, time.Now().Add(backoff[min(delivery.Attempts-1, len(backoff)-1)])
	}

	var projectID, url, keyID string
	var enabled bool
	err := d.store.DB.QueryRow(ctx, `SELECT project_id, url, api_key_id, enabled FROM webhooks WHERE id = $1`, delivery.WebhookID).
		Scan(&projectID, &url, &keyID, &enabled)
	if err != nil {
		return "failed", nil, "the webhook no longer exists", time.Now()
	}
	key, err := d.store.KeyByID(ctx, projectID, keyID)
	if err != nil {
		return "failed", nil, "the signing key has been revoked", time.Now()
	}
	req, err := BuildRequest(ctx, url, delivery.Payload, key)
	if err != nil {
		return "failed", nil, err.Error(), time.Now()
	}

	res, err := d.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrPrivateAddress) {
			return "failed", nil, err.Error(), time.Now()
		}
		return retry(err.Error(), nil)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	code := res.StatusCode
	switch {
	case code >= 200 && code < 300:
		return "delivered", &code, "", time.Now()
	case code == 429 || (code >= 500 && code != 501):
		return retry(fmt.Sprintf("received HTTP %d", code), &code)
	}
	// Other responses are the receiver's considered answer, and sending it again would not change it.
	return "failed", &code, fmt.Sprintf("received HTTP %d", code), time.Now()
}

// BuildRequest creates a signed delivery. The signature covers a hash of the exact body bytes.
func BuildRequest(ctx context.Context, target string, body []byte, key auth.Key) (*http.Request, error) {
	sum := sha256.Sum256(body)
	token, err := auth.Mint(key, &auth.Grants{Sha256: base64.StdEncoding.EncodeToString(sum[:])}, signatureTTL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Receivers parse this header as a raw token: a Bearer prefix fails their check.
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/webhook+json")
	req.Header.Set("User-Agent", "Elnova-StreamKit")
	return req, nil
}

// CheckURL does the checks possible when a URL is saved. The dialer repeats them when connecting.
func CheckURL(raw string, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return ErrPrivateAddress
	}
	if ip := net.ParseIP(host); ip != nil && IsPrivateIP(ip) {
		return ErrPrivateAddress
	}
	return nil
}
