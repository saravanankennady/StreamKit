package rules

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

// MaxMediaTimeout is the longest media timeout a trunk or request may ask for.
const MaxMediaTimeout = 10 * time.Minute

const (
	maxHeaderValueLen = 1024
	maxHeaderNameLen  = 255
	// PinMask stands in for a PIN in what the API returns, so that a client can see there is one, and can send a rule back
	// unchanged without removing it. A real PIN is only digits, so it cannot be mistaken for one.
	PinMask = "****"
	maxPin  = 16
)

// Headers that a SIP stack owns. Setting one of these by hand would corrupt the dialog, or be a way to spoof a route.
var forbiddenHeaders = map[string]bool{
	"via": true, "from": true, "to": true, "call-id": true, "cseq": true, "contact": true, "max-forwards": true, "route": true,
	"record-route": true, "proxy-authorization": true, "authorization": true, "proxy-authenticate": true, "www-authenticate": true,
	"content-length": true, "content-type": true, "content-encoding": true, "require": true, "supported": true, "allow": true, "allow-events": true,
	"accept": true, "accept-encoding": true, "accept-language": true, "event": true, "expires": true, "refer-to": true, "referred-by": true, "reply-to": true,
	"f": true, "t": true, "i": true, "v": true, "m": true, "l": true, "c": true, "k": true, "r": true, "o": true, "u": true,
}

func isToken(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case strings.ContainsRune("-.!%*_+`'~", r):
		default:
			return false
		}
	}
	return true
}

// ValidateHeaderName checks a SIP header name. With restrict set, names the SIP stack owns are refused.
func ValidateHeaderName(name string, restrict bool) error {
	if name == "" {
		return errors.New("header name cannot be empty")
	}
	if len(name) > maxHeaderNameLen {
		return errors.New("header name is too long")
	}
	if !isToken(name) {
		return fmt.Errorf("header name %q contains invalid characters", name)
	}
	if restrict && forbiddenHeaders[strings.ToLower(name)] {
		return fmt.Errorf("header %s cannot be set", name)
	}
	return nil
}

// ValidateHeaderValue checks a header's value. Line breaks are what this is really about: one would let a value write
// further headers into the message.
func ValidateHeaderValue(name, value string) error {
	if len(value) > maxHeaderValueLen {
		return fmt.Errorf("header %s: the value is too long", name)
	}
	for _, r := range value {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r >= 0x7f {
			return fmt.Errorf("header %s: the value may only hold printable ASCII", name)
		}
	}
	return nil
}

// ValidateHeaders checks a map of headers to send.
func ValidateHeaders(headers map[string]string) error {
	for k, v := range headers {
		if err := ValidateHeaderName(k, true); err != nil {
			return err
		}
		if err := ValidateHeaderValue(k, v); err != nil {
			return err
		}
	}
	return nil
}

func validateAttrToHeader(m map[string]string) error {
	for _, h := range m {
		if err := ValidateHeaderName(h, true); err != nil {
			return err
		}
	}
	return nil
}

func validateHeaderToAttr(m map[string]string) error {
	for h := range m {
		if err := ValidateHeaderName(h, false); err != nil {
			return err
		}
	}
	return nil
}

// ValidateHostname checks that a value is a host name or IP, with an optional port, and not a SIP URI.
func ValidateHostname(value, field string) error {
	if strings.Contains(value, "transport=") {
		return errors.New(field + " should not contain a transport parameter")
	}
	if strings.ContainsAny(value, "@;<> \t\r\n\"") || strings.HasPrefix(value, "sip:") || strings.HasPrefix(value, "sips:") {
		return errors.New(field + " should be a hostname or IP, not a SIP URI")
	}
	return nil
}

// ValidateMedia checks a media configuration.
func ValidateMedia(m *pb.SIPMediaConfig) error {
	if m == nil {
		return nil
	}
	for _, c := range m.Codecs {
		if c.Name == "" {
			return errors.New("missing codec name")
		}
		if strings.ContainsAny(c.Name, " \t\r\n/") {
			return errors.New("invalid codec name")
		}
	}
	if m.MediaTimeout != nil {
		d := m.MediaTimeout.AsDuration()
		if d < 0 {
			return errors.New("media_timeout must not be negative")
		}
		if d > MaxMediaTimeout {
			return fmt.Errorf("media_timeout must not exceed %v", MaxMediaTimeout)
		}
	}
	return nil
}

