package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/room"
)

// meeting is one made through the API, with people holding its links.
type meeting struct {
	h         *harness
	owner     *client
	key       response
	room      string
	hostCode  string
	guestCode string
}

func codeIn(t *testing.T, url string) string {
	t.Helper()
	_, code, ok := strings.Cut(url, meetingJoinPath)
	if !ok || code == "" {
		t.Fatalf("no code in %q", url)
	}
	return code
}

func newMeeting(t *testing.T, h *harness, email string) *meeting {
	t.Helper()
	owner, key := shareSetup(t, h, email)
	r := expect(t, owner.do("POST", "/api/v1/meetings", map[string]any{"title": "Planning"}), 201)
	return &meeting{h: h, owner: owner, key: key, room: r.body["room"].(string), hostCode: codeIn(t, r.body["host_url"].(string)), guestCode: codeIn(t, r.body["guest_url"].(string))}
}

// join swaps a code for a token, and returns a client carrying it and the claims inside it.
func (m *meeting) join(t *testing.T, code, name string) (*client, *auth.Verified) {
	t.Helper()
	r := expect(t, m.h.client().do("POST", "/api/v1/join", map[string]any{"code": code, "name": name}), 201)
	token := r.body["token"].(string)
	v, err := auth.Verify(token, m.h.keys)
	if err != nil {
		t.Fatal(err)
	}
	c := m.h.client()
	c.bearer = token
	return c, v
}

func (m *meeting) path(rest string) string { return "/api/v1/meetings/" + m.room + rest }

func TestMeetingsCanOnlyBeMadeBySignedInMembers(t *testing.T) {
	h := newHarness(t, nil)
	owner, key := shareSetup(t, h, "owner@example.com")

	// Nobody signed in, and no token. This is the abuse case: anyone who can reach the server, minting rooms.
	expect(t, h.client().do("POST", "/api/v1/meetings", map[string]any{"title": "x"}), 401)

	// A token cannot make itself a way back in, even one with every grant.
	bearer := h.client()
	bearer.bearer = mustMint(t, key, &auth.Grants{Identity: "x", Video: &auth.VideoGrant{RoomAdmin: true, RoomList: true, RoomCreate: true, RoomRecord: true}})
	expect(t, bearer.do("POST", "/api/v1/meetings", map[string]any{"title": "x"}), 403)

	// Someone else's project does not exist, as far as they can tell.
	other := h.signUp("other@example.com")
	other.project = owner.project
	expect(t, other.do("POST", "/api/v1/meetings", map[string]any{"title": "x"}), 404)

	// A project with no API key cannot sign tokens, so a meeting there could never admit anyone.
	bare := h.signUp("bare@example.com")
	bare.newProject("no keys")
	if r := bare.do("POST", "/api/v1/meetings", map[string]any{"title": "x"}); r.status != 409 || r.errCode() != "no_api_key" {
		t.Fatalf("no key: %d %s", r.status, r.raw)
	}

	r := expect(t, owner.do("POST", "/api/v1/meetings", map[string]any{"title": "  Planning  "}), 201)
	if r.body["title"] != "Planning" || !strings.HasPrefix(r.body["room"].(string), "meet-") {
		t.Fatalf("meeting: %s", r.raw)
	}
	if r.body["host_url"] == r.body["guest_url"] {
		t.Fatal("the guest link is the host link")
	}
	if strings.Contains(r.raw, key.body["secret"].(string)) {
		t.Fatal("the response contains the API secret")
	}
	if h.rooms.Get(owner.project, r.body["room"].(string)) != nil {
		t.Fatal("a room was opened before anyone came")
	}
	if !strings.Contains(expect(t, owner.do("GET", "/api/v1/audit", nil), 200).raw, "meeting.created") {
		t.Fatal("no audit entry")
	}

	expect(t, owner.do("POST", "/api/v1/meetings", map[string]any{"title": strings.Repeat("x", 101)}), 422)
	expect(t, owner.do("POST", "/api/v1/meetings", map[string]any{"title": "x", "room": "chosen"}), 400)
}

func TestMakingMeetingsIsRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	owner, _ := shareSetup(t, h, "owner@example.com")
	for range 40 {
		if r := owner.do("POST", "/api/v1/meetings", map[string]any{}); r.status == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatal("40 meetings in a row were all allowed")
}

