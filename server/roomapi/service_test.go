package roomapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/room"
)

const secret = "a-long-enough-secret-for-testing-0001"

type multiKeys map[string]auth.Key

func (m multiKeys) Lookup(k string) (auth.Key, bool) { v, ok := m[k]; return v, ok }

var keys = multiKeys{
	"k1": {APIKey: "k1", Secret: secret, ProjectID: "p1"},
	"k2": {APIKey: "k2", Secret: secret, ProjectID: "p2"},
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rooms := room.NewManager(ctx, cfg, nil, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	NewService(cfg, keys, rooms).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func token(t *testing.T, key string, v *auth.VideoGrant) string {
	t.Helper()
	tok, err := auth.Mint(keys[key], &auth.Grants{Identity: "api", Video: v}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func call(t *testing.T, srv *httptest.Server, method, tok, body string) (int, string, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+pathPrefix+method, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	var parsed map[string]any
	_ = json.Unmarshal(buf.Bytes(), &parsed)
	return res.StatusCode, buf.String(), parsed
}

func TestCreateRoomJSONShape(t *testing.T) {
	srv := newServer(t)
	admin := token(t, "k1", &auth.VideoGrant{RoomCreate: true})
	status, raw, got := call(t, srv, "CreateRoom", admin, `{"name":"r1","empty_timeout":60,"metadata":"m","an_unknown_field":1}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	if got["name"] != "r1" || got["metadata"] != "m" || got["empty_timeout"] != float64(60) {
		t.Fatalf("unexpected room: %s", raw)
	}
	// Proto field names, and defaults are present.
	if _, ok := got["num_participants"]; !ok {
		t.Fatalf("default values must be emitted: %s", raw)
	}
	if got["departure_timeout"] != float64(20) {
		t.Fatalf("server default not applied: %s", raw)
	}
	if _, ok := got["creation_time"].(string); !ok {
		t.Fatalf("64-bit integers are JSON strings: %s", raw)
	}

	// Creating it again returns the same room, unchanged.
	_, _, again := call(t, srv, "CreateRoom", admin, `{"name":"r1","empty_timeout":999}`)
	if again["sid"] != got["sid"] || again["empty_timeout"] != float64(60) {
		t.Fatalf("existing room changed: %v", again)
	}
}

func TestProtobufEncoding(t *testing.T) {
	srv := newServer(t)
	body, _ := proto.Marshal(&pb.CreateRoomRequest{Name: "binary"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+pathPrefix+"CreateRoom", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("Authorization", "Bearer "+token(t, "k1", &auth.VideoGrant{RoomCreate: true}))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/protobuf" {
		t.Fatalf("status %d, type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	var room pb.Room
	if err := proto.Unmarshal(buf.Bytes(), &room); err != nil || room.GetName() != "binary" {
		t.Fatalf("room %+v, err %v", &room, err)
	}
}

func TestDeniedIsUnauthenticated(t *testing.T) {
	srv := newServer(t)
	cases := []struct {
		name   string
		method string
		tok    string
		body   string
	}{
		{"no token", "CreateRoom", "", `{"name":"x"}`},
		{"wrong grant for create", "CreateRoom", token(t, "k1", &auth.VideoGrant{RoomJoin: true}), `{"name":"x"}`},
		{"delete needs create, not admin", "DeleteRoom", token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "x"}), `{"room":"x"}`},
		{"list needs list", "ListRooms", token(t, "k1", &auth.VideoGrant{RoomCreate: true}), `{}`},
		{"admin of another room", "ListParticipants", token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "other"}), `{"room":"x"}`},
		{"admin with no room", "ListParticipants", token(t, "k1", &auth.VideoGrant{RoomAdmin: true}), `{"room":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw, _ := call(t, srv, c.method, c.tok, c.body)
			if status != 401 || raw != `{"code":"unauthenticated","msg":"permissions denied"}` {
				t.Fatalf("status %d: %s", status, raw)
			}
		})
	}
}

func TestBadCredentialsArePlainText(t *testing.T) {
	srv := newServer(t)
	status, raw, parsed := call(t, srv, "ListRooms", "garbage", `{}`)
	if status != 401 || parsed != nil {
		t.Fatalf("status %d: %q", status, raw)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+pathPrefix+"ListRooms", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic abc")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestErrorShapes(t *testing.T) {
	srv := newServer(t)
	creator := token(t, "k1", &auth.VideoGrant{RoomCreate: true})
	admin := token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "missing"})

	status, raw, _ := call(t, srv, "DeleteRoom", creator, `{"room":"missing"}`)
	if status != 404 || raw != `{"code":"not_found","msg":"requested room does not exist"}` {
		t.Fatalf("delete: %d %s", status, raw)
	}
	status, raw, _ = call(t, srv, "CreateRoom", creator, `not json`)
	if status != 400 || raw != `{"code":"malformed","msg":"the json request could not be decoded"}` {
		t.Fatalf("malformed: %d %s", status, raw)
	}
	status, _, got := call(t, srv, "CreateRoom", creator, `{"name":""}`)
	if status != 400 || got["code"] != "invalid_argument" {
		t.Fatalf("empty name: %d %v", status, got)
	}
	status, _, got = call(t, srv, "NoSuchMethod", creator, `{}`)
	if status != 404 || got["code"] != "bad_route" {
		t.Fatalf("unknown method: %d %v", status, got)
	}
	status, _, got = call(t, srv, "PerformRpc", creator, `{}`)
	if status != 501 || got["code"] != "unimplemented" {
		t.Fatalf("unimplemented: %d %v", status, got)
	}
	status, raw, _ = call(t, srv, "MutePublishedTrack", admin, `{"room":"missing","identity":"a","track_sid":"t","muted":false}`)
	if status != 412 || raw != `{"code":"failed_precondition","msg":"remote unmute not enabled"}` {
		t.Fatalf("remote unmute: %d %s", status, raw)
	}
	status, _, got = call(t, srv, "SendData", admin, `{"room":"missing","data":"AA==","nonce":"AA=="}`)
	if status != 400 || got["msg"] != "nonce should be 16-bytes or not present, got: 1 bytes" {
		t.Fatalf("nonce: %d %v", status, got)
	}
	status, raw, _ = call(t, srv, "GetParticipant", admin, `{"room":"missing","identity":"a"}`)
	if status != 404 {
		t.Fatalf("get participant: %d %s", status, raw)
	}
}

func TestBadRoutes(t *testing.T) {
	srv := newServer(t)
	res, err := http.Get(srv.URL + pathPrefix + "ListRooms")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("GET: status %d", res.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+pathPrefix+"ListRooms", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("wrong type: status %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+pathPrefix+"ListRooms", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token(t, "k1", &auth.VideoGrant{RoomList: true}))
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("charset parameter: status %d", res.StatusCode)
	}
}

func TestListParticipantsOfUnknownRoomIsEmpty(t *testing.T) {
	srv := newServer(t)
	status, raw, got := call(t, srv, "ListParticipants", token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "nope"}), `{"room":"nope"}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	if list, ok := got["participants"].([]any); !ok || len(list) != 0 {
		t.Fatalf("participants = %v", got["participants"])
	}
}

func TestRoomMetadataCanBeCleared(t *testing.T) {
	srv := newServer(t)
	call(t, srv, "CreateRoom", token(t, "k1", &auth.VideoGrant{RoomCreate: true}), `{"name":"r","metadata":"m"}`)
	status, raw, got := call(t, srv, "UpdateRoomMetadata", token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "r"}), `{"room":"r","metadata":""}`)
	if status != 200 || got["metadata"] != "" {
		t.Fatalf("status %d: %s", status, raw)
	}
}

func TestProjectsAreIsolated(t *testing.T) {
	srv := newServer(t)
	p1 := token(t, "k1", &auth.VideoGrant{RoomCreate: true, RoomList: true})
	p2 := token(t, "k2", &auth.VideoGrant{RoomCreate: true, RoomList: true})

	_, _, first := call(t, srv, "CreateRoom", p1, `{"name":"shared-name"}`)
	_, _, second := call(t, srv, "CreateRoom", p2, `{"name":"shared-name"}`)
	if first["sid"] == second["sid"] {
		t.Fatal("two projects share a room with the same name")
	}

	_, _, listed := call(t, srv, "ListRooms", p2, `{}`)
	if rooms := listed["rooms"].([]any); len(rooms) != 1 || rooms[0].(map[string]any)["sid"] != second["sid"] {
		t.Fatalf("project 2 sees %v", listed["rooms"])
	}

	status, _, _ := call(t, srv, "DeleteRoom", p2, `{"room":"shared-name"}`)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	_, _, listed = call(t, srv, "ListRooms", p1, `{}`)
	if rooms := listed["rooms"].([]any); len(rooms) != 1 || rooms[0].(map[string]any)["sid"] != first["sid"] {
		t.Fatalf("deleting in project 2 affected project 1: %v", listed["rooms"])
	}
}
