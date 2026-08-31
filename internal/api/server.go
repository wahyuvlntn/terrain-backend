// Package api implements the HTTP API consumed by the web UI.
//
// The UI was written against a NodeLinx style platform where business logic
// lives in named scripts:
//
//	POST /api/js-service/exposed/execute/{org}/{project}/{category}/{script}
//	     multipart form body, response {"data": ..., "error": ...}
//	POST /api/database-connection/exposed/execute/{org}/{project}/{db}/{category}/{query}
//	     response: JSON array of rows
//	POST /api/user-management/exposed/get-session-add-expiry
//
// Each script is registered here as a Go handler.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"afdd/internal/auth"
	"afdd/internal/broker"
	"afdd/internal/config"
	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/license"
	"afdd/internal/maint"
	"afdd/internal/notify"
	"afdd/internal/storage"
)

// Access levels for routes.
type Access int

const (
	// AccessUser requires any authenticated user.
	AccessUser Access = iota
	// AccessPublic needs no session.
	AccessPublic
	// AccessAdmin requires an admin or superuser.
	AccessAdmin
	// AccessSuper requires a superuser.
	AccessSuper
)

// HandlerFunc handles one script call.
type HandlerFunc func(c *Ctx) (any, error)

type route struct {
	fn     HandlerFunc
	access Access
}

// Deps groups the services used by the handlers.
type Deps struct {
	Config  *config.Config
	DB      *db.DB
	Store   *storage.Store
	Auth    *auth.Service
	Notify  *notify.Service
	Hub     *datahub.Hub
	Engine  *engine.Engine
	Maint   *maint.Service
	License *license.Manager
	Broker  *broker.Broker
	// DocsStore is the private bucket of the RAG documents (MINIO_DOCS_BUCKET).
	// It is never mounted under /public: PDFs are streamed by
	// /api/ai/documents/{id}/file after the session and permission checks.
	DocsStore *storage.Store
}

// Server is the HTTP API.
type Server struct {
	Deps
	cfg      *config.Config
	routes   map[string]route
	dbRoutes map[string]route
}

// New builds the API server and registers all routes.
func New(d Deps) *Server {
	s := &Server{Deps: d, cfg: d.Config, routes: map[string]route{}, dbRoutes: map[string]route{}}
	s.registerAll()
	return s
}

func (s *Server) handle(path string, access Access, fn HandlerFunc) {
	key := strings.ToLower(strings.Trim(path, "/"))
	if _, dup := s.routes[key]; dup {
		panic("duplicate route " + key)
	}
	s.routes[key] = route{fn: fn, access: access}
}

func (s *Server) handleDB(path string, access Access, fn HandlerFunc) {
	key := strings.ToLower(strings.Trim(path, "/"))
	s.dbRoutes[key] = route{fn: fn, access: access}
}

// Routes returns the registered script paths (used by tests).
func (s *Server) Routes() []string {
	out := make([]string, 0, len(s.routes))
	for k := range s.routes {
		out = append(out, k)
	}
	return out
}

// DBRoutes returns the registered database query paths (used by tests).
func (s *Server) DBRoutes() []string {
	out := make([]string, 0, len(s.dbRoutes))
	for k := range s.dbRoutes {
		out = append(out, k)
	}
	return out
}

// Ctx is the per request context passed to handlers.
type Ctx struct {
	context.Context
	R    *http.Request
	W    http.ResponseWriter
	F    *httpx.Form
	User *auth.User
	s    *Server

	scope    *auth.Scope
	scopeErr error
	scoped   bool
}

// Scope returns the location scope of the current user (cached per request).
func (c *Ctx) Scope() (*auth.Scope, error) {
	if !c.scoped {
		c.scope, c.scopeErr = c.s.Auth.ScopeFor(c, c.User)
		c.scoped = true
	}
	return c.scope, c.scopeErr
}

// Require returns a 403 error unless the user holds the permission.
func (c *Ctx) Require(group, alias string) error {
	if c.User.Can(group, alias) {
		return nil
	}
	return httpx.Forbidden("")
}

