package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"afdd/internal/auth"
	"afdd/internal/gcpauth"
	"afdd/internal/httpx"
)

// AI assistant proxy (DESIGN §4.6): /api/agent/* is reverse-proxied to the
// agent service (AGENT_URL) with the prefix stripped. The browser only
// authenticates with its session; the proxy adds the shared secret and the
// user's identity, permissions and session token, so the agent calls the
// backend as that user (with X-AFDD-Origin: agent). Any X-AFDD-* / X-Agent-*
// header sent by the client is dropped first. Responses (SSE chat streams)
// are flushed as they arrive and are not cut by the server's write timeout.

const (
	agentPrefix  = "/api/agent"
	agentMaxBody = 1 << 20 // chat requests are small JSON documents

	agentSecretHeader = "X-Agent-Secret"
	// serverlessAuthHeader carries the Google ID token Cloud Run checks
	// (Authorization stays free for the application's own credentials).
	serverlessAuthHeader   = "X-Serverless-Authorization"
	agentUserIDHeader      = "X-AFDD-User-Id"
	agentUsernameHeader    = "X-AFDD-Username"
	agentUserNameHeader    = "X-AFDD-User-Name"
	agentSessionHeader     = "X-AFDD-Session"
	agentPermissionsHeader = "X-AFDD-Permissions"
)

// agentIdentity is what the agent learns about the calling user.
type agentIdentity struct {
	UserID   int64
	Username string
	FullName string
	Token    string
	// Permissions are "group.alias" pairs; admins and superusers send "admin".
	Permissions []string
}

var (
	agentTransportOnce sync.Once
	agentTransport     *http.Transport
	agentConfigWarning sync.Once
)

// agentHTTPTransport is shared by all proxied requests (keep-alive pool).
func agentHTTPTransport() *http.Transport {
	agentTransportOnce.Do(func() {
		agentTransport = &http.Transport{
			Proxy:                 nil, // internal service, never through an HTTP proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			ExpectContinueTimeout: time.Second,
		}
	})
	return agentTransport
}

// serveAgentProxy authenticates the session and proxies to the agent.
func (s *Server) serveAgentProxy(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.Header.Get(auth.SessionHeader))
	u, err := s.Auth.Authenticate(r.Context(), token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal(err))
		return
	}
	if u == nil {
		httpx.WriteError(w, r, httpx.Unauthorized())
		return
	}
	var perms []string
	if u.IsAdmin() {
		perms = []string{"admin"}
	} else {
		pairs, err := s.Auth.PermissionList(r.Context(), u)
		if err != nil {
			httpx.WriteError(w, r, httpx.Internal(err))
			return
		}
		for _, p := range pairs {
			perms = append(perms, p.GroupAlias+"."+p.Alias)
		}
	}
	s.proxyAgent(w, r, u, agentIdentity{UserID: u.ID, Username: u.Username, FullName: u.FullName, Token: token, Permissions: perms})
}

// proxyAgent checks the permission and forwards the request to the agent.
func (s *Server) proxyAgent(w http.ResponseWriter, r *http.Request, u *auth.User, id agentIdentity) {
	if !u.Can("aiassistant", "read") {
		httpx.WriteError(w, r, httpx.Forbidden(""))
		return
	}
	if strings.TrimSpace(s.cfg.AgentURL) == "" {
		writeAgentUnavailable(w) // assistant disabled (AGENT_URL empty)
		return
	}
	target, err := url.Parse(strings.TrimSpace(s.cfg.AgentURL))
	if err != nil || target.Scheme == "" || target.Host == "" || s.cfg.AgentSecret == "" {
		agentConfigWarning.Do(func() {
			slog.Error("AI assistant proxy is misconfigured (AGENT_URL / AGENT_SHARED_SECRET)", "url", s.cfg.AgentURL, "err", err)
		})
		writeAgentUnavailable(w)
		return
	}
	// Chat answers stream for longer than the server's write timeout.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Debug("agent proxy: lift write deadline", "err", err)
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, agentMaxBody)
	}
	// A private Cloud Run agent admits callers with an ID token of an invoker.
	var idToken string
	if s.cfg.AgentIDToken {
		aud := s.cfg.AgentIDTokenAudience
		if aud == "" {
			aud = gcpauth.Audience(target.String())
		}
		if idToken, err = gcpauth.IDToken(r.Context(), aud); err != nil {
			slog.Error("AI assistant: no ID token for the agent service", "audience", aud, "err", err)
			writeAgentUnavailable(w)
			return
		}
	}
	newAgentProxy(target, s.cfg.AgentSecret, id, idToken).ServeHTTP(w, r)
}