func TestHostLinkGivesARoleAndNoGrants(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")

	preview := expect(t, h.client().do("POST", "/api/v1/join/preview", map[string]any{"code": m.hostCode}), 200)
	if preview.body["role"] != "host" || preview.body["title"] != "Planning" {
		t.Fatalf("preview: %s", preview.raw)
	}
	if strings.Contains(preview.raw, m.owner.project) {
		t.Fatalf("the preview shows the project: %s", preview.raw)
	}

	_, host := m.join(t, m.hostCode, "Hana")
	_, guest := m.join(t, m.guestCode, "Gita")
	for _, v := range []*auth.Verified{host, guest} {
		g := v.Grants.Video
		if !g.RoomJoin || g.Room != m.room {
			t.Fatalf("grant is not for the meeting's room: %+v", g)
		}
		if g.RoomAdmin || g.RoomCreate || g.RoomList || g.RoomRecord || g.Hidden || g.IngressAdmin {
			t.Fatalf("a token got more than joining: %+v", g)
		}
		if g.CanUpdateOwnMetadata != nil && *g.CanUpdateOwnMetadata {
			t.Fatalf("a token can change its own attributes, and so its role: %+v", g)
		}
	}
	if host.Grants.Attributes[meetingRoleAttribute] != "host" || guest.Grants.Attributes[meetingRoleAttribute] != "guest" {
		t.Fatalf("roles: %v %v", host.Grants.Attributes, guest.Grants.Attributes)
	}
	if !strings.HasPrefix(host.Grants.Identity, "host-") || !strings.HasPrefix(guest.Grants.Identity, "guest-") {
		t.Fatalf("identities: %s %s", host.Grants.Identity, guest.Grants.Identity)
	}

	// The body still decides nothing but the name.
	expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "x", "role": "host"}), 400)

	// A host link can be made for any room, from the dashboard's API.
	made := expect(t, m.owner.do("POST", "/api/v1/join-links", map[string]any{"room": "any", "role": "host"}), 201)
	if made.body["role"] != "host" {
		t.Fatalf("link: %s", made.raw)
	}
	expect(t, m.owner.do("POST", "/api/v1/join-links", map[string]any{"room": "any", "role": "admin"}), 422)
}

// hostOnly lists every route that takes more than being in the meeting.
func hostOnlyRoutes(m *meeting) [][3]any {
	return [][3]any{
		{"PATCH", m.path(""), map[string]any{"locked": true}},
		{"POST", m.path("/end"), map[string]any{}},
		{"POST", m.path("/mute-all"), map[string]any{}},
		{"POST", m.path("/waiting/guest-x"), map[string]any{"admit": true}},
		{"DELETE", m.path("/waiting"), nil},
		{"POST", m.path("/participants/guest-x/mute"), map[string]any{}},
		{"POST", m.path("/participants/guest-x/remove"), map[string]any{}},
		{"POST", m.path("/participants/guest-x/lower-hand"), map[string]any{}},
		{"PATCH", m.path("/participants/guest-x"), map[string]any{"role": "moderator"}},
	}
}

