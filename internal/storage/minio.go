package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// minioBackend is MinIO or any S3 compatible store (the local deployment).
type minioBackend struct {
	client *minio.Client
	bucket string
	region string
}

func newMinio(o Options) (*minioBackend, error) {
	client, err := minio.New(o.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure: o.UseSSL,
		Region: o.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	return &minioBackend{client: client, bucket: o.Bucket, region: o.Region}, nil
}

func (m *minioBackend) ensure(ctx context.Context) error {
	exists, err := m.client.BucketExists(ctx, m.bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return m.client.MakeBucket(ctx, m.bucket, minio.MakeBucketOptions{Region: m.region})
}

func (m *minioBackend) ping(ctx context.Context) error {
	_, err := m.client.BucketExists(ctx, m.bucket)
	return err
}

func (m *minioBackend) put(ctx context.Context, key, contentType string, r io.Reader, size int64) (int64, error) {
	if _, err := m.client.PutObject(ctx, m.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return 0, err
	}
	if size < 0 {
		if st, err := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{}); err == nil {
			size = st.Size
		}
	}
	return size, nil
}

func (m *minioBackend) stat(ctx context.Context, key string) (objectInfo, error) {
	st, err := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && (resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound) {
			return objectInfo{}, ErrNotFound
		}
		return objectInfo{}, err
	}
	return objectInfo{Size: st.Size, ContentType: st.ContentType, ModTime: st.LastModified, ETag: st.ETag}, nil
}

func (m *minioBackend) open(ctx context.Context, key string, _ objectInfo) (io.ReadSeekCloser, error) {
	return m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
}

func (m *minioBackend) remove(ctx context.Context, key string) error {
	err := m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil
		}
	}
	return err
}
