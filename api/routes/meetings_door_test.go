package routes

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

// A token can be in flight when a meeting ends: read the meeting, the host ends it, then save the row. Only the door can catch
// that, and it has to look at the meeting as well as the row.
func TestARowMadeAsAMeetingEndsIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	ctx := context.Background()
	host, _ := m.join(t, m.hostCode, "Hana")
	guest, claims := m.join(t, m.guestCode, "Late")
	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)

	// The end has marked the row. This is what it looks like if the row came a moment after.
	if _, err := h.store.DB.Exec(ctx, `UPDATE meeting_members SET state = 'minted' WHERE identity = $1`, claims.Grants.Identity); err != nil {
		t.Fatal(err)
	}
	refused(t, h, m, claims, false, refusedEnded)
	refused(t, h, m, claims, true, refusedEnded)
	expect(t, guest.do("GET", m.path(""), nil), 403)
	expect(t, guest.do("POST", m.path("/hand"), map[string]any{"raised": true}), 403)

	// A host in the same position has the same answer, and opening the host link again starts it.
	if _, err := h.store.DB.Exec(ctx, `UPDATE meeting_members SET state = 'admitted' WHERE identity = $1`, claims.Grants.Identity); err != nil {
		t.Fatal(err)
	}
	refused(t, h, m, claims, true, refusedEnded)
	m.join(t, m.hostCode, "Hana")
	if _, err := h.api.Admit(m.owner.project, m.room, claims.Grants, true); err != nil {
		t.Fatalf("the meeting is on again: %v", err)
	}
}

// A ticket swap reads the state and then writes it, and a host's answer can come in between.
func TestSpendingATicketLooksAtTheStateAgain(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	ctx := context.Background()
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	knock := func() (ticket, identity string) {
		t.Helper()
		ticket = expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "Asha"}), 202).body["ticket"].(string)
		member, err := h.store.MemberByTicket(ctx, ticket)
		if err != nil {
			t.Fatal(err)
		}
		return ticket, member.Identity
	}
	state := func(identity string) string {
		t.Helper()
		member, err := h.store.MeetingMemberByIdentity(ctx, m.owner.project, m.room, identity)
		if err != nil {
			t.Fatal(err)
		}
		return member.State
	}

	// Still waiting, with the door shut, it cannot be spent.
	ticket, identity := knock()
	if err := h.store.SpendTicket(ctx, ticket); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spending a ticket someone is still waiting on: %v", err)
	}
	if state(identity) != store.MemberWaiting {
		t.Fatalf("state %s", state(identity))
	}

	// Turned away between the check and the swap.
	if err := h.store.AnswerWaiting(ctx, m.owner.project, m.room, identity, false); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SpendTicket(ctx, ticket); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spending a ticket after being turned away: %v", err)
	}
	if state(identity) != store.MemberDenied {
		t.Fatalf("being turned away was overwritten: %s", state(identity))
	}

	// Let in, it can be, once.
	ticket, identity = knock()
	if err := h.store.AnswerWaiting(ctx, m.owner.project, m.room, identity, true); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SpendTicket(ctx, ticket); err != nil {
		t.Fatalf("a ticket someone was let in on: %v", err)
	}
	if err := h.store.SpendTicket(ctx, ticket); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spending a ticket twice: %v", err)
	}

	// With the waiting room turned off, whoever is at the door goes in.
	ticket, identity = knock()
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": false}), 200)
	if err := h.store.SpendTicket(ctx, ticket); err != nil {
		t.Fatalf("a ticket with the waiting room off: %v", err)
	}
	if state(identity) != store.MemberAdmitted {
		t.Fatalf("state %s", state(identity))
	}

	// The meeting ends between the check and the swap.
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	ticket, identity = knock()
	if err := h.store.AnswerWaiting(ctx, m.owner.project, m.room, identity, true); err != nil {
		t.Fatal(err)
	}
	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)
	if _, err := h.store.DB.Exec(ctx, `UPDATE meeting_members SET state = 'admitted' WHERE identity = $1`, identity); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SpendTicket(ctx, ticket); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spending a ticket after the end: %v", err)
	}
}

func TestATokenFromALinkWithNoRowIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	_, claims := m.join(t, m.guestCode, "Gita")
	if _, err := h.store.DB.Exec(context.Background(), `DELETE FROM meeting_members WHERE identity = $1`, claims.Grants.Identity); err != nil {
		t.Fatal(err)
	}
	// Its row may have been purged, and with it any refusal it held. A valid token says nothing about what happened since.
	refused(t, h, m, claims, false, refusedNoRow)
	refused(t, h, m, claims, true, refusedNoRow)
}