func TestGuestsCannotRunAMeeting(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ControlPlane.AllowPrivateWebhooks = true })
	m := newMeeting(t, h, "owner@example.com")
	guest, _ := m.join(t, m.guestCode, "Gita")
	nobody := h.client()
	keys := expect(t, m.owner.do("GET", "/api/v1/keys", nil), 200).items()
	hook := expect(t, m.owner.do("POST", "/api/v1/webhooks", map[string]any{
		"url": "http://127.0.0.1:9/hook", "api_key_id": keys[0].(map[string]any)["id"],
	}), 201).body["id"].(string)

	for _, route := range hostOnlyRoutes(m) {
		method, path, body := route[0].(string), route[1].(string), route[2]
		if r := guest.do(method, path, body); r.status != 403 {
			t.Fatalf("a guest called %s %s: %d %s", method, path, r.status, r.raw)
		}
		if r := nobody.do(method, path, body); r.status != 401 {
			t.Fatalf("nobody called %s %s: %d %s", method, path, r.status, r.raw)
		}
	}
	// The general API refuses a guest too.
	for _, route := range [][2]string{
		{"DELETE", "/api/v1/participants/" + m.room + "/guest-x"},
		{"PATCH", "/api/v1/participants/" + m.room + "/guest-x"},
		{"POST", "/api/v1/tracks/" + m.room + "/guest-x/TR_x/mute"},
		{"DELETE", "/api/v1/rooms/" + m.room},
		{"PATCH", "/api/v1/rooms/" + m.room},
		{"GET", "/api/v1/participants?room=" + m.room},
		{"POST", "/api/v1/join-links"},
		{"POST", "/api/v1/tokens"},
		{"GET", "/api/v1/egress"},
		{"GET", "/api/v1/recordings"},
		// Webhook addresses and what was delivered to them cover the whole project.
		{"GET", "/api/v1/webhooks"},
		{"GET", "/api/v1/webhooks/" + hook},
		{"GET", "/api/v1/webhooks/" + hook + "/deliveries"},
	} {
		if r := guest.do(route[0], route[1], map[string]any{}); r.status != 403 {
			t.Fatalf("a guest called %s %s: %d %s", route[0], route[1], r.status, r.raw)
		}
	}
	// Nor can a guest start or stop a recording.
	recording := map[string]any{"type": "room_composite", "room": m.room, "output": map[string]any{"kind": "file", "format": "mp4"}}
	if r := guest.do("POST", "/api/v1/egress", recording); r.status != 403 {
		t.Fatalf("a guest started a recording: %d %s", r.status, r.raw)
	}

	// What a guest reads of the meeting does not include who is waiting or how to invite people.
	view := expect(t, guest.do("GET", m.path(""), nil), 200)
	if view.body["role"] != "guest" {
		t.Fatalf("view: %s", view.raw)
	}
	for _, field := range []string{"waiting", "invite_url", "recording"} {
		if _, ok := view.body[field]; ok {
			t.Fatalf("a guest can read %s: %s", field, view.raw)
		}
	}
}

func TestHostsOfOneMeetingHaveNoSayInAnother(t *testing.T) {
	h := newHarness(t, nil)
	a := newMeeting(t, h, "a@example.com")
	b := newMeeting(t, h, "b@example.com")
	host, _ := a.join(t, a.hostCode, "Hana")

	for _, route := range hostOnlyRoutes(b) {
		method, path, body := route[0].(string), route[1].(string), route[2]
		if r := host.do(method, path, body); r.status != 403 {
			t.Fatalf("a host of another meeting called %s %s: %d %s", method, path, r.status, r.raw)
		}
	}
	recording := map[string]any{"type": "room_composite", "room": b.room, "output": map[string]any{"kind": "file", "format": "mp4"}}
	if r := host.do("POST", "/api/v1/egress", recording); r.status != 403 {
		t.Fatalf("a host recorded another meeting: %d %s", r.status, r.raw)
	}
}

