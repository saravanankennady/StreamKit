package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"elnova-streamkit.local/server/config"
)

// S3 keeps files in a bucket on any S3-compatible service.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3 connects, creating the bucket if it does not exist yet.
func NewS3(cfg config.S3) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3 storage needs an endpoint and a bucket")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3 bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("create s3 bucket %q: %w", cfg.Bucket, err)
		}
	}
	return &S3{client: client, bucket: cfg.Bucket, prefix: strings.Trim(cfg.Prefix, "/")}, nil
}

func (s *S3) object(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// Kind implements Storage.
func (*S3) Kind() string { return "s3" }

// Put implements Storage.
func (s *S3) Put(ctx context.Context, key, localPath, contentType string) (int64, error) {
	if err := CheckKey(key); err != nil {
		return 0, err
	}
	info, err := s.client.FPutObject(ctx, s.bucket, s.object(key), localPath, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

type s3Object struct {
	*minio.Object
	modTime time.Time
}

func (o s3Object) ModTime() time.Time { return o.modTime }

// Open implements Storage. Nothing is downloaded until read, and each seek becomes a ranged GET, so a player
// jumping around a long recording fetches only what it plays. ctx lasts as long as the object is read.
func (s *S3) Open(ctx context.Context, key string) (Object, error) {
	if err := CheckKey(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.object(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s3Object{obj, info.LastModified}, nil
}

// Delete implements Storage.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, s.object(key), minio.RemoveObjectOptions{})
}

// Check implements Storage. It looks at the bucket, rather than writing to it on every check.
func (s *S3) Check(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("the recordings bucket cannot be reached: %w", err)
	}
	if !exists {
		return fmt.Errorf("the recordings bucket %q does not exist", s.bucket)
	}
	return nil
}