func TestRowsGoByLastActivity(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	ctx := context.Background()
	exists := func(identity string) bool {
		t.Helper()
		_, err := h.store.MeetingMemberByIdentity(ctx, m.owner.project, m.room, identity)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	age := func(identity string, days int) {
		t.Helper()
		if _, err := h.store.DB.Exec(ctx, `UPDATE meeting_members SET created_at = now() - make_interval(days => $2), seen_at = now() - make_interval(days => $2) WHERE identity = $1`, identity, days); err != nil {
			t.Fatal(err)
		}
	}
	_, inUse := m.join(t, m.guestCode, "In use")
	if _, err := h.api.Admit(m.owner.project, m.room, inUse.Grants, true); err != nil {
		t.Fatal(err)
	}
	_, forgotten := m.join(t, m.guestCode, "Forgotten")
	if _, err := h.api.Admit(m.owner.project, m.room, forgotten.Grants, true); err != nil {
		t.Fatal(err)
	}
	_, neverIn := m.join(t, m.guestCode, "Never came in")

	// Old rows, one of which someone came back to just now.
	age(inUse.Grants.Identity, 40)
	age(forgotten.Grants.Identity, 31)
	if _, err := h.api.Admit(m.owner.project, m.room, inUse.Grants, true); err != nil {
		t.Fatal(err)
	}
	age(neverIn.Grants.Identity, 2)

	if err := h.store.PurgeOld(ctx); err != nil {
		t.Fatal(err)
	}
	if !exists(inUse.Grants.Identity) {
		t.Fatal("the row of someone active just now went, because it was made long ago")
	}
	if exists(forgotten.Grants.Identity) {
		t.Fatal("the row of someone long gone is still here")
	}
	if exists(neverIn.Grants.Identity) {
		t.Fatal("the row of someone who never came in is still here")
	}
}

func TestSummaryIsForThoseWhoWereIn(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	ctx := context.Background()
	in, inClaims := m.join(t, m.guestCode, "Was in")
	holding, _ := m.join(t, m.guestCode, "Never connected")
	started := time.Now().Add(-time.Hour)
	if err := h.store.RoomStarted(ctx, store.RoomRecord{SID: "RM_first", ProjectID: m.owner.project, Name: m.room, CreatedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ParticipantJoined(ctx, "PA_first", "RM_first", m.owner.project, inClaims.Grants.Identity, "Was in", "STANDARD", started, 1); err != nil {
		t.Fatal(err)
	}

	// Holding a token shows nothing about who was there, whether or not it was ever used.
	if r := holding.do("GET", m.path("/summary"), nil); r.status != http.StatusNotFound {
		t.Fatalf("someone who never connected read the summary: %d %s", r.status, r.raw)
	}
	expect(t, in.do("GET", m.path("/summary"), nil), 200)

	// That goes for once it has ended, too, when every token is marked the same way.
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)
	if r := holding.do("GET", m.path("/summary"), nil); r.status != http.StatusNotFound {
		t.Fatalf("someone who never connected read the summary after the end: %d %s", r.status, r.raw)
	}
	expect(t, in.do("GET", m.path("/summary"), nil), 200)

	// A later session is not theirs to read. They get the one they were in.
	m.join(t, m.hostCode, "Hana")
	later, laterClaims := m.join(t, m.guestCode, "Only later")
	second := time.Now()
	if err := h.store.RoomStarted(ctx, store.RoomRecord{SID: "RM_second", ProjectID: m.owner.project, Name: m.room, CreatedAt: second}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ParticipantJoined(ctx, "PA_second", "RM_second", m.owner.project, laterClaims.Grants.Identity, "Only later", "STANDARD", second, 1); err != nil {
		t.Fatal(err)
	}
	first := expect(t, in.do("GET", m.path("/summary"), nil), 200)
	people, _ := first.body["people"].([]any)
	if len(people) != 1 || people[0].(map[string]any)["name"] != "Was in" {
		t.Fatalf("someone read a session they were not in: %s", first.raw)
	}
	seen := expect(t, later.do("GET", m.path("/summary"), nil), 200)
	people, _ = seen.body["people"].([]any)
	if len(people) != 1 || people[0].(map[string]any)["name"] != "Only later" {
		t.Fatalf("someone read an earlier session: %s", seen.raw)
	}
}

func TestAWaitingRoomPlaceLapsesAfterAQuietSpell(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	ctx := context.Background()
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	knock := func() string {
		t.Helper()
		return expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "Asha"}), 202).body["ticket"].(string)
	}
	wait := func(ticket string) response {
		t.Helper()
		return h.client().do("POST", "/api/v1/join/wait", map[string]any{"ticket": ticket})
	}
	quiet := func(ticket string) {
		t.Helper()
		if _, err := h.store.DB.Exec(ctx, `UPDATE meeting_members SET seen_at = now() - interval '1 minute' WHERE ticket_hash = $1`, store.HashToken(ticket)); err != nil {
			t.Fatal(err)
		}
	}

	ticket := knock()
	expect(t, wait(ticket), 200)
	quiet(ticket)
	if r := wait(ticket); r.status != 404 || r.errCode() != "ticket_not_found" {
		t.Fatalf("a place nobody had asked about for a minute: %d %s", r.status, r.raw)
	}

	// A browser that slept through being let in has still been let in, and being turned away still gets said.
	ticket = knock()
	view := expect(t, host.do("GET", m.path(""), nil), 200)
	waiting, _ := view.body["waiting"].([]any)
	if len(waiting) != 1 {
		t.Fatalf("waiting: %s", view.raw)
	}
	expect(t, host.do("POST", m.path("/waiting/"+waiting[0].(map[string]any)["identity"].(string)), map[string]any{"admit": true}), 204)
	quiet(ticket)
	if r := wait(ticket); r.status != 201 {
		t.Fatalf("someone let in, asking late: %d %s", r.status, r.raw)
	}
	ticket = knock()
	view = expect(t, host.do("GET", m.path(""), nil), 200)
	waiting, _ = view.body["waiting"].([]any)
	expect(t, host.do("POST", m.path("/waiting/"+waiting[0].(map[string]any)["identity"].(string)), map[string]any{"admit": false}), 204)
	quiet(ticket)
	if r := wait(ticket); r.status != 403 || r.errCode() != "waiting_denied" {
		t.Fatalf("someone turned away, asking late: %d %s", r.status, r.raw)
	}
}

func TestTextWithControlCharactersIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")

	for _, name := range []string{"a\u0000b", "a\nb", "a\tb", "\u001b[31mred", "a\u0085b"} {
		r := h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": name})
		if r.status != http.StatusUnprocessableEntity {
			t.Fatalf("joining as %q: %d %s", name, r.status, r.raw)
		}
	}
	// Anything else a person can be called, in any language, is fine.
	for _, name := range []string{"Zoë", "தமிழ்", "日本語", "O'Brien-Smith", "👩🏽‍💻"} {
		expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": name}), 201)
	}

	// The other text people type.
	expect(t, host.do("PATCH", m.path(""), map[string]any{"title": "a\u0000b"}), 422)
	expect(t, m.owner.do("POST", "/api/v1/meetings", map[string]any{"title": "a\u0000b"}), 422)
	expect(t, m.owner.do("POST", "/api/v1/join-links", map[string]any{"room": m.room, "label": "a\u0000b"}), 422)
	expect(t, m.owner.do("POST", "/api/v1/projects", map[string]any{"name": "a\u0000b"}), 422)
	expect(t, h.client().do("POST", "/api/v1/auth/register", map[string]string{"email": "n@example.com", "password": password, "name": "a\u0000b", "organization": "o"}), 422)
	expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": "a\u0000b"}), 422)

	// A password can hold what it likes, and has to keep working at login.
	strange := "a-good-long-password\twith a tab"
	c := h.client()
	expect(t, c.do("POST", "/api/v1/auth/register", map[string]string{"email": "tab@example.com", "password": strange, "name": "T", "organization": "o"}), 201)
	expect(t, h.client().do("POST", "/api/v1/auth/login", map[string]string{"email": "tab@example.com", "password": strange}), 200)

	// However it gets as far as the database, that is the client's mistake and not a failure here.
	if r := host.do("POST", m.path("/waiting/guest%00x"), map[string]any{"admit": true}); r.status >= 500 {
		t.Fatalf("a NUL in a path: %d %s", r.status, r.raw)
	}
	if r := host.do("POST", m.path("/participants/guest%00x/remove"), map[string]any{}); r.status >= 500 {
		t.Fatalf("a NUL in a path: %d %s", r.status, r.raw)
	}
	admin := h.client()
	admin.bearer = mustMint(t, m.key, &auth.Grants{Identity: "api", Video: &auth.VideoGrant{RoomAdmin: true, Room: m.room}})
	if r := admin.do("DELETE", "/api/v1/participants/"+m.room+"/guest%00x", nil); r.status >= 500 {
		t.Fatalf("a NUL in a path: %d %s", r.status, r.raw)
	}
}
