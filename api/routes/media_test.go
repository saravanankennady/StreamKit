package routes

// These run the media routes against a real PostgreSQL and real files, with store calls standing in for a worker.
// tests/media covers the same routes with real workers and real media.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/storage"
	"elnova-streamkit.local/ingress/whip"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/room"
)

func newMediaHarness(t *testing.T, tweak func(*config.Config)) (*harness, *client) {
	t.Helper()
	h := newHarness(t, func(c *config.Config) {
		c.Media.Storage.Dir = filepath.Join(t.TempDir(), "recordings")
		if tweak != nil {
			tweak(c)
		}
	})
	c := h.signUp(fmt.Sprintf("media-%d@example.com", time.Now().UnixNano()))
	c.newProject("Media")
	return h, c
}

// fakeWorker makes the API believe a recording worker with this many slots is running.
func (h *harness) fakeWorker(id string, maxJobs int) {
	h.t.Helper()
	stats, _ := json.Marshal(map[string]any{"kind": store.WorkerKindEgress, "max_jobs": maxJobs, "healthy": true})
	if _, err := h.store.HeartbeatServer(context.Background(), store.ServerNode{NodeID: id, StartedAt: time.Now(), Stats: stats}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) openRoom(project, name string) *room.Room {
	r, _ := h.rooms.GetOrCreate(project, name, room.CreateOptions{})
	return r
}

func TestEgressStartNeedsAWorker(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.openRoom(c.project, "r1")
	body := map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}}

	r := expect(t, c.do("POST", "/api/v1/egress", body), http.StatusServiceUnavailable)
	if r.errCode() != "egress_unavailable" {
		t.Fatalf("code %q", r.errCode())
	}

	// A worker that has stopped reporting does not count.
	h.fakeWorker("EW_old", 4)
	if _, err := h.store.DB.Exec(context.Background(), `UPDATE servers SET last_seen_at = now() - interval '1 minute' WHERE node_id = 'EW_old'`); err != nil {
		t.Fatal(err)
	}
	expect(t, c.do("POST", "/api/v1/egress", body), http.StatusServiceUnavailable)

	// Nor does one that says it cannot do the work.
	sick, _ := json.Marshal(map[string]any{"kind": store.WorkerKindEgress, "max_jobs": 4, "healthy": false, "unhealthy_reason": "no disk"})
	if _, err := h.store.HeartbeatServer(context.Background(), store.ServerNode{NodeID: "EW_sick", StartedAt: time.Now(), Stats: sick}); err != nil {
		t.Fatal(err)
	}
	expect(t, c.do("POST", "/api/v1/egress", body), http.StatusServiceUnavailable)

	// Neither does one being drained.
	h.fakeWorker("EW_draining", 4)
	if err := h.store.SetServerDraining(context.Background(), "EW_draining", true); err != nil {
		t.Fatal(err)
	}
	expect(t, c.do("POST", "/api/v1/egress", body), http.StatusServiceUnavailable)

	// One slot, so one job, and then there is no worker available again.
	h.fakeWorker("EW_ok", 1)
	job := expect(t, c.do("POST", "/api/v1/egress", body), http.StatusCreated)
	if job.body["status"] != "starting" || !strings.HasPrefix(job.body["id"].(string), "EG_") {
		t.Fatalf("%s", job.raw)
	}
	expect(t, c.do("POST", "/api/v1/egress", body), http.StatusServiceUnavailable)
}

func TestEgressValidation(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	file := map[string]string{"kind": "file", "format": "mp4"}
	for name, c2 := range map[string]struct {
		body  map[string]any
		field string
	}{
		"no type":          {map[string]any{"room": "r1", "output": file}, "type"},
		"bad type":         {map[string]any{"type": "track", "room": "r1", "output": file}, "type"},
		"no room":          {map[string]any{"type": "room_composite", "output": file}, "room"},
		"no output":        {map[string]any{"type": "room_composite", "room": "r1"}, "output"},
		"bad format":       {map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "avi"}}, "output"},
		"bad kind":         {map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "ftp", "format": "mp4"}}, "output"},
		"no participant":   {map[string]any{"type": "participant", "room": "r1", "output": file}, "participant"},
		"composite of one": {map[string]any{"type": "room_composite", "room": "r1", "participant": "a", "output": file}, "participant"},
		"bad layout":       {map[string]any{"type": "room_composite", "room": "r1", "layout": "wall", "output": file}, "layout"},
		"file url":         {map[string]any{"type": "room_composite", "room": "r1", "stream_urls": []string{"file:///etc/passwd"}}, "stream_urls"},
		"http url":         {map[string]any{"type": "room_composite", "room": "r1", "stream_urls": []string{"http://example.com/x"}}, "stream_urls"},
		"private rtmp":     {map[string]any{"type": "room_composite", "room": "r1", "stream_urls": []string{"rtmp://127.0.0.1/live/key"}}, "stream_urls"},
		"metadata rtmp":    {map[string]any{"type": "room_composite", "room": "r1", "stream_urls": []string{"rtmp://169.254.169.254/live/key"}}, "stream_urls"},
	} {
		r := c.do("POST", "/api/v1/egress", c2.body)
		if r.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d: %s", name, r.status, r.raw)
			continue
		}
		fields, _ := r.body["error"].(map[string]any)["fields"].(map[string]any)
		if _, ok := fields[c2.field]; !ok {
			t.Errorf("%s: wanted an error on %s: %s", name, c2.field, r.raw)
		}
	}
	// Unknown fields are refused, like everywhere else in this API.
	expect(t, c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": file, "bucket": "x"}), http.StatusBadRequest)
	// A room that is not open.
	r := expect(t, c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "nope", "output": file}), http.StatusNotFound)
	if r.errCode() != "room_not_found" {
		t.Fatal(r.raw)
	}
}

