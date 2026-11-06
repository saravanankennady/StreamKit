package room

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/transport"
)

// door is an admission check with a list of who has been removed. It can run something at the worst moment for a join:
// straight after it said yes, with no session yet for anything to find.
type door struct {
	mu       sync.Mutex
	removed  map[string]bool
	afterYes func()
}

func (d *door) Admit(_, _ string, g *auth.Grants, commit bool) (Admitted, error) {
	d.mu.Lock()
	refused := d.removed[g.Identity]
	hook := d.afterYes
	if commit {
		d.afterYes = nil
	}
	d.mu.Unlock()
	if refused {
		return Admitted{}, &RefusedError{Reason: "removed"}
	}
	if commit && hook != nil {
		hook()
	}
	return Admitted{}, nil
}

func (d *door) Removed(_, _, identity string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed[identity] = true
}

type quietSink struct{}

func (quietSink) Send(*pb.SignalResponse) error { return nil }
func (quietSink) Close(string)                  {}

func freePorts(t *testing.T) (udp, tcp int) {
	t.Helper()
	u, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Close() }()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return u.LocalAddr().(*net.UDPAddr).Port, l.Addr().(*net.TCPAddr).Port
}

func managerWithEngine(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.Development = true
	cfg.RTC.UDPPort, cfg.RTC.TCPPort = freePorts(t)
	cfg.RTC.IncludeLoopback = true
	engine, err := transport.New(cfg.RTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewManager(ctx, cfg, engine, slog.New(slog.DiscardHandler))
}

func joinRequest(identity string) JoinRequest {
	return JoinRequest{
		RoomName: "r",
		Verified: &auth.Verified{Key: auth.Key{ProjectID: "p"}, Grants: &auth.Grants{Identity: identity, Video: &auth.VideoGrant{RoomJoin: true, Room: "r"}}},
		Sink:     quietSink{},
	}
}

// Someone can be removed in the moment between the door letting them in and their session existing, when there is nothing
// for the removal to close. They must not end up in the room.
func TestARemovalDuringAJoinKeepsThemOut(t *testing.T) {
	m := managerWithEngine(t)
	d := &door{removed: map[string]bool{}}
	m.SetAdmission(d)

	// Someone else is here, so the room stays open throughout.
	bystander, err := m.Join(joinRequest("bystander"))
	if err != nil {
		t.Fatal(err)
	}
	defer bystander.Close(false, pb.DisconnectReason_CLIENT_INITIATED)

	d.afterYes = func() {
		if m.Remove("p", "r", "guest") {
			t.Error("there should be no session yet for this to close")
		}
	}
	p, err := m.Join(joinRequest("guest"))
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a removal during the join: %v, %v", p, err)
	}
	if got := m.Get("p", "r").Participant("guest"); got != nil && !got.IsClosed() {
		t.Fatal("someone removed is in the room")
	}

	// Trying again is refused at the door, and nobody else was disturbed.
	if _, err := m.Join(joinRequest("guest")); !errors.As(err, &refused) {
		t.Fatalf("joining again: %v", err)
	}
	if bystander.IsClosed() {
		t.Fatal("this closed someone else's session")
	}
}

// If the removal comes after the session is there, it finds it. Between this and the test above, there is no gap.
func TestARemovalAfterAJoinClosesTheSession(t *testing.T) {
	m := managerWithEngine(t)
	m.SetAdmission(&door{removed: map[string]bool{}})
	p, err := m.Join(joinRequest("guest"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Remove("p", "r", "guest") {
		t.Fatal("the removal did not find the session")
	}
	if !p.IsClosed() {
		t.Fatal("the session is still open")
	}
}

// Those who are not refused pay for a second look and nothing more.
func TestAJoinNobodyInterruptedGoesAhead(t *testing.T) {
	m := managerWithEngine(t)
	m.SetAdmission(&door{removed: map[string]bool{}})
	p, err := m.Join(joinRequest("guest"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(false, pb.DisconnectReason_CLIENT_INITIATED)
	if p.IsClosed() || m.Get("p", "r").Participant("guest") != p {
		t.Fatal("an ordinary join did not end up in the room")
	}
}
