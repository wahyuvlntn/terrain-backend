// Package auth implements users, sessions, permissions and location scoping.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"afdd/internal/db"
)

// SessionHeader carries the session token on every request.
const SessionHeader = "X-AFDD-Session-Id"

// Roles.
const (
	RoleSuperuser = "superuser"
	RoleAdmin     = "admin"
	RoleBasic     = "basic"
)

// User is the authenticated principal of a request.
type User struct {
	ID          int64
	Username    string
	FullName    string
	Email       string
	EmployeeID  *string
	Photo       *string
	Role        string
	UserGroupID *int64
	// permissions[group][alias] = granted (only populated for basic users)
	permissions map[string]map[string]bool
}

// IsSuperuser reports whether the user is a superuser.
func (u *User) IsSuperuser() bool { return u != nil && u.Role == RoleSuperuser }

// IsAdmin reports whether the user is an admin or superuser.
func (u *User) IsAdmin() bool {
	return u != nil && (u.Role == RoleAdmin || u.Role == RoleSuperuser)
}

// Can reports whether the user holds a permission. Admins and superusers hold
// every permission, mirroring the web UI.
func (u *User) Can(group, alias string) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin() {
		return true
	}
	return u.permissions[group][alias]
}

// HasExplicit reports whether the permission is explicitly granted (ignores
// role based bypass).
func (u *User) HasExplicit(group, alias string) bool {
	return u != nil && u.permissions[group][alias]
}

// Service manages sessions and users.
type Service struct {
	db  *db.DB
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]cachedSession
}

type cachedSession struct {
	user      *User
	expiresAt time.Time
	fetchedAt time.Time
}

// NewService creates the auth service.
func NewService(d *db.DB, ttl time.Duration) *Service {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &Service{db: d, ttl: ttl, cache: map[string]cachedSession{}}
}