func TestHostsCanStartOnlyTheirOwnRoomsRecording(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")

	// Past the permission check, this gets as far as there being no worker.
	file := map[string]any{"kind": "file", "format": "mp4"}
	if r := host.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": m.room, "output": file}); r.status != 503 || r.errCode() != "egress_unavailable" {
		t.Fatalf("a host could not ask for a recording: %d %s", r.status, r.raw)
	}
	// Everything else about recording stays with the grant, which reaches the whole project.
	for _, body := range []map[string]any{
		{"type": "room_composite", "room": "elsewhere", "output": file},
		{"type": "participant", "participant": "guest-x", "room": m.room, "output": file},
		{"type": "audio_only", "room": m.room, "output": file},
		{"type": "room_composite", "room": m.room, "stream_urls": []string{"rtmp://example.com/live/key"}},
		{"type": "room_composite", "room": m.room, "output": file, "stream_urls": []string{"rtmp://example.com/live/key"}},
	} {
		if r := host.do("POST", "/api/v1/egress", body); r.status != 403 {
			t.Fatalf("a host could start %v: %d %s", body, r.status, r.raw)
		}
	}
	for _, path := range []string{"/api/v1/egress", "/api/v1/recordings", "/api/v1/recordings/EG_x", "/api/v1/recordings/EG_x/file", "/api/v1/egress/EG_x"} {
		if r := host.do("GET", path, nil); r.status != 403 {
			t.Fatalf("a host read %s: %d %s", path, r.status, r.raw)
		}
	}

	// Stopping is limited the same way, by the room of the job.
	ctx := context.Background()
	mine, err := h.store.CreateEgress(ctx, &store.Egress{ProjectID: m.owner.project, RoomName: m.room, RoomSID: "RM_a", Type: "room_composite", Format: "mp4", Request: store.EgressRequest{Type: "room_composite", Room: m.room}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.store.CreateEgress(ctx, &store.Egress{ProjectID: m.owner.project, RoomName: "elsewhere", RoomSID: "RM_b", Type: "room_composite", Format: "mp4", Request: store.EgressRequest{Type: "room_composite", Room: "elsewhere"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest, _ := m.join(t, m.guestCode, "Gita")
	expect(t, guest.do("POST", "/api/v1/egress/"+mine.ID+"/stop", map[string]any{}), 403)
	expect(t, host.do("POST", "/api/v1/egress/"+other.ID+"/stop", map[string]any{}), 403)

	view := expect(t, host.do("GET", m.path(""), nil), 200)
	if rec, _ := view.body["recording"].(map[string]any); rec["id"] != mine.ID {
		t.Fatalf("a host cannot see what is recording: %s", view.raw)
	}
	expect(t, host.do("POST", "/api/v1/egress/"+mine.ID+"/stop", map[string]any{}), 200)
}

func TestInviteLinkIsNeverTheHostLink(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	view := expect(t, host.do("GET", m.path(""), nil), 200)
	if code := codeIn(t, view.body["invite_url"].(string)); code != m.guestCode {
		t.Fatalf("invite code %q, guest code %q", code, m.guestCode)
	}
}

func refused(t *testing.T, h *harness, m *meeting, v *auth.Verified, commit bool, want error) {
	t.Helper()
	_, err := h.api.Admit(m.owner.project, m.room, v.Grants, commit)
	if !errors.Is(err, want) {
		t.Fatalf("admission said %v, want %v", err, want)
	}
}

func TestLockKeepsNewPeopleOut(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, hostClaims := m.join(t, m.hostCode, "Hana")
	_, inside := m.join(t, m.guestCode, "In early")
	_, holding := m.join(t, m.guestCode, "Token, not connected")
	if _, err := h.api.Admit(m.owner.project, m.room, inside.Grants, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.api.Admit(m.owner.project, m.room, hostClaims.Grants, true); err != nil {
		t.Fatal(err)
	}

	locked := expect(t, host.do("PATCH", m.path(""), map[string]any{"locked": true}), 200)
	if locked.body["locked"] != true {
		t.Fatalf("lock: %s", locked.raw)
	}

	// No new token.
	preview := expect(t, h.client().do("POST", "/api/v1/join/preview", map[string]any{"code": m.guestCode}), 200)
	if preview.body["locked"] != true {
		t.Fatalf("preview: %s", preview.raw)
	}
	if r := h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "Late"}); r.status != http.StatusLocked || r.errCode() != "meeting_locked" {
		t.Fatalf("join while locked: %d %s", r.status, r.raw)
	}
	// A token from before the lock, not used yet, does not get in either. Both questions the server is asked say so.
	refused(t, h, m, holding, false, refusedLocked)
	refused(t, h, m, holding, true, refusedLocked)
	// Asking must not have let them in.
	refused(t, h, m, holding, true, refusedLocked)
	// Whoever was already in can come back after a dropped connection, and so can a host.
	if _, err := h.api.Admit(m.owner.project, m.room, inside.Grants, true); err != nil {
		t.Fatalf("someone in the meeting could not reconnect: %v", err)
	}
	if _, err := h.api.Admit(m.owner.project, m.room, hostClaims.Grants, true); err != nil {
		t.Fatalf("the host could not reconnect: %v", err)
	}
	_, secondHost := m.join(t, m.hostCode, "Second host")
	if _, err := h.api.Admit(m.owner.project, m.room, secondHost.Grants, true); err != nil {
		t.Fatalf("a host could not join a locked meeting: %v", err)
	}

	expect(t, host.do("PATCH", m.path(""), map[string]any{"locked": false}), 200)
	if _, err := h.api.Admit(m.owner.project, m.room, holding.Grants, true); err != nil {
		t.Fatalf("unlocking did not open the door: %v", err)
	}
	m.join(t, m.guestCode, "After")
}

func TestRemovedTokenStaysOut(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	guest, claims := m.join(t, m.guestCode, "Gita")
	if _, err := h.api.Admit(m.owner.project, m.room, claims.Grants, true); err != nil {
		t.Fatal(err)
	}
	expect(t, guest.do("GET", m.path(""), nil), 200)

	h.rooms.SetAdmission(h.api)
	h.api.Removed(m.owner.project, m.room, claims.Grants.Identity)

	refused(t, h, m, claims, false, refusedRemoved)
	refused(t, h, m, claims, true, refusedRemoved)
	expect(t, guest.do("GET", m.path(""), nil), 403)
	expect(t, guest.do("POST", m.path("/hand"), map[string]any{"raised": true}), 403)
	// The refusal holds for a token the server refreshed for them, which names the same person with hours left.
	refreshed, err := auth.Mint(auth.Key{APIKey: claims.Key.APIKey, Secret: claims.Key.Secret}, claims.Grants, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	again, err := auth.Verify(refreshed, h.keys)
	if err != nil {
		t.Fatal(err)
	}
	refused(t, h, m, again, true, refusedRemoved)

	// The manager asks the same question for a real connection, before it would open a room.
	if err := h.rooms.CheckAdmission(m.owner.project, m.room, claims.Grants); !errors.Is(err, refusedRemoved) {
		t.Fatalf("check: %v", err)
	}
	if _, err := h.rooms.Join(room.JoinRequest{RoomName: m.room, Verified: claims}); !errors.Is(err, refusedRemoved) {
		t.Fatalf("join: %v", err)
	}
	if h.rooms.Get(m.owner.project, m.room) != nil {
		t.Fatal("a refused token opened the room")
	}
}

func TestTokensWithoutARowAreNotTheMeetingsBusiness(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"locked": true}), 200)

	// A recorder joining a locked meeting, and a project's own token.
	for _, g := range []*auth.Grants{
		{Identity: "EG_x", Video: &auth.VideoGrant{RoomJoin: true, Room: m.room, Hidden: true, Recorder: true}},
		{Identity: "backend", Video: &auth.VideoGrant{RoomJoin: true, Room: m.room}},
	} {
		if _, err := h.api.Admit(m.owner.project, m.room, g, true); err != nil {
			t.Fatalf("%s was refused: %v", g.Identity, err)
		}
	}
}

func TestWaitingRoom(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	_, early := m.join(t, m.guestCode, "Token from before")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)

	knock := func(name string) string {
		t.Helper()
		r := expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": name}), 202)
		if r.body["status"] != "waiting" || r.body["token"] != nil {
			t.Fatalf("knocking: %s", r.raw)
		}
		return r.body["ticket"].(string)
	}
	wait := func(ticket string) response {
		t.Helper()
		return h.client().do("POST", "/api/v1/join/wait", map[string]any{"ticket": ticket})
	}

	// A host link goes straight in, and a token from before the waiting room does not.
	m.join(t, m.hostCode, "Second host")
	refused(t, h, m, early, true, refusedWaiting)

	asha := knock("Asha")
	if r := expect(t, wait(asha), 200); r.body["status"] != "waiting" {
		t.Fatalf("waiting: %s", r.raw)
	}
	ben := knock("Ben")
	wait(ben)

	view := expect(t, host.do("GET", m.path(""), nil), 200)
	waiting, _ := view.body["waiting"].([]any)
	if len(waiting) != 2 || waiting[0].(map[string]any)["name"] != "Asha" {
		t.Fatalf("waiting list: %s", view.raw)
	}
	ashaID := waiting[0].(map[string]any)["identity"].(string)
	benID := waiting[1].(map[string]any)["identity"].(string)

	// Being let in comes with the token, once.
	expect(t, host.do("POST", m.path("/waiting/"+ashaID), map[string]any{"admit": true}), 204)
	in := expect(t, wait(asha), 201)
	v, err := auth.Verify(in.body["token"].(string), h.keys)
	if err != nil {
		t.Fatal(err)
	}
	if v.Grants.Identity != ashaID || v.Grants.Name != "Asha" || v.Grants.Video.Room != m.room {
		t.Fatalf("token: %+v", v.Grants)
	}
	if r := wait(asha); r.status != 404 {
		t.Fatalf("a ticket gave two tokens: %d %s", r.status, r.raw)
	}
	// A host let them in, so a lock after that does not stop them.
	expect(t, host.do("PATCH", m.path(""), map[string]any{"locked": true}), 200)
	if _, err := h.api.Admit(m.owner.project, m.room, v.Grants, true); err != nil {
		t.Fatalf("someone let in was refused: %v", err)
	}
	// Locking sends away whoever is still waiting.
	if r := wait(ben); r.status != http.StatusLocked || r.errCode() != "meeting_locked" {
		t.Fatalf("waiting through a lock: %d %s", r.status, r.raw)
	}
	if r := host.do("POST", m.path("/waiting/"+benID), map[string]any{"admit": true}); r.status != 404 {
		t.Fatalf("admitting someone sent away: %d %s", r.status, r.raw)
	}
	expect(t, host.do("PATCH", m.path(""), map[string]any{"locked": false}), 200)

	// Being turned away.
	carol := knock("Carol")
	wait(carol)
	view = expect(t, host.do("GET", m.path(""), nil), 200)
	waiting, _ = view.body["waiting"].([]any)
	if len(waiting) != 1 {
		t.Fatalf("waiting list: %s", view.raw)
	}
	expect(t, host.do("POST", m.path("/waiting/"+waiting[0].(map[string]any)["identity"].(string)), map[string]any{"admit": false}), 204)
	if r := wait(carol); r.status != 403 || r.errCode() != "waiting_denied" {
		t.Fatalf("turned away: %d %s", r.status, r.raw)
	}

	// Giving up a place.
	dev := knock("Dev")
	expect(t, h.client().do("POST", "/api/v1/join/wait", map[string]any{"ticket": dev, "cancel": true}), 204)
	if r := wait(dev); r.status != 404 {
		t.Fatalf("a cancelled ticket still works: %d %s", r.status, r.raw)
	}

	// Turning the waiting room off opens the door for whoever is at it.
	eli := knock("Eli")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": false}), 200)
	if r := wait(eli); r.status != 201 || r.body["token"] == nil {
		t.Fatalf("waiting room off: %d %s", r.status, r.raw)
	}
	if r := wait("not-a-real-ticket"); r.status != 404 {
		t.Fatalf("made-up ticket: %d %s", r.status, r.raw)
	}

	// A waiting room can be on from the start.
	made := expect(t, m.owner.do("POST", "/api/v1/meetings", map[string]any{"title": "Closed door", "waiting_room": true}), 201)
	expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": codeIn(t, made.body["guest_url"].(string)), "name": "Fay"}), 202)
}