func TestEgressJobShapeAndStreamKeyRedaction(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	h.fakeWorker("EW_a", 10)
	x := h.openRoom(c.project, "r1")
	const secret = "live_abcdefghijklmnopqrstuvwxyz"
	r := expect(t, c.do("POST", "/api/v1/egress", map[string]any{
		"type": "room_composite", "room": "r1", "layout": "speaker",
		"output": map[string]string{"kind": "file", "format": "webm"}, "stream_urls": []string{"rtmp://127.0.0.1/app/" + secret},
	}), http.StatusCreated)
	if strings.Contains(r.raw, "abcdefghij") {
		t.Fatalf("the stream key leaked: %s", r.raw)
	}
	id := r.body["id"].(string)
	want := map[string]any{
		"project_id": c.project, "room_name": "r1", "room_sid": x.SID(), "type": "room_composite", "status": "starting",
		"format": "webm", "output_url": "/api/v1/recordings/" + id + "/file", "file_size": float64(0), "duration_ms": float64(0), "error": "",
		"started_at": nil, "ended_at": nil, "retry_of": nil,
	}
	for k, v := range want {
		if got, ok := r.body[k]; !ok || got != v {
			t.Errorf("%s: got %v, want %v", k, got, v)
		}
	}
	urls := r.body["stream_urls"].([]any)
	if len(urls) != 1 || urls[0] != "rtmp://127.0.0.1/app/{liv...xyz}" {
		t.Errorf("stream_urls: %v", urls)
	}
	// Nor is it readable in the database.
	var column string
	if err := h.store.DB.QueryRow(context.Background(), `SELECT request::text || array_to_string(stream_urls, ',') FROM egresses WHERE id = $1`, id).Scan(&column); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(column, "abcdefghij") {
		t.Fatalf("plain stream key in the database: %s", column)
	}
	// A worker can read it back.
	job, err := h.store.EgressByID(context.Background(), c.project, id)
	if err != nil {
		t.Fatal(err)
	}
	real, err := h.store.EgressStreamURLs(job)
	if err != nil || len(real) != 1 || !strings.HasSuffix(real[0], secret) {
		t.Fatalf("%v %v", real, err)
	}

	// Stream-only jobs are not recordings.
	stream := expect(t, c.do("POST", "/api/v1/egress", map[string]any{
		"type": "room_composite", "room": "r1", "stream_urls": []string{"rtmp://127.0.0.1/app/" + secret},
	}), http.StatusCreated)
	if stream.body["format"] != "" || stream.body["output_url"] != "" {
		t.Fatal(stream.raw)
	}
	all := expect(t, c.do("GET", "/api/v1/egress", nil), http.StatusOK)
	recs := expect(t, c.do("GET", "/api/v1/recordings", nil), http.StatusOK)
	if len(all.items()) != 2 || len(recs.items()) != 1 {
		t.Fatalf("egress %d, recordings %d", len(all.items()), len(recs.items()))
	}
	if all.items()[0].(map[string]any)["id"] != stream.body["id"] {
		t.Fatal("newest first")
	}
	expect(t, c.do("GET", "/api/v1/recordings/"+stream.body["id"].(string), nil), http.StatusNotFound)
	expect(t, c.do("GET", "/api/v1/egress/"+stream.body["id"].(string), nil), http.StatusOK)
}

func TestEgressListPages(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	seen := map[string]bool{}
	for range 5 {
		r := expect(t, c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}}), http.StatusCreated)
		seen[r.body["id"].(string)] = false
	}
	cursor, pages := "", 0
	for {
		r := expect(t, c.do("GET", "/api/v1/recordings?limit=2&cursor="+cursor, nil), http.StatusOK)
		for _, item := range r.items() {
			id := item.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("%s came up twice", id)
			}
			seen[id] = true
		}
		pages++
		cursor, _ = r.body["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	for id, ok := range seen {
		if !ok {
			t.Errorf("%s never came up", id)
		}
	}
	if pages != 3 {
		t.Errorf("%d pages", pages)
	}
}

func TestWorkersNeverGetTheSameJob(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	for range 8 {
		expect(t, c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}}), http.StatusCreated)
	}
	ctx := context.Background()
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := h.store.ClaimEgress(ctx, fmt.Sprintf("EW_%d", w), time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			if job != nil {
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != 8 {
		t.Fatalf("%d jobs claimed: %v", len(claimed), claimed)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("%s claimed %d times", id, n)
		}
	}
	if job, err := h.store.ClaimEgress(ctx, "EW_x", time.Minute); job != nil || err != nil {
		t.Errorf("an empty queue gave %v, %v", job, err)
	}
}

func startJob(t *testing.T, c *client) string {
	t.Helper()
	r := expect(t, c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}}), http.StatusCreated)
	return r.body["id"].(string)
}

