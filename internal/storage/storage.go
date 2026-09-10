// Package storage stores uploaded files in an object store - MinIO (or any
// S3 compatible store) or Google Cloud Storage - and serves them under the
// /public/ URL prefix.
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
)

// PublicPrefix is the URL prefix under which stored objects are served.
const PublicPrefix = "/public/"

// Store is a bucket of an object store.
type Store struct {
	b      backend
	bucket string
}

// backend is an object store provider.
type backend interface {
	// ensure makes the bucket usable (MinIO creates it; GCS checks it).
	ensure(ctx context.Context) error
	ping(ctx context.Context) error
	put(ctx context.Context, key, contentType string, r io.Reader, size int64) (int64, error)
	// stat returns ErrNotFound for a missing object.
	stat(ctx context.Context, key string) (objectInfo, error)
	open(ctx context.Context, key string, info objectInfo) (io.ReadSeekCloser, error)
	// remove ignores a missing object.
	remove(ctx context.Context, key string) error
}

type objectInfo struct {
	Size        int64
	ContentType string
	ModTime     time.Time
	ETag        string
}

// Options configure the store.
type Options struct {
	Provider  string // "minio" (default) or "gcs"
	Endpoint  string // MinIO endpoint; for GCS an emulator (STORAGE_EMULATOR_HOST), empty = Google
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
	Region    string
	// Wait is how long New retries an unready store (default 60 s).
	Wait time.Duration
}

// New opens the bucket, retrying while the store is not ready.
func New(ctx context.Context, o Options) (*Store, error) {
	var b backend
	var err error
	switch o.Provider {
	case "gcs":
		b = newGCS(o.Endpoint, o.Bucket)
	case "", "minio":
		if b, err = newMinio(o); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown storage provider %q", o.Provider)
	}
	s := &Store{b: b, bucket: o.Bucket}
	wait := o.Wait
	if wait <= 0 {
		wait = 60 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = b.ensure(cctx)
		cancel()
		if err == nil {
			return s, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("prepare bucket %q: %w", o.Bucket, err)
		}
		slog.Warn("object storage not ready, retrying", "bucket", o.Bucket, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Ping verifies connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.b.ping(ctx)
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
	size, err := s.b.put(ctx, key, contentType, r, size)
	if err != nil {
		return nil, fmt.Errorf("upload object: %w", err)
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
	return s.b.remove(ctx, key)
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
	_, err := s.b.stat(ctx, key)
	return err == nil
}

// ErrNotFound is returned by Open for a missing object.
var ErrNotFound = errors.New("object not found")

// Bucket returns the name of the store's bucket.
func (s *Store) Bucket() string { return s.bucket }

// Put stores r under an explicit object key (e.g. rag/2026/<uuid>.pdf in the
// private documents bucket). The key must be a clean relative path. URL is
// left empty: only the public store is served under /public/.
func (s *Store) Put(ctx context.Context, key, contentType string, r io.Reader, size int64) (*Saved, error) {
	clean := strings.Trim(path.Clean("/"+strings.ReplaceAll(key, `\`, "/")), "/")
	if clean == "" || clean == "." || clean != key {
		return nil, fmt.Errorf("invalid object key %q", key)
	}
	if size <= 0 {
		size = -1
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	size, err := s.b.put(ctx, key, contentType, r, size)
	if err != nil {
		return nil, fmt.Errorf("upload object: %w", err)
	}
	return &Saved{Key: key, Name: path.Base(key), ContentType: contentType, Size: size}, nil
}

// Object is an opened stored object: a seekable reader (for range requests)
// with its metadata. Close it after use.
type Object struct {
	io.ReadSeekCloser
	Size        int64
	ContentType string
	ModTime     time.Time
	ETag        string
}

// Open opens an object for streaming; ErrNotFound when it does not exist.
func (s *Store) Open(ctx context.Context, key string) (*Object, error) {
	st, err := s.b.stat(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat object: %w", err)
	}
	obj, err := s.b.open(ctx, key, st)
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	return &Object{ReadSeekCloser: obj, Size: st.Size, ContentType: st.ContentType, ModTime: st.ModTime, ETag: st.ETag}, nil
}

// Read returns the full content of an object.
func (s *Store) Read(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.Open(ctx, key)
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
	st, err := s.b.stat(ctx, key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	obj, err := s.b.open(ctx, key, st)
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
	http.ServeContent(w, r, path.Base(key), st.ModTime, obj)
}
