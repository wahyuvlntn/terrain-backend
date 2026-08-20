package storage

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func testStore(t *testing.T, bucket string) *Store {
	t.Helper()
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(srv.Close)
	s, err := New(context.Background(), Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), AccessKey: "k", SecretKey: "s",
		Bucket: bucket, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutOpen(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, "afdd-docs")
	if s.Bucket() != "afdd-docs" {
		t.Fatalf("bucket %q", s.Bucket())
	}
	body := "%PDF-1.7\n" + strings.Repeat("x", 4096) + "\n%%EOF\n"
	saved, err := s.Put(ctx, "rag/2026/0b9c-test.pdf", "application/pdf", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Key != "rag/2026/0b9c-test.pdf" || saved.URL != "" || saved.Size != int64(len(body)) {
		t.Fatalf("saved %+v (a private object has no public URL)", saved)
	}

	obj, err := s.Open(ctx, saved.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	if obj.Size != int64(len(body)) || obj.ContentType != "application/pdf" || obj.ModTime.IsZero() {
		t.Fatalf("object %+v", obj)
	}
	// Seekable for range requests.
	if _, err := obj.Seek(int64(len(body))-6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(obj)
	if string(tail) != "%%EOF\n" {
		t.Fatalf("tail %q", tail)
	}

	if _, err := s.Open(ctx, "rag/2026/missing.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if err := s.Delete(ctx, saved.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, saved.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted object: %v", err)
	}
}

func TestPutRejectsUncleanKeys(t *testing.T) {
	s := testStore(t, "afdd-docs")
	for _, key := range []string{"", "/rag/a.pdf", "rag/../a.pdf", "rag//a.pdf", `rag\a.pdf`, "."} {
		if _, err := s.Put(context.Background(), key, "application/pdf", strings.NewReader("x"), 1); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
}