func TestEgressLeaseAndStatusRules(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	ctx := context.Background()

	// A worker that dies mid-job: the lease runs out, the job fails, and it says why. Only once.
	id := startJob(t, c)
	if job, err := h.store.ClaimEgress(ctx, "EW_dead", 50*time.Millisecond); err != nil || job.ID != id {
		t.Fatalf("%v %v", job, err)
	}
	if _, err := h.store.TransitionEgress(ctx, id, "EW_dead", store.EgressActive, store.EgressResult{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE egresses SET lease_expires_at = now() - make_interval(secs => $2) WHERE id = $1`, id, (store.ReapGrace + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	reaped, err := h.store.ReapEgress(ctx)
	if err != nil || len(reaped) != 1 || reaped[0].ID != id {
		t.Fatalf("%v %v", reaped, err)
	}
	r := expect(t, c.do("GET", "/api/v1/egress/"+id, nil), http.StatusOK)
	if r.body["status"] != "failed" || !strings.Contains(r.body["error"].(string), "stopped responding") || r.body["ended_at"] == nil {
		t.Fatal(r.raw)
	}
	if reaped, _ := h.store.ReapEgress(ctx); len(reaped) != 0 {
		t.Fatalf("reaped twice: %v", reaped)
	}

	// It cannot come back from that: a worker that was only slow finds out it has lost the job.
	if _, err := h.store.TransitionEgress(ctx, id, "EW_dead", store.EgressComplete, store.EgressResult{}); err != store.ErrJobLost {
		t.Fatalf("got %v", err)
	}
	if _, err := h.store.RenewEgress(ctx, id, "EW_dead", time.Minute); err != store.ErrJobLost {
		t.Fatalf("got %v", err)
	}

	// A lease being renewed keeps a job alive.
	id2 := startJob(t, c)
	if _, err := h.store.ClaimEgress(ctx, "EW_live", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RenewEgress(ctx, id2, "EW_live", time.Minute); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if reaped, _ := h.store.ReapEgress(ctx); len(reaped) != 0 {
		t.Fatalf("reaped a live job: %v", reaped)
	}

	// Only the owner can move a job, and only along legal lines.
	if _, err := h.store.TransitionEgress(ctx, id2, "EW_other", store.EgressActive, store.EgressResult{}); err != store.ErrJobLost {
		t.Fatalf("got %v", err)
	}
	if _, err := h.store.TransitionEgress(ctx, id2, "EW_live", store.EgressComplete, store.EgressResult{}); err != store.ErrJobLost {
		t.Fatalf("starting -> complete: got %v", err)
	}

	// A stop request reaches the worker through its next renewal.
	if stop, err := h.store.RenewEgress(ctx, id2, "EW_live", time.Minute); err != nil || stop {
		t.Fatalf("%v %v", stop, err)
	}
	expect(t, c.do("POST", "/api/v1/egress/"+id2+"/stop", map[string]any{}), http.StatusOK)
	if stop, err := h.store.RenewEgress(ctx, id2, "EW_live", time.Minute); err != nil || !stop {
		t.Fatalf("%v %v", stop, err)
	}

	// Stopping a job no worker has taken ends it there and then.
	id3 := startJob(t, c)
	r = expect(t, c.do("POST", "/api/v1/egress/"+id3+"/stop", map[string]any{}), http.StatusOK)
	if r.body["status"] != "aborted" {
		t.Fatal(r.raw)
	}
	// And stopping it again says so.
	r = expect(t, c.do("POST", "/api/v1/egress/"+id3+"/stop", map[string]any{}), http.StatusConflict)
	if r.errCode() != "egress_not_running" {
		t.Fatal(r.raw)
	}

	// A running job cannot be deleted or retried.
	expect(t, c.do("DELETE", "/api/v1/recordings/"+id2, nil), http.StatusConflict)
	expect(t, c.do("POST", "/api/v1/recordings/"+id2+"/retry", map[string]any{}), http.StatusConflict)

	// A queued job nobody takes fails rather than waiting forever.
	id4 := startJob(t, c)
	if _, err := h.store.DB.Exec(ctx, `UPDATE egresses SET created_at = now() - interval '10 minutes' WHERE id = $1`, id4); err != nil {
		t.Fatal(err)
	}
	if reaped, _ := h.store.ReapEgress(ctx); len(reaped) != 1 || reaped[0].ID != id4 || !strings.Contains(reaped[0].Error, "No worker") {
		t.Fatalf("%v", reaped)
	}
}

// finishedRecording makes a job with a real file in storage, the way a worker leaves one.
func finishedRecording(t *testing.T, h *harness, c *client, contents []byte) string {
	t.Helper()
	ctx := context.Background()
	id := startJob(t, c)
	if job, err := h.store.ClaimEgress(ctx, "EW_w", time.Minute); err != nil || job.ID != id {
		t.Fatalf("%v %v", job, err)
	}
	backend, err := storage.New(h.cfg.Media.Storage)
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.RecordingKey(c.project, id, "mp4")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, src, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.TransitionEgress(ctx, id, "EW_w", store.EgressActive, store.EgressResult{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.TransitionEgress(ctx, id, "EW_w", store.EgressComplete, store.EgressResult{
		OutputPath: key, Storage: backend.Kind(), FileSize: int64(len(contents)), DurationMs: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (c *client) get(path string, headers map[string]string) (*http.Response, []byte) {
	c.h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.h.srv.URL+path, nil)
	if c.project != "" {
		req.Header.Set("X-Project-ID", c.project)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, data
}

func TestRecordingFileAndRanges(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	contents := []byte(strings.Repeat("0123456789", 1000))
	id := finishedRecording(t, h, c, contents)

	res, data := c.get("/api/v1/recordings/"+id+"/file", nil)
	if res.StatusCode != 200 || string(data) != string(contents) {
		t.Fatalf("%d, %d bytes", res.StatusCode, len(data))
	}
	if res.Header.Get("Content-Type") != "video/mp4" || res.Header.Get("Accept-Ranges") != "bytes" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%v", res.Header)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("%v", res.Header)
	}

	res, data = c.get("/api/v1/recordings/"+id+"/file", map[string]string{"Range": "bytes=10-19"})
	if res.StatusCode != http.StatusPartialContent || string(data) != "0123456789" || res.Header.Get("Content-Range") != "bytes 10-19/10000" {
		t.Fatalf("%d %q %v", res.StatusCode, data, res.Header)
	}
	res, _ = c.get("/api/v1/recordings/"+id+"/file", map[string]string{"Range": "bytes=99999-"})
	if res.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("%d", res.StatusCode)
	}
	res, _ = c.get("/api/v1/recordings/"+id+"/file?download=1", nil)
	if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("%v", res.Header)
	}
	// A <video> element cannot set a header, so the project can come from the query.
	project := c.project
	c.project = ""
	res, _ = c.get("/api/v1/recordings/"+id+"/file?project_id="+project, map[string]string{"Range": "bytes=0-1"})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("%d", res.StatusCode)
	}
	c.project = project

	// A job without a file says so, rather than failing some other way.
	running := startJob(t, c)
	r, _ := c.get("/api/v1/recordings/"+running+"/file", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("%d", r.StatusCode)
	}

	// Deleting takes the file and the record.
	key, _ := storage.RecordingKey(c.project, id, "mp4")
	expect(t, c.do("DELETE", "/api/v1/recordings/"+id, nil), http.StatusNoContent)
	expect(t, c.do("GET", "/api/v1/recordings/"+id, nil), http.StatusNotFound)
	if _, err := os.Stat(filepath.Join(h.cfg.Media.Storage.Dir, key)); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}
}

// Even if a row held a hostile path, it could not read outside the storage directory.
func TestRecordingFileCannotEscapeStorage(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	id := finishedRecording(t, h, c, []byte("video"))
	secret := filepath.Join(filepath.Dir(h.cfg.Media.Storage.Dir), "secret")
	if err := os.WriteFile(secret, []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "/etc/passwd", secret, "a/../../secret"} {
		if _, err := h.store.DB.Exec(context.Background(), `UPDATE egresses SET output_path = $2 WHERE id = $1`, id, path); err != nil {
			t.Fatal(err)
		}
		res, data := c.get("/api/v1/recordings/"+id+"/file", nil)
		if res.StatusCode == 200 || strings.Contains(string(data), "do not read") || strings.Contains(string(data), "root:") {
			t.Fatalf("%q: %d %s", path, res.StatusCode, data)
		}
	}
	// The ID in the URL is only ever a value to look up.
	for _, path := range []string{"..%2F..%2Fetc%2Fpasswd", "%2e%2e", "EG_x%27%20OR%201=1--"} {
		res, _ := c.get("/api/v1/recordings/"+path+"/file", nil)
		if res.StatusCode == 200 {
			t.Fatalf("%q: %d", path, res.StatusCode)
		}
	}
}

func TestRetryMakesANewJob(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	id := finishedRecording(t, h, c, []byte("video"))
	r := expect(t, c.do("POST", "/api/v1/recordings/"+id+"/retry", map[string]any{}), http.StatusCreated)
	if r.body["id"] == id || r.body["retry_of"] != id || r.body["status"] != "starting" || r.body["format"] != "mp4" || r.body["room_name"] != "r1" {
		t.Fatal(r.raw)
	}
	if r.body["file_size"] != float64(0) {
		t.Fatal(r.raw)
	}
	// With no room left to record, it says so.
	if err := h.rooms.Delete(c.project, "r1"); err != nil {
		t.Fatal(err)
	}
	r = expect(t, c.do("POST", "/api/v1/recordings/"+id+"/retry", map[string]any{}), http.StatusNotFound)
	if r.errCode() != "room_not_found" {
		t.Fatal(r.raw)
	}
}

func TestMediaIsolation(t *testing.T) {
	h, owner := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(owner.project, "r1")
	id := finishedRecording(t, h, owner, []byte("video"))
	ing := expect(t, owner.do("POST", "/api/v1/ingress", map[string]any{"input_type": "rtmp", "room": "r1", "participant_identity": "stage"}), http.StatusCreated)
	ingID := ing.body["id"].(string)

	// Another account with its own project.
	other := h.signUp("other-media@example.com")
	other.newProject("Other")
	// A project of mine, but not the one holding these.
	sibling := h.client()
	sibling.http = owner.http
	sibling.project = expect(t, owner.do("POST", "/api/v1/projects", map[string]string{"name": "Sibling"}), http.StatusCreated).body["id"].(string)
	owner.project = id2project(t, h, id)

	for name, c := range map[string]*client{"another account": other, "another project": sibling} {
		for _, call := range []struct{ method, path string }{
			{"GET", "/api/v1/egress/" + id},
			{"POST", "/api/v1/egress/" + id + "/stop"},
			{"GET", "/api/v1/recordings/" + id},
			{"GET", "/api/v1/recordings/" + id + "/file"},
			{"POST", "/api/v1/recordings/" + id + "/retry"},
			{"DELETE", "/api/v1/recordings/" + id},
			{"GET", "/api/v1/ingress/" + ingID},
			{"PATCH", "/api/v1/ingress/" + ingID},
			{"DELETE", "/api/v1/ingress/" + ingID},
			{"POST", "/api/v1/ingress/" + ingID + "/stop"},
			{"POST", "/api/v1/ingress/" + ingID + "/rotate-key"},
			{"GET", "/api/v1/ingress/" + ingID + "/logs"},
		} {
			var body any
			if call.method != "GET" && call.method != "DELETE" {
				body = map[string]any{}
			}
			r := c.do(call.method, call.path, body)
			if r.status != http.StatusNotFound {
				t.Errorf("%s: %s %s: status %d: %s", name, call.method, call.path, r.status, r.raw)
			}
		}
		for _, path := range []string{"/api/v1/egress", "/api/v1/recordings", "/api/v1/ingress"} {
			if r := expect(t, c.do("GET", path, nil), http.StatusOK); len(r.items()) != 0 {
				t.Errorf("%s: %s lists another project's work: %s", name, path, r.raw)
			}
		}
		// A project's own room name does not reach another project's room.
		h.fakeWorker("EW_b", 100)
		r := c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}})
		if r.status != http.StatusNotFound {
			t.Errorf("%s: recorded another project's room: %d %s", name, r.status, r.raw)
		}
	}
	// All still there.
	expect(t, owner.do("GET", "/api/v1/recordings/"+id, nil), http.StatusOK)
	expect(t, owner.do("GET", "/api/v1/ingress/"+ingID, nil), http.StatusOK)
	res, data := owner.get("/api/v1/recordings/"+id+"/file", nil)
	if res.StatusCode != 200 || string(data) != "video" {
		t.Fatalf("%d %s", res.StatusCode, data)
	}
}

func id2project(t *testing.T, h *harness, id string) string {
	t.Helper()
	var project string
	if err := h.store.DB.QueryRow(context.Background(), `SELECT project_id FROM egresses WHERE id = $1`, id).Scan(&project); err != nil {
		t.Fatal(err)
	}
	return project
}

func TestAccessTokensNeedTheirGrant(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{"name": "k"}), http.StatusCreated)
	k := auth.Key{APIKey: key.body["api_key"].(string), Secret: key.body["secret"].(string), ProjectID: c.project}
	mint := func(v *auth.VideoGrant) *client {
		token, err := auth.Mint(k, &auth.Grants{Identity: "api", Video: v}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tc := h.client()
		tc.bearer = token
		return tc
	}
	h.keys.InvalidateAll()

	none := mint(&auth.VideoGrant{RoomJoin: true, Room: "r1"})
	expect(t, none.do("GET", "/api/v1/egress", nil), http.StatusForbidden)
	expect(t, none.do("GET", "/api/v1/ingress", nil), http.StatusForbidden)
	expect(t, none.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}}), http.StatusForbidden)

	record := mint(&auth.VideoGrant{RoomRecord: true})
	job := expect(t, record.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}}), http.StatusCreated)
	if job.body["project_id"] != c.project {
		t.Fatal(job.raw)
	}
	expect(t, record.do("GET", "/api/v1/ingress", nil), http.StatusForbidden)

	ingest := mint(&auth.VideoGrant{IngressAdmin: true})
	expect(t, ingest.do("POST", "/api/v1/ingress", map[string]any{"input_type": "whip", "room": "r1", "participant_identity": "x"}), http.StatusCreated)
	expect(t, ingest.do("GET", "/api/v1/egress", nil), http.StatusForbidden)
}

func TestIngressRoutes(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	ctx := context.Background()

	for name, body := range map[string]map[string]any{
		"no input type":    {"room": "r", "participant_identity": "i"},
		"bad input type":   {"input_type": "ndi", "room": "r", "participant_identity": "i"},
		"no room":          {"input_type": "rtmp", "participant_identity": "i"},
		"no identity":      {"input_type": "rtmp", "room": "r"},
		"url with no url":  {"input_type": "url", "room": "r", "participant_identity": "i"},
		"rtmp with a url":  {"input_type": "rtmp", "room": "r", "participant_identity": "i", "source_url": "https://example.com/a"},
		"file url":         {"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "file:///etc/passwd"},
		"private url":      {"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "http://127.0.0.1/a"},
		"cloud metadata":   {"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "http://169.254.169.254/latest/meta-data/"},
		"localhost":        {"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "http://localhost/a"},
		"listening on srt": {"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "srt://1.2.3.4:9000?mode=listener"},
	} {
		if r := c.do("POST", "/api/v1/ingress", body); r.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}

	// Creating needs no worker: it is only configuration.
	r := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"name": "Stage", "input_type": "rtmp", "room": "r", "participant_identity": "i", "participant_name": "N"}), http.StatusCreated)
	id := r.body["id"].(string)
	key := r.body["stream_key"].(string)
	want := map[string]any{"name": "Stage", "input_type": "rtmp", "room_name": "r", "participant_identity": "i", "participant_name": "N", "source_url": "", "enabled": true, "status": "inactive", "error": ""}
	for k, v := range want {
		if r.body[k] != v {
			t.Errorf("%s: %v", k, r.body[k])
		}
	}
	if !strings.HasPrefix(id, "IN_") || !strings.HasSuffix(r.body["publish_url"].(string), ":1935/live") || !strings.HasPrefix(r.body["publish_url"].(string), "rtmp://") {
		t.Fatal(r.raw)
	}
	stats := r.body["stats"].(map[string]any)
	for _, f := range []string{"bitrate_in", "width", "height", "fps", "audio_codec", "video_codec", "started_at"} {
		if _, ok := stats[f]; !ok {
			t.Errorf("stats has no %s: %s", f, r.raw)
		}
	}

	// The key is hashed for lookup, encrypted so it can be replaced, and readable in neither.
	var hash, sealed []byte
	if err := h.store.DB.QueryRow(ctx, `SELECT stream_key_hash, stream_key_ciphertext FROM ingresses WHERE id = $1`, id).Scan(&hash, &sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hash), key) || strings.Contains(string(sealed), key) {
		t.Fatal("the key is in the database in the clear")
	}
	if opened, err := h.store.Sealer.Open(sealed); err != nil || opened != key {
		t.Fatalf("%q %v", opened, err)
	}

	// Shown only at create and rotate.
	for _, raw := range []string{
		expect(t, c.do("GET", "/api/v1/ingress/"+id, nil), http.StatusOK).raw,
		expect(t, c.do("GET", "/api/v1/ingress", nil), http.StatusOK).raw,
		expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"name": "New"}), http.StatusOK).raw,
		expect(t, c.do("POST", "/api/v1/ingress/"+id+"/stop", map[string]any{}), http.StatusOK).raw,
	} {
		if strings.Contains(raw, key) || strings.Contains(raw, "stream_key") {
			t.Fatalf("the key is showing: %s", raw)
		}
	}

	// Patching.
	r = expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"room": "r2", "participant_identity": "i2", "enabled": false}), http.StatusOK)
	if r.body["room_name"] != "r2" || r.body["participant_identity"] != "i2" || r.body["enabled"] != false || r.body["name"] != "New" {
		t.Fatal(r.raw)
	}
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"room": ""}), http.StatusUnprocessableEntity)
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"source_url": "http://x"}), http.StatusBadRequest)
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"enabled": true}), http.StatusOK)

	// Publishers are found by the hash of the key, and only with the right input type.
	if in, err := h.store.ClaimIngressByKey(ctx, key, store.InputWHIP, "W", time.Minute); err != store.ErrNotFound {
		t.Fatalf("%v %v", in, err)
	}
	in, err := h.store.ClaimIngressByKey(ctx, key, store.InputRTMP, "W1", time.Minute)
	if err != nil || in.ID != id || in.Status != "buffering" {
		t.Fatalf("%v %v", in, err)
	}
	if _, err := h.store.ClaimIngressByKey(ctx, key, store.InputRTMP, "W2", time.Minute); err != store.ErrIngressBusy {
		t.Fatalf("second publisher: %v", err)
	}
	if _, err := h.store.ClaimIngressByKey(ctx, "wrong", store.InputRTMP, "W2", time.Minute); err != store.ErrNotFound {
		t.Fatalf("wrong key: %v", err)
	}

	// A patch that a running stream cannot take on ends it. One it can, does not.
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"name": "Newer"}), http.StatusOK)
	if stop, err := h.store.RenewIngress(ctx, id, "W1", time.Minute); err != nil || stop {
		t.Fatalf("%v %v", stop, err)
	}
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"room": "r3"}), http.StatusOK)
	if stop, err := h.store.RenewIngress(ctx, id, "W1", time.Minute); err != nil || !stop {
		t.Fatalf("%v %v", stop, err)
	}
	if _, err := h.store.ReleaseIngress(ctx, id, "W1", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}

	// Rotating replaces the key.
	r = expect(t, c.do("POST", "/api/v1/ingress/"+id+"/rotate-key", map[string]any{}), http.StatusOK)
	newKey := r.body["stream_key"].(string)
	if newKey == "" || newKey == key {
		t.Fatal(r.raw)
	}
	if _, err := h.store.ClaimIngressByKey(ctx, key, store.InputRTMP, "W", time.Minute); err != store.ErrNotFound {
		t.Fatalf("old key: %v", err)
	}
	if _, err := h.store.ClaimIngressByKey(ctx, newKey, store.InputRTMP, "W", time.Minute); err != nil {
		t.Fatalf("new key: %v", err)
	}
	if _, err := h.store.ReleaseIngress(ctx, id, "W", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}

	// A dead worker's stream is released by its lease, so the key works again.
	if _, err := h.store.ClaimIngressByKey(ctx, newKey, store.InputRTMP, "dead", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE ingresses SET lease_expires_at = now() - make_interval(secs => $2) WHERE id = $1`, id, (store.ReapGrace + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ReapMedia(ctx); err != nil {
		t.Fatal(err)
	}
	r = expect(t, c.do("GET", "/api/v1/ingress/"+id, nil), http.StatusOK)
	if r.body["status"] != "error" || !strings.Contains(r.body["error"].(string), "stopped responding") {
		t.Fatal(r.raw)
	}
	if _, err := h.store.ClaimIngressByKey(ctx, newKey, store.InputRTMP, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ReleaseIngress(ctx, id, "W", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}

	// Logs.
	r = expect(t, c.do("GET", "/api/v1/ingress/"+id+"/logs", nil), http.StatusOK)
	if len(r.items()) == 0 {
		t.Fatal("no logs from the failure above")
	}

	// WHIP's address.
	w := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "whip", "room": "r", "participant_identity": "i"}), http.StatusCreated)
	if !strings.HasSuffix(w.body["publish_url"].(string), "/whip/v1") || w.body["stream_key"] == "" {
		t.Fatal(w.raw)
	}

	// A URL input has no key to rotate.
	expect(t, c.do("DELETE", "/api/v1/ingress/"+id, nil), http.StatusNoContent)
	expect(t, c.do("GET", "/api/v1/ingress/"+id, nil), http.StatusNotFound)
	if stop, err := h.store.RenewIngress(ctx, id, "W", time.Minute); err != store.ErrJobLost {
		t.Fatalf("a deleted ingress: %v %v", stop, err)
	}
}

func TestURLIngressLifecycle(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	r := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "http://127.0.0.1:1/a.mp4"}), http.StatusCreated)
	id := r.body["id"].(string)
	if r.body["publish_url"] != "" {
		t.Fatal(r.raw)
	}
	if _, ok := r.body["stream_key"]; ok {
		t.Fatal(r.raw)
	}
	r2 := c.do("POST", "/api/v1/ingress/"+id+"/rotate-key", map[string]any{})
	if r2.status != http.StatusConflict || r2.errCode() != "no_stream_key" {
		t.Fatal(r2.raw)
	}

	in, err := h.store.ClaimIngressPull(ctx, "W", time.Minute)
	if err != nil || in == nil || in.ID != id {
		t.Fatalf("%v %v", in, err)
	}
	if again, err := h.store.ClaimIngressPull(ctx, "W2", time.Minute); again != nil || err != nil {
		t.Fatalf("claimed twice: %v %v", again, err)
	}
	if _, err := h.store.ReleaseIngress(ctx, id, "W", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	// Finished means finished, until someone asks for it again.
	if again, _ := h.store.ClaimIngressPull(ctx, "W2", time.Minute); again != nil {
		t.Fatalf("restarted by itself: %v", again)
	}
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"enabled": true}), http.StatusOK)
	if again, _ := h.store.ClaimIngressPull(ctx, "W2", time.Minute); again == nil {
		t.Fatal("did not restart")
	}
	if _, err := h.store.ReleaseIngress(ctx, id, "W2", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	// Disabled and requested again: stays off.
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"enabled": false}), http.StatusOK)
	if again, _ := h.store.ClaimIngressPull(ctx, "W2", time.Minute); again != nil {
		t.Fatalf("started while disabled: %v", again)
	}
}

