package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is the server-side, endpoint-agnostic object-store configuration for
// the archive tier (SEA-1667 T4, slice B). It mirrors the COMPASS_DATABASE_DSN
// flag/env precedence (cmd/compass-server/main.go): the server holds the
// endpoint/bucket/credentials, the agent and Runner hold none (DL-089). It is
// S3-compatible and endpoint-agnostic — the same fields drive Garage, R2,
// MinIO, or AWS S3 (endpoint + region + static credentials).
type S3Config struct {
	// Endpoint is the S3-compatible host[:port] (no scheme), e.g.
	// "s3.amazonaws.com", "garage.internal:3900", or an R2 account endpoint.
	Endpoint string
	// Bucket is the bucket archive segments are written under.
	Bucket string
	// AccessKey / SecretKey are the static credentials.
	AccessKey string
	SecretKey string
	// Region is the S3 region (e.g. "us-east-1", or "garage" for Garage).
	Region string
	// UseTLS selects https (true) vs http (false) to the endpoint.
	UseTLS bool
}

// present reports whether enough config is set to construct a client. An
// endpoint AND a bucket are the minimum: with neither the server boots without
// an archive tier (the store's nil object-store guard fails a flush loudly only
// if one is ever attempted).
func (c S3Config) present() bool {
	return c.Endpoint != "" && c.Bucket != ""
}

// S3ObjectStore is the real minio-go-backed ObjectStore implementation. It is
// S3-compatible and endpoint-agnostic (Garage/R2/MinIO/AWS), holding the single
// bucket every archive segment is keyed under. The store passes the full object
// key (segmentKey: sessions/<id>/<min>-<max>.jsonl) through unchanged.
type S3ObjectStore struct {
	client *minio.Client
	bucket string
}

// compile-time assertion that S3ObjectStore satisfies the store seam.
var _ ObjectStore = (*S3ObjectStore)(nil)

// NewS3ObjectStore constructs a minio-go client from cfg. It does NOT contact
// the endpoint (minio.New is lazy — the first PUT/GET is the first network I/O),
// so a bad endpoint fails a flush loudly rather than blocking startup.
func NewS3ObjectStore(cfg S3Config) (*S3ObjectStore, error) {
	if !cfg.present() {
		return nil, errors.New("s3 object store: endpoint and bucket are required")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 object store: constructing client for %q: %w", cfg.Endpoint, err)
	}
	return &S3ObjectStore{client: client, bucket: cfg.Bucket}, nil
}

// PutSegment writes body under key in the configured bucket. The body is the
// verbatim JSONL segment the store hands over; it is stored octet-for-octet.
func (s *S3ObjectStore) PutSegment(ctx context.Context, key string, body []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/x-ndjson"})
	if err != nil {
		return fmt.Errorf("s3 object store: putting %q: %w", key, err)
	}
	return nil
}

// GetSegment reads the object at key from the configured bucket, returning its
// verbatim body.
func (s *S3ObjectStore) GetSegment(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 object store: getting %q: %w", key, err)
	}
	// Deferred close of a read-only object handle: a close error after the body
	// is already read is not actionable, so the discard is explicit and intentional.
	defer func() { _ = obj.Close() }()
	body, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("s3 object store: reading %q: %w", key, err)
	}
	return body, nil
}
