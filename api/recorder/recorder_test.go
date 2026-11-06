package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/api/webhook"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/monitor"
	"elnova-streamkit.local/server/room"
)

func TestAbortedSessionsCountAsConnectionFailures(t *testing.T) {
	base := os.Getenv("STREAMKIT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("STREAMKIT_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("streamkit_test_rec_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String(), "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})

	log := slog.New(slog.DiscardHandler)
	cfg := config.Default()
	rooms := room.NewManager(ctx, cfg, nil, log)
	r := New(st, monitor.New(rooms), rooms, webhook.New(st, log, false), cfg, log)

	const project = "project-with-only-failures"
	aborted := room.Event{
		Type: room.EventParticipantConnectionAborted, ProjectID: project, At: time.Now(),
		Room:        &pb.Room{Sid: "RM_x", Name: "r"},
		Participant: &pb.ParticipantInfo{Sid: "PA_x", Identity: "someone"},
	}
	r.handle(ctx, aborted)
	r.handle(ctx, aborted)
	left := aborted
	left.Type = room.EventParticipantLeft
	r.handle(ctx, left)
	r.recordUsage(ctx)
	// Counted once: a second write must not add them again.
	r.recordUsage(ctx)

	points, err := st.UsageSeries(ctx, project, "day", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].ConnectionFailures != 2 {
		t.Fatalf("usage = %+v, want 2 failures: leaving normally is not a failure", points)
	}
}
