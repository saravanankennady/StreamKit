package routes

import (
	"context"
	"testing"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

func TestRemovingSomeoneWhoIsNotConnected(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, hostClaims := m.join(t, m.hostCode, "Hana")
	guest, guestClaims := m.join(t, m.guestCode, "Gita")
	remove := func(c *client, identity string) response {
		t.Helper()
		return c.do("POST", m.path("/participants/"+identity+"/remove"), map[string]any{})
	}

	// Nobody has connected, so there is not even a room. The refusal is what counts: their token is good for a while yet.
	expect(t, remove(host, guestClaims.Grants.Identity), 204)
	refused(t, h, m, guestClaims, false, refusedRemoved)
	refused(t, h, m, guestClaims, true, refusedRemoved)
	expect(t, guest.do("GET", m.path(""), nil), 403)

	// Someone who was in and has dropped off goes the same way.
	_, gone := m.join(t, m.guestCode, "Dropped off")
	if _, err := h.api.Admit(m.owner.project, m.room, gone.Grants, true); err != nil {
		t.Fatal(err)
	}
	expect(t, remove(host, gone.Grants.Identity), 204)
	refused(t, h, m, gone, true, refusedRemoved)

	// The same limits apply as for someone connected.
	if r := remove(host, hostClaims.Grants.Identity); r.status != 422 || r.errCode() != "cannot_remove_self" {
		t.Fatalf("removing yourself: %d %s", r.status, r.raw)
	}
	if r := remove(host, "guest-nobody"); r.status != 404 || r.errCode() != "participant_not_found" {
		t.Fatalf("removing nobody: %d %s", r.status, r.raw)
	}
	_, secondHost := m.join(t, m.hostCode, "Second host")
	mod, modClaims := m.join(t, m.guestCode, "Mo")
	if err := h.store.SetMeetingMemberRole(context.Background(), m.owner.project, m.room, modClaims.Grants.Identity, store.RoleModerator); err != nil {
		t.Fatal(err)
	}
	expect(t, remove(mod, secondHost.Grants.Identity), 403)
	if _, err := h.api.Admit(m.owner.project, m.room, secondHost.Grants, true); err != nil {
		t.Fatalf("a moderator's refused try still removed a host: %v", err)
	}
	_, third := m.join(t, m.guestCode, "Third")
	expect(t, remove(mod, third.Grants.Identity), 204)
	refused(t, h, m, third, true, refusedRemoved)
	expect(t, remove(guest, third.Grants.Identity), 403)

	// The general API holds to it too, for a project member and for a token with the grant.
	_, fourth := m.join(t, m.guestCode, "Fourth")
	expect(t, m.owner.do("DELETE", "/api/v1/participants/"+m.room+"/"+fourth.Grants.Identity, nil), 204)
	refused(t, h, m, fourth, true, refusedRemoved)
	_, fifth := m.join(t, m.guestCode, "Fifth")
	admin := h.client()
	admin.bearer = mustMint(t, m.key, &auth.Grants{Identity: "api", Video: &auth.VideoGrant{RoomAdmin: true, Room: m.room}})
	expect(t, admin.do("DELETE", "/api/v1/participants/"+m.room+"/"+fifth.Grants.Identity, nil), 204)
	refused(t, h, m, fifth, true, refusedRemoved)
	expect(t, m.owner.do("DELETE", "/api/v1/participants/"+m.room+"/guest-nobody", nil), 404)
}

// linkWithRole finds a meeting's link of one role, as a project member sees them.
func (m *meeting) linkWithRole(t *testing.T, role string) string {
	t.Helper()
	for _, l := range expect(t, m.owner.do("GET", "/api/v1/join-links?room="+m.room, nil), 200).items() {
		if link := l.(map[string]any); link["role"] == role {
			return link["id"].(string)
		}
	}
	t.Fatalf("no %s link", role)
	return ""
}

func TestRevokingAHostLinkStopsWhoeverUsedIt(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	// Whoever has the leaked link used it from somewhere else, and their token has hours of refreshes left in it.
	leaked, leakedClaims := m.join(t, m.hostCode, "Leaked")
	never, neverClaims := m.join(t, m.hostCode, "Never connected")
	if _, err := h.api.Admit(m.owner.project, m.room, leakedClaims.Grants, true); err != nil {
		t.Fatal(err)
	}
	expect(t, leaked.do("PATCH", m.path(""), map[string]any{"title": "Mine"}), 200)

	expect(t, m.owner.do("DELETE", "/api/v1/join-links/"+m.linkWithRole(t, "host"), nil), 204)

	for _, c := range []*client{leaked, never} {
		for _, route := range hostOnlyRoutes(m) {
			if r := c.do(route[0].(string), route[1].(string), route[2]); r.status != 403 {
				t.Fatalf("a revoked host called %s %s: %d %s", route[0], route[1], r.status, r.raw)
			}
		}
		expect(t, c.do("GET", m.path(""), nil), 403)
		recording := map[string]any{"type": "room_composite", "room": m.room, "output": map[string]any{"kind": "file", "format": "mp4"}}
		expect(t, c.do("POST", "/api/v1/egress", recording), 403)
	}
	refused(t, h, m, leakedClaims, false, refusedLinkRevoked)
	refused(t, h, m, leakedClaims, true, refusedLinkRevoked)
	refused(t, h, m, neverClaims, true, refusedLinkRevoked)

	// A new host link is how the meeting goes on.
	made := expect(t, m.owner.do("POST", "/api/v1/join-links", map[string]any{"room": m.room, "role": "host"}), 201)
	fresh, freshClaims := m.join(t, codeOf(t, made), "Hana")
	expect(t, fresh.do("PATCH", m.path(""), map[string]any{"title": "Ours"}), 200)
	if _, err := h.api.Admit(m.owner.project, m.room, freshClaims.Grants, true); err != nil {
		t.Fatalf("a host from a new link was refused: %v", err)
	}
}

func TestRevokingAGuestLinkLeavesThoseInTheMeeting(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	inside, insideClaims := m.join(t, m.guestCode, "Inside")
	holding, holdingClaims := m.join(t, m.guestCode, "Holding a token")
	if _, err := h.api.Admit(m.owner.project, m.room, insideClaims.Grants, true); err != nil {
		t.Fatal(err)
	}
	expect(t, m.owner.do("DELETE", "/api/v1/join-links/"+m.linkWithRole(t, "guest"), nil), 204)

	// Someone already in the meeting keeps their place, and can come back after a dropped connection.
	expect(t, inside.do("GET", m.path(""), nil), 200)
	if _, err := h.api.Admit(m.owner.project, m.room, insideClaims.Grants, true); err != nil {
		t.Fatalf("someone in the meeting could not reconnect: %v", err)
	}
	// A token that was never used is as good as the link it came from.
	expect(t, holding.do("GET", m.path(""), nil), 403)
	refused(t, h, m, holdingClaims, true, refusedLinkRevoked)
}
