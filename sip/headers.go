package sip

import (
	"strings"

	sipmsg "github.com/emiago/sipgo/sip"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/sip/rules"
)

const (
	maxMappedHeaders = 64
	maxHeaderValue   = 1024
)

// Headers that carry credentials never become attributes, or get handed to anyone in a room who asks.
var secretHeaders = map[string]bool{"authorization": true, "proxy-authorization": true, "www-authenticate": true, "proxy-authenticate": true}

// Headers well-known carriers send, which are always worth keeping. The names of these attributes are what integrations expect.
var wellKnownHeaders = map[string]string{
	"x-twilio-accountsid":            "sip.twilio.accountSid",
	"x-twilio-callsid":               "sip.twilio.callSid",
	"x-call_leg_id":                  "sip.telnyx.callLegID",
	"x-call_session_id":              "sip.telnyx.callSessionID",
	"x-amzn-connectcontactid":        "sip.amazon.contactId",
	"x-amzn-connectinitialcontactid": "sip.amazon.initialContactId",
}

// cleanHeaderValue makes a value from outside safe to pass on. Anything unprintable is dropped, and it is cut to a sensible length.
func cleanHeaderValue(v string) string {
	if len(v) > maxHeaderValue {
		v = v[:maxHeaderValue]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
}

type headerLister interface {
	Headers() []sipmsg.Header
}

// mapHeaders copies headers from a message into attributes as a trunk or request asks: named ones under the names given, and with
// include_headers, X- headers or all of them as sip.h.<lower-case name>. It also returns them all, for the RPC that reads them.
func mapHeaders(msg headerLister, attrs map[string]string, mapping map[string]string, include pb.SIPHeaderOptions) map[string]string {
	all := map[string]string{}
	lowerMapping := make(map[string]string, len(mapping))
	for h, a := range mapping {
		lowerMapping[strings.ToLower(h)] = a
	}
	n := 0
	for _, h := range msg.Headers() {
		name := h.Name()
		lower := strings.ToLower(name)
		if secretHeaders[lower] {
			continue
		}
		if n >= maxMappedHeaders {
			break
		}
		n++
		value := cleanHeaderValue(h.Value())
		all[name] = value
		if a, ok := wellKnownHeaders[lower]; ok {
			attrs[a] = value
		}
		if a, ok := lowerMapping[lower]; ok && a != "" {
			attrs[a] = value
		}
		switch include {
		case pb.SIPHeaderOptions_SIP_ALL_HEADERS:
			attrs[rules.AttrHeaderPrefix+lower] = value
		case pb.SIPHeaderOptions_SIP_X_HEADERS:
			if strings.HasPrefix(lower, "x-") {
				attrs[rules.AttrHeaderPrefix+lower] = value
			}
		}
	}
	return all
}
