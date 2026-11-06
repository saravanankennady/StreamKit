package rules

import (
	"errors"
	"strings"
	"testing"

	"elnova-streamkit.local/pkg/pb"
)

func TestNormalizeNumber(t *testing.T) {
	for in, want := range map[string]string{"": "", "123": "+123", "+123": "+123", "user": "user", "(123) 456 7890": "+1234567890"} {
		if got := NormalizeNumber(in); got != want {
			t.Errorf("NormalizeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchTrunkSameNumberTwoFormsIsNotAConflict(t *testing.T) {
	trunks := []*pb.SIPInboundTrunkInfo{{SipTrunkId: "aaa", Numbers: []string{"+22222222", "22222222"}}}
	got, err := MatchTrunk(trunks, Call{FromUser: "1", ToUser: "22222222", SourceIP: "1.1.1.1"})
	if err != nil || got != trunks[0] {
		t.Fatalf("got %v, %v", got, err)
	}
}

// The reference lets a call through when the source address cannot be read, or when every allow-list entry is invalid. Here both
// keep the call out.
func TestAllowListsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		mask []string
		src  string
	}{
		{"unreadable source", []string{"10.0.0.0/8"}, "not an address"},
		{"empty source", []string{"10.0.0.0/8"}, ""},
		{"only invalid entries", []string{"http://example.com", "1.2.3.4, 5.6.7.8"}, "1.2.3.4"},
		{"other network", []string{"10.0.0.0/8"}, "11.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trunks := []*pb.SIPInboundTrunkInfo{{SipTrunkId: "a", AllowedAddresses: c.mask}}
			got, err := MatchTrunk(trunks, Call{FromUser: "1", FromHost: "example.com", ToUser: "2", SourceIP: c.src})
			if err != nil || got != nil {
				t.Fatalf("expected no trunk, got %v, %v", got, err)
			}
		})
	}
}

