// Package storage stores uploaded files in MinIO (or any S3 compatible
// object store) and serves them under the /public/ URL prefix.
package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// PublicPrefix is the URL prefix under which stored objects are served.
const PublicPrefix = "/public/"

// Store is a thin wrapper around a MinIO bucket.
type Store struct {
	client *minio.Client
	bucket string
}

// Options configure the store.
type Options struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
	Region    string
}

// New connects to MinIO and makes sure the bucket exists.
func New(ctx context.Context, o Options) (*Store, error) {
	client, err := minio.New(o.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure: o.UseSSL,
		Region: o.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	s := &Store{client: client, bucket: o.Bucket}

	deadline := time.Now().Add(60 * time.Second)
	for {
		err = s.ensureBucket(ctx, o.Region)
		if err == nil {
			return s, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("prepare bucket %q: %w", o.Bucket, err)
		}
		slog.Warn("object storage not ready, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Store) ensureBucket(ctx context.Context, region string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	exists, err := s.client.BucketExists(cctx, s.bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.client.MakeBucket(cctx, s.bucket, minio.MakeBucketOptions{Region: region})
}

// Ping verifies connectivity.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.BucketExists(ctx, s.bucket)
	return err
}

// Saved describes a stored object.
type Saved struct {
	Key         string // object key inside the bucket
	URL         string // public URL path, e.g. /public/images/abc.png
	Name        string // original file name
	ContentType string
	Size        int64
}

// AllowedImageTypes lists the image MIME types accepted for uploads.
var AllowedImageTypes = map[string]bool{
	"image/jpg": true, "image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/svg+xml": true,
}

// SaveMultipart stores an uploaded file under folder/ with a random name.
func (s *Store) SaveMultipart(ctx context.Context, folder string, fh *multipart.FileHeader, allowed map[string]bool) (*Saved, error) {
	if fh == nil {
		return nil, errors.New("no file")
	}
	ct := detectContentType(fh)
	if allowed != nil && !allowed[ct] {
		return nil, &UnsupportedTypeError{ContentType: ct}
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.Save(ctx, folder, fh.Filename, ct, f, fh.Size)
}

// UnsupportedTypeError is returned for rejected content types.
type UnsupportedTypeError struct{ ContentType string }

func (e *UnsupportedTypeError) Error() string { return "unsupported file type " + e.ContentType }

func detectContentType(fh *multipart.FileHeader) string {
	ct := strings.ToLower(strings.TrimSpace(fh.Header.Get("Content-Type")))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" || ct == "application/octet-stream" {
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(fh.Filename))); byExt != "" {
			ct = byExt
			if i := strings.IndexByte(ct, ';'); i >= 0 {
				ct = ct[:i]
			}
		}
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ct
}

// Save writes r to folder/<random><ext>.
func (s *Store) Save(ctx context.Context, folder, originalName, contentType string, r io.Reader, size int64) (*Saved, error) {
	ext := strings.ToLower(filepath.Ext(originalName))
	if len(ext) > 10 || strings.ContainsAny(ext, `/\ `) {
		ext = ""
	}
	key := path.Join(cleanFolder(folder), randomName()+ext)
	if size <= 0 {
		size = -1
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return nil, fmt.Errorf("upload object: %w", err)
	}
	if size < 0 {
		if st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
			size = st.Size
		}
	}
	return &Saved{Key: key, URL: PublicPrefix + key, Name: originalName, ContentType: contentType, Size: size}, nil
}

func cleanFolder(folder string) string {
	folder = strings.Trim(path.Clean("/"+strings.ReplaceAll(folder, `\`, "/")), "/")
	if folder == "" || folder == "." {
		return "files"
	}
	return folder
}

func randomName() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102") + "-" + hex.EncodeToString(b)
}

// KeyFromURL converts a public URL (/public/x/y or public/x/y) to an object
// key. Returns "" for values that do not point into the store.
func KeyFromURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, PublicPrefix); i >= 0 {
		u = u[i+len(PublicPrefix):]
	} else if strings.HasPrefix(u, "public/") {
		u = strings.TrimPrefix(u, "public/")
	} else {
		return ""
	}
	if j := strings.IndexAny(u, "?#"); j >= 0 {
		u = u[:j]
	}
	u = strings.Trim(path.Clean("/"+u), "/")
	if u == "" || u == "." || strings.Contains(u, "..") {
		return ""
	}
	return u
}

// Delete removes an object; missing objects are ignored.
func (s *Store) Delete(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil
		}
	}
	return err
}

// DeleteURL removes the object referenced by a public URL (best effort).
func (s *Store) DeleteURL(ctx context.Context, u string) {
	if key := KeyFromURL(u); key != "" {
		if err := s.Delete(ctx, key); err != nil {
			slog.Warn("delete object", "key", key, "err", err)
		}
	}
}

// Exists reports whether an object exists.
func (s *Store) Exists(ctx context.Context, key string) bool {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	return err == nil
}

// Read returns the full content of an object.
func (s *Store) Read(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

// ServeHTTP streams objects below /public/. Range requests are supported so
// PDFs can be loaded progressively.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := KeyFromURL(r.URL.Path)
	if key == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, "unable to read object", http.StatusBadGateway)
		return
	}
	defer obj.Close()
	if st.ContentType != "" {
		w.Header().Set("Content-Type", st.ContentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if st.ETag != "" {
		w.Header().Set("ETag", strconv.Quote(st.ETag))
	}
	http.ServeContent(w, r, path.Base(key), st.LastModified, obj)
}
