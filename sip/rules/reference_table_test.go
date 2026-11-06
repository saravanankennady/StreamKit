// Portions of this file are copied from livekit/protocol, sip/sip_test.go, at commit eecdef3, and changed: type names, the removal
// of one case for a deprecated field, and the case marked below that differs on purpose. See NOTICE.md.
//
// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rules

import (
	"fmt"
	"testing"

	"elnova-streamkit.local/pkg/pb"
)

const (
	sipNumber1  = "1111 1111"
	sipNumber2  = "2222 2222"
	sipNumber3  = "3333 3333"
	sipTrunkID1 = "aaa"
	sipTrunkID2 = "bbb"
)

var trunkCases = []struct {
	name    string
	trunks  []*pb.SIPTrunkInfo
	exp     int
	expErr  bool
	invalid bool
	from    string
	to      string
	src     string
	host    string
}{
	{
		name:   "empty",
		trunks: nil,
		exp:    -1, // no error; nil result
	},
	{
		name: "one wildcard",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa"},
		},
		exp: 0,
	},
	{
		name: "matching",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber2},
		},
		exp: 0,
	},
	{
		name: "matching inbound",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber2, InboundNumbers: []string{sipNumber1}},
		},
		exp: 0,
	},
	{
		name: "not matching",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
		},
		exp: -1,
	},
	{
		name: "not matching inbound",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber2, InboundNumbers: []string{sipNumber1 + "1"}},
		},
		exp: -1,
	},
	{
		name: "one match",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2},
		},
		exp: 1,
	},
	{
		name: "many matches",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2},
			{SipTrunkId: "ccc", OutboundNumber: sipNumber2},
		},
		expErr:  true,
		invalid: true,
	},
	{
		name: "many matches default",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
			{SipTrunkId: "bbb"},
			{SipTrunkId: "ccc", OutboundNumber: sipNumber2},
			{SipTrunkId: "ddd"},
		},
		exp:     2,
		invalid: true, // it can successfully select "ccc", but the overall configuration is invalid
	},
	{
		name: "inbound",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2},
			{SipTrunkId: "ccc", OutboundNumber: sipNumber2, InboundNumbers: []string{sipNumber1 + "1"}},
		},
		exp: 1,
	},
	{
		name: "multiple defaults",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
			{SipTrunkId: "bbb"},
			{SipTrunkId: "ccc"},
		},
		expErr:  true,
		invalid: true,
	},
	{
		name: "inbound with ip exact",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2, InboundAddresses: []string{
				"10.10.10.10",
				"1.1.1.1",
			}},
		},
		exp: 0,
	},
	{
		name: "inbound with ip exact miss",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2, InboundAddresses: []string{
				"10.10.10.10",
			}},
		},
		exp: -1,
	},
	{
		name: "inbound with ip mask",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2, InboundAddresses: []string{
				"10.10.10.0/24",
				"1.1.1.0/24",
			}},
		},
		exp: 0,
	},
	{
		name: "inbound with ip mask miss",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2, InboundAddresses: []string{
				"10.10.10.0/24",
			}},
		},
		exp: -1,
	},
	{
		name: "inbound with host mask",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2, InboundAddresses: []string{
				"10.10.10.0/24",
				"sip.example.com",
			}},
		},
		// Deliberately different from the reference, which answers 0: a From host can say anything.
		exp: -1,
	},
	{
		name: "inbound with host mask and a password",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2, InboundUsername: "u", InboundPassword: "p", InboundAddresses: []string{
				"10.10.10.0/24",
				"sip.example.com",
			}},
		},
		exp: 0,
	},
	{
		name: "inbound with plus",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: "+" + sipNumber3},
			{SipTrunkId: "bbb", OutboundNumber: "+" + sipNumber2},
		},
		exp: 1,
	},
	{
		name: "inbound without plus",
		trunks: []*pb.SIPTrunkInfo{
			{SipTrunkId: "aaa", OutboundNumber: sipNumber3},
			{SipTrunkId: "bbb", OutboundNumber: sipNumber2},
		},
		from: "+" + sipNumber1,
		to:   "+" + sipNumber2,
		exp:  1,
	},
}