func TestWaitingTicketStopsWithItsLink(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	ticket := expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "Asha"}), 202).body["ticket"].(string)
	links := expect(t, m.owner.do("GET", "/api/v1/join-links?room="+m.room, nil), 200).items()
	for _, l := range links {
		link := l.(map[string]any)
		if link["role"] == "guest" {
			expect(t, m.owner.do("DELETE", "/api/v1/join-links/"+link["id"].(string), nil), 204)
		}
	}
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": false}), 200)
	if r := h.client().do("POST", "/api/v1/join/wait", map[string]any{"ticket": ticket}); r.status != http.StatusGone || r.errCode() != "link_revoked" {
		t.Fatalf("turned-off link: %d %s", r.status, r.raw)
	}
}

func TestEndingAMeeting(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, hostClaims := m.join(t, m.hostCode, "Hana")
	guest, guestClaims := m.join(t, m.guestCode, "Gita")
	if _, err := h.api.Admit(m.owner.project, m.room, guestClaims.Grants, true); err != nil {
		t.Fatal(err)
	}
	expect(t, host.do("PATCH", m.path(""), map[string]any{"locked": true, "waiting_room": true}), 200)
	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)

	// Every token it handed out stops, host's included.
	refused(t, h, m, guestClaims, true, refusedEnded)
	refused(t, h, m, hostClaims, true, refusedEnded)
	expect(t, host.do("POST", m.path("/mute-all"), map[string]any{}), 403)
	expect(t, guest.do("GET", m.path(""), nil), 403)

	// The guest link says it has ended.
	preview := expect(t, h.client().do("POST", "/api/v1/join/preview", map[string]any{"code": m.guestCode}), 200)
	if preview.body["ended"] != true {
		t.Fatalf("preview: %s", preview.raw)
	}
	if r := h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode}); r.status != http.StatusGone || r.errCode() != "meeting_ended" {
		t.Fatalf("join after the end: %d %s", r.status, r.raw)
	}

	// A host starts it again, with the door back to how a new meeting starts, and old tokens stay dead.
	m.join(t, m.hostCode, "Hana")
	preview = expect(t, h.client().do("POST", "/api/v1/join/preview", map[string]any{"code": m.guestCode}), 200)
	if preview.body["ended"] != false || preview.body["locked"] != false || preview.body["waiting_room"] != false {
		t.Fatalf("preview: %s", preview.raw)
	}
	m.join(t, m.guestCode, "Gita")
	refused(t, h, m, guestClaims, true, refusedEnded)
}