// newAgentProxy builds the reverse proxy of one request.
func newAgentProxy(target *url.URL, secret string, id agentIdentity, idToken string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			p := strings.TrimPrefix(pr.In.URL.Path, agentPrefix)
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			pr.Out.URL.Path = p
			pr.Out.URL.RawPath = ""
			if raw := pr.In.URL.RawPath; raw != "" {
				if rp := strings.TrimPrefix(raw, agentPrefix); strings.HasPrefix(rp, "/") {
					pr.Out.URL.RawPath = rp
				}
			}
			pr.SetURL(target)
			pr.SetXForwarded()
			setAgentHeaders(pr.Out.Header, secret, id)
			if idToken != "" {
				pr.Out.Header.Set(serverlessAuthHeader, "Bearer "+idToken)
			}
		},
		Transport:     agentHTTPTransport(),
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				// The agent only answers 401 when the proxy's own credentials are
				// wrong (shared secret, identity headers). Passing it on would make
				// the browser treat it as an expired session and sign the user out.
				slog.Warn("AI assistant rejected the proxy credentials (check AGENT_SHARED_SECRET)", "path", resp.Request.URL.Path)
				resp.Body.Close()
				const msg = "LEVO is not configured correctly"
				body, _ := json.Marshal(map[string]any{"data": nil, "error": msg, "message": msg})
				resp.StatusCode = http.StatusBadGateway
				resp.Status = "502 Bad Gateway"
				resp.Header = http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(body))}}
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				return nil
			}
			if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				// Reverse proxies in front of the backend (nginx) must not buffer the stream.
				resp.Header.Set("X-Accel-Buffering", "no")
				resp.Header.Set("Cache-Control", "no-cache")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				return // the browser went away
			}
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				httpx.WriteError(w, r, &httpx.Error{Status: http.StatusRequestEntityTooLarge, Msg: "The request is too large"})
				return
			}
			slog.Warn("AI assistant unreachable", "path", r.URL.Path, "err", err)
			writeAgentUnavailable(w)
		},
	}
}

// setAgentHeaders replaces every client-supplied X-AFDD-* / X-Agent-* header
// (and the browser's credentials) with the proxy's own identity headers.
func setAgentHeaders(h http.Header, secret string, id agentIdentity) {
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-afdd-") || strings.HasPrefix(lk, "x-agent-") {
			h.Del(k)
		}
	}
	h.Del(auth.SessionHeader)
	h.Del("Authorization")
	h.Del(serverlessAuthHeader)
	h.Del("Cookie")
	perms := append([]string(nil), id.Permissions...)
	sort.Strings(perms)
	h.Set(agentSecretHeader, secret)
	h.Set(agentUserIDHeader, strconv.FormatInt(id.UserID, 10))
	h.Set(agentUsernameHeader, headerSafe(id.Username))
	h.Set(agentUserNameHeader, headerSafe(id.FullName))
	h.Set(agentSessionHeader, id.Token)
	h.Set(agentPermissionsHeader, strings.Join(perms, ","))
}

// headerSafe drops control characters (invalid in header values); other
// characters are sent as UTF-8 bytes.
func headerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
}

// writeAgentUnavailable answers 503 in the error envelope of the API (logged
// by the caller where useful, not per request).
func writeAgentUnavailable(w http.ResponseWriter) {
	const msg = "LEVO is not available right now"
	httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"data": nil, "error": msg, "message": msg})
}
