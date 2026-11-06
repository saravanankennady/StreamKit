package routes

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
)

func TestLinksAndMeetingsHandOutOnlySoManyIdentities(t *testing.T) {
	h := newHarness(t, nil)
	h.api.memberLimits = store.MemberLimits{PerLink: 3, PerMeeting: 5}
	m := newMeeting(t, h, "owner@example.com")
	join := func(code string) response {
		t.Helper()
		return h.client().do("POST", "/api/v1/join", map[string]any{"code": code, "name": "Someone"})
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := h.store.DB.QueryRow(context.Background(), `SELECT count(*) FROM meeting_members WHERE room = $1`, m.room).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for range 3 {
		expect(t, join(m.guestCode), 201)
	}
	if r := join(m.guestCode); r.status != http.StatusTooManyRequests || r.errCode() != "too_many_joins" {
		t.Fatalf("a fourth use of the link: %d %s", r.status, r.raw)
	}
	if rows() != 3 {
		t.Fatalf("%d rows", rows())
	}

	// Another link to the meeting has its own count, up to the meeting's.
	other := codeOf(t, expect(t, m.owner.do("POST", "/api/v1/join-links", map[string]any{"room": m.room}), 201))
	expect(t, join(other), 201)
	expect(t, join(other), 201)
	if r := join(other); r.status != http.StatusTooManyRequests {
		t.Fatalf("over the meeting's count: %d %s", r.status, r.raw)
	}
	if rows() != 5 {
		t.Fatalf("%d rows", rows())
	}

	// A flood of guests must not lock out whoever can do something about it.
	expect(t, join(m.hostCode), 201)

	// Knocking at the waiting room counts as well.
	host, _ := m.join(t, m.hostCode, "Hana")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	third := codeOf(t, expect(t, m.owner.do("POST", "/api/v1/join-links", map[string]any{"room": m.room}), 201))
	if r := join(third); r.status != http.StatusTooManyRequests {
		t.Fatalf("knocking over the meeting's count: %d %s", r.status, r.raw)
	}
}

func TestWaitingListIsLimitedAndCounted(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	ctx := context.Background()
	const waiting = maxWaitingShown + 5
	for i := range waiting {
		member := store.MeetingMember{ProjectID: m.owner.project, Room: m.room, Identity: fmt.Sprintf("guest-%03d", i), Name: fmt.Sprintf("Person %d", i), Role: store.RoleGuest, State: store.MemberWaiting}
		if err := h.store.AddMeetingMember(ctx, member, fmt.Sprintf("ticket-%d", i), store.MemberLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	view := expect(t, host.do("GET", m.path(""), nil), 200)
	list, _ := view.body["waiting"].([]any)
	if len(list) != maxWaitingShown {
		t.Fatalf("the list has %d people, with a limit of %d", len(list), maxWaitingShown)
	}
	if view.body["waiting_count"] != float64(waiting) {
		t.Fatalf("waiting_count = %v, want %d", view.body["waiting_count"], waiting)
	}
	if list[0].(map[string]any)["identity"] != "guest-000" {
		t.Fatalf("whoever has waited longest should be first: %v", list[0])
	}
	// Someone who stopped asking is not waiting, and is not counted.
	if _, err := h.store.DB.Exec(ctx, `UPDATE meeting_members SET seen_at = now() - interval '1 hour' WHERE identity = 'guest-000'`); err != nil {
		t.Fatal(err)
	}
	view = expect(t, host.do("GET", m.path(""), nil), 200)
	if view.body["waiting_count"] != float64(waiting-1) {
		t.Fatalf("waiting_count = %v, want %d", view.body["waiting_count"], waiting-1)
	}
}

func TestTurningAwayEveryoneWaiting(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	inside, _ := m.join(t, m.guestCode, "Already in")
	expect(t, host.do("PATCH", m.path(""), map[string]any{"waiting_room": true}), 200)
	knock := func() string {
		t.Helper()
		return expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": m.guestCode, "name": "Knocking"}), 202).body["ticket"].(string)
	}
	wait := func(ticket string) response {
		t.Helper()
		return h.client().do("POST", "/api/v1/join/wait", map[string]any{"ticket": ticket})
	}
	first, second := knock(), knock()
	wait(first)
	wait(second)

	expect(t, inside.do("DELETE", m.path("/waiting"), nil), 403)
	expect(t, h.client().do("DELETE", m.path("/waiting"), nil), 401)

	r := expect(t, host.do("DELETE", m.path("/waiting"), nil), 200)
	if r.body["turned_away"] != float64(2) {
		t.Fatalf("turned away: %s", r.raw)
	}
	for _, ticket := range []string{first, second} {
		if r := wait(ticket); r.status != 403 || r.errCode() != "waiting_denied" {
			t.Fatalf("after being turned away: %d %s", r.status, r.raw)
		}
	}
	view := expect(t, host.do("GET", m.path(""), nil), 200)
	if view.body["waiting_count"] != float64(0) {
		t.Fatalf("waiting: %s", view.raw)
	}
	// It is about who was waiting, not what happens next.
	if view.body["waiting_room"] != true || view.body["locked"] != false {
		t.Fatalf("the door changed: %s", view.raw)
	}
	expect(t, inside.do("GET", m.path(""), nil), 200)
	third := knock()
	if r := expect(t, wait(third), 200); r.body["status"] != "waiting" {
		t.Fatalf("someone new: %s", r.raw)
	}
}

func TestMeetingsHaveAParticipantCapByDefault(t *testing.T) {
	admitted := func(t *testing.T, h *harness, m *meeting) uint32 {
		t.Helper()
		_, claims := m.join(t, m.guestCode, "Gita")
		got, err := h.api.Admit(m.owner.project, m.room, claims.Grants, true)
		if err != nil {
			t.Fatal(err)
		}
		return got.MaxParticipants
	}
	h := newHarness(t, nil)
	m := newMeeting(t, h, "a@example.com")
	if got := admitted(t, h, m); got != config.Default().Room.MeetingMaxParticipants || got == 0 {
		t.Fatalf("the default cap is %d", got)
	}

	// What an operator set for rooms in general, or a project for its own, says more than this default does.
	id := m.owner.project
	expect(t, m.owner.do("PATCH", "/api/v1/projects/"+id, map[string]any{"settings": map[string]any{"max_participants": 3}}), 200)
	if got := admitted(t, h, m); got != 3 {
		t.Fatalf("with a project's own limit, the cap is %d", got)
	}

	h = newHarness(t, func(c *config.Config) { c.Room.MeetingMaxParticipants = 7 })
	if got := admitted(t, h, newMeeting(t, h, "b@example.com")); got != 7 {
		t.Fatalf("with this set in the config, the cap is %d", got)
	}
	h = newHarness(t, func(c *config.Config) { c.Room.MeetingMaxParticipants = 0 })
	if got := admitted(t, h, newMeeting(t, h, "c@example.com")); got != 0 {
		t.Fatalf("with no cap set, the cap is %d", got)
	}
	h = newHarness(t, func(c *config.Config) { c.Room.MaxParticipants = 9 })
	if got := admitted(t, h, newMeeting(t, h, "d@example.com")); got != 9 {
		t.Fatalf("with room.max_participants set, the cap is %d", got)
	}

	// A project's own token has no say from this.
	h = newHarness(t, nil)
	m = newMeeting(t, h, "e@example.com")
	got, err := h.api.Admit(m.owner.project, m.room, &auth.Grants{Identity: "backend", Video: &auth.VideoGrant{RoomJoin: true, Room: m.room}}, true)
	if err != nil || got.MaxParticipants != 0 {
		t.Fatalf("a project's own token: %+v %v", got, err)
	}
}

func TestAuditRowsGoAfterTheirRetention(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	ctx := context.Background()
	project := m.owner.project
	for _, action := range []string{"old", "kept"} {
		if err := h.store.Audit(ctx, project, "", action, "x", "127.0.0.1", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE audit_logs SET created_at = now() - make_interval(days => 366) WHERE action = 'old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE audit_logs SET created_at = now() - make_interval(days => 364) WHERE action = 'kept'`); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PurgeOld(ctx); err != nil {
		t.Fatal(err)
	}
	var old, kept int
	if err := h.store.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action = 'old'), count(*) FILTER (WHERE action = 'kept') FROM audit_logs`).Scan(&old, &kept); err != nil {
		t.Fatal(err)
	}
	if old != 0 || kept != 1 {
		t.Fatalf("%d rows past the retention, %d within it", old, kept)
	}
}
