package gcpauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "aud": "x"})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// TestTokens: access and ID tokens come from the metadata server with its
// flavor header and are cached until shortly before they expire; audiences
// are service root URLs.
func TestTokens(t *testing.T) {
	var accessCalls, idCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing flavor", http.StatusForbidden)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/service-accounts/default/token"):
			accessCalls.Add(1)
			fmt.Fprint(w, `{"access_token":"ya29.test","expires_in":3599,"token_type":"Bearer"}`)
		case strings.HasSuffix(r.URL.Path, "/service-accounts/default/identity"):
			idCalls.Add(1)
			if r.URL.Query().Get("audience") != "https://agent-abc-as.a.run.app" {
				http.Error(w, "bad audience", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, fakeJWT(time.Now().Add(time.Hour)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("GOOGLE_OAUTH_ACCESS_TOKEN", "")
	reset()
	ctx := context.Background()
	for range 3 {
		tok, err := AccessToken(ctx)
		if err != nil || tok != "ya29.test" {
			t.Fatalf("access token %q %v", tok, err)
		}
	}
	aud := Audience("https://agent-abc-as.a.run.app/chat?x=1")
	if aud != "https://agent-abc-as.a.run.app" {
		t.Fatalf("audience %q", aud)
	}
	for range 3 {
		tok, err := IDToken(ctx, aud)
		if err != nil || strings.Count(tok, ".") != 2 {
			t.Fatalf("id token %q %v", tok, err)
		}
	}
	if accessCalls.Load() != 1 || idCalls.Load() != 1 {
		t.Errorf("metadata calls: access %d, id %d (want cached)", accessCalls.Load(), idCalls.Load())
	}
	// a token close to expiry is renewed
	reset()
	mu.Lock()
	ids[aud] = cached{fakeJWT(time.Now().Add(time.Minute)), time.Now().Add(time.Minute)}
	mu.Unlock()
	if _, err := IDToken(ctx, aud); err != nil || idCalls.Load() != 2 {
		t.Errorf("renewal: %v, calls %d", err, idCalls.Load())
	}
	t.Setenv("GOOGLE_OAUTH_ACCESS_TOKEN", "static")
	if tok, _ := AccessToken(ctx); tok != "static" {
		t.Errorf("override %q", tok)
	}
}