func toInbound(p *pb.SIPTrunkInfo) *pb.SIPInboundTrunkInfo {
	if p == nil {
		return nil
	}
	var nums []string
	if p.OutboundNumber != "" {
		nums = []string{p.OutboundNumber}
	}
	return &pb.SIPInboundTrunkInfo{
		SipTrunkId: p.SipTrunkId, Name: p.Name, Metadata: p.Metadata, Numbers: nums, AllowedAddresses: p.InboundAddresses,
		AllowedNumbers: p.InboundNumbers, AuthUsername: p.InboundUsername, AuthPassword: p.InboundPassword,
	}
}

func TestMatchTrunkAgainstReferenceTable(t *testing.T) {
	for _, c := range trunkCases {
		t.Run(c.name, func(t *testing.T) {
			from, to, src, host := c.from, c.to, c.src, c.host
			if from == "" {
				from = sipNumber1
			}
			if to == "" {
				to = sipNumber2
			}
			if src == "" {
				src = "1.1.1.1"
			}
			if host == "" {
				host = "sip.example.com"
			}
			trunks := make([]*pb.SIPInboundTrunkInfo, 0, len(c.trunks))
			for _, tr := range c.trunks {
				trunks = append(trunks, toInbound(tr))
			}
			got, err := MatchTrunk(trunks, Call{FromUser: from, FromHost: host, ToUser: to, SourceIP: src})
			if c.expErr {
				if err == nil || got != nil {
					t.Fatalf("expected an error, got %v, %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want *pb.SIPInboundTrunkInfo
			if c.exp >= 0 {
				want = trunks[c.exp]
			}
			if got != want {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestValidateTrunksAgainstReferenceTable(t *testing.T) {
	for _, c := range trunkCases {
		t.Run(c.name, func(t *testing.T) {
			trunks := make([]*pb.SIPInboundTrunkInfo, 0, len(c.trunks))
			for _, tr := range c.trunks {
				trunks = append(trunks, toInbound(tr))
			}
			err := ValidateInboundTrunks(trunks)
			if c.invalid && err == nil {
				t.Fatal("expected this set of trunks to be refused")
			}
			if !c.invalid && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func newSIPTrunkDispatch() *pb.SIPTrunkInfo {
	return &pb.SIPTrunkInfo{SipTrunkId: sipTrunkID1, OutboundNumber: sipNumber2}
}

func newDirectDispatch(room, pin string) *pb.SIPDispatchRule {
	return &pb.SIPDispatchRule{Rule: &pb.SIPDispatchRule_DispatchRuleDirect{
		DispatchRuleDirect: &pb.SIPDispatchRuleDirect{RoomName: room, Pin: pin},
	}}
}

func newIndividualDispatch(roomPref, pin string, randomize bool) *pb.SIPDispatchRule {
	return &pb.SIPDispatchRule{Rule: &pb.SIPDispatchRule_DispatchRuleIndividual{
		DispatchRuleIndividual: &pb.SIPDispatchRuleIndividual{RoomPrefix: roomPref, Pin: pin, NoRandomness: !randomize},
	}}
}

var dispatchCases = []struct {
	name    string
	trunk   *pb.SIPTrunkInfo
	rules   []*pb.SIPDispatchRuleInfo
	reqPin  string
	noPin   bool
	exp     int
	expErr  bool
	invalid bool
}{
	// These cases just validate that no rules produce an error.
	{
		name:   "empty",
		trunk:  nil,
		rules:  nil,
		expErr: true,
	},
	{
		name:   "only trunk",
		trunk:  newSIPTrunkDispatch(),
		rules:  nil,
		expErr: true,
	},
	// Default rules should work even if no trunk is defined.
	{
		name:  "one rule/no trunk",
		trunk: nil,
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip", "")},
		},
		exp: 0,
	},
	// Default rule should work with a trunk too.
	{
		name:  "one rule/default trunk",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip", "")},
		},
		exp: 0,
	},
	// Rule matching the trunk should be selected.
	{
		name:  "one rule/specific trunk",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: []string{sipTrunkID1, sipTrunkID2}, Rule: newDirectDispatch("sip", "")},
		},
		exp: 0,
	},
	// Rule NOT matching the trunk should NOT be selected.
	{
		name:  "one rule/wrong trunk",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: []string{"zzz"}, Rule: newDirectDispatch("sip", "")},
		},
		expErr: true,
	},
	// Direct rule with a pin should be selected, even if no pin is provided.
	{
		name:  "direct pin/correct",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip", "123")},
			{TrunkIds: []string{sipTrunkID2}, Rule: newDirectDispatch("sip", "456")},
		},
		reqPin: "123",
		exp:    0,
	},
	// Direct rule with a pin should reject wrong pin.
	{
		name:  "direct pin/wrong",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip", "123")},
			{TrunkIds: []string{sipTrunkID2}, Rule: newDirectDispatch("sip", "456")},
		},
		reqPin: "zzz",
		expErr: true,
	},
	// Multiple direct rules with the same pin should result in an error.
	{
		name:  "direct pin/conflict",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip1", "123")},
			{TrunkIds: []string{sipTrunkID1, sipTrunkID2}, Rule: newDirectDispatch("sip2", "123")},
		},
		reqPin:  "123",
		expErr:  true,
		invalid: true,
	},
	// Multiple direct rules with the same pin on different trunks are ok.
	{
		name:  "direct pin/no conflict on different trunk",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip1", "123")},
			{TrunkIds: []string{sipTrunkID2}, Rule: newDirectDispatch("sip2", "123")},
		},
		reqPin: "123",
		exp:    0,
	},
	// Specific direct rules should take priority over default direct rules.
	{
		name:  "direct pin/default and specific",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "123")},
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip2", "123")},
		},
		reqPin: "123",
		exp:    1,
	},
	// Specific direct rules should take priority over default direct rules. No pin.
	{
		name:  "direct/default and specific",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "")},
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip2", "")},
		},
		exp: 1,
	},
	// Specific direct rules should take priority over default direct rules. One with pin, other without.
	{
		name:  "direct/default and specific/mixed 1",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "123")},
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip2", "")},
		},
		exp: 1,
	},
	{
		name:  "direct/default and specific/mixed 2",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "")},
			{TrunkIds: []string{sipTrunkID1}, Rule: newDirectDispatch("sip2", "123")},
		},
		exp: 1,
	},
	// Multiple default direct rules are not allowed.
	{
		name:  "direct/multiple defaults",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip2", "")},
		},
		expErr:  true,
		invalid: true,
	},
	// Rules for specific inbound numbers take priority.
	{
		name:  "direct/inbound number specific",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip2", ""), InboundNumbers: []string{sipNumber1}},
		},
		exp: 1,
	},
	{
		name:  "direct/inbound number specific pin",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "123")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip2", "123"), InboundNumbers: []string{sipNumber1}},
		},
		exp: 1,
	},
	{
		name:  "direct/inbound number specific conflict",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", ""), InboundNumbers: []string{sipNumber1}},
			{TrunkIds: nil, Rule: newDirectDispatch("sip2", ""), InboundNumbers: []string{sipNumber1, sipNumber2}},
		},
		expErr:  true,
		invalid: true,
	},
	// Check the "personal room" use case. Rule that accepts an inbound number without a pin and requires pin for everyone else.
	{
		name:  "direct/open specific vs pin generic",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip1", "123")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip2", ""), InboundNumbers: []string{sipNumber1}},
		},
		exp: 1,
	},
	// Cannot use both direct and individual rules with the same pin setup.
	{
		name:  "direct vs individual/private",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newIndividualDispatch("pref_", "123", true)},
			{TrunkIds: nil, Rule: newDirectDispatch("sip", "123")},
		},
		expErr:  true,
		invalid: true,
	},
	{
		name:  "direct vs individual/open",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newIndividualDispatch("pref_", "", true)},
			{TrunkIds: nil, Rule: newDirectDispatch("sip", "")},
		},
		expErr:  true,
		invalid: true,
	},
	// Direct rules take priority over individual rules.
	{
		name:  "direct vs individual/priority",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newIndividualDispatch("pref_", "123", true)},
			{TrunkIds: nil, Rule: newDirectDispatch("sip", "456")},
		},
		reqPin: "456",
		exp:    1,
	},
	// Rules for specific numbers take priority.
	{
		name:  "direct/number specific",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip_1", "")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip_2", ""), Numbers: []string{sipNumber2}},
		},
		exp: 1,
	},
	{
		name:  "direct/number specific pin",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip_1", "123")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip_2", "123"), Numbers: []string{sipNumber2}},
		},
		exp: 1,
	},
	{
		name:  "direct/number specific conflict",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip_1", ""), Numbers: []string{sipNumber1}},
			{TrunkIds: nil, Rule: newDirectDispatch("sip_2", ""), Numbers: []string{sipNumber1, sipNumber2}},
		},
		expErr:  true,
		invalid: true,
	},
	{
		name:  "direct/number + inbound number specific conflict",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip_1", ""), Numbers: []string{sipNumber1}, InboundNumbers: []string{sipNumber1}},
			{TrunkIds: nil, Rule: newDirectDispatch("sip_2", ""), Numbers: []string{sipNumber1, sipNumber2}, InboundNumbers: []string{sipNumber1}},
		},
		expErr:  true,
		invalid: true,
	},
	// Check the "personal room" use case. Rule that accepts a number without a pin and requires pin for everyone else.
	{
		name:  "direct/open specific vs pin generic",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip_1", "123")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip_2", ""), Numbers: []string{sipNumber2}},
		},
		exp: 1,
	},
	{
		name:  "direct/open specific vs pin generic",
		trunk: newSIPTrunkDispatch(),
		rules: []*pb.SIPDispatchRuleInfo{
			{TrunkIds: nil, Rule: newDirectDispatch("sip_1", "123")},
			{TrunkIds: nil, Rule: newDirectDispatch("sip_2", ""), Numbers: []string{sipNumber2}, InboundNumbers: []string{sipNumber1}},
		},
		exp: 1,
	},
}

