// Package source decides which outside addresses the media workers may talk to. A caller supplies these
// addresses, and a worker connects to them from inside the network, so without these checks anyone with a project
// could reach internal services through it.
package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"elnova-streamkit.local/api/webhook"
)

// Errors for bad addresses. Their text reaches the API's caller.
var (
	ErrScheme   = errors.New("the address must start with http://, https://, rtmp:// or srt://")
	ErrPrivate  = errors.New("the address points at a private network")
	ErrHost     = errors.New("the address has no host")
	ErrSRTMode  = errors.New("an srt:// address must connect out: mode=listener is not allowed")
	ErrResolve  = errors.New("the address's host could not be resolved")
	ErrOutput   = errors.New("a stream output must start with rtmp:// or rtmps://")
	ErrRTMPUser = errors.New("an RTMP address cannot hold a username and password: put the stream key at the end of the path")
)

const resolveTimeout = 5 * time.Second

var inputSchemes = map[string]bool{"http": true, "https": true, "rtmp": true, "srt": true}

// CheckInput validates a URL input and looks up its host. It runs when the URL is saved, so a mistake shows up
// straight away, and again when a worker connects, because a name can change what it points at.
func CheckInput(ctx context.Context, raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the address is not a valid URL")
	}
	if !inputSchemes[strings.ToLower(u.Scheme)] {
		return nil, ErrScheme
	}
	if u.Hostname() == "" {
		return nil, ErrHost
	}
	if strings.EqualFold(u.Scheme, "srt") {
		if m := u.Query().Get("mode"); m != "" && !strings.EqualFold(m, "caller") {
			return nil, ErrSRTMode
		}
	}
	if strings.EqualFold(u.Scheme, "rtmp") && u.User != nil {
		return nil, ErrRTMPUser
	}
	if err := CheckHost(ctx, u.Hostname(), allowPrivate); err != nil {
		return nil, err
	}
	return u, nil
}

// CheckOutput validates a stream output. RTMP takes every option in the URL, so the path and query pass unchanged. A username
// and password are not accepted, because ffmpeg would need them on its command line. See Relay.
func CheckOutput(ctx context.Context, raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the address is not a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "rtmp", "rtmps":
	default:
		return nil, ErrOutput
	}
	if u.Hostname() == "" {
		return nil, ErrHost
	}
	if u.User != nil {
		return nil, ErrRTMPUser
	}
	if err := CheckHost(ctx, u.Hostname(), allowPrivate); err != nil {
		return nil, err
	}
	return u, nil
}

// CheckHost resolves a host and refuses it if any address it has is private. All of them are checked, since a
// client may use any of them.
func CheckHost(ctx context.Context, host string, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	_, err := PublicAddress(ctx, host)
	return err
}

// PublicAddress resolves a host and returns one of its addresses, or fails if any is private. Connecting to the
// returned address rather than the name means the lookup that was checked is the lookup that gets used.
func PublicAddress(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if webhook.IsPrivateIP(ip) {
			return nil, ErrPrivate
		}
		return ip, nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return nil, ErrPrivate
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, ErrResolve
	}
	for _, a := range addrs {
		if webhook.IsPrivateIP(a.IP) {
			return nil, ErrPrivate
		}
	}
	return addrs[0].IP, nil
}

// PinHost rewrites a URL to the address that was just checked. It works only for protocols where the name in the
// URL carries no meaning once connected, which leaves out anything using TLS, HTTP virtual hosts or RTMP's tcUrl.
func PinHost(ctx context.Context, u *url.URL, allowPrivate bool) (*url.URL, error) {
	if allowPrivate {
		return u, nil
	}
	ip, err := PublicAddress(ctx, u.Hostname())
	if err != nil {
		return nil, err
	}
	out := *u
	if port := u.Port(); port != "" {
		out.Host = net.JoinHostPort(ip.String(), port)
	} else if ip.To4() == nil {
		out.Host = "[" + ip.String() + "]"
	} else {
		out.Host = ip.String()
	}
	return &out, nil
}
