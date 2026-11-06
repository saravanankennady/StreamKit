package routes

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"elnova-streamkit.local/server/auth"
)

// shareSetup signs someone up with a project and an API key, and returns the key.
func shareSetup(t *testing.T, h *harness, email string) (*client, response) {
	t.Helper()
	c := h.signUp(email)
	c.newProject("share")
	return c, expect(t, c.do("POST", "/api/v1/keys", map[string]string{"name": "k"}), 201)
}

func codeOf(t *testing.T, link response) string {
	t.Helper()
	_, code, ok := strings.Cut(link.body["url"].(string), meetingJoinPath)
	if !ok || code == "" {
		t.Fatalf("no code in link URL: %s", link.raw)
	}
	return code
}

func TestJoinLinkAdmitsGuestsToOneRoomOnly(t *testing.T) {
	h := newHarness(t, nil)
	owner, key := shareSetup(t, h, "owner@example.com")
	secret := key.body["secret"].(string)

	link := expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "standup", "label": "Daily"}), 201)
	code := codeOf(t, link)
	if strings.Contains(link.raw, secret) {
		t.Fatal("the link response contains the API secret")
	}

	guest := h.client()
	preview := expect(t, guest.do("POST", "/api/v1/join/preview", map[string]any{"code": code}), 200)
	if preview.body["room"] != "standup" || preview.body["label"] != "Daily" || preview.body["can_publish"] != true {
		t.Fatalf("preview: %s", preview.raw)
	}
	if strings.Contains(preview.raw, owner.project) {
		t.Fatalf("the preview shows the project to someone with no account: %s", preview.raw)
	}

	join := func(name string) (*auth.Verified, response) {
		t.Helper()
		r := expect(t, guest.do("POST", "/api/v1/join", map[string]any{"code": code, "name": name}), 201)
		if strings.Contains(r.raw, secret) {
			t.Fatal("the join response contains the API secret")
		}
		v, err := auth.Verify(r.body["token"].(string), h.keys)
		if err != nil {
			t.Fatalf("the token does not verify: %v", err)
		}
		return v, r
	}

	first, res := join("  Priya  ")
	if res.body["url"] == "" || res.body["room"] != "standup" {
		t.Fatalf("join: %s", res.raw)
	}
	if first.Grants.Name != "Priya" {
		t.Fatalf("name %q", first.Grants.Name)
	}
	v := first.Grants.Video
	if !v.RoomJoin || v.Room != "standup" {
		t.Fatalf("grant is not for the link's room: %+v", v)
	}
	if v.RoomAdmin || v.RoomCreate || v.RoomList || v.RoomRecord || v.Hidden {
		t.Fatalf("a guest got more than joining: %+v", v)
	}
	if v.CanPublish == nil || !*v.CanPublish || v.CanSubscribe == nil || !*v.CanSubscribe {
		t.Fatalf("a guest cannot talk or listen: %+v", v)
	}
	if v.CanUpdateOwnMetadata != nil && *v.CanUpdateOwnMetadata {
		t.Fatalf("a guest can rename themselves past the server: %+v", v)
	}
	if until := time.Until(first.ExpiresAt); until > guestTokenTTL || until <= 0 {
		t.Fatalf("token lasts %v", until)
	}

	// Sharing an identity would disconnect the first person, so each guest gets their own.
	second, _ := join("")
	if second.Grants.Identity == first.Grants.Identity {
		t.Fatalf("two guests share the identity %q", first.Grants.Identity)
	}
	if second.Grants.Name != guestDefaultName {
		t.Fatalf("default name %q", second.Grants.Name)
	}

	// The body decides nothing but the name.
	expect(t, guest.do("POST", "/api/v1/join", map[string]any{"code": code, "name": "x", "identity": "owner"}), 400)
	expect(t, guest.do("POST", "/api/v1/join", map[string]any{"code": code, "name": "x", "room": "other"}), 400)

	items := expect(t, owner.do("GET", "/api/v1/join-links?room=standup", nil), 200).items()
	if len(items) != 1 {
		t.Fatalf("%d links", len(items))
	}
	listed := items[0].(map[string]any)
	if listed["uses"] != float64(2) {
		t.Fatalf("uses: %v", listed["uses"])
	}
	if listed["url"] != link.body["url"] {
		t.Fatalf("the listed link differs from the created one: %v", listed["url"])
	}
	if got := expect(t, owner.do("GET", "/api/v1/join-links?room=elsewhere", nil), 200).items(); len(got) != 0 {
		t.Fatalf("room filter returned %d links", len(got))
	}

	audit := expect(t, owner.do("GET", "/api/v1/audit", nil), 200).raw
	for _, action := range []string{"join_link.created", "join_link.used"} {
		if !strings.Contains(audit, action) {
			t.Fatalf("audit log has no %s", action)
		}
	}
}

