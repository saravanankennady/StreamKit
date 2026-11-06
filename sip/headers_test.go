package sip

import (
	"strings"
	"testing"

	sipmsg "github.com/emiago/sipgo/sip"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/sip/media"
)

func requestWith(headers map[string]string) *sipmsg.Request {
	req := sipmsg.NewRequest(sipmsg.INVITE, sipmsg.Uri{Scheme: "sip", User: "a", Host: "b"})
	for k, v := range headers {
		req.AppendHeader(sipmsg.NewHeader(k, v))
	}
	return req
}

func TestMapHeaders(t *testing.T) {
	req := requestWith(map[string]string{
		"X-Customer":          "acme",
		"X-Other":             "two",
		"P-Asserted-Identity": "<sip:a@b>",
		"X-Twilio-CallSid":    "CA123",
		"Proxy-Authorization": "Digest secret",
		"Authorization":       "Digest secret",
	})
	attrs := map[string]string{}
	all := mapHeaders(req, attrs, map[string]string{"x-customer": "customer"}, pb.SIPHeaderOptions_SIP_X_HEADERS)
	if attrs["customer"] != "acme" {
		t.Errorf("a named header: %v", attrs)
	}
	if attrs["sip.h.x-other"] != "two" || attrs["sip.h.x-customer"] != "acme" {
		t.Errorf("X- headers: %v", attrs)
	}
	if _, ok := attrs["sip.h.p-asserted-identity"]; ok {
		t.Errorf("X- headers only should not map this: %v", attrs)
	}
	if attrs["sip.twilio.callSid"] != "CA123" {
		t.Errorf("a well-known header: %v", attrs)
	}
	for name, value := range all {
		if strings.Contains(value, "secret") {
			t.Errorf("%s reached the list anyone in the room can read", name)
		}
	}
	allAttrs := map[string]string{}
	mapHeaders(req, allAttrs, nil, pb.SIPHeaderOptions_SIP_ALL_HEADERS)
	if allAttrs["sip.h.p-asserted-identity"] != "<sip:a@b>" {
		t.Errorf("all headers: %v", allAttrs)
	}
	for name, value := range allAttrs {
		if strings.Contains(value, "secret") {
			t.Errorf("%s became an attribute", name)
		}
	}
	noneAttrs := map[string]string{}
	mapHeaders(req, noneAttrs, nil, pb.SIPHeaderOptions_SIP_NO_HEADERS)
	for name := range noneAttrs {
		if strings.HasPrefix(name, "sip.h.") {
			t.Errorf("%s was mapped with mapping off", name)
		}
	}
}

// A header from outside is only ever a value in an attribute, however many there are and whatever they hold.
func TestMapHeadersKeepsToLimits(t *testing.T) {
	headers := map[string]string{"X-Long": strings.Repeat("a", 5000), "X-Odd": "a\x01b\x7fc"}
	for i := range 200 {
		headers["X-"+strings.Repeat("n", i+1)] = "v"
	}
	attrs := map[string]string{}
	all := mapHeaders(requestWith(headers), attrs, nil, pb.SIPHeaderOptions_SIP_ALL_HEADERS)
	if len(all) > maxMappedHeaders {
		t.Errorf("kept %d headers", len(all))
	}
	if v, ok := all["X-Long"]; ok && len(v) > maxHeaderValue {
		t.Errorf("kept a value of %d bytes", len(v))
	}
	if v, ok := all["X-Odd"]; ok && v != "abc" {
		t.Errorf("control characters were kept: %q", v)
	}
}

func TestPolicyFor(t *testing.T) {
	if p := policyFor(nil); len(p.Codecs) != 2 || p.Encryption != media.EncryptDisable {
		t.Errorf("default: %+v", p)
	}
	require := pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_REQUIRE
	if p := policyFor(&pb.SIPMediaConfig{Encryption: &require}); p.Encryption != media.EncryptRequire {
		t.Errorf("require: %+v", p)
	}
	allow := pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_ALLOW
	if p := policyFor(&pb.SIPMediaConfig{Encryption: &allow}); p.Encryption != media.EncryptAllow {
		t.Errorf("allow: %+v", p)
	}
	p := policyFor(&pb.SIPMediaConfig{OnlyListedCodecs: true, Codecs: []*pb.SIPCodec{{Name: "pcma"}}})
	if len(p.Codecs) != 1 || p.Codecs[0] != media.CodecPCMA {
		t.Errorf("listed: %+v", p)
	}
	// Asking only for something this cannot do is refused later, rather than quietly using something else.
	if p := policyFor(&pb.SIPMediaConfig{OnlyListedCodecs: true, Codecs: []*pb.SIPCodec{{Name: "G722"}}}); len(p.Codecs) != 0 {
		t.Errorf("unavailable: %+v", p)
	}
	// Adding to the defaults something this cannot do changes nothing.
	if p := policyFor(&pb.SIPMediaConfig{Codecs: []*pb.SIPCodec{{Name: "G722"}}}); len(p.Codecs) != 2 {
		t.Errorf("added: %+v", p)
	}
}

func TestContactUnroutable(t *testing.T) {
	req := func(contact string) *sipmsg.Request {
		r := sipmsg.NewRequest(sipmsg.INVITE, sipmsg.Uri{Scheme: "sip", User: "a", Host: "b"})
		r.AppendHeader(&sipmsg.ContactHeader{Address: sipmsg.Uri{Scheme: "sip", Host: contact}})
		return r
	}
	for _, c := range []struct {
		contact, source string
		want            bool
	}{
		{"10.0.0.5", "203.0.113.1", true},
		{"192.168.1.5", "203.0.113.1", true},
		{"127.0.0.1", "203.0.113.1", true},
		{"203.0.113.2", "203.0.113.1", false},
		{"carrier.example.com", "203.0.113.1", false},
		{"10.0.0.5", "10.0.0.9", false},
		{"127.0.0.1", "127.0.0.1", false},
	} {
		if got := contactUnroutable(req(c.contact), addr(c.source)); got != c.want {
			t.Errorf("%s from %s: got %v", c.contact, c.source, got)
		}
	}
}