func TestMediaWebhooksAreQueued(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.ControlPlane.AllowPrivateWebhooks = true })
	h.fakeWorker("EW_a", 100)
	h.openRoom(c.project, "r1")
	ctx := context.Background()
	key := expect(t, c.do("POST", "/api/v1/keys", map[string]string{"name": "k"}), http.StatusCreated)
	hook := expect(t, c.do("POST", "/api/v1/webhooks", map[string]any{"url": "http://127.0.0.1:1/hook", "api_key_id": key.body["id"], "events": []string{"egress_started", "egress_updated", "egress_ended"}}), http.StatusCreated)
	id := startJob(t, c)
	if _, err := h.store.ClaimEgress(ctx, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	e, err := h.store.TransitionEgress(ctx, id, "W", store.EgressActive, store.EgressResult{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordEgressEvent(ctx, e, "egress_updated", "info", "egress_active", "Recording started.", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := h.store.ListDeliveries(ctx, c.project, hook.body["id"].(string), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d deliveries", len(rows))
	}
	var body map[string]any
	if err := json.Unmarshal(rows[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	info, _ := body["egressInfo"].(map[string]any)
	if body["event"] != "egress_updated" || info["egressId"] != id || info["status"] != "EGRESS_ACTIVE" || info["roomName"] != "r1" {
		t.Fatalf("%s", rows[0].Payload)
	}
	var category string
	if err := h.store.DB.QueryRow(ctx, `SELECT category FROM events WHERE project_id = $1 AND type = 'egress_active'`, c.project).Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != store.CategoryRecording {
		t.Fatal(category)
	}
}

// whipPost sends what a publisher would, with an offer that does not matter: the key is looked at first.
func whipPost(t *testing.T, endpoint, key string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", endpoint+"/whip/v1", strings.NewReader("not an offer"))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/sdp")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestWHIPStopsWithTheProject(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	ctx := context.Background()
	mux := http.NewServeMux()
	whip.New(h.store, h.rooms, auth.StaticKeys{}, slog.New(slog.DiscardHandler)).Start(ctx, mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	created := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "whip", "room": "r", "participant_identity": "i"}), http.StatusCreated)
	key := created.body["stream_key"].(string)
	id := created.body["id"].(string)
	// tests/media covers a real publisher on an active project. Here the rooms have no media engine, so this claims the
	// key directly to show that it works for an active project.
	claimWorks := func() {
		t.Helper()
		if _, err := h.store.ClaimIngressByKey(ctx, key, store.InputWHIP, "node", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.ReleaseIngress(ctx, id, "node", store.IngressInactive, ""); err != nil {
			t.Fatal(err)
		}
	}
	claimWorks()

	expect(t, c.do("PATCH", "/api/v1/projects/"+c.project, map[string]any{"status": "suspended"}), http.StatusOK)
	if got := whipPost(t, srv.URL, key); got != http.StatusUnauthorized {
		t.Fatalf("a suspended project: HTTP %d", got)
	}
	expect(t, c.do("PATCH", "/api/v1/projects/"+c.project, map[string]any{"status": "active"}), http.StatusOK)
	claimWorks()

	expect(t, c.do("DELETE", "/api/v1/projects/"+c.project, nil), http.StatusNoContent)
	if got := whipPost(t, srv.URL, key); got != http.StatusUnauthorized {
		t.Fatalf("a deleted project: HTTP %d", got)
	}
	var live int
	if err := h.store.DB.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE project_id = $1 AND revoked_at IS NULL`, c.project).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("deleting a project revokes its keys, and %d have come back", live)
	}
}

func TestMediaSigningKeyRefusesAProjectThatHasEnded(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	ctx := context.Background()
	if _, err := h.store.MediaSigningKey(ctx, c.project); err != nil {
		t.Fatal(err)
	}
	expect(t, c.do("PATCH", "/api/v1/projects/"+c.project, map[string]any{"status": "suspended"}), http.StatusOK)
	if _, err := h.store.MediaSigningKey(ctx, c.project); !errors.Is(err, store.ErrProjectEnded) {
		t.Fatalf("suspended: %v", err)
	}
	expect(t, c.do("PATCH", "/api/v1/projects/"+c.project, map[string]any{"status": "active"}), http.StatusOK)
	expect(t, c.do("DELETE", "/api/v1/projects/"+c.project, nil), http.StatusNoContent)
	if _, err := h.store.MediaSigningKey(ctx, c.project); !errors.Is(err, store.ErrProjectEnded) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestDeletingAProjectEndsItsMedia(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	h.fakeWorker("W", 5)
	h.openRoom(c.project, "r1")

	running := startJob(t, c)
	if e, err := h.store.ClaimEgress(ctx, "W", time.Minute); err != nil || e == nil || e.ID != running {
		t.Fatalf("%v %v", e, err)
	}
	queued := startJob(t, c)
	pull := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "p", "source_url": "http://127.0.0.1:1/a.mp4"}), http.StatusCreated).body["id"].(string)
	waiting := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "q", "source_url": "http://127.0.0.1:1/b.mp4"}), http.StatusCreated).body["id"].(string)
	in, err := h.store.ClaimIngressPull(ctx, "IW", time.Minute)
	if err != nil || in == nil || in.ID != pull {
		t.Fatalf("%v %v", in, err)
	}

	expect(t, c.do("DELETE", "/api/v1/projects/"+c.project, nil), http.StatusNoContent)

	// What is running hears about it the way any stop request is heard, on whichever node or worker it is on.
	if stop, err := h.store.RenewEgress(ctx, running, "W", time.Minute); err != nil || !stop {
		t.Fatalf("running recording: stop %v, %v", stop, err)
	}
	if stop, err := h.store.RenewIngress(ctx, pull, "IW", time.Minute); err != nil || !stop {
		t.Fatalf("running pull: stop %v, %v", stop, err)
	}
	// What has not started never does.
	if e, err := h.store.ClaimEgress(ctx, "W", time.Minute); err != nil || e != nil {
		t.Fatalf("claimed a queued job of a deleted project, %s: %v %v", queued, e, err)
	}
	if again, err := h.store.ClaimIngressPull(ctx, "IW", time.Minute); err != nil || again != nil {
		t.Fatalf("claimed a pull of a deleted project, %s: %v %v", waiting, again, err)
	}
}

func TestSuspendingAProjectStopsItsMediaUntilItComesBack(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	h.fakeWorker("W", 5)
	h.openRoom(c.project, "r1")
	running := startJob(t, c)
	if e, err := h.store.ClaimEgress(ctx, "W", time.Minute); err != nil || e == nil {
		t.Fatalf("%v %v", e, err)
	}
	queued := startJob(t, c)
	pull := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "p", "source_url": "http://127.0.0.1:1/a.mp4"}), http.StatusCreated).body["id"].(string)
	if in, err := h.store.ClaimIngressPull(ctx, "IW", time.Minute); err != nil || in == nil {
		t.Fatalf("%v %v", in, err)
	}

	expect(t, c.do("PATCH", "/api/v1/projects/"+c.project, map[string]any{"status": "suspended"}), http.StatusOK)
	if stop, err := h.store.RenewEgress(ctx, running, "W", time.Minute); err != nil || !stop {
		t.Fatalf("running recording: stop %v, %v", stop, err)
	}
	if stop, err := h.store.RenewIngress(ctx, pull, "IW", time.Minute); err != nil || !stop {
		t.Fatalf("running pull: stop %v, %v", stop, err)
	}
	e, err := h.store.EgressByID(ctx, c.project, queued)
	if err != nil || e.Status != store.EgressAborted {
		t.Fatalf("queued job: %+v, %v", e, err)
	}
	// The worker stops. Nothing starts again until the project is back.
	if _, err := h.store.ReleaseIngress(ctx, pull, "IW", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	if in, err := h.store.ClaimIngressPull(ctx, "IW", time.Minute); err != nil || in != nil {
		t.Fatalf("started while suspended: %v %v", in, err)
	}
	expect(t, c.do("PATCH", "/api/v1/projects/"+c.project, map[string]any{"status": "active"}), http.StatusOK)
	if in, err := h.store.ClaimIngressPull(ctx, "IW", time.Minute); err != nil || in == nil || in.ID != pull {
		t.Fatalf("did not resume: %v %v", in, err)
	}
}

func TestProjectLimitOnRecordings(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.Egress.MaxJobsPerProject = 1 })
	h.fakeWorker("W", 10)
	h.openRoom(c.project, "r1")
	startJob(t, c)
	r := c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}})
	if r.status != http.StatusTooManyRequests || r.errCode() != "project_limit" {
		t.Fatal(r.raw)
	}
	// Another project still has room, which is the point of it.
	other := h.signUp(fmt.Sprintf("other-%d@example.com", time.Now().UnixNano()))
	other.newProject("Other")
	h.openRoom(other.project, "r1")
	startJob(t, other)
}

func TestABurstOfRequestsCannotGetPastTheCapacity(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("W", 3)
	h.openRoom(c.project, "r1")
	var mu sync.Mutex
	statuses := map[int]int{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := c.do("POST", "/api/v1/egress", map[string]any{"type": "room_composite", "room": "r1", "output": map[string]string{"kind": "file", "format": "mp4"}})
			mu.Lock()
			statuses[r.status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if statuses[http.StatusCreated] != 3 || statuses[http.StatusServiceUnavailable] != 13 {
		t.Fatalf("%v", statuses)
	}
}

func TestDeletingAFileOnAnotherBackendIsRefused(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	h.fakeWorker("W", 5)
	h.openRoom(c.project, "r1")
	id := finishedRecording(t, h, c, []byte("the recording"))
	if _, err := h.store.DB.Exec(context.Background(), `UPDATE egresses SET storage = 's3' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	r := c.do("DELETE", "/api/v1/recordings/"+id, nil)
	if r.status != http.StatusConflict || r.errCode() != "recording_unavailable" {
		t.Fatal(r.raw)
	}
	expect(t, c.do("GET", "/api/v1/recordings/"+id, nil), http.StatusOK)
}

func TestSweepRemovesWhatNobodyCanAskFor(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.Storage.RetentionDays = 30 })
	ctx := context.Background()
	h.fakeWorker("W", 5)
	h.openRoom(c.project, "r1")
	backend, err := h.api.recordings()
	if err != nil {
		t.Fatal(err)
	}
	stored := func(id string) bool {
		e, err := h.store.EgressForWorker(ctx, id)
		if err != nil {
			return false
		}
		obj, err := backend.Open(ctx, e.OutputPath)
		if err != nil {
			return false
		}
		_ = obj.Close()
		return true
	}
	recent := finishedRecording(t, h, c, []byte("recent"))
	old := finishedRecording(t, h, c, []byte("old"))
	oldPath, err := h.store.EgressForWorker(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE egresses SET ended_at = now() - interval '31 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	h.api.sweepRecordings(ctx)
	if !stored(recent) {
		t.Error("a recent recording should stay")
	}
	if _, err := h.store.EgressForWorker(ctx, old); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("one past its retention should go: %v", err)
	}
	if obj, err := backend.Open(ctx, oldPath.OutputPath); !errors.Is(err, storage.ErrNotFound) {
		if obj != nil {
			_ = obj.Close()
		}
		t.Errorf("and its file: %v", err)
	}

	// A project being deleted takes everything with it, whatever its age.
	path, err := h.store.EgressForWorker(ctx, recent)
	if err != nil {
		t.Fatal(err)
	}
	created := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "rtmp", "room": "r", "participant_identity": "i"}), http.StatusCreated)
	expect(t, c.do("DELETE", "/api/v1/projects/"+c.project, nil), http.StatusNoContent)
	h.api.sweepRecordings(ctx)
	if _, err := h.store.EgressForWorker(ctx, recent); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row: %v", err)
	}
	if obj, err := backend.Open(ctx, path.OutputPath); !errors.Is(err, storage.ErrNotFound) {
		if obj != nil {
			_ = obj.Close()
		}
		t.Errorf("the file: %v", err)
	}
	var left int
	if err := h.store.DB.QueryRow(ctx, `SELECT count(*) FROM ingresses WHERE id = $1`, created.body["id"]).Scan(&left); err != nil || left != 0 {
		t.Errorf("the ingress: %d, %v", left, err)
	}
}

