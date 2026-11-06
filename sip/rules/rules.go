// Package rules decides what happens to a call: which inbound trunk an INVITE belongs to, which dispatch rule applies, and what
// room and identity that gives. It does no input or output, so both the API and the worker use it.
//
// The matching follows the reference service's behaviour, because integrators see it. It differs on purpose where the reference fails open: see docs/deployment/sip.md, under "Differences from
// the reference".
package rules

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"elnova-streamkit.local/pkg/pb"
)

// Names of participant attributes. Agent frameworks and integrations read these, so they are part of the contract.
const (
	AttrCallID       = "sip.callID"
	AttrCallIDFull   = "sip.callIDFull"
	AttrCallTag      = "sip.callTag"
	AttrTrunkID      = "sip.trunkID"
	AttrRuleID       = "sip.ruleID"
	AttrTrunkNumber  = "sip.trunkPhoneNumber"
	AttrPhoneNumber  = "sip.phoneNumber"
	AttrHostname     = "sip.hostname"
	AttrCallStatus   = "sip.callStatus"
	AttrHeaderPrefix = "sip.h."
	AttrInviteTime   = "lk.sip.inviteTime"
)

// Call is what an INVITE says about itself, and where it came from.
type Call struct {
	ID       string
	FromUser string
	FromHost string
	ToUser   string
	ToHost   string
	SourceIP string
}

// ErrConflict means more than one trunk claims a call. Nothing can tell which was meant, so it is refused.
var ErrConflict = errors.New("more than one SIP trunk matched")

// ErrNoRule means no dispatch rule applies to a call.
var ErrNoRule = errors.New("no SIP dispatch rule matched")

var (
	numberPattern = regexp.MustCompile(`^\+?[\d\- ()]+$`)
	numberStrip   = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
)

// NormalizeNumber removes formatting from a phone number and makes sure it starts with a plus. Anything that is not a phone
// number, such as a SIP user name, comes back unchanged.
func NormalizeNumber(num string) string {
	if num == "" || !numberPattern.MatchString(num) {
		return num
	}
	num = numberStrip.Replace(num)
	if !strings.HasPrefix(num, "+") {
		return "+" + num
	}
	return num
}