func TestModeratorsSitBetweenGuestsAndHosts(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	mod, claims := m.join(t, m.guestCode, "Mo")
	if err := h.store.SetMeetingMemberRole(context.Background(), m.owner.project, m.room, claims.Grants.Identity, store.RoleModerator); err != nil {
		t.Fatal(err)
	}
	// Authority is read when it is used, so this took effect with the token already issued.
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	view := expect(t, mod.do("GET", m.path(""), nil), 200)
	if view.body["role"] != "moderator" || view.body["invite_url"] == nil {
		t.Fatalf("moderator's view: %s", view.raw)
	}
	r := expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "Asha"}), 202)
	h.client().do("POST", "/api/v1/join/wait", map[string]any{"ticket": r.body["ticket"]})
	view = expect(t, mod.do("GET", m.path(""), nil), 200)
	waiting, _ := view.body["waiting"].([]any)
	expect(t, mod.do("POST", m.path("/waiting/"+waiting[0].(map[string]any)["identity"].(string)), map[string]any{"admit": true}), 204)

	// What decides the whole meeting stays with hosts.
	expect(t, mod.do("PATCH", m.path(""), map[string]any{"locked": true}), 403)
	expect(t, mod.do("POST", m.path("/end"), map[string]any{}), 403)
	expect(t, mod.do("PATCH", m.path("/participants/guest-x"), map[string]any{"role": "moderator"}), 403)
	recording := map[string]any{"type": "room_composite", "room": m.room, "output": map[string]any{"kind": "file", "format": "mp4"}}
	expect(t, mod.do("POST", "/api/v1/egress", recording), 403)
}

