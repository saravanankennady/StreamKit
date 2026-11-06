package routes

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"elnova-streamkit.local/server/auth"
)

const testProviderKey = "sk-this-is-not-a-real-provider-key"

// workerRequest calls a route the way a worker does: an API-key-signed token and no cookie.
func workerRequest(t *testing.T, h *harness, method, path, body string, grant *auth.VideoGrant, key response, headers map[string]string) (int, string) {
	t.Helper()
	token, err := auth.Mint(auth.Key{APIKey: key.body["api_key"].(string), Secret: key.body["secret"].(string)}, &auth.Grants{Identity: "worker", Video: grant}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res.StatusCode, sb.String()
}

func TestProviderKeysNeverReachABrowser(t *testing.T) {
	h := newHarness(t, nil)
	owner, key := shareSetup(t, h, "owner@example.com")

	saved := expect(t, owner.do("PUT", "/api/v1/provider-credentials", map[string]any{"provider": "openai", "secret": testProviderKey}), 200)
	if strings.Contains(saved.raw, testProviderKey) {
		t.Fatal("saving a key echoed it back")
	}
	if saved.body["provider"] != "openai" || saved.body["name"] != "default" {
		t.Fatalf("saved: %s", saved.raw)
	}

	list := expect(t, owner.do("GET", "/api/v1/provider-credentials", nil), 200)
	if strings.Contains(list.raw, testProviderKey) {
		t.Fatal("the list shows the key")
	}
	if !strings.Contains(list.raw, `"openai"`) {
		t.Fatalf("the list does not show the credential: %s", list.raw)
	}

	// It is sealed at rest.
	var sealed []byte
	if err := h.store.DB.QueryRow(context.Background(), `SELECT secret_sealed FROM provider_credentials WHERE id = $1`, saved.body["id"]).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), testProviderKey) {
		t.Fatal("the key is in the database in plain text")
	}

	// A signed-in person cannot read it back, even the owner.
	expect(t, owner.do("POST", "/api/v1/agent-credentials/resolve", map[string]any{"provider": "openai"}), 403)

	// It must not appear in the audit log either.
	audit := expect(t, owner.do("GET", "/api/v1/audit", nil), 200)
	if strings.Contains(audit.raw, testProviderKey) {
		t.Fatal("the audit log holds the key")
	}
	if !strings.Contains(audit.raw, "provider_credential.saved") {
		t.Fatalf("saving a key was not audited: %s", audit.raw)
	}

	worker := &auth.VideoGrant{Agent: true}
	body := `{"provider":"openai"}`

	// A worker can.
	status, raw := workerRequest(t, h, "POST", "/api/v1/agent-credentials/resolve", body, worker, key, nil)
	if status != 200 || !strings.Contains(raw, testProviderKey) {
		t.Fatalf("worker read: %d %s", status, raw)
	}

	// A page holding such a token cannot.
	status, raw = workerRequest(t, h, "POST", "/api/v1/agent-credentials/resolve", body, worker, key, map[string]string{"Origin": "https://example.com"})
	if status != 403 || strings.Contains(raw, testProviderKey) {
		t.Fatalf("request from a page: %d %s", status, raw)
	}

	// Nor can a token for joining a room, including a job's own token, or one with no agent grant.
	for name, grant := range map[string]*auth.VideoGrant{
		"participant": {RoomJoin: true, Room: "r"},
		"job":         {RoomJoin: true, Room: "r", Agent: true},
		"admin":       {RoomAdmin: true, Room: "r"},
	} {
		status, raw = workerRequest(t, h, "POST", "/api/v1/agent-credentials/resolve", body, grant, key, nil)
		if status != 403 || strings.Contains(raw, testProviderKey) {
			t.Fatalf("%s token: %d %s", name, status, raw)
		}
	}

	// Replacing keeps one row, and the new key is what comes back.
	expect(t, owner.do("PUT", "/api/v1/provider-credentials", map[string]any{"provider": "openai", "secret": "sk-replacement"}), 200)
	status, raw = workerRequest(t, h, "POST", "/api/v1/agent-credentials/resolve", body, worker, key, nil)
	if status != 200 || !strings.Contains(raw, "sk-replacement") || strings.Contains(raw, testProviderKey) {
		t.Fatalf("after replacing: %d %s", status, raw)
	}

	// Deleting removes it.
	expect(t, owner.do("DELETE", fmt.Sprintf("/api/v1/provider-credentials/%s", saved.body["id"]), nil), 204)
	status, _ = workerRequest(t, h, "POST", "/api/v1/agent-credentials/resolve", body, worker, key, nil)
	if status != 404 {
		t.Fatalf("after deleting: %d", status)
	}
}

