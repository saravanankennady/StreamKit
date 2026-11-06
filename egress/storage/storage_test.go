package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"elnova-streamkit.local/server/config"
)

func TestCheckKeyRejectsAnythingThatCouldEscape(t *testing.T) {
	good := []string{"a", "project/EG_abc.mp4", "9f3a-1/EG_K4dXw2qRt8mN.webm"}
	for _, k := range good {
		if err := CheckKey(k); err != nil {
			t.Errorf("%q should be accepted: %v", k, err)
		}
	}
	bad := []string{
		"", "/etc/passwd", "../x", "a/../b", "a/..", "..", ".", "a//b", "a/", "./a", `a\b`, "a/.hidden", "a b", "a\x00b", "a/b..c",
		"%2e%2e/x", "a?b", strings.Repeat("a", 600),
	}
	for _, k := range bad {
		if err := CheckKey(k); !errors.Is(err, ErrBadKey) {
			t.Errorf("%q should be rejected, got %v", k, err)
		}
	}
}

func TestRecordingKeyRefusesHostileIDs(t *testing.T) {
	if _, err := RecordingKey("../../etc", "EG_x", "mp4"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("got %v", err)
	}
	if _, err := RecordingKey("p", "../x", "mp4"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("got %v", err)
	}
	if k, err := RecordingKey("p1", "EG_x", "webm"); err != nil || k != "p1/EG_x.webm" {
		t.Fatalf("got %q, %v", k, err)
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// exercise is what the API needs from any backend: durable puts, Range requests, and idempotent deletes.
func exercise(t *testing.T, s Storage) {
	t.Helper()
	ctx := context.Background()
	if err := s.Check(ctx); err != nil {
		t.Fatalf("a working backend failed its check: %v", err)
	}
	data := bytes.Repeat([]byte("0123456789"), 40_000)
	const key = "project-1/EG_test.mp4"
	size, err := s.Put(ctx, key, writeTemp(t, data), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) {
		t.Fatalf("size %d", size)
	}
	serve := func(rangeHeader string) *http.Response {
		obj, err := s.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Close()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		rec := httptest.NewRecorder()
		http.ServeContent(rec, req, "x.mp4", obj.ModTime(), obj)
		return rec.Result()
	}
	whole := serve("")
	if whole.StatusCode != http.StatusOK || whole.ContentLength != int64(len(data)) {
		t.Fatalf("whole file: %d, %d bytes", whole.StatusCode, whole.ContentLength)
	}
	part := serve("bytes=399990-399999")
	body, _ := io.ReadAll(part.Body)
	if part.StatusCode != http.StatusPartialContent || string(body) != "0123456789" {
		t.Fatalf("range: %d %q", part.StatusCode, body)
	}
	if got := part.Header.Get("Content-Range"); got != "bytes 399990-399999/400000" {
		t.Fatalf("Content-Range %q", got)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("deleting again: %v", err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := s.Put(ctx, "../outside", writeTemp(t, data), ""); !errors.Is(err, ErrBadKey) {
		t.Fatalf("put outside: %v", err)
	}
	if _, err := s.Open(ctx, "../outside"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("open outside: %v", err)
	}
}

func TestLocal(t *testing.T) {
	s, err := NewLocal(filepath.Join(t.TempDir(), "recordings"))
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)
}

func TestLocalCheckNoticesAReadOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	s, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o750) }()
	if err := s.Check(context.Background()); err == nil {
		t.Fatal("a read-only directory passed the check")
	}
}

// A symlink inside the directory must not lead a read out of it.
func TestLocalDoesNotFollowSymlinksOut(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "recordings")
	s, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if obj, err := s.Open(context.Background(), "link"); err == nil {
		obj.Close()
		t.Fatal("followed a symlink out of the storage root")
	}
}

// TestS3 runs against a real service: set STREAMKIT_TEST_S3_ENDPOINT, for example 127.0.0.1:59000 with MinIO in Docker.
func TestS3(t *testing.T) {
	endpoint := os.Getenv("STREAMKIT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("STREAMKIT_TEST_S3_ENDPOINT is not set")
	}
	s, err := New(config.MediaStorage{Kind: "s3", S3: config.S3{
		Endpoint: endpoint, Bucket: "streamkit-test-storage", Prefix: "unit",
		AccessKey: os.Getenv("STREAMKIT_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("STREAMKIT_TEST_S3_SECRET_KEY"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)
}
