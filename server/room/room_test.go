package room

import (
	"testing"
	"time"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/config"
)

func TestScoreConnection(t *testing.T) {
	cases := []struct {
		name   string
		loss   float64
		rtt    time.Duration
		jitter time.Duration
		want   pb.ConnectionQuality
	}{
		{"clean", 0, 20 * time.Millisecond, 2 * time.Millisecond, pb.ConnectionQuality_EXCELLENT},
		{"light loss", 0.01, 50 * time.Millisecond, 5 * time.Millisecond, pb.ConnectionQuality_EXCELLENT},
		{"moderate loss", 0.05, 50 * time.Millisecond, 5 * time.Millisecond, pb.ConnectionQuality_GOOD},
		{"heavy loss", 0.15, 50 * time.Millisecond, 5 * time.Millisecond, pb.ConnectionQuality_POOR},
		{"long delay", 0, 800 * time.Millisecond, 10 * time.Millisecond, pb.ConnectionQuality_POOR},
		{"moderate delay", 0, 400 * time.Millisecond, 20 * time.Millisecond, pb.ConnectionQuality_GOOD},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			score, got := ScoreConnection(c.loss, c.rtt, c.jitter)
			if got != c.want {
				t.Fatalf("quality = %v (score %.2f), want %v", got, score, c.want)
			}
			if score < 1 || score > 4.5 {
				t.Fatalf("score %v out of range", score)
			}
		})
	}
}

func TestIdleExpiry(t *testing.T) {
	now := time.Now()
	r := &Room{
		info: &pb.Room{EmptyTimeout: 300, DepartureTimeout: 20, CreationTimeMs: now.UnixMilli()},
	}
	if r.idleExpired(now.Add(299 * time.Second)) {
		t.Fatal("empty timeout has not passed")
	}
	if !r.idleExpired(now.Add(300 * time.Second)) {
		t.Fatal("a room nobody joined should close after its empty timeout")
	}

	r.everJoined = true
	r.lastLeftAt = now
	if r.idleExpired(now.Add(19 * time.Second)) {
		t.Fatal("departure timeout has not passed")
	}
	if !r.idleExpired(now.Add(20 * time.Second)) {
		t.Fatal("a room people left should close after its departure timeout")
	}

	r.state = StateClosed
	if r.idleExpired(now.Add(time.Hour)) {
		t.Fatal("a closed room cannot expire again")
	}
}

func TestPick(t *testing.T) {
	if pick(0, 5) != 5 || pick(3, 5) != 3 {
		t.Fatal("pick")
	}
}

func TestMintTURN(t *testing.T) {
	cfg := config.TURN{Secret: "shared", URLs: []string{"turn:t.example.com:3478"}}
	at := time.Unix(1_700_000_000, 0)
	s := mintTURN(cfg, "alice", at)
	if s.GetUsername() != "1700086400:alice" {
		t.Fatalf("username = %q", s.GetUsername())
	}
	// Computed separately with: printf '1700086400:alice' | openssl dgst -sha1 -hmac shared -binary | base64
	if s.GetCredential() != "x8DliSGB55yI7kN3Gx4/JYMqqZA=" {
		t.Fatalf("credential = %q", s.GetCredential())
	}
	if len(s.GetUrls()) != 1 {
		t.Fatalf("urls = %v", s.GetUrls())
	}
}
