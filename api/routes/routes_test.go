package routes

import (
	"slices"
	"strings"
	"testing"

	"elnova-streamkit.local/api/middleware"
	"elnova-streamkit.local/server/auth"
)

// publicRoutes run without the wrapper checking anything. The ones under one meeting authenticate in meetingCaller, and
// hostOnlyRoutes tests those. A new public route has to be added here on purpose.
var publicRoutes = []string{
	"POST /api/v1/auth/register",
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/forgot-password",
	"POST /api/v1/auth/reset-password",
	"POST /api/v1/auth/verify-email",
	"POST /api/v1/join/preview",
	"POST /api/v1/join",
	"POST /api/v1/join/wait",
	"GET /api/v1/meetings/{room}",
	"PATCH /api/v1/meetings/{room}",
	"POST /api/v1/meetings/{room}/end",
	"POST /api/v1/meetings/{room}/mute-all",
	"POST /api/v1/meetings/{room}/hand",
	"GET /api/v1/meetings/{room}/summary",
	"POST /api/v1/meetings/{room}/waiting/{identity}",
	"DELETE /api/v1/meetings/{room}/waiting",
	"POST /api/v1/meetings/{room}/participants/{identity}/mute",
	"POST /api/v1/meetings/{room}/participants/{identity}/remove",
	"POST /api/v1/meetings/{room}/participants/{identity}/lower-hand",
	"PATCH /api/v1/meetings/{room}/participants/{identity}",
}

// tokenGrants names the grants that get a token past each route at projectOrToken. Where the reference server has the same
// API, it asks for the same grant: pkg/service/auth.go and the services that call it. Create and delete need roomCreate,
// listing needs roomList, anything inside one room needs roomAdmin for that room, recording needs roomRecord, and ingress
// needs ingressAdmin. Reading one room also takes roomList, and usage figures are project totals with no names in them.
var tokenGrants = map[string][]string{
	// The sip admin grant reads these. It is not a video grant, so every token tried here is refused; tests/sip covers it.
	"GET /api/v1/sip/trunks/inbound":                   {},
	"GET /api/v1/sip/trunks/inbound/{id}":              {},
	"GET /api/v1/sip/trunks/outbound":                  {},
	"GET /api/v1/sip/trunks/outbound/{id}":             {},
	"GET /api/v1/sip/dispatch-rules":                   {},
	"GET /api/v1/sip/calls":                            {},
	"GET /api/v1/sip/calls/{id}":                       {},
	"POST /api/v1/agent-credentials/resolve":           {"agent"},
	"POST /api/v1/agent-sessions":                      {"agent"},
	"GET /api/v1/rooms":                                {"list"},
	"POST /api/v1/rooms":                               {"create"},
	"GET /api/v1/rooms/history":                        {"list"},
	"GET /api/v1/rooms/{name}":                         {"list", "admin"},
	"PATCH /api/v1/rooms/{name}":                       {"admin"},
	"DELETE /api/v1/rooms/{name}":                      {"create"},
	"GET /api/v1/participants":                         {"admin"},
	"GET /api/v1/participants/{room}/{identity}":       {"admin"},
	"PATCH /api/v1/participants/{room}/{identity}":     {"admin"},
	"DELETE /api/v1/participants/{room}/{identity}":    {"admin"},
	"GET /api/v1/tracks":                               {"admin"},
	"POST /api/v1/tracks/{room}/{identity}/{sid}/mute": {"admin"},
	"GET /api/v1/analytics/overview":                   {"list"},
	"GET /api/v1/analytics/usage":                      {"list"},
	"GET /api/v1/egress":                               {"record"},
	"POST /api/v1/egress":                              {"record"},
	"GET /api/v1/egress/{id}":                          {"record"},
	"POST /api/v1/egress/{id}/stop":                    {"record"},
	"GET /api/v1/recordings":                           {"record"},
	"GET /api/v1/recordings/{id}":                      {"record"},
	"GET /api/v1/recordings/{id}/file":                 {"record"},
	"POST /api/v1/recordings/{id}/retry":               {"record"},
	"DELETE /api/v1/recordings/{id}":                   {"record"},
	"GET /api/v1/ingress":                              {"ingress"},
	"POST /api/v1/ingress":                             {"ingress"},
	"GET /api/v1/ingress/{id}":                         {"ingress"},
	"PATCH /api/v1/ingress/{id}":                       {"ingress"},
	"DELETE /api/v1/ingress/{id}":                      {"ingress"},
	"POST /api/v1/ingress/{id}/stop":                   {"ingress"},
	"POST /api/v1/ingress/{id}/rotate-key":             {"ingress"},
	"GET /api/v1/ingress/{id}/logs":                    {"ingress"},
}