func TestProviderKeysStayInTheirProject(t *testing.T) {
	h := newHarness(t, nil)
	a, _ := shareSetup(t, h, "a@example.com")
	_, keyB := shareSetup(t, h, "b@example.com")
	expect(t, a.do("PUT", "/api/v1/provider-credentials", map[string]any{"provider": "anthropic", "secret": testProviderKey}), 200)
	status, raw := workerRequest(t, h, "POST", "/api/v1/agent-credentials/resolve", `{"provider":"anthropic"}`, &auth.VideoGrant{Agent: true}, keyB, nil)
	if status != 404 || strings.Contains(raw, testProviderKey) {
		t.Fatalf("another project's worker: %d %s", status, raw)
	}
}

func TestProviderCredentialValidation(t *testing.T) {
	h := newHarness(t, nil)
	owner, _ := shareSetup(t, h, "owner@example.com")
	for _, body := range []map[string]any{
		{"provider": "openai"},
		{"provider": "", "secret": "x"},
		{"provider": "Not Valid!", "secret": "x"},
		{"provider": "openai", "name": "bad name", "secret": "x"},
		{"provider": "openai", "secret": strings.Repeat("x", 8193)},
	} {
		expect(t, owner.do("PUT", "/api/v1/provider-credentials", body), 422)
	}
}

func TestAgentSessionReports(t *testing.T) {
	h := newHarness(t, nil)
	owner, key := shareSetup(t, h, "owner@example.com")
	report := `{
		"session_id": "abc123", "room": "support", "job_id": "AJ_1", "agent_name": "helper", "pipeline": "cascaded",
		"models": {"llm": "local/qwen", "tts": "local/piper"}, "started_at": 1793790000.5, "ended_at": 1793790060.25,
		"cost_usd": null, "errors": [{"source": "stt", "message": "x"}],
		"turns": [
			{"turn_id": "t1", "interrupted": false, "user_text": "hello", "latency": {"end_to_end": 1.0, "llm_first_token": 0.2}, "usage": []},
			{"turn_id": "t2", "interrupted": true, "latency": {"end_to_end": 2.0}},
			{"turn_id": "t3", "interrupted": false, "latency": {"end_to_end": null}}
		]
	}`
	grant := &auth.VideoGrant{Agent: true}

	// People and room tokens cannot write reports.
	expect(t, owner.do("POST", "/api/v1/agent-sessions", map[string]any{"session_id": "x", "started_at": 1}), 403)
	if status, _ := workerRequest(t, h, "POST", "/api/v1/agent-sessions", report, &auth.VideoGrant{RoomJoin: true, Room: "r"}, key, nil); status != 403 {
		t.Fatalf("a participant token wrote a report: %d", status)
	}

	status, raw := workerRequest(t, h, "POST", "/api/v1/agent-sessions", report, grant, key, nil)
	if status != 200 {
		t.Fatalf("report: %d %s", status, raw)
	}
	// Sending it again replaces it rather than making a second one.
	if status, raw = workerRequest(t, h, "POST", "/api/v1/agent-sessions", report, grant, key, nil); status != 200 {
		t.Fatalf("second report: %d %s", status, raw)
	}

	list := expect(t, owner.do("GET", "/api/v1/agent-sessions?room=support", nil), 200)
	items := list.body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list: %s", list.raw)
	}
	item := items[0].(map[string]any)
	if item["turn_count"].(float64) != 3 || item["interrupted"].(float64) != 1 || item["error_count"].(float64) != 1 {
		t.Fatalf("summary: %s", list.raw)
	}
	// Averaged over the turns that measured it, and no made-up cost.
	if item["avg_end_to_end"].(float64) != 1.5 || item["cost_usd"] != nil {
		t.Fatalf("summary numbers: %s", list.raw)
	}
	if strings.Contains(list.raw, "hello") {
		t.Fatal("the list holds what people said")
	}

	one := expect(t, owner.do("GET", fmt.Sprintf("/api/v1/agent-sessions/%s", item["id"]), nil), 200)
	if !strings.Contains(one.raw, "hello") {
		t.Fatalf("the session's turns are missing: %s", one.raw)
	}

	expect(t, h.signUp("other@example.com").do("GET", fmt.Sprintf("/api/v1/agent-sessions/%s", item["id"]), nil), 400)
	if status, _ := workerRequest(t, h, "POST", "/api/v1/agent-sessions", `{"session_id":"","started_at":1}`, grant, key, nil); status != 422 {
		t.Fatalf("invalid report: %d", status)
	}
}