// OriginHeader marks the backend calls the AI agent makes with a user's
// session ("X-AFDD-Origin: agent"). Such calls carry the user's permissions
// and scope, but actions that need a person are refused (RequireHuman).
const OriginHeader = "X-AFDD-Origin"

// RequireHuman returns a 403 error when the request comes from the AI agent
// (X-AFDD-Origin present, whatever its value): plant writes, approvals and
// configuration changes must be made by a person in the application.
func (c *Ctx) RequireHuman() error {
	if c.fromAgent() {
		return httpx.Forbidden("This action needs a person: approve it in the application")
	}
	return nil
}

// fromAgent reports whether the request was made by the AI agent.
func (c *Ctx) fromAgent() bool {
	return c.R != nil && len(c.R.Header.Values(OriginHeader)) > 0
}

// Location returns the client's time zone derived from timezone_offset
// (minutes, as returned by Date#getTimezoneOffset) or the server default.
func (c *Ctx) Location() *time.Location {
	for _, key := range []string{"timezone_offset", "timezoneOffset"} {
		if off := c.F.IntPtr(key); off != nil && *off > -1000 && *off < 1000 {
			return time.FixedZone("client", int(-*off)*60)
		}
	}
	return c.s.defaultLocation()
}

func (s *Server) defaultLocation() *time.Location {
	if loc, err := time.LoadLocation(s.cfg.DefaultTimezone); err == nil {
		return loc
	}
	return time.UTC
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/js-service/exposed/execute/", s.serveScript)
	mux.HandleFunc("/api/database-connection/exposed/execute/", s.serveDBQuery)
	mux.HandleFunc("/api/user-management/exposed/get-session-add-expiry", s.serveLogin)
	mux.HandleFunc("/api/user-management/exposed/logout", s.serveLogout)
	mux.HandleFunc("/api/datahub/rest/", s.serveRestIngest)
	mux.HandleFunc("/healthz", s.serveHealth)
	// AI assistant (SSE reverse proxy to the agent service) and the RAG
	// document originals (private bucket, session and permission checked).
	mux.HandleFunc(agentPrefix+"/", s.serveAgentProxy)
	mux.HandleFunc("/api/ai/documents/{id}/file", s.serveDocumentFile)
	if s.Store != nil {
		mux.Handle(storage.PublicPrefix, s.Store)
	}
	if s.cfg.StaticDir != "" {
		mux.Handle("/", spaHandler(s.cfg.StaticDir, s.cfg.StaticMobileDir))
	}
	return s.recoverer(s.cors(mux))
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic in handler", "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				httpx.WriteError(w, r, httpx.Internal(errors.New("panic")))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+auth.SessionHeader+", Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.cfg.CORSOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status := map[string]any{"status": "ok"}
	code := http.StatusOK
	if err := s.DB.Ping(ctx); err != nil {
		status["database"] = err.Error()
		status["status"] = "degraded"
		code = http.StatusServiceUnavailable
	}
	if s.Store != nil {
		if err := s.Store.Ping(ctx); err != nil {
			status["storage"] = err.Error()
			status["status"] = "degraded"
			code = http.StatusServiceUnavailable
		}
	}
	httpx.WriteJSON(w, code, status)
}

// checkOrgProject validates the {org}/{project} prefix when configured.
func (s *Server) checkOrgProject(org, prj string) bool {
	if !s.cfg.StrictProjectPath {
		return true
	}
	if s.cfg.OrgAlias != "" && !strings.EqualFold(org, s.cfg.OrgAlias) {
		return false
	}
	if s.cfg.ProjectAlias != "" && !strings.EqualFold(prj, s.cfg.ProjectAlias) {
		return false
	}
	return true
}

