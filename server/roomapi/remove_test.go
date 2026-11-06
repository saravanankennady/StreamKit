package roomapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/room"
)

// noted is an admission check that only remembers who it heard about.
type noted struct{ removed []string }

func (*noted) Admit(string, string, *auth.Grants, bool) (room.Admitted, error) {
	return room.Admitted{}, nil
}
func (n *noted) Removed(project, roomName, identity string) {
	n.removed = append(n.removed, project+"/"+roomName+"/"+identity)
}

func TestRemovingSomeoneNotConnectedStillKeepsThemOut(t *testing.T) {
	cfg := config.Default()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rooms := room.NewManager(ctx, cfg, nil, slog.New(slog.DiscardHandler))
	admission := &noted{}
	rooms.SetAdmission(admission)
	mux := http.NewServeMux()
	NewService(cfg, keys, rooms).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The answer stays what the reference gives, and the token they still hold is refused from here on.
	admin := token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "r"})
	status, raw, _ := call(t, srv, "RemoveParticipant", admin, `{"room":"r","identity":"guest-a"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status %d: %s", status, raw)
	}
	if !slices.Contains(admission.removed, "p1/r/guest-a") {
		t.Fatalf("admission heard %v", admission.removed)
	}

	// Without the grant, nothing happens at all.
	admission.removed = nil
	other := token(t, "k1", &auth.VideoGrant{RoomAdmin: true, Room: "elsewhere"})
	if status, raw, _ := call(t, srv, "RemoveParticipant", other, `{"room":"r","identity":"guest-a"}`); status == http.StatusOK || status == http.StatusNotFound {
		t.Fatalf("status %d: %s", status, raw)
	}
	if len(admission.removed) != 0 {
		t.Fatalf("admission heard %v from someone with no grant", admission.removed)
	}
}