// TestEveryRouteSaysWhatATokenGets walks the API's whole table, with a token holding each grant in turn, a real guest's and
// a real host's. A route that takes tokens and does not check a grant fails here, and so does a route nobody has said
// anything about.
func TestEveryRouteSaysWhatATokenGets(t *testing.T) {
	h := newHarness(t, nil)
	// All of this comes from one address, and a good many requests.
	h.api.apiLimit = middleware.NewRateLimiter(1e6, 1e6)
	m := newMeeting(t, h, "owner@example.com")
	guest, _ := m.join(t, m.guestCode, "Gita")
	host, _ := m.join(t, m.hostCode, "Hana")

	token := func(g *auth.VideoGrant) *client {
		c := h.client()
		c.bearer = mustMint(t, m.key, &auth.Grants{Identity: "api", Video: g})
		return c
	}
	grants := map[string]*auth.VideoGrant{
		"none":            nil,
		"join":            {RoomJoin: true, Room: m.room},
		"list":            {RoomList: true},
		"create":          {RoomCreate: true},
		"admin":           {RoomAdmin: true, Room: m.room},
		"admin elsewhere": {RoomAdmin: true, Room: "elsewhere"},
		"record":          {RoomRecord: true},
		"ingress":         {IngressAdmin: true},
		"agent":           {Agent: true},
	}
	every := token(&auth.VideoGrant{RoomJoin: true, Room: m.room, RoomList: true, RoomCreate: true, RoomAdmin: true, RoomRecord: true, IngressAdmin: true})

	fill := strings.NewReplacer("{room}", m.room, "{name}", m.room, "{identity}", "guest-x", "{id}", "x", "{sid}", "x")
	request := func(c *client, x route) response {
		t.Helper()
		method, path, _ := strings.Cut(x.pattern, " ")
		path = fill.Replace(path)
		var body any
		if method != "GET" {
			body = map[string]any{}
		}
		// These two read the room to ask about from the address.
		if x.pattern == "GET /api/v1/participants" || x.pattern == "GET /api/v1/tracks" {
			path += "?room=" + m.room
		}
		return c.do(method, path, body)
	}

	seen := map[string]bool{}
	for _, x := range h.api.routes() {
		seen[x.pattern] = true
		switch x.level {
		case public:
			if !slices.Contains(publicRoutes, x.pattern) {
				t.Errorf("%s takes no authentication: add it to publicRoutes if that is meant", x.pattern)
			}
		case signedIn, platformAdmin, projectAdmin:
			if r := request(every, x); r.status != 401 {
				t.Errorf("a token with every grant called %s: %d %s", x.pattern, r.status, r.raw)
			}
		case projectMember:
			for _, c := range []*client{every, guest, host} {
				if r := request(c, x); r.status != 403 {
					t.Errorf("a token called %s, which is for project members: %d %s", x.pattern, r.status, r.raw)
				}
			}
		case projectOrToken:
			allowed, ok := tokenGrants[x.pattern]
			if !ok {
				t.Errorf("%s takes tokens: say which grant it needs in tokenGrants", x.pattern)
				continue
			}
			for name, g := range grants {
				r := request(token(g), x)
				switch {
				case slices.Contains(allowed, name) && r.status == 403:
					t.Errorf("a %q token was refused at %s: %s", name, x.pattern, r.raw)
				case !slices.Contains(allowed, name) && r.status != 403:
					t.Errorf("a %q token got past %s: %d %s", name, x.pattern, r.status, r.raw)
				}
			}
			if r := request(guest, x); r.status != 403 {
				t.Errorf("a guest called %s: %d %s", x.pattern, r.status, r.raw)
			}
			// A host has no grants either. Its way to the record button has a test of its own.
			if r := request(host, x); r.status != 403 && x.pattern != "POST /api/v1/egress" {
				t.Errorf("a host called %s: %d %s", x.pattern, r.status, r.raw)
			}
		}
	}
	for _, listed := range [][]string{publicRoutes, keysOf(tokenGrants)} {
		for _, pattern := range listed {
			if !seen[pattern] {
				t.Errorf("%s is listed here and is not a route", pattern)
			}
		}
	}

	// Not naming a room is no way past the check on these two.
	for _, path := range []string{"/api/v1/participants", "/api/v1/tracks"} {
		if r := guest.do("GET", path, nil); r.status != 403 {
			t.Errorf("a guest called %s with no room: %d %s", path, r.status, r.raw)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
