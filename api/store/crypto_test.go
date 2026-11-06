package store

import (
	"encoding/hex"
	"strings"
	"testing"
)

func testSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(hex.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealerRoundTrip(t *testing.T) {
	s := testSealer(t)
	a, err := s.Seal("the secret")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Seal("the secret")
	if string(a) == string(b) {
		t.Fatal("two seals of one value should differ: the nonce must be random")
	}
	if strings.Contains(string(a), "the secret") {
		t.Fatal("plaintext is visible")
	}
	got, err := s.Open(a)
	if err != nil || got != "the secret" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSealerRejectsTampering(t *testing.T) {
	s := testSealer(t)
	sealed, _ := s.Seal("x")
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed); err == nil {
		t.Fatal("tampered value decrypted")
	}
	if _, err := s.Open([]byte{1, 2}); err == nil {
		t.Fatal("short value decrypted")
	}
	other, _ := NewSealer(strings.Repeat("ab", 32))
	sealed, _ = s.Seal("x")
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

func TestNewSealerValidatesKey(t *testing.T) {
	for _, bad := range []string{"", "short", strings.Repeat("ab", 16)} {
		if _, err := NewSealer(bad); err == nil {
			t.Fatalf("key %q was accepted", bad)
		}
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") || strings.Contains(h, "correct") {
		t.Fatalf("hash = %q", h)
	}
	if !CheckPassword(h, "correct horse") {
		t.Fatal("right password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
	for _, bad := range []string{"", "plain", "$argon2id$x", "$bcrypt$v=19$m=1,t=1,p=1$AA$AA"} {
		if CheckPassword(bad, "x") {
			t.Fatalf("hash %q accepted", bad)
		}
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("salt must differ")
	}
}