func TestIPv4MappedSourceMatches(t *testing.T) {
	trunks := []*pb.SIPInboundTrunkInfo{{SipTrunkId: "a", AllowedAddresses: []string{"192.0.2.0/24"}}}
	got, err := MatchTrunk(trunks, Call{FromUser: "1", ToUser: "2", SourceIP: "::ffff:192.0.2.9"})
	if err != nil || got != trunks[0] {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestIsAuthenticated(t *testing.T) {
	cases := []struct {
		name string
		t    *pb.SIPInboundTrunkInfo
		want bool
	}{
		{"nothing", &pb.SIPInboundTrunkInfo{}, false},
		{"numbers only", &pb.SIPInboundTrunkInfo{Numbers: []string{"+1"}}, false},
		{"password", &pb.SIPInboundTrunkInfo{AuthUsername: "u", AuthPassword: "p"}, true},
		{"username only", &pb.SIPInboundTrunkInfo{AuthUsername: "u"}, false},
		{"host", &pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"203.0.113.9"}}, true},
		{"network", &pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"203.0.113.0/24"}}, true},
		{"everything", &pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"0.0.0.0/0"}}, false},
		{"half of everything", &pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"0.0.0.0/1"}}, false},
		{"everything v6", &pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"[::]/0"}}, false},
		{"host name", &pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"sip.example.com"}}, false},
	}
	for _, c := range cases {
		if got := IsAuthenticated(c.t); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func direct(room, pin string) *pb.SIPDispatchRule { return newDirectDispatch(room, pin) }

func TestWrongPinNeverFallsThroughToAnOpenRule(t *testing.T) {
	rules := []*pb.SIPDispatchRuleInfo{
		{SipDispatchRuleId: "a", Rule: direct("secret", "123")},
		{SipDispatchRuleId: "b", Rule: direct("open", ""), InboundNumbers: []string{"+999"}},
		{SipDispatchRuleId: "c", Rule: direct("open2", "")},
	}
	if _, err := MatchRule(nil, rules, Call{FromUser: "1", ToUser: "2"}, "000", false); !errors.Is(err, ErrNoRule) {
		t.Fatalf("got %v", err)
	}
	got, err := MatchRule(nil, rules, Call{FromUser: "1", ToUser: "2"}, "123", false)
	if err != nil || got.SipDispatchRuleId != "a" {
		t.Fatalf("got %v, %v", got, err)
	}
	got, err = MatchRule(nil, rules, Call{FromUser: "1", ToUser: "2"}, "", true)
	if err != nil || got.SipDispatchRuleId != "c" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestPriorityOrder(t *testing.T) {
	individual := func(pin string) *pb.SIPDispatchRule { return newIndividualDispatch("p", pin, false) }
	callee := func(pin string) *pb.SIPDispatchRule {
		return &pb.SIPDispatchRule{Rule: &pb.SIPDispatchRule_DispatchRuleCallee{DispatchRuleCallee: &pb.SIPDispatchRuleCallee{Pin: pin}}}
	}
	num := []string{"1"}
	order := []*pb.SIPDispatchRuleInfo{
		{Rule: direct("a", "1"), InboundNumbers: num, Numbers: num},
		{Rule: individual("1"), InboundNumbers: num, Numbers: num},
		{Rule: callee("1"), InboundNumbers: num, Numbers: num},
		{Rule: direct("a", ""), InboundNumbers: num, Numbers: num},
		{Rule: individual(""), InboundNumbers: num, Numbers: num},
		{Rule: callee(""), InboundNumbers: num, Numbers: num},
		{Rule: direct("a", "1"), Numbers: num},
		{Rule: direct("a", "")},
	}
	for i := 1; i < len(order); i++ {
		if Priority(order[i-1]) >= Priority(order[i]) {
			t.Errorf("rule %d (%d) should come before rule %d (%d)", i-1, Priority(order[i-1]), i, Priority(order[i]))
		}
	}
}

func TestEvaluateRooms(t *testing.T) {
	trunk := &pb.SIPInboundTrunkInfo{SipTrunkId: "ST_a"}
	call := Call{ID: "SCL_x", FromUser: "+15550100", FromHost: "carrier.example", ToUser: "+15550199"}
	eval := func(rule *pb.SIPDispatchRule, hide bool) *Dispatch {
		t.Helper()
		d, err := Evaluate(trunk, &pb.SIPDispatchRuleInfo{SipDispatchRuleId: "SDR_a", Rule: rule, HidePhoneNumber: hide}, call, "")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := eval(direct("lobby", ""), false); d.Room != "lobby" || d.Identity != "sip_+15550100" || d.Name != "Phone +15550100" {
		t.Errorf("direct: %+v", d)
	}
	d := eval(newIndividualDispatch("call", "", false), false)
	if d.Room != "call_+15550100" {
		t.Errorf("individual without randomness: %q", d.Room)
	}
	d = eval(newIndividualDispatch("call", "", true), false)
	if !strings.HasPrefix(d.Room, "call_+15550100_") || len(d.Room) <= len("call_+15550100_") {
		t.Errorf("individual: %q", d.Room)
	}
	callee := func(randomize bool) *pb.SIPDispatchRule {
		return &pb.SIPDispatchRule{Rule: &pb.SIPDispatchRule_DispatchRuleCallee{
			DispatchRuleCallee: &pb.SIPDispatchRuleCallee{RoomPrefix: "line", Randomize: randomize},
		}}
	}
	if d := eval(callee(false), false); d.Room != "line_+15550199" {
		t.Errorf("callee: %q", d.Room)
	}
	if d := eval(callee(true), false); !strings.HasPrefix(d.Room, "line_+15550199_") {
		t.Errorf("callee randomized: %q", d.Room)
	}
	d = eval(direct("lobby", ""), false)
	for k, want := range map[string]string{
		AttrCallID: "SCL_x", AttrTrunkID: "ST_a", AttrRuleID: "SDR_a", AttrPhoneNumber: "+15550100", AttrHostname: "carrier.example", AttrTrunkNumber: "+15550199",
	} {
		if d.Attributes[k] != want {
			t.Errorf("attribute %s = %q, want %q", k, d.Attributes[k], want)
		}
	}
}

func TestEvaluateHidesThePhoneNumber(t *testing.T) {
	call := Call{ID: "SCL_x", FromUser: "+15550100", FromHost: "carrier.example", ToUser: "+15550199"}
	d, err := Evaluate(nil, &pb.SIPDispatchRuleInfo{Rule: newIndividualDispatch("call", "", false), HidePhoneNumber: true}, call, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Identity, "5550100") || d.Name != "Phone 0100" || d.Room != "call_0100" {
		t.Errorf("number shown: %+v", d)
	}
	for _, k := range []string{AttrPhoneNumber, AttrHostname, AttrTrunkNumber} {
		if _, ok := d.Attributes[k]; ok {
			t.Errorf("attribute %s should be missing", k)
		}
	}
	if d.FromUser != "" || d.ToUser != "" {
		t.Errorf("number kept on record: %+v", d)
	}
}

func TestEvaluateAsksForThePin(t *testing.T) {
	rule := &pb.SIPDispatchRuleInfo{Rule: direct("lobby", "4242")}
	d, err := Evaluate(nil, rule, Call{FromUser: "1"}, "")
	if err != nil || !d.NeedPin || d.Room != "" {
		t.Fatalf("got %+v, %v", d, err)
	}
	if _, err := Evaluate(nil, rule, Call{FromUser: "1"}, "0000"); err == nil {
		t.Fatal("a wrong PIN was accepted")
	}
	d, err = Evaluate(nil, rule, Call{FromUser: "1"}, "4242")
	if err != nil || d.NeedPin || d.Room != "lobby" {
		t.Fatalf("got %+v, %v", d, err)
	}
}

// A From header can hold anything, and what it holds ends up in room names and identities.
func TestCleanUser(t *testing.T) {
	cases := map[string]string{
		"+15550100":                 "+15550100",
		"alice.smith_1":             "alice.smith_1",
		"a\r\nX-Evil: 1":            "aX-Evil1",
		"<script>alert(1)</script>": "scriptalert1script",
		"../../etc/passwd":          "....etcpasswd",
		strings.Repeat("9", 500):    strings.Repeat("9", 64),
		"‮\u0000name":               "name",
	}
	for in, want := range cases {
		if got := CleanUser(in); got != want {
			t.Errorf("CleanUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateInboundRefusesAnOpenTrunk(t *testing.T) {
	open := &pb.SIPInboundTrunkInfo{Numbers: []string{"+15550100"}}
	if err := ValidateInbound(open, true); err == nil {
		t.Fatal("a trunk with only numbers was accepted")
	}
	if err := ValidateInbound(open, false); err != nil {
		t.Fatalf("with the operator's permission: %v", err)
	}
	if err := ValidateInbound(&pb.SIPInboundTrunkInfo{AllowedAddresses: []string{"0.0.0.0/0"}}, true); err == nil {
		t.Fatal("an allow-list of the whole internet was accepted")
	}
	if err := ValidateInbound(&pb.SIPInboundTrunkInfo{AuthUsername: "u"}, true); err == nil {
		t.Fatal("a user name with no password was accepted")
	}
	if err := ValidateInbound(&pb.SIPInboundTrunkInfo{AuthUsername: "u", AuthPassword: "p"}, true); err != nil {
		t.Fatal(err)
	}
}

func TestHeadersCannotInjectOrSpoof(t *testing.T) {
	for name, value := range map[string]string{
		"X-Ok\r\nVia":  "1",
		"Via":          "SIP/2.0/UDP evil",
		"from":         "evil",
		"Route":        "<sip:evil>",
		"X-Good":       "line\r\nX-Evil: 1",
		"X-Binary":     "a\x00b",
		"":             "x",
		"X has spaces": "x",
	} {
		if err := ValidateHeaders(map[string]string{name: value}); err == nil {
			t.Errorf("header %q: %q was accepted", name, value)
		}
	}
	if err := ValidateHeaders(map[string]string{"X-Customer": "acme 42"}); err != nil {
		t.Error(err)
	}
}

func TestCreateParticipantValidation(t *testing.T) {
	good := func() *pb.CreateSIPParticipantRequest {
		return &pb.CreateSIPParticipantRequest{SipTrunkId: "ST_a", SipCallTo: "+15550100", RoomName: "r"}
	}
	if err := ValidateCreateParticipant(good()); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*pb.CreateSIPParticipantRequest){
		"no trunk":        func(p *pb.CreateSIPParticipantRequest) { p.SipTrunkId = "" },
		"no number":       func(p *pb.CreateSIPParticipantRequest) { p.SipCallTo = "" },
		"a full URI":      func(p *pb.CreateSIPParticipantRequest) { p.SipCallTo = "alice@evil.example" },
		"a line break":    func(p *pb.CreateSIPParticipantRequest) { p.SipCallTo = "1\r\nX-Evil: 1" },
		"no room":         func(p *pb.CreateSIPParticipantRequest) { p.RoomName = "" },
		"bad digits":      func(p *pb.CreateSIPParticipantRequest) { p.Dtmf = "12z" },
		"bad header":      func(p *pb.CreateSIPParticipantRequest) { p.Headers = map[string]string{"Via": "evil"} },
		"inline URI host": func(p *pb.CreateSIPParticipantRequest) { p.Trunk = &pb.SIPOutboundConfig{Hostname: "sip:evil"} },
	}
	for name, change := range bad {
		p := good()
		change(p)
		if err := ValidateCreateParticipant(p); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestNormalizeTransferTo(t *testing.T) {
	for in, want := range map[string]string{
		"sip:a@b":   "<sip:a@b>",
		"<sip:a@b>": "<sip:a@b>",
		"sips:a@b":  "<sips:a@b>",
		"tel:+1555": "tel:+1555",
	} {
		got, err := NormalizeTransferTo(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "http://example.com", "sip:a@b>\r\nX: 1", "+15550100"} {
		if _, err := NormalizeTransferTo(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}

func TestListUpdates(t *testing.T) {
	tr := &pb.SIPInboundTrunkInfo{Numbers: []string{"1", "2"}}
	if err := ApplyInboundUpdate(tr, &pb.SIPInboundTrunkUpdate{Numbers: &pb.ListUpdate{Add: []string{"3", "2"}, Remove: []string{"1"}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tr.Numbers, ",") != "2,3" {
		t.Errorf("got %v", tr.Numbers)
	}
	if err := ApplyInboundUpdate(tr, &pb.SIPInboundTrunkUpdate{Numbers: &pb.ListUpdate{Clear: true}}); err != nil || len(tr.Numbers) != 0 {
		t.Errorf("got %v, %v", tr.Numbers, err)
	}
	if err := ApplyInboundUpdate(tr, &pb.SIPInboundTrunkUpdate{Numbers: &pb.ListUpdate{Set: []string{"1"}, Add: []string{"2"}}}); err == nil {
		t.Error("set with add was accepted")
	}
}