func TestMeetingSummaryIsWhatHappened(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	guest, claims := m.join(t, m.guestCode, "Gita")
	ctx := context.Background()

	// Nothing has happened yet.
	expect(t, guest.do("GET", m.path("/summary"), nil), 404)

	started := time.Now().Add(-time.Minute)
	if err := h.store.RoomStarted(ctx, store.RoomRecord{SID: "RM_s", ProjectID: m.owner.project, Name: m.room, CreatedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ParticipantJoined(ctx, "PA_a", "RM_s", m.owner.project, claims.Grants.Identity, "Gita", "STANDARD", started, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateEgress(ctx, &store.Egress{ProjectID: m.owner.project, RoomName: m.room, RoomSID: "RM_s", Type: "room_composite", Format: "mp4", Request: store.EgressRequest{Type: "room_composite", Room: m.room}}, nil); err != nil {
		t.Fatal(err)
	}
	r := expect(t, guest.do("GET", m.path("/summary"), nil), 200)
	people, _ := r.body["people"].([]any)
	if r.body["title"] != "Planning" || len(people) != 1 || people[0].(map[string]any)["name"] != "Gita" || people[0].(map[string]any)["role"] != "guest" {
		t.Fatalf("summary: %s", r.raw)
	}
	if recordings, _ := r.body["recordings"].([]any); len(recordings) != 1 {
		t.Fatalf("summary: %s", r.raw)
	}

	// Still readable once the meeting has ended. Not for someone removed, or from outside.
	expect(t, h.client().do("GET", m.path("/summary"), nil), 401)
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)
	expect(t, guest.do("GET", m.path("/summary"), nil), 200)
	m.join(t, m.hostCode, "Hana")
	gone, goneClaims := m.join(t, m.guestCode, "Gone")
	h.api.Removed(m.owner.project, m.room, goneClaims.Grants.Identity)
	expect(t, gone.do("GET", m.path("/summary"), nil), 403)
}
