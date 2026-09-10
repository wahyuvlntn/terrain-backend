package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCS is the part of the GCS JSON API the backend uses.
type fakeGCS struct {
	mu      sync.Mutex
	buckets map[string]bool
	objects map[string][]byte // bucket/key
	types   map[string]string
	ranges  []string
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{buckets: map[string]bool{}, objects: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.EscapedPath()
	switch {
	case r.Method == http.MethodPost && p == "/storage/v1/b":
		var b struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.buckets[b.Name] = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/upload/storage/v1/b/"):
		bucket, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(p, "/upload/storage/v1/b/"), "/o"))
		name := r.URL.Query().Get("name")
		body, _ := io.ReadAll(r.Body)
		f.objects[bucket+"/"+name] = body
		f.types[bucket+"/"+name] = r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]string{"size": strconv.Itoa(len(body))})
	case strings.HasPrefix(p, "/storage/v1/b/"):
		rest := strings.TrimPrefix(p, "/storage/v1/b/")
		i := strings.Index(rest, "/o")
		bucket, _ := url.PathUnescape(rest[:i])
		if !f.buckets[bucket] {
			http.Error(w, `{"error":{"message":"no bucket"}}`, http.StatusNotFound)
			return
		}
		if rest[i:] == "/o" {
			_, _ = w.Write([]byte(`{"kind":"storage#objects"}`))
			return
		}
		key, _ := url.PathUnescape(strings.TrimPrefix(rest[i:], "/o/"))
		data, ok := f.objects[bucket+"/"+key]
		if !ok {
			http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodDelete:
			delete(f.objects, bucket+"/"+key)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Query().Get("alt") == "media":
			start := 0
			if rg := r.Header.Get("Range"); rg != "" {
				f.ranges = append(f.ranges, rg)
				start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(data[start:])
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"size": strconv.Itoa(len(data)), "contentType": f.types[bucket+"/"+key],
				"updated": "2026-10-14T01:02:03.000Z", "md5Hash": "abc"})
		}
	default:
		http.NotFound(w, r)
	}
}

// TestGCSStore: uploads, metadata, ranged reads under /public, deletes and a
// missing object against the JSON API (emulator mode: the bucket is created).
func TestGCSStore(t *testing.T) {
	fake := newFakeGCS()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	ctx := context.Background()
	s, err := New(ctx, Options{Provider: "gcs", Endpoint: srv.URL, Bucket: "dt-assets", Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("0123456789", 100)
	saved, err := s.Save(ctx, "images", "pump.png", "image/png", strings.NewReader(content), -1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Size != int64(len(content)) || !strings.HasPrefix(saved.URL, "/public/images/") || !strings.HasSuffix(saved.Key, ".png") {
		t.Fatalf("saved %+v", saved)
	}
	if !s.Exists(ctx, saved.Key) {
		t.Fatal("saved object missing")
	}
	b, err := s.Read(ctx, saved.Key)
	if err != nil || string(b) != content {
		t.Fatalf("read %d bytes, %v", len(b), err)
	}
	// a ranged request under /public
	req := httptest.NewRequest(http.MethodGet, saved.URL, nil)
	req.Header.Set("Range", "bytes=995-")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "56789" || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("range: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if len(fake.ranges) == 0 || fake.ranges[len(fake.ranges)-1] != "bytes=995-" {
		t.Errorf("ranges sent %v", fake.ranges)
	}
	if _, err := s.Put(ctx, "rag/2026/doc.pdf", "application/pdf", strings.NewReader("%PDF"), 4); err != nil {
		t.Fatal(err)
	}
	obj, err := s.Open(ctx, "rag/2026/doc.pdf")
	if err != nil || obj.ContentType != "application/pdf" || obj.Size != 4 {
		t.Fatalf("open %+v %v", obj, err)
	}
	obj.Close()
	s.DeleteURL(ctx, saved.URL)
	if s.Exists(ctx, saved.Key) {
		t.Fatal("deleted object still there")
	}
	if _, err := s.Open(ctx, saved.Key); err != ErrNotFound {
		t.Fatalf("missing object: %v", err)
	}
	if err := s.Delete(ctx, saved.Key); err != nil {
		t.Fatalf("deleting a missing object: %v", err)
	}
}