// ErrInvalidCredentials is returned for a failed login.
var ErrInvalidCredentials = errors.New("invalid credentials")

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// HashPassword hashes a password with bcrypt.
func HashPassword(pw string) (string, error) {
	if len(pw) > 72 {
		return "", errors.New("password too long")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword compares a bcrypt hash with a password.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

var specialRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// ValidatePasswordPolicy enforces the same policy as the profile page and
// returns the translation key of the first violated rule.
func ValidatePasswordPolicy(pw string) string {
	switch {
	case len(pw) < 8:
		return "err_password_minimum_length"
	case len(pw) > 72:
		return "err_password_maximum_length"
	case !strings.ContainsFunc(pw, unicode.IsUpper):
		return "err_password_contain_one_uppercase"
	case !strings.ContainsFunc(pw, unicode.IsLower):
		return "err_password_contain_one_lowercase"
	case !strings.ContainsFunc(pw, unicode.IsDigit):
		return "err_password_contain_one_numeric"
	case !specialRe.MatchString(pw):
		return "err_password_contain_one_special_character"
	}
	return ""
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Login validates credentials and creates a session.
func (s *Service) Login(ctx context.Context, username, password string) (string, error) {
	username = strings.TrimSpace(username)
	var id int64
	var hash string
	err := s.db.QueryRow(ctx, `SELECT id, password_hash FROM core.users
		WHERE lower(username) = lower($1) AND NOT is_deleted`, username).Scan(&id, &hash)
	if err != nil {
		if db.IsNoRows(err) {
			// Spend comparable time to avoid user enumeration by timing.
			dummyOnce.Do(func() { dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost) })
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			return "", ErrInvalidCredentials
		}
		return "", err
	}
	if !CheckPassword(hash, password) {
		return "", ErrInvalidCredentials
	}
	token := newToken()
	_, err = s.db.Exec(ctx, `INSERT INTO core.sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`,
		token, id, time.Now().Add(s.ttl))
	if err != nil {
		return "", err
	}
	return token, nil
}

// Logout deletes a session.
func (s *Service) Logout(ctx context.Context, token string) {
	s.mu.Lock()
	delete(s.cache, token)
	s.mu.Unlock()
	_, _ = s.db.Exec(ctx, `DELETE FROM core.sessions WHERE token = $1`, token)
}

// InvalidateUser drops cached sessions of a user (after permission or
// profile changes) so they are reloaded on the next request.
func (s *Service) InvalidateUser(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.cache {
		if v.user.ID == userID {
			delete(s.cache, k)
		}
	}
}

// InvalidateAll clears the session cache (after group permission changes).
func (s *Service) InvalidateAll() {
	s.mu.Lock()
	s.cache = map[string]cachedSession{}
	s.mu.Unlock()
}

// DeleteUserSessions logs a user out everywhere.
func (s *Service) DeleteUserSessions(ctx context.Context, userID int64) {
	s.InvalidateUser(userID)
	_, _ = s.db.Exec(ctx, `DELETE FROM core.sessions WHERE user_id = $1`, userID)
}

const cacheFreshness = 30 * time.Second

// Authenticate resolves the session token to a user. Sessions slide: every
// use (throttled) extends the expiry by the configured TTL.
func (s *Service) Authenticate(ctx context.Context, token string) (*User, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 200 {
		return nil, nil
	}
	now := time.Now()
	s.mu.Lock()
	if c, ok := s.cache[token]; ok && now.Sub(c.fetchedAt) < cacheFreshness && now.Before(c.expiresAt) {
		s.mu.Unlock()
		return c.user, nil
	}
	s.mu.Unlock()

	var userID int64
	var expires time.Time
	err := s.db.QueryRow(ctx, `SELECT user_id, expires_at FROM core.sessions WHERE token = $1`, token).Scan(&userID, &expires)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	if now.After(expires) {
		_, _ = s.db.Exec(ctx, `DELETE FROM core.sessions WHERE token = $1`, token)
		return nil, nil
	}
	u, err := s.LoadUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}
	newExpiry := now.Add(s.ttl)
	_, _ = s.db.Exec(ctx, `UPDATE core.sessions SET last_seen_at = now(), expires_at = $2 WHERE token = $1`, token, newExpiry)

	s.mu.Lock()
	if len(s.cache) > 10000 {
		s.cache = map[string]cachedSession{}
	}
	s.cache[token] = cachedSession{user: u, expiresAt: newExpiry, fetchedAt: now}
	s.mu.Unlock()
	return u, nil
}

// LoadUser loads an active user with its permissions.
func (s *Service) LoadUser(ctx context.Context, userID int64) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(ctx, `SELECT id, username, full_name, email, employee_id, photo, role, user_group_id
		FROM core.users WHERE id = $1 AND NOT is_deleted`, userID).
		Scan(&u.ID, &u.Username, &u.FullName, &u.Email, &u.EmployeeID, &u.Photo, &u.Role, &u.UserGroupID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	u.permissions = map[string]map[string]bool{}
	if u.UserGroupID != nil {
		rows, err := s.db.Query(ctx, `SELECT g.alias, p.alias FROM core.user_group_permissions ugp
			JOIN core.permissions p ON p.id = ugp.permission_id
			JOIN core.permission_groups g ON g.id = p.permission_group_id
			WHERE ugp.user_group_id = $1 AND ugp.value`, *u.UserGroupID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var g, a string
			if err := rows.Scan(&g, &a); err != nil {
				return nil, err
			}
			if u.permissions[g] == nil {
				u.permissions[g] = map[string]bool{}
			}
			u.permissions[g][a] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// PermissionPair is a granted (alias, group alias) pair.
type PermissionPair struct {
	Alias      string `json:"alias"`
	GroupAlias string `json:"group_alias"`
}

// PermissionList returns the permissions reported to the web UI. Admins and
// superusers receive every permission so client-side notification filters
// treat them as fully authorised.
func (s *Service) PermissionList(ctx context.Context, u *User) ([]PermissionPair, error) {
	out := []PermissionPair{}
	if u.IsAdmin() {
		rows, err := s.db.Query(ctx, `SELECT p.alias, g.alias FROM core.permissions p
			JOIN core.permission_groups g ON g.id = p.permission_group_id ORDER BY g.sort_order, p.id`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var p PermissionPair
			if err := rows.Scan(&p.Alias, &p.GroupAlias); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	}
	for g, m := range u.permissions {
		for a, ok := range m {
			if ok {
				out = append(out, PermissionPair{Alias: a, GroupAlias: g})
			}
		}
	}
	return out, nil
}

// Scope describes the locations/assets a user may see. A nil Scope (or
// Unrestricted) means everything.
type Scope struct {
	Unrestricted bool
	Locations    map[int64]bool
}

// AllowsLocation reports whether a location is visible.
func (sc *Scope) AllowsLocation(id int64) bool {
	return sc == nil || sc.Unrestricted || sc.Locations[id]
}

// LocationIDs returns the visible location ids (nil when unrestricted).
func (sc *Scope) LocationIDs() []int64 {
	if sc == nil || sc.Unrestricted {
		return nil
	}
	out := make([]int64, 0, len(sc.Locations))
	for id := range sc.Locations {
		out = append(out, id)
	}
	return out
}

// ScopeFor computes the location scope of a user: the locations assigned to
// the user's group plus all of their descendants.
func (s *Service) ScopeFor(ctx context.Context, u *User) (*Scope, error) {
	if u == nil {
		return &Scope{Locations: map[int64]bool{}}, nil
	}
	if u.IsAdmin() {
		return &Scope{Unrestricted: true}, nil
	}
	sc := &Scope{Locations: map[int64]bool{}}
	if u.UserGroupID == nil {
		return sc, nil
	}
	ids, err := db.Int64s(ctx, s.db, `WITH RECURSIVE t AS (
			SELECT l.id FROM asset.locations l
			JOIN core.user_group_locations ugl ON ugl.location_id = l.id
			WHERE ugl.user_group_id = $1
			UNION
			SELECT c.id FROM asset.locations c JOIN t ON c.parent_id = t.id
		) SELECT id FROM t`, *u.UserGroupID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		sc.Locations[id] = true
	}
	return sc, nil
}

// CleanupExpired removes expired sessions.
func (s *Service) CleanupExpired(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM core.sessions WHERE expires_at < now()`)
	return err
}

// EnsureBootstrapUser creates the initial superuser when no active superuser
// exists. The generated password is returned when one had to be generated.
func (s *Service) EnsureBootstrapUser(ctx context.Context, username, password string) (created bool, generated string, err error) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM core.users WHERE role = 'superuser' AND NOT is_deleted`).Scan(&n); err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "", nil
	}
	if username == "" {
		username = "superuser"
	}
	if password == "" {
		generated = "Su-" + newToken()[:12] + "!"
		password = generated
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, "", err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO core.users (username, password_hash, full_name, role)
		VALUES ($1, $2, 'Super User', 'superuser')`, username, hash)
	if err != nil {
		return false, "", fmt.Errorf("create bootstrap superuser: %w", err)
	}
	return true, generated, nil
}
