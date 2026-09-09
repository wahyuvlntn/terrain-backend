// Package gcpauth fetches Google credentials of the attached service account
// from the metadata server (Cloud Run, GCE): OAuth access tokens for Google
// APIs and ID tokens for calling private Cloud Run services. Tokens are
// cached until shortly before they expire.
package gcpauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// metadataHost can be overridden with GCE_METADATA_HOST (tests, emulators).
func metadataBase() string {
	if h := strings.TrimSpace(os.Getenv("GCE_METADATA_HOST")); h != "" {
		return "http://" + h
	}
	return "http://metadata.google.internal"
}

var client = &http.Client{Timeout: 10 * time.Second}

type cached struct {
	token   string
	expires time.Time
}

var (
	mu     sync.Mutex
	access cached
	ids    = map[string]cached{}
)

// refreshBefore renews a token this long before it expires.
const refreshBefore = 5 * time.Minute

func metadataGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase()+"/computeMetadata/v1/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metadata server: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata server: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// AccessToken is an OAuth access token of the attached service account
// (GOOGLE_OAUTH_ACCESS_TOKEN overrides it, e.g. for a local test against GCS).
func AccessToken(ctx context.Context) (string, error) {
	if t := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN")); t != "" {
		return t, nil
	}
	mu.Lock()
	c := access
	mu.Unlock()
	if c.token != "" && time.Until(c.expires) > refreshBefore {
		return c.token, nil
	}
	body, err := metadataGet(ctx, "instance/service-accounts/default/token")
	if err != nil {
		return "", err
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return "", errors.New("metadata server: no access token")
	}
	mu.Lock()
	access = cached{r.AccessToken, time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)}
	mu.Unlock()
	return r.AccessToken, nil
}

// IDToken is a Google-signed ID token for the audience (a Cloud Run
// service's root URL), as Cloud Run expects from an authorized caller.
func IDToken(ctx context.Context, audience string) (string, error) {
	mu.Lock()
	c := ids[audience]
	mu.Unlock()
	if c.token != "" && time.Until(c.expires) > refreshBefore {
		return c.token, nil
	}
	body, err := metadataGet(ctx, "instance/service-accounts/default/identity?format=full&audience="+url.QueryEscape(audience))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(body))
	exp := jwtExpiry(tok)
	if tok == "" || exp.IsZero() {
		return "", errors.New("metadata server: no ID token")
	}
	mu.Lock()
	ids[audience] = cached{tok, exp}
	mu.Unlock()
	return tok, nil
}

// Audience is the root URL (scheme://host) of a service URL.
func Audience(serviceURL string) string {
	u, err := url.Parse(strings.TrimSpace(serviceURL))
	if err != nil || u.Host == "" {
		return strings.TrimRight(serviceURL, "/")
	}
	return u.Scheme + "://" + u.Host
}

// jwtExpiry reads the exp claim of a JWT (zero when unreadable).
func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// reset clears the caches (tests).
func reset() {
	mu.Lock()
	access = cached{}
	ids = map[string]cached{}
	mu.Unlock()
}
