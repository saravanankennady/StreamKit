package routes

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

func TestAMeetingRecordsOneFileAtATime(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	h.fakeWorker("worker", 50)
	h.openRoom(m.owner.project, m.room)
	body := map[string]any{"type": "room_composite", "room": m.room, "output": map[string]any{"kind": "file", "format": "mp4"}}
	active := func() []string {
		t.Helper()
		var ids []string
		rows, err := h.store.DB.Query(context.Background(), `SELECT id FROM egresses WHERE project_id = $1 AND status IN ('starting', 'active', 'ending')`, m.owner.project)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	first := expect(t, host.do("POST", "/api/v1/egress", body), 201)
	// Each one would take a worker's slot, and a host link holder could fill the whole installation's capacity.
	if r := host.do("POST", "/api/v1/egress", body); r.status != http.StatusConflict || r.errCode() != "recording_active" {
		t.Fatalf("a second recording: %d %s", r.status, r.raw)
	}

	// Asking at the same moment is no way around it.
	expect(t, host.do("POST", "/api/v1/egress/"+first.body["id"].(string)+"/stop", map[string]any{}), 200)
	var wg sync.WaitGroup
	statuses := make([]int, 8)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = host.do("POST", "/api/v1/egress", body).status
		}()
	}
	wg.Wait()
	made := 0
	for _, status := range statuses {
		switch status {
		case 201:
			made++
		case http.StatusConflict:
		default:
			t.Fatalf("statuses %v", statuses)
		}
	}
	if made != 1 {
		t.Fatalf("%d recordings started together: %v", made, statuses)
	}

	// Finishing one makes room for the next.
	jobs := active()
	if len(jobs) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	expect(t, host.do("POST", "/api/v1/egress/"+jobs[0]+"/stop", map[string]any{}), 200)
	expect(t, host.do("POST", "/api/v1/egress", body), 201)

	// A project's own credentials keep what the reference gives them, because they are the project's to spend. A host still
	// cannot add to them.
	admin := h.client()
	admin.bearer = mustMint(t, m.key, &auth.Grants{Identity: "api", Video: &auth.VideoGrant{RoomRecord: true}})
	expect(t, admin.do("POST", "/api/v1/egress", body), 201)
	expect(t, m.owner.do("POST", "/api/v1/egress", body), 201)
	for _, job := range active() {
		expect(t, m.owner.do("POST", "/api/v1/egress/"+job+"/stop", map[string]any{}), 200)
	}
	expect(t, admin.do("POST", "/api/v1/egress", body), 201)
	if r := host.do("POST", "/api/v1/egress", body); r.status != http.StatusConflict {
		t.Fatalf("a host recording alongside the project's own: %d %s", r.status, r.raw)
	}
}

func TestRetryingARecordingIsNotHeldToTheHostsLimit(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	h.fakeWorker("worker", 50)
	h.openRoom(m.owner.project, m.room)
	body := map[string]any{"type": "room_composite", "room": m.room, "output": map[string]any{"kind": "file", "format": "mp4"}}
	ctx := context.Background()

	first := expect(t, host.do("POST", "/api/v1/egress", body), 201).body["id"].(string)
	expect(t, host.do("POST", "/api/v1/egress/"+first+"/stop", map[string]any{}), 200)
	expect(t, host.do("POST", "/api/v1/egress", body), 201)
	if _, err := h.store.EgressByID(ctx, m.owner.project, first); err != nil {
		t.Fatal(err)
	}
	expect(t, m.owner.do("POST", "/api/v1/recordings/"+first+"/retry", map[string]any{}), 201)
}

func TestEndingAMeetingStopsEveryJobInItsRoom(t *testing.T) {
	h := newHarness(t, nil)
	m := newMeeting(t, h, "owner@example.com")
	host, _ := m.join(t, m.hostCode, "Hana")
	ctx := context.Background()
	add := func(room, kind, format string) *store.Egress {
		t.Helper()
		e, err := h.store.CreateEgress(ctx, &store.Egress{ProjectID: m.owner.project, RoomName: room, RoomSID: "RM_x", Type: kind, Format: format, Request: store.EgressRequest{Type: kind, Room: room}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	jobs := []*store.Egress{add(m.room, "room_composite", "mp4"), add(m.room, "participant", "mp4"), add(m.room, "room_composite", "")}
	claimed, err := h.store.ClaimEgress(ctx, "worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	elsewhere := add("elsewhere", "room_composite", "mp4")

	expect(t, host.do("POST", m.path("/end"), map[string]any{}), 204)

	// A worker has this one, so it finishes the file. It has been asked to, and hears when it next renews.
	if stop, err := h.store.RenewEgress(ctx, claimed.ID, "worker", time.Minute); err != nil || !stop {
		t.Fatalf("the running job was not asked to stop: %v %v", stop, err)
	}
	// The ones still in the queue end here.
	for _, job := range jobs {
		if job.ID == claimed.ID {
			continue
		}
		e, err := h.store.EgressByID(ctx, m.owner.project, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Status != store.EgressAborted {
			t.Fatalf("%s is %s", e.ID, e.Status)
		}
	}
	other, err := h.store.EgressByID(ctx, m.owner.project, elsewhere.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != store.EgressStarting {
		t.Fatalf("ending a meeting touched a job in another room: %s", other.Status)
	}
}
