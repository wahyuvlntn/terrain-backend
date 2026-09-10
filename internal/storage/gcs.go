package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"afdd/internal/gcpauth"
)

// gcsBackend is a Google Cloud Storage bucket through the JSON API, with the
// attached service account's access token (metadata server). An emulator
// endpoint (STORAGE_EMULATOR_HOST, e.g. fake-gcs-server) is used without
// credentials and its bucket is created when missing. Buckets in Google
// Cloud are provisioned beforehand: the runtime identities only need object
// permissions (roles/storage.objectUser / objectViewer).
type gcsBackend struct {
	base     string // https://storage.googleapis.com or the emulator
	bucket   string
	emulator bool
	http     *http.Client
}

func newGCS(endpoint, bucket string) *gcsBackend {
	g := &gcsBackend{base: "https://storage.googleapis.com", bucket: bucket, http: &http.Client{}}
	if e := strings.TrimRight(strings.TrimSpace(endpoint), "/"); e != "" {
		if !strings.Contains(e, "://") {
			e = "http://" + e
		}
		g.base, g.emulator = e, true
	}
	return g
}

func (g *gcsBackend) objectURL(key string) string {
	return g.base + "/storage/v1/b/" + url.PathEscape(g.bucket) + "/o/" + url.PathEscape(key)
}

func (g *gcsBackend) do(ctx context.Context, method, u string, body io.Reader, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if !g.emulator {
		tok, err := gcpauth.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return g.http.Do(req)
}

// gcsError reads a failed response.
func gcsError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	return fmt.Errorf("gcs: %s: %s", resp.Status, msg)
}

// ensure checks that the bucket can be listed (objects.list, which the
// object roles include; bucket metadata needs more). The emulator's bucket
// is created when missing.
func (g *gcsBackend) ensure(ctx context.Context) error {
	err := g.ping(ctx)
	if err == nil || !g.emulator {
		return err
	}
	body, _ := json.Marshal(map[string]string{"name": g.bucket})
	resp, err := g.do(ctx, http.MethodPost, g.base+"/storage/v1/b?project=local", bytes.NewReader(body),
		http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return gcsError(resp)
	}
	return g.ping(ctx)
}

func (g *gcsBackend) ping(ctx context.Context) error {
	resp, err := g.do(ctx, http.MethodGet, g.base+"/storage/v1/b/"+url.PathEscape(g.bucket)+"/o?maxResults=1&fields=kind", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return gcsError(resp)
	}
	return nil
}

type gcsObject struct {
	Size        string `json:"size"`
	ContentType string `json:"contentType"`
	Updated     string `json:"updated"`
	Etag        string `json:"etag"`
	Md5Hash     string `json:"md5Hash"`
}

func (o gcsObject) info() objectInfo {
	n, _ := strconv.ParseInt(o.Size, 10, 64)
	t, _ := time.Parse(time.RFC3339Nano, o.Updated)
	etag := o.Md5Hash
	if etag == "" {
		etag = o.Etag
	}
	return objectInfo{Size: n, ContentType: o.ContentType, ModTime: t, ETag: etag}
}

func (g *gcsBackend) put(ctx context.Context, key, contentType string, r io.Reader, size int64) (int64, error) {
	if size < 0 {
		// a single-request (media) upload needs the length
		buf, err := io.ReadAll(r)
		if err != nil {
			return 0, err
		}
		r, size = bytes.NewReader(buf), int64(len(buf))
	}
	u := g.base + "/upload/storage/v1/b/" + url.PathEscape(g.bucket) + "/o?uploadType=media&name=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	if !g.emulator {
		tok, err := gcpauth.AccessToken(ctx)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, gcsError(resp)
	}
	var o gcsObject
	if err := json.NewDecoder(resp.Body).Decode(&o); err == nil {
		if n := o.info().Size; n > 0 {
			size = n
		}
	}
	return size, nil
}

func (g *gcsBackend) stat(ctx context.Context, key string) (objectInfo, error) {
	resp, err := g.do(ctx, http.MethodGet, g.objectURL(key), nil, nil)
	if err != nil {
		return objectInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return objectInfo{}, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return objectInfo{}, gcsError(resp)
	}
	var o gcsObject
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return objectInfo{}, err
	}
	return o.info(), nil
}

func (g *gcsBackend) open(ctx context.Context, key string, info objectInfo) (io.ReadSeekCloser, error) {
	return &gcsReader{ctx: ctx, g: g, key: key, size: info.Size}, nil
}

func (g *gcsBackend) remove(ctx context.Context, key string) error {
	resp, err := g.do(ctx, http.MethodDelete, g.objectURL(key), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return gcsError(resp)
}

// gcsReader reads an object from the current position with a ranged GET, so
// http.ServeContent serves byte ranges (seeking only moves the position).
type gcsReader struct {
	ctx  context.Context
	g    *gcsBackend
	key  string
	size int64
	pos  int64
	body io.ReadCloser
}

func (r *gcsReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		hdr := http.Header{}
		if r.pos > 0 {
			hdr.Set("Range", "bytes="+strconv.FormatInt(r.pos, 10)+"-")
		}
		resp, err := r.g.do(r.ctx, http.MethodGet, r.g.objectURL(r.key)+"?alt=media", nil, hdr)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			defer resp.Body.Close()
			return 0, gcsError(resp)
		}
		if r.pos > 0 && resp.StatusCode == http.StatusOK {
			// no range support: skip to the position
			if _, err := io.CopyN(io.Discard, resp.Body, r.pos); err != nil {
				resp.Body.Close()
				return 0, err
			}
		}
		r.body = resp.Body
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *gcsReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("gcs: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("gcs: negative position")
	}
	if abs != r.pos && r.body != nil {
		r.body.Close()
		r.body = nil
	}
	r.pos = abs
	return abs, nil
}

func (r *gcsReader) Close() error {
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}
