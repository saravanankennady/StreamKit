package routes

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/config"
)

func closedRegistration(c *config.Config) {
	allow := false
	c.ControlPlane.AllowRegistration = &allow
}

func TestRegistrationCanBeClosed(t *testing.T) {
	h := newHarness(t, closedRegistration)
	form := func(email string) map[string]string {
		return map[string]string{"email": email, "password": password, "name": "Test", "organization": email}
	}

	// A fresh installation still gets its administrator, because nothing else can make one.
	first := expect(t, h.client().do("POST", "/api/v1/auth/register", form("first@example.com")), 201)
	if !first.body["user"].(map[string]any)["is_platform_admin"].(bool) {
		t.Fatalf("the first account is not the administrator: %s", first.raw)
	}

	r := h.client().do("POST", "/api/v1/auth/register", form("second@example.com"))
	if r.status != http.StatusForbidden || r.errCode() != "registration_closed" {
		t.Fatalf("a second account: %d %s", r.status, r.raw)
	}
	var users int
	if err := h.store.DB.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatalf("%d users", users)
	}
	// It says the same whether the address has an account or not, so this cannot be used to find out.
	r = h.client().do("POST", "/api/v1/auth/register", form("first@example.com"))
	if r.status != http.StatusForbidden || r.errCode() != "registration_closed" {
		t.Fatalf("an address with an account: %d %s", r.status, r.raw)
	}

	// Having an account is unaffected.
	c := h.client()
	expect(t, c.do("POST", "/api/v1/auth/login", map[string]string{"email": "first@example.com", "password": password}), 200)
	expect(t, c.do("GET", "/api/v1/me", nil), 200)
}

func TestOnlyOneCanBeTheFirstAccountOfAClosedInstallation(t *testing.T) {
	h := newHarness(t, closedRegistration)
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			email := fmt.Sprintf("racer-%d@example.com", i)
			statuses[i] = h.client().do("POST", "/api/v1/auth/register", map[string]string{"email": email, "password": password, "name": "T", "organization": email}).status
		}()
	}
	wg.Wait()
	made := 0
	for _, status := range statuses {
		if status == http.StatusCreated {
			made++
		} else if status != http.StatusForbidden {
			t.Fatalf("statuses %v", statuses)
		}
	}
	if made != 1 {
		t.Fatalf("%d first accounts: %v", made, statuses)
	}
}

func TestMeetingsPerProjectAreCapped(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ControlPlane.MaxMeetingsPerProject = 2 })
	owner, _ := shareSetup(t, h, "owner@example.com")
	start := func(c *client) response {
		t.Helper()
		return c.do("POST", "/api/v1/meetings", map[string]any{"title": "x"})
	}
	first := expect(t, start(owner), 201)
	expect(t, start(owner), 201)
	if r := start(owner); r.status != http.StatusTooManyRequests || r.errCode() != "too_many_meetings" {
		t.Fatalf("a third meeting: %d %s", r.status, r.raw)
	}

	// Another project has its own count.
	other, _ := shareSetup(t, h, "other@example.com")
	expect(t, start(other), 201)

	// A meeting nobody can join any more does not count. Ending one, or its links running out, makes room.
	hostCode := codeIn(t, first.body["host_url"].(string))
	m := &meeting{h: h, owner: owner, room: first.body["room"].(string)}
	host, _ := m.join(t, hostCode, "Hana")
	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)
	third := expect(t, start(owner), 201)
	if r := start(owner); r.status != http.StatusTooManyRequests {
		t.Fatalf("over the limit again: %d %s", r.status, r.raw)
	}
	if _, err := h.store.DB.Exec(context.Background(), `UPDATE join_links SET expires_at = now() - interval '1 minute' WHERE room = $1`, third.body["room"]); err != nil {
		t.Fatal(err)
	}
	expect(t, start(owner), 201)
}

func TestOldMeetingsAndLinksAreRemoved(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	owner, _ := shareSetup(t, h, "owner@example.com")
	made := func() *meeting {
		t.Helper()
		r := expect(t, owner.do("POST", "/api/v1/meetings", map[string]any{"title": "x"}), 201)
		return &meeting{h: h, owner: owner, room: r.body["room"].(string), hostCode: codeIn(t, r.body["host_url"].(string)), guestCode: codeIn(t, r.body["guest_url"].(string))}
	}
	age := func(m *meeting, days int) {
		t.Helper()
		for _, q := range []string{
			`UPDATE meetings SET created_at = now() - make_interval(days => $2) WHERE room = $1`,
			`UPDATE join_links SET created_at = now() - make_interval(days => $2), expires_at = now() - make_interval(days => $2) WHERE room = $1`,
		} {
			if _, err := h.store.DB.Exec(ctx, q, m.room, days); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(table string, m *meeting) int {
		t.Helper()
		var n int
		if err := h.store.DB.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE room = $1`, m.room).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	purge := func() {
		t.Helper()
		if err := h.store.PurgeOld(ctx); err != nil {
			t.Fatal(err)
		}
	}

	gone := made()
	age(gone, 31)
	recent := made()
	age(recent, 29)
	stillGood := made()
	if _, err := h.store.DB.Exec(ctx, `UPDATE meetings SET created_at = now() - make_interval(days => 40) WHERE room = $1`, stillGood.room); err != nil {
		t.Fatal(err)
	}
	// A link turned off long ago is finished with, wherever its expiry says.
	revoked := made()
	if _, err := h.store.DB.Exec(ctx, `UPDATE meetings SET created_at = now() - make_interval(days => 40) WHERE room = $1`, revoked.room); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE join_links SET revoked_at = now() - make_interval(days => 31), expires_at = now() + interval '1 day' WHERE room = $1`, revoked.room); err != nil {
		t.Fatal(err)
	}
	// Someone is still connected to this one, so its row is what their door is decided by.
	open := made()
	age(open, 31)
	if err := h.store.RoomStarted(ctx, store.RoomRecord{SID: "RM_open", ProjectID: owner.project, Name: open.room, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	purge()
	if count("meetings", gone) != 0 || count("join_links", gone) != 0 {
		t.Fatalf("an old meeting is still here: %d %d", count("meetings", gone), count("join_links", gone))
	}
	if count("meetings", recent) != 1 || count("join_links", recent) != 2 {
		t.Fatalf("a recent meeting went: %d %d", count("meetings", recent), count("join_links", recent))
	}
	if count("meetings", stillGood) != 1 || count("join_links", stillGood) != 2 {
		t.Fatalf("a meeting with working links went: %d %d", count("meetings", stillGood), count("join_links", stillGood))
	}
	if count("meetings", revoked) != 0 || count("join_links", revoked) != 0 {
		t.Fatalf("a meeting with links turned off long ago is still here: %d %d", count("meetings", revoked), count("join_links", revoked))
	}
	if count("meetings", open) != 1 {
		t.Fatal("the meeting of a room still open went")
	}
}