// ValidateInbound checks one inbound trunk. requireAuth refuses a trunk that would take calls from anyone who finds it. Whether it
// has credentials is a separate question from whether they were sent in this request, so the caller says so.
func ValidateInbound(t *pb.SIPInboundTrunkInfo, requireAuth bool) error {
	hasAuth := t.AuthUsername != "" || t.AuthPassword != ""
	if (t.AuthUsername != "") != (t.AuthPassword != "") {
		return errors.New("auth_username and auth_password must be set together")
	}
	if !hasAuth && len(t.AllowedAddresses) == 0 && len(t.Numbers) == 0 {
		return errors.New("for security, one of the fields must be set: AuthUsername+AuthPassword, AllowedAddresses or Numbers")
	}
	for _, mask := range t.AllowedAddresses {
		if !validMask(mask) {
			return fmt.Errorf("allowed_addresses: %q is not an address, network or host name", mask)
		}
	}
	if requireAuth && !IsAuthenticated(t) {
		return errors.New("for security, an inbound trunk needs allowed_addresses that name a real network, or auth_username and auth_password. Numbers alone do not say who is calling")
	}
	if err := ValidateMedia(t.Media); err != nil {
		return err
	}
	if c := t.RingingTimeoutStatus; c != pb.SIPStatusCode_SIP_STATUS_UNKNOWN && (c < 400 || c > 699) {
		return fmt.Errorf("ringing timeout status must be 4xx-6xx, got %d", c)
	}
	if err := ValidateHeaders(t.Headers); err != nil {
		return err
	}
	if err := validateAttrToHeader(t.AttributesToHeaders); err != nil {
		return err
	}
	return validateHeaderToAttr(t.HeadersToAttributes)
}

// ValidateOutbound checks one outbound trunk.
func ValidateOutbound(t *pb.SIPOutboundTrunkInfo) error {
	if len(t.Numbers) == 0 {
		return errors.New("no trunk numbers specified")
	}
	if t.Address == "" {
		return errors.New("no outbound address specified")
	}
	if (t.AuthUsername != "") != (t.AuthPassword != "") {
		return errors.New("auth_username and auth_password must be set together")
	}
	if err := ValidateHostname(t.Address, "trunk address"); err != nil {
		return err
	}
	if err := ValidateHostname(t.FromHost, "from_host"); err != nil {
		return err
	}
	if err := ValidateHeaders(t.Headers); err != nil {
		return err
	}
	if err := validateAttrToHeader(t.AttributesToHeaders); err != nil {
		return err
	}
	if err := validateHeaderToAttr(t.HeadersToAttributes); err != nil {
		return err
	}
	return ValidateMedia(t.Media)
}

// ValidateRule checks one dispatch rule.
func ValidateRule(r *pb.SIPDispatchRuleInfo) error {
	if r.Rule == nil || r.Rule.Rule == nil {
		return errors.New("missing rule")
	}
	room, pin, err := PinAndRoom(r)
	if err != nil {
		return err
	}
	if _, ok := r.Rule.Rule.(*pb.SIPDispatchRule_DispatchRuleDirect); ok && room == "" {
		return errors.New("a direct rule needs room_name")
	}
	if pin != "" && pin != PinMask {
		if len(pin) > maxPin {
			return fmt.Errorf("a PIN can have at most %d digits", maxPin)
		}
		for _, c := range pin {
			if c < '0' || c > '9' {
				return errors.New("a PIN can only hold digits")
			}
		}
	}
	return ValidateMedia(r.Media)
}

func applyList(dst *[]string, u *pb.ListUpdate) {
	if u == nil {
		return
	}
	if u.Clear {
		*dst = nil
	}
	if len(u.Set) != 0 {
		*dst = slices.Clone(u.Set)
	}
	for _, v := range u.Remove {
		*dst = slices.DeleteFunc(*dst, func(x string) bool { return x == v })
	}
	for _, v := range u.Add {
		if !slices.Contains(*dst, v) {
			*dst = append(*dst, v)
		}
	}
}

func validateList(u *pb.ListUpdate) error {
	if u == nil {
		return nil
	}
	if len(u.Set) != 0 && (len(u.Add) != 0 || len(u.Remove) != 0) {
		return errors.New("a list update can set a list, or add and remove items, but not both")
	}
	if u.Clear && len(u.Set) != 0 {
		return errors.New("a list update cannot clear and set a list")
	}
	return nil
}

