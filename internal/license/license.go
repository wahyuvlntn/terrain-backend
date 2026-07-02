// Package license implements signed license files.
//
// A license file is a base64 encoded JSON envelope:
//
//	{"payload": "<base64 JSON Content>", "signature": "<base64 ed25519 signature of payload bytes>"}
//
// The payload is signed with the vendor's private key; the backend verifies it
// with LICENSE_PUBLIC_KEY. When no public key is configured signatures are not
// checked (development mode).
package license

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"afdd/internal/db"
)

// Types.
const (
	TypeBase      = "BASE"
	TypeEquipment = "EQUIPMENT"
)

// Content is the signed license payload.
type Content struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	AssetKeys   []string `json:"asset_keys"`
	ExpiredAt   int64    `json:"expired_at"` // unix ms, 0 = never
	SystemID    string   `json:"system_id,omitempty"`
	Product     string   `json:"product,omitempty"`
	IssuedAt    int64    `json:"issued_at,omitempty"`
}

type envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Errors (translation keys understood by the web UI).
var (
	ErrInvalidFile   = errors.New("err_invalid_license_file")
	ErrInvalidSig    = errors.New("err_invalid_license")
	ErrExpired       = errors.New("err_license_expired")
	ErrNotApplicable = errors.New("err_license_not_applicable")
)

// Manager verifies licenses.
type Manager struct {
	db     *db.DB
	pubKey ed25519.PublicKey
}

// NewManager creates a manager. pub is a base64 (or hex) ed25519 public key.
func NewManager(d *db.DB, pub string) (*Manager, error) {
	m := &Manager{db: d}
	pub = strings.TrimSpace(pub)
	if pub != "" {
		k, err := decodeKey(pub)
		if err != nil {
			return nil, fmt.Errorf("invalid LICENSE_PUBLIC_KEY: %w", err)
		}
		if len(k) != ed25519.PublicKeySize {
			return nil, errors.New("invalid LICENSE_PUBLIC_KEY length")
		}
		m.pubKey = ed25519.PublicKey(k)
	}
	return m, nil
}

// Enforcing reports whether signatures are verified.
func (m *Manager) Enforcing() bool { return len(m.pubKey) > 0 }

func decodeKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return hex.DecodeString(s)
}

// SystemID returns the identifier of this installation (generated once).
func (m *Manager) SystemID(ctx context.Context) (string, error) {
	var id string
	err := m.db.QueryRow(ctx, `SELECT value #>> '{}' FROM core.settings WHERE key = 'system_id'`).Scan(&id)
	if err == nil && id != "" {
		return id, nil
	}
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	id = strings.ToUpper(hex.EncodeToString(b))
	id = id[0:5] + "-" + id[5:10] + "-" + id[10:15] + "-" + id[15:20]
	_, err = m.db.Exec(ctx, `INSERT INTO core.settings (key, value) VALUES ('system_id', to_jsonb($1::text)) ON CONFLICT (key) DO NOTHING`, id)
	if err != nil {
		return "", err
	}
	err = m.db.QueryRow(ctx, `SELECT value #>> '{}' FROM core.settings WHERE key = 'system_id'`).Scan(&id)
	return id, err
}

// Parse decodes and verifies a license file.
func (m *Manager) Parse(ctx context.Context, raw []byte) (*Content, error) {
	text := strings.TrimSpace(string(raw))
	var env envelope
	if dec, err := base64.StdEncoding.DecodeString(text); err == nil && json.Unmarshal(dec, &env) == nil && env.Payload != "" {
		// base64 wrapped envelope
	} else if err := json.Unmarshal([]byte(text), &env); err != nil || env.Payload == "" {
		return nil, ErrInvalidFile
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, ErrInvalidFile
	}
	if m.Enforcing() {
		sig, err := base64.StdEncoding.DecodeString(env.Signature)
		if err != nil || !ed25519.Verify(m.pubKey, payload, sig) {
			return nil, ErrInvalidSig
		}
	}
	var c Content
	if err := json.Unmarshal(payload, &c); err != nil || strings.TrimSpace(c.Key) == "" {
		return nil, ErrInvalidFile
	}
	c.Type = strings.ToUpper(strings.TrimSpace(c.Type))
	if c.Type == "" {
		c.Type = TypeEquipment
	}
	if c.AssetKeys == nil {
		c.AssetKeys = []string{}
	}
	if c.ExpiredAt > 0 && time.UnixMilli(c.ExpiredAt).Before(time.Now()) {
		return &c, ErrExpired
	}
	if c.SystemID != "" {
		sys, err := m.SystemID(ctx)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(sys, c.SystemID) {
			return &c, ErrNotApplicable
		}
	}
	return &c, nil
}

// Issue creates a signed license file (used by the CLI).
func Issue(priv ed25519.PrivateKey, c Content) (string, error) {
	if c.IssuedAt == 0 {
		c.IssuedAt = time.Now().UnixMilli()
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	env := envelope{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)),
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// GenerateKeys creates a new ed25519 key pair (base64 encoded).
func GenerateKeys() (pub, priv string, err error) {
	p, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p), base64.StdEncoding.EncodeToString(k), nil
}

// DecodePrivateKey decodes a base64 ed25519 private key.
func DecodePrivateKey(s string) (ed25519.PrivateKey, error) {
	k, err := decodeKey(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	switch len(k) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(k), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(k), nil
	}
	return nil, errors.New("invalid private key length")
}

// Status returns (is_active, status message) for a stored license.
func Status(expiredAt *time.Time) (bool, string) {
	if expiredAt != nil && expiredAt.Before(time.Now()) {
		return false, "Expired"
	}
	return true, "Active"
}