func (s *Server) serveScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		httpx.WriteError(w, r, &httpx.Error{Status: http.StatusMethodNotAllowed, Msg: "Method not allowed"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/js-service/exposed/execute/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || !s.checkOrgProject(parts[0], parts[1]) {
		httpx.WriteError(w, r, httpx.NotFound("Script not found"))
		return
	}
	rt, ok := s.routes[strings.ToLower(strings.Trim(parts[2], "/"))]
	if !ok {
		httpx.WriteError(w, r, httpx.NotFound("Script not found"))
		return
	}
	data, err := s.run(w, r, rt)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteEnvelope(w, data)
}

func (s *Server) serveDBQuery(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/database-connection/exposed/execute/")
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 || !s.checkOrgProject(parts[0], parts[1]) {
		httpx.WriteError(w, r, httpx.NotFound("Query not found"))
		return
	}
	// parts[2] is the database alias (business / machine); queries are unique
	// across databases so it is not part of the lookup key.
	rt, ok := s.dbRoutes[strings.ToLower(strings.Trim(parts[3], "/"))]
	if !ok {
		httpx.WriteError(w, r, httpx.NotFound("Query not found"))
		return
	}
	data, err := s.run(w, r, rt)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, data)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request, rt route) (any, error) {
	ctx := r.Context()
	var user *auth.User
	if rt.access != AccessPublic {
		u, err := s.Auth.Authenticate(ctx, r.Header.Get(auth.SessionHeader))
		if err != nil {
			return nil, httpx.Internal(err)
		}
		if u == nil {
			return nil, httpx.Unauthorized()
		}
		switch rt.access {
		case AccessAdmin:
			if !u.IsAdmin() {
				return nil, httpx.Forbidden("")
			}
		case AccessSuper:
			if !u.IsSuperuser() {
				return nil, httpx.Forbidden("")
			}
		}
		user = u
	}
	form, err := httpx.ParseRequest(r)
	if err != nil {
		return nil, httpx.BadRequest(err.Error())
	}
	c := &Ctx{Context: ctx, R: r, W: w, F: form, User: user, s: s}
	data, err := rt.fn(c)
	if err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			return nil, he
		}
		if errors.Is(err, context.Canceled) {
			return nil, &httpx.Error{Status: 499, Msg: "Request cancelled"}
		}
		return nil, httpx.Internal(err)
	}
	return data, nil
}

func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteError(w, r, &httpx.Error{Status: http.StatusMethodNotAllowed, Msg: "Method not allowed"})
		return
	}
	form, err := httpx.ParseRequest(r)
	if err != nil {
		httpx.WriteError(w, r, httpx.BadRequest(err.Error()))
		return
	}
	username := form.StrTrim("username")
	password := form.Str("password")
	if username == "" || password == "" {
		httpx.WriteError(w, r, httpx.BadRequest("err_invalid_username_password"))
		return
	}
	token, err := s.Auth.Login(r.Context(), username, password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			httpx.WriteError(w, r, httpx.BadRequest("err_invalid_username_password"))
			return
		}
		httpx.WriteError(w, r, httpx.Internal(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"nodelinx_exposed_session_id": token})
}

func (s *Server) serveLogout(w http.ResponseWriter, r *http.Request) {
	if tok := r.Header.Get(auth.SessionHeader); tok != "" {
		s.Auth.Logout(r.Context(), tok)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": "ok", "error": nil})
}

// serveRestIngest receives data pushed to REST_SERVER gateways:
// POST /api/datahub/rest/{gateway_alias} with a JSON body, authenticated with
// the gateway's API key (header X-Api-Key or ?api_key=).
func (s *Server) serveRestIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		httpx.WriteError(w, r, &httpx.Error{Status: http.StatusMethodNotAllowed, Msg: "Method not allowed"})
		return
	}
	alias := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/datahub/rest/"), "/")
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	n, err := s.Hub.IngestREST(r.Context(), alias, key, r.Body)
	if err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			httpx.WriteError(w, r, he)
			return
		}
		httpx.WriteError(w, r, httpx.BadRequest(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"accepted": n})
}