func applyOptional[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// ApplyInboundUpdate applies a partial update to a trunk.
func ApplyInboundUpdate(t *pb.SIPInboundTrunkInfo, u *pb.SIPInboundTrunkUpdate) error {
	for _, l := range []*pb.ListUpdate{u.Numbers, u.AllowedAddresses, u.AllowedNumbers} {
		if err := validateList(l); err != nil {
			return err
		}
	}
	applyList(&t.Numbers, u.Numbers)
	applyList(&t.AllowedAddresses, u.AllowedAddresses)
	applyList(&t.AllowedNumbers, u.AllowedNumbers)
	applyOptional(&t.AuthUsername, u.AuthUsername)
	applyOptional(&t.AuthPassword, u.AuthPassword)
	applyOptional(&t.AuthRealm, u.AuthRealm)
	applyOptional(&t.Name, u.Name)
	applyOptional(&t.Metadata, u.Metadata)
	if u.Media != nil {
		t.Media = proto.Clone(u.Media).(*pb.SIPMediaConfig)
	}
	applyOptional(&t.MediaEncryption, u.MediaEncryption)
	return nil
}

// ApplyOutboundUpdate applies a partial update to a trunk.
func ApplyOutboundUpdate(t *pb.SIPOutboundTrunkInfo, u *pb.SIPOutboundTrunkUpdate) error {
	if err := validateList(u.Numbers); err != nil {
		return err
	}
	applyOptional(&t.Address, u.Address)
	applyOptional(&t.Transport, u.Transport)
	applyOptional(&t.DestinationCountry, u.DestinationCountry)
	applyList(&t.Numbers, u.Numbers)
	applyOptional(&t.AuthUsername, u.AuthUsername)
	applyOptional(&t.AuthPassword, u.AuthPassword)
	applyOptional(&t.Name, u.Name)
	applyOptional(&t.Metadata, u.Metadata)
	applyOptional(&t.FromHost, u.FromHost)
	if u.Media != nil {
		t.Media = proto.Clone(u.Media).(*pb.SIPMediaConfig)
	}
	applyOptional(&t.MediaEncryption, u.MediaEncryption)
	return nil
}

// ApplyRuleUpdate applies a partial update to a rule.
func ApplyRuleUpdate(r *pb.SIPDispatchRuleInfo, u *pb.SIPDispatchRuleUpdate) error {
	if err := validateList(u.TrunkIds); err != nil {
		return err
	}
	applyList(&r.TrunkIds, u.TrunkIds)
	if u.Rule != nil {
		r.Rule = proto.Clone(u.Rule).(*pb.SIPDispatchRule)
	}
	applyOptional(&r.Name, u.Name)
	applyOptional(&r.Metadata, u.Metadata)
	for k, v := range u.Attributes {
		if v == "" {
			delete(r.Attributes, k)
			continue
		}
		if r.Attributes == nil {
			r.Attributes = map[string]string{}
		}
		r.Attributes[k] = v
	}
	if u.Media != nil {
		r.Media = proto.Clone(u.Media).(*pb.SIPMediaConfig)
	}
	applyOptional(&r.MediaEncryption, u.MediaEncryption)
	return nil
}

// ValidateCreateParticipant checks a request to place a call.
func ValidateCreateParticipant(p *pb.CreateSIPParticipantRequest) error {
	if p.SipTrunkId == "" && p.Trunk == nil {
		return errors.New("missing sip trunk id")
	}
	if p.Trunk != nil {
		t := p.Trunk
		if t.Hostname == "" {
			return errors.New("no outbound hostname specified")
		}
		if err := ValidateHostname(t.Hostname, "trunk hostname"); err != nil {
			return err
		}
		if err := ValidateHostname(t.FromHost, "from_host"); err != nil {
			return err
		}
		if err := validateAttrToHeader(t.AttributesToHeaders); err != nil {
			return err
		}
		if err := validateHeaderToAttr(t.HeadersToAttributes); err != nil {
			return err
		}
		if (t.AuthUsername != "") != (t.AuthPassword != "") {
			return errors.New("auth_username and auth_password must be set together")
		}
	}
	if p.SipCallTo == "" {
		return errors.New("missing sip callee number")
	}
	if strings.Contains(p.SipCallTo, "@") {
		return errors.New("SipCallTo should be a phone number or SIP user, not a full SIP URI")
	}
	if !isToken(p.SipCallTo) {
		return errors.New("SipCallTo contains invalid characters")
	}
	if p.SipNumber != "" && !isToken(p.SipNumber) {
		return errors.New("SipNumber contains invalid characters")
	}
	if p.ToUserOverride != "" {
		if strings.Contains(p.ToUserOverride, "@") {
			return errors.New("ToUserOverride should be a phone number or SIP user, not a full SIP URI")
		}
		if !isToken(p.ToUserOverride) {
			return errors.New("ToUserOverride contains invalid characters")
		}
	}
	if p.RoomName == "" {
		return errors.New("missing room name")
	}
	if p.DisplayName != nil {
		if len(*p.DisplayName) > 128 {
			return errors.New("DisplayName too long (max 128 characters)")
		}
		if _, err := strconv.Unquote(`"` + *p.DisplayName + `"`); err != nil {
			return errors.New("DisplayName must be a valid quoted string")
		}
	}
	if err := ValidateHeaders(p.Headers); err != nil {
		return err
	}
	for _, c := range p.Dtmf {
		if !strings.ContainsRune("0123456789*#ABCDabcdw", c) {
			return errors.New("dtmf can only hold 0-9, *, #, A-D and w")
		}
	}
	return ValidateMedia(p.Media)
}

// NormalizeTransferTo checks a transfer target and puts it in the form a Refer-To header wants. Only SIP and tel URIs are allowed,
// so this cannot be used to send a phone somewhere strange.
func NormalizeTransferTo(to string) (string, error) {
	inner := to
	if strings.HasPrefix(to, "<") && strings.HasSuffix(to, ">") {
		inner = to[1 : len(to)-1]
	}
	if strings.ContainsAny(inner, "<>\r\n \t") {
		return "", errors.New("transfer_to must be a valid SIP(s) or TEL URI (sip:, sips: or tel:)")
	}
	switch {
	case strings.HasPrefix(inner, "sip:"), strings.HasPrefix(inner, "sips:"):
		return "<" + inner + ">", nil
	case strings.HasPrefix(inner, "tel:"):
		return inner, nil
	}
	return "", errors.New("transfer_to must be a valid SIP(s) or TEL URI (sip:, sips: or tel:)")
}
