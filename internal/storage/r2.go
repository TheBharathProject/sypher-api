// Package storage wraps Cloudflare R2 (S3-compatible) for the file-upload
// flows. It issues presigned PUT/GET URLs so the browser uploads directly,
// avoiding bytes streaming through the Go process.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2 is the configured client. Construct once at startup and reuse.
type R2 struct {
	client    *s3.Client
	presigner *s3.PresignClient
	bucket    string
}

// Settings is the subset of config the R2 client cares about.
type Settings struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// New constructs an R2 client. Returns an error if any required setting is
// missing. Call this lazily (e.g. on first hit to a Phase-2 endpoint) so
// Phase-1-only deployments don't need R2 credentials.
func New(ctx context.Context, s Settings) (*R2, error) {
	if s.AccountID == "" || s.AccessKeyID == "" || s.SecretAccessKey == "" || s.Bucket == "" {
		return nil, errors.New("storage: missing R2 credentials (set R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET)")
	}
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", s.AccountID)

	cfg, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion("auto"),
		awsConfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s.AccessKeyID, s.SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("storage: load config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = true
	})
	return &R2{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    s.Bucket,
	}, nil
}

// PresignPut returns a URL the browser can PUT to. The TTL caps how long
// the URL is valid for; keep it short (a few minutes is standard).
func (r *R2) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	out, err := r.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      &r.bucket,
		Key:         &key,
		ContentType: &contentType,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return out.URL, nil
}

// PresignGet returns a URL the browser can GET to download the object.
func (r *R2) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	out, err := r.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &r.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return out.URL, nil
}

// Put uploads bytes directly from the server. Used for artifacts generated
// server-side (e.g. resume-builder compiled PDFs) where bouncing through a
// presigned-URL round-trip via the browser would be wasteful. Content-Type
// is required so range-fetches + inline-iframe viewing behave correctly.
func (r *R2) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &r.bucket,
		Key:         &key,
		ContentType: &contentType,
		Body:        bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// FetchAll downloads the object and returns its full body. Used by the
// resume-extract handler when it needs the raw bytes server-side.
func (r *R2) FetchAll(ctx context.Context, key string) ([]byte, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &r.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// Delete removes an object from the bucket.
func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &r.bucket,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}