func TestSweepKeepsAFileItCannotDelete(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.Storage.RetentionDays = 1 })
	ctx := context.Background()
	h.fakeWorker("W", 5)
	h.openRoom(c.project, "r1")
	id := finishedRecording(t, h, c, []byte("elsewhere"))
	if _, err := h.store.DB.Exec(ctx, `UPDATE egresses SET storage = 's3', ended_at = now() - interval '2 days' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	h.api.sweepRecordings(ctx)
	if _, err := h.store.EgressForWorker(ctx, id); err != nil {
		t.Fatalf("its row is the only way to find the file: %v", err)
	}
}

func TestReapWaitsAMomentPastTheLease(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	ctx := context.Background()
	h.fakeWorker("W", 5)
	h.openRoom(c.project, "r1")
	id := startJob(t, c)
	if _, err := h.store.ClaimEgress(ctx, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	expire := func(ago time.Duration) {
		t.Helper()
		if _, err := h.store.DB.Exec(ctx, `UPDATE egresses SET lease_expires_at = now() - make_interval(secs => $2) WHERE id = $1`, id, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	expire(store.ReapGrace / 2)
	if jobs, err := h.store.ReapEgress(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("a lease that has only just run out: %v %v", jobs, err)
	}
	// The worker comes back in that time, and it still has its job.
	if _, err := h.store.RenewEgress(ctx, id, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	expire(store.ReapGrace + time.Second)
	if jobs, err := h.store.ReapEgress(ctx); err != nil || len(jobs) != 1 {
		t.Fatalf("a lease long gone: %v %v", jobs, err)
	}
}

func TestSourceURLIsSealedAndRedacted(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	raw := "srt://127.0.0.1:9000?passphrase=hunter2-hunter2&streamid=topsecret"
	r := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "i", "source_url": raw}), http.StatusCreated)
	id := r.body["id"].(string)
	got := expect(t, c.do("GET", "/api/v1/ingress/"+id, nil), http.StatusOK)
	list := expect(t, c.do("GET", "/api/v1/ingress", nil), http.StatusOK)
	for _, text := range []string{r.raw, got.raw, list.raw} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "topsecret") {
			t.Fatalf("a secret in a response: %s", text)
		}
	}
	var shown string
	var sealed []byte
	if err := h.store.DB.QueryRow(ctx, `SELECT source_url, source_url_sealed FROM ingresses WHERE id = $1`, id).Scan(&shown, &sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shown, "hunter2") || len(sealed) == 0 || strings.Contains(string(sealed), "hunter2") {
		t.Fatalf("%q %q", shown, sealed)
	}
	in, err := h.store.ClaimIngressPull(ctx, "W", time.Minute)
	if err != nil || in == nil {
		t.Fatalf("%v %v", in, err)
	}
	if real, err := h.store.IngressSourceURL(in); err != nil || real != raw {
		t.Fatalf("a worker should get the real address: %q %v", real, err)
	}
}

func TestOldSourceURLsGetSealed(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	raw := "http://user:hunter2@127.0.0.1:1/a.mp4"
	id := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "i", "source_url": raw}), http.StatusCreated).body["id"].(string)
	if _, err := h.store.DB.Exec(ctx, `UPDATE ingresses SET source_url = $2, source_url_sealed = NULL WHERE id = $1`, id, raw); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SealOldSourceURLs(ctx); err != nil {
		t.Fatal(err)
	}
	if got := expect(t, c.do("GET", "/api/v1/ingress/"+id, nil), http.StatusOK); strings.Contains(got.raw, "hunter2") {
		t.Fatal(got.raw)
	}
	in, err := h.store.IngressByID(ctx, c.project, id)
	if err != nil {
		t.Fatal(err)
	}
	if real, err := h.store.IngressSourceURL(in); err != nil || real != raw {
		t.Fatalf("%q %v", real, err)
	}
}

func TestPullsGoBackInTheQueueWhenAWorkerGoesAway(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	id := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "url", "room": "r", "participant_identity": "i", "source_url": "http://127.0.0.1:1/a.mp4"}), http.StatusCreated).body["id"].(string)
	claim := func(worker string) {
		t.Helper()
		in, err := h.store.ClaimIngressPull(ctx, worker, time.Minute)
		if err != nil || in == nil || in.ID != id {
			t.Fatalf("%s: %v %v", worker, in, err)
		}
	}
	claim("W1")
	if _, err := h.store.ReleaseIngressForRestart(ctx, id, "W1", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	claim("W2")
	// A worker that dies: another takes it when the lease is well gone.
	if _, err := h.store.DB.Exec(ctx, `UPDATE ingresses SET lease_expires_at = now() - make_interval(secs => $2) WHERE id = $1`, id, (store.ReapGrace + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	if streams, err := h.store.ReapIngress(ctx); err != nil || len(streams) != 1 {
		t.Fatalf("%v %v", streams, err)
	}
	claim("W3")
	// Moving a running pull to another room stops it, and it starts again there.
	expect(t, c.do("PATCH", "/api/v1/ingress/"+id, map[string]any{"room": "elsewhere"}), http.StatusOK)
	if stop, err := h.store.RenewIngress(ctx, id, "W3", time.Minute); err != nil || !stop {
		t.Fatalf("stop %v, %v", stop, err)
	}
	if _, err := h.store.ReleaseIngress(ctx, id, "W3", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	claim("W4")
	// Stopped by request stays stopped, even when the worker then goes away too.
	expect(t, c.do("POST", "/api/v1/ingress/"+id+"/stop", map[string]any{}), http.StatusOK)
	if _, err := h.store.ReleaseIngressForRestart(ctx, id, "W4", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	if in, err := h.store.ClaimIngressPull(ctx, "W5", time.Minute); err != nil || in != nil {
		t.Fatalf("restarted by itself: %v %v", in, err)
	}
}

func TestProjectLimitOnStreams(t *testing.T) {
	h, c := newMediaHarness(t, nil)
	ctx := context.Background()
	h.store.MaxIngressPerProject = 1
	key := func() string {
		t.Helper()
		return expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "rtmp", "room": "r", "participant_identity": "i"}), http.StatusCreated).body["stream_key"].(string)
	}
	first, second := key(), key()
	if _, err := h.store.ClaimIngressByKey(ctx, first, store.InputRTMP, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ClaimIngressByKey(ctx, second, store.InputRTMP, "W", time.Minute); !errors.Is(err, store.ErrProjectAtLimit) {
		t.Fatalf("got %v", err)
	}
	// A second publisher on the first key is still told it is busy, which is what it needs to hear.
	if _, err := h.store.ClaimIngressByKey(ctx, first, store.InputRTMP, "W2", time.Minute); !errors.Is(err, store.ErrIngressBusy) {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerRoleCanDoAWorkersJob(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.Media.AllowPrivateAddresses = true })
	ctx := context.Background()
	if err := h.store.SetWorkerPassword(ctx, "worker-role-test-password"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(h.cfg.ControlPlane.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("streamkit_worker", "worker-role-test-password")
	worker, err := store.OpenWorker(ctx, u.String(), testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()

	stats, _ := json.Marshal(map[string]any{"kind": store.WorkerKindEgress, "max_jobs": 5, "healthy": true})
	if _, err := worker.HeartbeatServer(ctx, store.ServerNode{NodeID: "W", StartedAt: time.Now(), Stats: stats}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.HeartbeatServer(ctx, store.ServerNode{NodeID: "W", StartedAt: time.Now(), Stats: stats}); err != nil {
		t.Fatal(err)
	}
	h.openRoom(c.project, "r1")
	id := startJob(t, c)
	e, err := worker.ClaimEgress(ctx, "W", time.Minute)
	if err != nil || e == nil || e.ID != id {
		t.Fatalf("%v %v", e, err)
	}
	if _, err := worker.MediaSigningKey(ctx, e.ProjectID); err != nil {
		t.Fatal(err)
	}
	// A project with no key at all gets one, which is the one write to api_keys a worker makes.
	if _, err := h.store.DB.Exec(ctx, `DELETE FROM api_keys WHERE project_id = $1`, c.project); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.MediaSigningKey(ctx, e.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RenewEgress(ctx, id, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	active, err := worker.TransitionEgress(ctx, id, "W", store.EgressActive, store.EgressResult{})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RecordEgressEvent(ctx, active, "egress_updated", "info", "egress_active", "x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.TransitionEgress(ctx, id, "W", store.EgressFailed, store.EgressResult{Error: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.AttachSalvagedFile(ctx, id, store.EgressResult{OutputPath: "p/x.mp4", Storage: "local", FileSize: 1, DurationMs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.EgressForWorker(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := worker.ReapMedia(ctx); err != nil {
		t.Fatal(err)
	}

	ingress := expect(t, c.do("POST", "/api/v1/ingress", map[string]any{"input_type": "rtmp", "room": "r", "participant_identity": "i"}), http.StatusCreated)
	in, err := worker.ClaimIngressByKey(ctx, ingress.body["stream_key"].(string), store.InputRTMP, "W", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RenewIngress(ctx, in.ID, "W", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.UpdateIngressRunning(ctx, in.ID, "W", store.IngressPublishing, store.IngressStats{}); err != nil {
		t.Fatal(err)
	}
	if err := worker.RecordIngressEvent(ctx, in, "ingress_started", "info", "x", "x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ReleaseIngressForRestart(ctx, in.ID, "W", store.IngressInactive, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ClaimIngressPull(ctx, "W", time.Minute); err != nil {
		t.Fatal(err)
	}

	// And not much else.
	for _, statement := range []string{`SELECT * FROM users`, `SELECT name FROM projects`, `DELETE FROM egresses`, `CREATE TABLE stolen (x int)`, `UPDATE api_keys SET revoked_at = NULL`} {
		if _, err := worker.DB.Exec(ctx, statement); err == nil {
			t.Errorf("a worker should not be able to run: %s", statement)
		}
	}
}

func TestDeliveriesAreClaimedInTheOrderTheyWereQueued(t *testing.T) {
	h, c := newMediaHarness(t, func(c *config.Config) { c.ControlPlane.AllowPrivateWebhooks = true })
	ctx := context.Background()
	key := expect(t, c.do("GET", "/api/v1/keys", nil), http.StatusOK).items()
	var keyID string
	if len(key) > 0 {
		keyID = key[0].(map[string]any)["id"].(string)
	} else {
		keyID = expect(t, c.do("POST", "/api/v1/keys", map[string]any{"name": "k"}), http.StatusCreated).body["id"].(string)
	}
	receiver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer receiver.Close()
	hook := expect(t, c.do("POST", "/api/v1/webhooks", map[string]any{"url": receiver.URL + "/hook", "api_key_id": keyID, "events": []string{"egress_ended"}}), http.StatusCreated).body["id"].(string)
	// The dispatcher in this harness must not get to them first, so they are made due only once they are all there.
	var ids []string
	for i := range 6 {
		var id string
		err := h.store.DB.QueryRow(ctx, `INSERT INTO webhook_deliveries (webhook_id, event_type, payload, next_attempt_at, created_at)
			VALUES ($1, 'egress_ended', '{}', now() + interval '1 hour', now() + make_interval(secs => $2)) RETURNING id`, hook, i).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Changing the oldest rows moves them to the end of the table, which is the order an UPDATE then finds them in.
	if _, err := h.store.DB.Exec(ctx, `UPDATE webhook_deliveries SET attempts = attempts WHERE id = ANY($1)`, ids[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < len(ids) {
		due, err := h.store.ClaimDueDeliveries(ctx, 50, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) == 0 {
			break
		}
		for _, d := range due {
			got = append(got, d.ID)
		}
	}
	// The harness's own dispatcher may take some first, in order or not, so only what this claimed is looked at, as a subsequence.
	at := 0
	for _, id := range got {
		for at < len(ids) && ids[at] != id {
			at++
		}
		if at == len(ids) {
			t.Fatalf("out of order: queued %v, claimed %v", ids, got)
		}
	}
}
