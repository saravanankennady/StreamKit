package store

import (
	"strings"
	"testing"
)

func TestCanTransition(t *testing.T) {
	all := []string{EgressStarting, EgressActive, EgressEnding, EgressComplete, EgressFailed, EgressAborted}
	allowed := map[[2]string]bool{
		{EgressStarting, EgressActive}:  true,
		{EgressStarting, EgressEnding}:  true,
		{EgressStarting, EgressFailed}:  true,
		{EgressStarting, EgressAborted}: true,
		{EgressActive, EgressEnding}:    true,
		{EgressActive, EgressComplete}:  true,
		{EgressActive, EgressFailed}:    true,
		{EgressActive, EgressAborted}:   true,
		{EgressEnding, EgressComplete}:  true,
		{EgressEnding, EgressFailed}:    true,
		{EgressEnding, EgressAborted}:   true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := CanTransition(from, to); got != allowed[[2]string{from, to}] {
				t.Errorf("%s -> %s: %v", from, to, got)
			}
		}
	}
	// Nothing leaves a final status, which is what makes it final.
	for _, final := range []string{EgressComplete, EgressFailed, EgressAborted} {
		if !EgressIsTerminal(final) {
			t.Errorf("%s should be terminal", final)
		}
		for _, to := range all {
			if CanTransition(final, to) {
				t.Errorf("%s -> %s should not be possible", final, to)
			}
		}
	}
	for _, running := range []string{EgressStarting, EgressActive, EgressEnding} {
		if EgressIsTerminal(running) {
			t.Errorf("%s should not be terminal", running)
		}
	}
}

func TestRedactStreamURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"rtmp://live.example.com/app/abcdefghijklmnop", "rtmp://live.example.com/app/{abc...nop}"},
		{"rtmp://live.example.com:1935/a/b/abcdefghijklmnop", "rtmp://live.example.com:1935/a/b/{abc...nop}"},
		{"rtmps://live.example.com/app/short", "rtmps://live.example.com/app/{***}"},
		{"rtmp://user:password@live.example.com/app/abcdefghijklmnop?token=secret", "rtmp://live.example.com/app/{abc...nop}"},
		{"rtmp://live.example.com", "rtmp://live.example.com"},
	} {
		got := RedactStreamURL(c.in)
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
		for _, secret := range []string{"password", "secret", "defghijklm"} {
			if strings.Contains(got, secret) {
				t.Errorf("%q leaked %q", got, secret)
			}
		}
	}
}