func TestMatchRuleAgainstReferenceTable(t *testing.T) {
	for _, c := range dispatchCases {
		t.Run(c.name, func(t *testing.T) {
			pins := []string{c.reqPin}
			if !c.expErr && c.reqPin != "" {
				// Should match the same rule even with no PIN yet, so that it can be asked for.
				pins = append(pins, "")
			}
			for i, r := range c.rules {
				if r.SipDispatchRuleId == "" {
					r.SipDispatchRuleId = fmt.Sprintf("rule_%d", i)
				}
			}
			for _, pin := range pins {
				got, err := MatchRule(toInbound(c.trunk), c.rules, Call{FromUser: sipNumber1, ToUser: sipNumber2}, pin, c.noPin)
				if c.expErr {
					if err == nil || got != nil {
						t.Fatalf("pin %q: expected an error, got %v, %v", pin, got, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("pin %q: %v", pin, err)
				}
				if got != c.rules[c.exp] {
					t.Fatalf("pin %q: got %v, want %v", pin, got, c.rules[c.exp])
				}
			}
		})
	}
}

func TestValidateRulesAgainstReferenceTable(t *testing.T) {
	for _, c := range dispatchCases {
		t.Run(c.name, func(t *testing.T) {
			for i, r := range c.rules {
				if r.SipDispatchRuleId == "" {
					r.SipDispatchRuleId = fmt.Sprintf("rule_%d", i)
				}
			}
			err := ValidateRules(c.rules)
			if c.invalid && err == nil {
				t.Fatal("expected this set of rules to be refused")
			}
			if !c.invalid && err != nil {
				t.Fatal(err)
			}
		})
	}
}
