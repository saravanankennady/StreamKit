// Package storage keeps finished recordings: in a local directory, or in any S3-compatible service.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"elnova-streamkit.local/server/config"
)

// Errors callers branch on.
var (
	ErrNotFound = errors.New("object not found")
	// ErrBadKey means a key could reach outside the storage root. Keys are built from IDs this system generated,
	// so reaching this means something is badly wrong, and it must not be retried with a cleaned-up key.
	ErrBadKey = errors.New("invalid object key")
)

// Object is a stored file that can be read from any position, which is what serving Range requests takes.
type Object interface {
	io.ReadSeekCloser
	ModTime() time.Time
}

// Storage keeps recordings.
type Storage interface {
	// Kind names the backend, for the job's record.
	Kind() string
	// Put stores a local file under a key, and returns its size. It returns once the file is durable.
	Put(ctx context.Context, key, localPath, contentType string) (int64, error)
	// Open returns a stored file, or ErrNotFound.
	Open(ctx context.Context, key string) (Object, error)
	// Delete removes a stored file. A missing file is not an error, so a retry after a partial failure works.
	Delete(ctx context.Context, key string) error
	// Check reports whether files could be stored right now.
	Check(ctx context.Context) error
}

var keyPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// CheckKey accepts only keys made of plain path segments: nothing absolute, no "..", and no characters a
// filesystem or an S3 service would treat specially.
func CheckKey(key string) error {
	if key == "" || len(key) > 512 || path.Clean(key) != key {
		return ErrBadKey
	}
	for _, part := range strings.Split(key, "/") {
		if !keyPart.MatchString(part) || strings.Contains(part, "..") {
			return ErrBadKey
		}
	}
	return nil
}

// RecordingKey is where a job's file lives. Both parts come from the job's row, never from a request.
func RecordingKey(projectID, egressID, format string) (string, error) {
	key := projectID + "/" + egressID + "." + format
	if err := CheckKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// New creates the configured backend.
func New(cfg config.MediaStorage) (Storage, error) {
	switch cfg.Kind {
	case "", "local":
		dir := cfg.Dir
		if dir == "" {
			dir = "recordings"
		}
		return NewLocal(dir)
	case "s3":
		return NewS3(cfg.S3)
	}
	return nil, fmt.Errorf("unknown media storage kind %q", cfg.Kind)
}

// Local keeps files in a directory.
type Local struct{ root *os.Root }

// NewLocal creates the directory if needed. Every operation goes through os.Root, so even a symlink placed in the
// directory cannot lead outside it.
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Local{root: root}, nil
}

// Kind implements Storage.
func (*Local) Kind() string { return "local" }

// Put implements Storage. A temporary name and a rename keep a half-copied file from ever being served.
func (l *Local) Put(_ context.Context, key, localPath, _ string) (int64, error) {
	if err := CheckKey(key); err != nil {
		return 0, err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	if dir := path.Dir(key); dir != "." {
		if err := l.root.MkdirAll(dir, 0o750); err != nil {
			return 0, err
		}
	}
	tmp := key + ".part"
	dst, err := l.root.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	size, err := io.Copy(dst, src)
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = l.root.Rename(tmp, key)
	}
	if err != nil {
		_ = l.root.Remove(tmp)
		return 0, err
	}
	return size, nil
}

type localObject struct {
	*os.File
	modTime time.Time
}

func (o localObject) ModTime() time.Time { return o.modTime }

// Open implements Storage.
func (l *Local) Open(_ context.Context, key string) (Object, error) {
	if err := CheckKey(key); err != nil {
		return nil, err
	}
	f, err := l.root.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrNotFound
	}
	return localObject{f, info.ModTime()}, nil
}

// Delete implements Storage.
func (l *Local) Delete(_ context.Context, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	err := l.root.Remove(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Check implements Storage, by writing something.
func (l *Local) Check(context.Context) error {
	const probe = ".write-check"
	f, err := l.root.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("the recordings directory cannot be written: %w", err)
	}
	_ = f.Close()
	return l.root.Remove(probe)
}