// NormalizeNumbers returns the normal forms of a list, without repeats.
func NormalizeNumbers(numbers []string) []string {
	out := make([]string, 0, len(numbers))
	for _, n := range numbers {
		n = NormalizeNumber(n)
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// CleanUser reduces the user part of a SIP address to something safe to put in a room name, an identity and an attribute. It
// comes from whoever sent the INVITE, so it cannot be trusted to be sensible.
func CleanUser(user string) string {
	const maxLen = 64
	var b strings.Builder
	for _, r := range user {
		if b.Len() >= maxLen {
			break
		}
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '+', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func matchNumbers(num string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	norm := NormalizeNumber(num)
	for _, a := range allowed {
		if a == num || NormalizeNumber(a) == norm {
			return true
		}
	}
	return false
}

// ParseMask reads an entry in an allow-list: 1.2.3.4, 1.2.3.0/24, [::1] or [::]/8. Anything else is taken to be a host name.
func ParseMask(mask string) (netip.Prefix, bool) {
	addr, bits, hasBits := strings.Cut(mask, "/")
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
	}
	if !hasBits {
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			return netip.Prefix{}, false
		}
		return netip.PrefixFrom(ip, ip.BitLen()), true
	}
	pref, err := netip.ParsePrefix(addr + "/" + bits)
	if err != nil {
		return netip.Prefix{}, false
	}
	return pref, true
}

func validMask(mask string) bool {
	return mask != "" && !strings.ContainsAny(mask, "()+*;, \t\n\r") && !strings.Contains(mask, "://")
}

// sourceAllowed checks a call against a trunk's allow-list. It fails closed: a source address that cannot be read matches nothing,
// and neither does a bad entry. A From host can say anything, so a host name in the list only counts when the trunk also checks a
// password.
func sourceAllowed(t *pb.SIPInboundTrunkInfo, sourceIP, fromHost string) bool {
	if len(t.AllowedAddresses) == 0 {
		return true
	}
	ip, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, mask := range t.AllowedAddresses {
		if !validMask(mask) {
			continue
		}
		if pref, ok := ParseMask(mask); ok {
			if pref.Contains(ip) {
				return true
			}
			continue
		}
		if HasDigest(t) && mask == fromHost {
			return true
		}
	}
	return false
}

// HasDigest reports whether a trunk checks a password.
func HasDigest(t *pb.SIPInboundTrunkInfo) bool {
	return t.GetAuthUsername() != "" && t.GetAuthPassword() != ""
}

// narrowMask reports whether an allow-list entry names a real network rather than most of the internet.
func narrowMask(mask string) bool {
	if !validMask(mask) {
		return false
	}
	pref, ok := ParseMask(mask)
	if !ok {
		return false
	}
	if pref.Addr().Is4() || pref.Addr().Is4In6() {
		return pref.Bits() >= 8
	}
	return pref.Bits() >= 16
}

// IsAuthenticated reports whether a trunk checks who is calling at all. A list of numbers is not a check: anyone can dial them.
func IsAuthenticated(t *pb.SIPInboundTrunkInfo) bool {
	if HasDigest(t) {
		return true
	}
	return slices.ContainsFunc(t.GetAllowedAddresses(), narrowMask)
}

// MatchTrunk finds the trunk for a call, or nil. A trunk for the called number beats one for any number. Two of the same kind is
// a conflict.
func MatchTrunk(trunks []*pb.SIPInboundTrunkInfo, c Call) (*pb.SIPInboundTrunkInfo, error) {
	var specific, wildcard *pb.SIPInboundTrunkInfo
	wildcards := 0
	calledNorm := NormalizeNumber(c.ToUser)
	for _, t := range trunks {
		if !matchNumbers(c.FromUser, t.AllowedNumbers) || !sourceAllowed(t, c.SourceIP, c.FromHost) {
			continue
		}
		if len(t.Numbers) == 0 {
			wildcard = t
			wildcards++
			continue
		}
		for _, num := range t.Numbers {
			if num == c.ToUser || NormalizeNumber(num) == calledNorm {
				if specific != nil {
					return nil, ErrConflict
				}
				specific = t
				break
			}
		}
	}
	if specific != nil {
		return specific, nil
	}
	if wildcards > 1 {
		return nil, ErrConflict
	}
	return wildcard, nil
}

// ValidateInboundTrunks looks for two trunks that could claim the same call: the same number, and the same calling number or
// none. Sharing a number is fine when their allowed calling numbers do not overlap.
func ValidateInboundTrunks(trunks []*pb.SIPInboundTrunkInfo) error {
	byCalled := map[string]map[string]*pb.SIPInboundTrunkInfo{}
	for _, t := range trunks {
		called := NormalizeNumbers(t.Numbers)
		if len(called) == 0 {
			called = []string{""}
		}
		calling := NormalizeNumbers(t.AllowedNumbers)
		if len(calling) == 0 {
			calling = []string{""}
		}
		for _, to := range called {
			seen := byCalled[to]
			if seen == nil {
				seen = map[string]*pb.SIPInboundTrunkInfo{}
				byCalled[to] = seen
			}
			for _, from := range calling {
				if other := seen[from]; other != nil && other.SipTrunkId != t.SipTrunkId {
					// Their allow-lists may keep them apart, when each names a different network.
					if addressesDisjoint(t, other) {
						continue
					}
					return fmt.Errorf("conflicting inbound SIP trunks: %q and %q, using the same number %q", printID(t.SipTrunkId), printID(other.SipTrunkId), to)
				}
				seen[from] = t
			}
		}
	}
	return nil
}

func addressesDisjoint(a, b *pb.SIPInboundTrunkInfo) bool {
	if len(a.AllowedAddresses) == 0 || len(b.AllowedAddresses) == 0 {
		return false
	}
	for _, m1 := range a.AllowedAddresses {
		p1, ok := ParseMask(m1)
		if !ok {
			return false
		}
		for _, m2 := range b.AllowedAddresses {
			p2, ok := ParseMask(m2)
			if !ok || p1.Overlaps(p2) {
				return false
			}
		}
	}
	return true
}

func printID(id string) string {
	if id == "" {
		return "<new>"
	}
	return id
}

// PinAndRoom returns a rule's PIN and its room name or prefix.
func PinAndRoom(info *pb.SIPDispatchRuleInfo) (room, pin string, err error) {
	switch r := info.GetRule().GetRule().(type) {
	case *pb.SIPDispatchRule_DispatchRuleDirect:
		return r.DispatchRuleDirect.GetRoomName(), r.DispatchRuleDirect.GetPin(), nil
	case *pb.SIPDispatchRule_DispatchRuleIndividual:
		return r.DispatchRuleIndividual.GetRoomPrefix(), r.DispatchRuleIndividual.GetPin(), nil
	case *pb.SIPDispatchRule_DispatchRuleCallee:
		return r.DispatchRuleCallee.GetRoomPrefix(), r.DispatchRuleCallee.GetPin(), nil
	}
	return "", "", errors.New("unsupported SIP dispatch rule")
}

// SetPin changes a rule's PIN.
func SetPin(info *pb.SIPDispatchRuleInfo, pin string) {
	switch r := info.GetRule().GetRule().(type) {
	case *pb.SIPDispatchRule_DispatchRuleDirect:
		r.DispatchRuleDirect.Pin = pin
	case *pb.SIPDispatchRule_DispatchRuleIndividual:
		r.DispatchRuleIndividual.Pin = pin
	case *pb.SIPDispatchRule_DispatchRuleCallee:
		r.DispatchRuleCallee.Pin = pin
	}
}

// Priority orders rules: lower wins. PIN-protected rules come before open ones, then direct, caller and callee in that order, and
// a rule that does not name the calling or called number loses to one that does.
func Priority(info *pb.SIPDispatchRuleInfo) int32 {
	var p int32
	switch r := info.GetRule().GetRule().(type) {
	case *pb.SIPDispatchRule_DispatchRuleDirect:
		p = 100
		if r.DispatchRuleDirect.GetPin() != "" {
			p = 0
		}
	case *pb.SIPDispatchRule_DispatchRuleIndividual:
		p = 101
		if r.DispatchRuleIndividual.GetPin() != "" {
			p = 1
		}
	case *pb.SIPDispatchRule_DispatchRuleCallee:
		p = 102
		if r.DispatchRuleCallee.GetPin() != "" {
			p = 2
		}
	default:
		return 1 << 30
	}
	if len(info.InboundNumbers) == 0 {
		p += 1000
	}
	if len(info.Numbers) == 0 {
		p += 1000
	}
	return p
}

func better(a, b *pb.SIPDispatchRuleInfo) bool {
	first, second := Priority(a), Priority(b)
	if first != second {
		return first < second
	}
	ra, _, _ := PinAndRoom(a)
	rb, _, _ := PinAndRoom(b)
	return ra < rb
}

// SortRules puts rules in the order they would be tried.
func SortRules(rules []*pb.SIPDispatchRuleInfo) {
	sort.SliceStable(rules, func(i, j int) bool { return better(rules[i], rules[j]) })
}

// ValidateRules looks for two rules for the same trunk, calling number, called number and PIN.
func ValidateRules(rules []*pb.SIPDispatchRuleInfo) error {
	type key struct{ pin, trunk, from, to string }
	seen := map[key]*pb.SIPDispatchRuleInfo{}
	orAny := func(list []string) []string {
		if len(list) == 0 {
			return []string{""}
		}
		return list
	}
	for _, r := range rules {
		_, pin, err := PinAndRoom(r)
		if err != nil {
			return err
		}
		for _, trunk := range orAny(r.TrunkIds) {
			for _, from := range orAny(r.InboundNumbers) {
				for _, to := range orAny(r.Numbers) {
					k := key{pin, trunk, NormalizeNumber(from), NormalizeNumber(to)}
					if other := seen[k]; other != nil && other.SipDispatchRuleId != r.SipDispatchRuleId {
						return fmt.Errorf("a dispatch rule for the same trunk, inbound number, number and PIN already exists: %q", printID(other.SipDispatchRuleId))
					}
					seen[k] = r
				}
			}
		}
	}
	return nil
}

// MatchRule finds the best rule for a call. A rule that names the trunk beats one for any trunk. With a PIN sent, only rules with
// exactly that PIN match, so a wrong PIN can never fall through to an open rule. With noPin set, rules with a PIN are skipped.
func MatchRule(trunk *pb.SIPInboundTrunkInfo, rules []*pb.SIPDispatchRuleInfo, c Call, pin string, noPin bool) (*pb.SIPDispatchRuleInfo, error) {
	if err := ValidateRules(rules); err != nil {
		return nil, err
	}
	var specific, wildcard *pb.SIPDispatchRuleInfo
	for _, r := range rules {
		if len(r.InboundNumbers) != 0 && !matchNumbers(c.FromUser, r.InboundNumbers) {
			continue
		}
		if len(r.Numbers) != 0 && !matchNumbers(c.ToUser, r.Numbers) {
			continue
		}
		_, rulePin, err := PinAndRoom(r)
		if err != nil {
			continue
		}
		if noPin && rulePin != "" {
			continue
		}
		if pin != "" && rulePin != pin {
			continue
		}
		if len(r.TrunkIds) == 0 {
			if wildcard == nil || better(r, wildcard) {
				wildcard = r
			}
			continue
		}
		if trunk == nil || !slices.Contains(r.TrunkIds, trunk.SipTrunkId) {
			continue
		}
		if specific == nil || better(r, specific) {
			specific = r
		}
	}
	if specific != nil {
		return specific, nil
	}
	if wildcard != nil {
		return wildcard, nil
	}
	return nil, ErrNoRule
}

// Dispatch is what a call gets from its trunk and rule.
type Dispatch struct {
	// NeedPin means the caller must enter the rule's PIN, and the rest of this does not apply yet.
	NeedPin bool

	TrunkID    string
	RuleID     string
	Room       string
	Identity   string
	Name       string
	Metadata   string
	Attributes map[string]string
	RoomConfig *pb.RoomConfiguration
	Media      *pb.SIPMediaConfig
	// FromUser and ToUser are the numbers it is fine to keep on record: empty when the phone number is hidden.
	FromUser string
	ToUser   string

	Headers              map[string]string
	HeadersToAttributes  map[string]string
	AttributesToHeaders  map[string]string
	IncludeHeaders       pb.SIPHeaderOptions
	RingingTimeout       *durationpb.Duration
	RingingTimeoutStatus pb.SIPStatusCode
	MaxCallDuration      *durationpb.Duration
}

func shortRandom() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

// HideNumber gives the identity and display number for a caller whose number should not be shown.
func HideNumber(from string) (identity, shown string) {
	h := sha256.Sum256([]byte(from))
	shown = from
	if len(shown) > 4 {
		shown = shown[len(shown)-4:]
	} else if len(shown) > 1 {
		shown = shown[len(shown)-1:]
	}
	return "sip_" + hex.EncodeToString(h[:8]), shown
}

// Evaluate works out what a call gets from its trunk and rule. pin is what the caller has entered.
func Evaluate(trunk *pb.SIPInboundTrunkInfo, rule *pb.SIPDispatchRuleInfo, c Call, pin string) (*Dispatch, error) {
	if rule == nil {
		return nil, errors.New("no dispatch rule")
	}
	room, rulePin, err := PinAndRoom(rule)
	if err != nil {
		return nil, err
	}
	d := &Dispatch{TrunkID: trunk.GetSipTrunkId(), RuleID: rule.GetSipDispatchRuleId()}
	d.Media = MergeMedia(UpgradeMedia(trunk.GetMedia(), trunk.GetMediaEncryption()), UpgradeMedia(rule.GetMedia(), rule.GetMediaEncryption()))
	if rulePin != "" {
		if pin == "" {
			d.NeedPin = true
			return d, nil
		}
		if pin != rulePin {
			return nil, errors.New("incorrect PIN")
		}
	}
	from, to := CleanUser(c.FromUser), CleanUser(c.ToUser)
	attrs := maps.Clone(rule.Attributes)
	if attrs == nil {
		attrs = map[string]string{}
	}
	attrs[AttrCallID] = c.ID
	attrs[AttrTrunkID] = d.TrunkID
	attrs[AttrRuleID] = d.RuleID
	d.Identity = "sip_" + from
	d.Name = "Phone " + from
	shown := from
	if rule.HidePhoneNumber {
		d.Identity, shown = HideNumber(from)
		d.Name = "Phone " + shown
	} else {
		attrs[AttrPhoneNumber] = from
		attrs[AttrHostname] = c.FromHost
		attrs[AttrTrunkNumber] = to
		d.FromUser, d.ToUser = from, to
	}
	switch r := rule.GetRule().GetRule().(type) {
	case *pb.SIPDispatchRule_DispatchRuleIndividual:
		room = shown
		if p := r.DispatchRuleIndividual.GetRoomPrefix(); p != "" {
			room = p + "_" + shown
		}
		if !r.DispatchRuleIndividual.GetNoRandomness() {
			room += "_" + shortRandom()
		}
	case *pb.SIPDispatchRule_DispatchRuleCallee:
		room = to
		if p := r.DispatchRuleCallee.GetRoomPrefix(); p != "" {
			room = p + "_" + to
		}
		if r.DispatchRuleCallee.GetRandomize() {
			room += "_" + shortRandom()
		}
	}
	d.Room = room
	d.Metadata = rule.GetMetadata()
	d.Attributes = attrs
	d.RoomConfig = rule.GetRoomConfig()
	if trunk != nil {
		d.Headers = trunk.Headers
		d.HeadersToAttributes = trunk.HeadersToAttributes
		d.AttributesToHeaders = trunk.AttributesToHeaders
		d.IncludeHeaders = trunk.IncludeHeaders
		d.RingingTimeout = trunk.RingingTimeout
		d.RingingTimeoutStatus = trunk.RingingTimeoutStatus
		d.MaxCallDuration = trunk.MaxCallDuration
	}
	return d, nil
}

// UpgradeMedia moves the old media_encryption field into a media configuration.
func UpgradeMedia(m *pb.SIPMediaConfig, enc pb.SIPMediaEncryption) *pb.SIPMediaConfig {
	if m == nil {
		m = &pb.SIPMediaConfig{}
	} else {
		m = proto.Clone(m).(*pb.SIPMediaConfig)
	}
	if m.Encryption == nil && enc != pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_DISABLE {
		m.Encryption = &enc
	}
	return m
}

// MergeMedia lays one configuration over another, with the second one winning where it says anything.
func MergeMedia(base, over *pb.SIPMediaConfig) *pb.SIPMediaConfig {
	if base == nil {
		return over
	}
	if over == nil {
		return base
	}
	r := proto.Clone(over).(*pb.SIPMediaConfig)
	switch {
	case over.Encryption == nil || *over.Encryption == pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_DISABLE:
		if base.Encryption != nil {
			v := *base.Encryption
			r.Encryption = &v
		}
	}
	if over.MediaTimeout == nil {
		r.MediaTimeout = base.MediaTimeout
	}
	if len(over.Codecs) == 0 {
		r.OnlyListedCodecs = base.OnlyListedCodecs
		r.Codecs = base.Codecs
	}
	return r
}

// Encryption reads a media configuration's encryption setting.
func Encryption(m *pb.SIPMediaConfig) pb.SIPMediaEncryption {
	if m == nil || m.Encryption == nil {
		return pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_DISABLE
	}
	return *m.Encryption
}