func TestJoinLinkCanBeWatchOnly(t *testing.T) {
	h := newHarness(t, nil)
	owner, _ := shareSetup(t, h, "owner@example.com")
	code := codeOf(t, expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "talk", "can_publish": false}), 201))

	guest := h.client()
	if p := expect(t, guest.do("POST", "/api/v1/join/preview", map[string]any{"code": code}), 200); p.body["can_publish"] != false {
		t.Fatalf("preview: %s", p.raw)
	}
	r := expect(t, guest.do("POST", "/api/v1/join", map[string]any{"code": code}), 201)
	v, err := auth.Verify(r.body["token"].(string), h.keys)
	if err != nil {
		t.Fatal(err)
	}
	if g := v.Grants.Video; g.CanPublish == nil || *g.CanPublish || g.CanSubscribe == nil || !*g.CanSubscribe {
		t.Fatalf("watch-only grant: %+v", g)
	}
}

func TestJoinLinkStopsWorking(t *testing.T) {
	h := newHarness(t, nil)
	owner, _ := shareSetup(t, h, "owner@example.com")
	guest := h.client()

	expectStopped := func(code, want string) {
		t.Helper()
		for _, path := range []string{"/api/v1/join/preview", "/api/v1/join"} {
			if r := guest.do("POST", path, map[string]any{"code": code}); r.status != http.StatusGone || r.errCode() != want {
				t.Fatalf("%s: %d %s", path, r.status, r.raw)
			}
		}
	}

	revoked := expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}), 201)
	expect(t, guest.do("POST", "/api/v1/join", map[string]any{"code": codeOf(t, revoked)}), 201)
	expect(t, owner.do("DELETE", "/api/v1/join-links/"+revoked.body["id"].(string), nil), 204)
	expectStopped(codeOf(t, revoked), "link_revoked")
	if r := owner.do("DELETE", "/api/v1/join-links/"+revoked.body["id"].(string), nil); r.status != 404 {
		t.Fatalf("revoking twice: %d", r.status)
	}

	expired := expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "b"}), 201)
	if _, err := h.store.DB.Exec(context.Background(), `UPDATE join_links SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.body["id"]); err != nil {
		t.Fatal(err)
	}
	expectStopped(codeOf(t, expired), "link_expired")

	if items := expect(t, owner.do("GET", "/api/v1/join-links", nil), 200).items(); len(items) != 0 {
		t.Fatalf("links that stopped working are still listed: %v", items)
	}

	for _, path := range []string{"/api/v1/join/preview", "/api/v1/join"} {
		if r := guest.do("POST", path, map[string]any{"code": "not-a-real-code"}); r.status != 404 || r.errCode() != "link_not_found" {
			t.Fatalf("%s with an unknown code: %d %s", path, r.status, r.raw)
		}
		expect(t, guest.do("POST", path, map[string]any{}), 422)
	}
}

func TestJoinLinkCodeIsNotStoredInTheClear(t *testing.T) {
	h := newHarness(t, nil)
	owner, _ := shareSetup(t, h, "owner@example.com")
	link := expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}), 201)
	code := codeOf(t, link)

	var hash, sealed []byte
	var row string
	err := h.store.DB.QueryRow(context.Background(), `SELECT code_hash, code_sealed, j::text FROM join_links j WHERE id = $1`, link.body["id"]).Scan(&hash, &sealed, &row)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, code) || bytes.Contains(sealed, []byte(code)) || bytes.Contains(hash, []byte(code)) {
		t.Fatal("the code is readable in the database")
	}
}

func TestJoinLinkPermissions(t *testing.T) {
	h := newHarness(t, nil)
	owner, key := shareSetup(t, h, "owner@example.com")
	link := expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}), 201)
	id := link.body["id"].(string)

	// Nobody signed in, and no token.
	anon := h.client()
	expect(t, anon.do("GET", "/api/v1/join-links", nil), 401)
	expect(t, anon.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}), 401)
	expect(t, anon.do("DELETE", "/api/v1/join-links/"+id, nil), 401)

	// Someone else's project cannot see or stop this link.
	other, _ := shareSetup(t, h, "other@example.com")
	if items := expect(t, other.do("GET", "/api/v1/join-links", nil), 200).items(); len(items) != 0 {
		t.Fatalf("another project sees %d links", len(items))
	}
	expect(t, other.do("DELETE", "/api/v1/join-links/"+id, nil), 404)
	expect(t, other.do("DELETE", "/api/v1/join-links/not-a-uuid", nil), 404)

	// A leaked access token, even an admin one, cannot make itself a way back in.
	bearer := h.client()
	bearer.bearer = mustMint(t, key, &auth.Grants{Identity: "x", Video: &auth.VideoGrant{RoomAdmin: true, RoomList: true, RoomCreate: true}})
	expect(t, bearer.do("GET", "/api/v1/join-links", nil), 403)
	expect(t, bearer.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}), 403)
	expect(t, bearer.do("DELETE", "/api/v1/join-links/"+id, nil), 403)
}

func TestJoinLinkValidationAndMissingKey(t *testing.T) {
	h := newHarness(t, nil)
	c := h.signUp("owner@example.com")
	c.newProject("share")

	// With no API key the link could never admit anyone, so it is not made.
	if r := c.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}); r.status != 409 || r.errCode() != "no_api_key" {
		t.Fatalf("no key: %d %s", r.status, r.raw)
	}
	expect(t, c.do("POST", "/api/v1/keys", map[string]string{"name": "k"}), 201)

	expect(t, c.do("POST", "/api/v1/join-links", map[string]any{"room": "  "}), 422)
	expect(t, c.do("POST", "/api/v1/join-links", map[string]any{"room": "a", "expires_in_seconds": 30}), 422)
	expect(t, c.do("POST", "/api/v1/join-links", map[string]any{"room": "a", "expires_in_seconds": 31 * 24 * 3600}), 422)
	expect(t, c.do("POST", "/api/v1/join-links", map[string]any{"room": "a", "label": strings.Repeat("x", 101)}), 422)

	link := expect(t, c.do("POST", "/api/v1/join-links", map[string]any{"room": "a", "expires_in_seconds": 3600}), 201)
	expires, err := time.Parse(time.RFC3339Nano, link.body["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(expires); left < 59*time.Minute || left > time.Hour {
		t.Fatalf("expires in %v", left)
	}
	code := codeOf(t, link)
	expect(t, h.client().do("POST", "/api/v1/join", map[string]any{"code": code, "name": strings.Repeat("x", 101)}), 422)
}

func TestJoinIsRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	owner, _ := shareSetup(t, h, "owner@example.com")
	code := codeOf(t, expect(t, owner.do("POST", "/api/v1/join-links", map[string]any{"room": "a"}), 201))

	guest := h.client()
	limited := false
	for range 200 {
		if r := guest.do("POST", "/api/v1/join", map[string]any{"code": code}); r.status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("200 joins from one address were all allowed")
	}
}
