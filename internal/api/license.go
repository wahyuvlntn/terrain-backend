package api

import (
	"errors"
	"io"
	"strings"
	"time"

	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/license"
)

func (s *Server) registerLicense() {
	p := "license/"
	s.handle(p+"get_license_system_id", AccessSuper, s.licSystemID)
	s.handle(p+"get_licenses", AccessSuper, s.licList)
	s.handle(p+"get_license_information", AccessSuper, s.licInfo)
	s.handle(p+"delete_license", AccessSuper, s.licDelete)
	s.handle(p+"upload_license", AccessSuper, s.licUpload)
	s.handle(p+"update_license", AccessSuper, s.licUpdate)
	s.handle(p+"verify_license", AccessSuper, s.licVerify)
}

func licenseStatus(exp *time.Time) (bool, string) { return license.Status(exp) }

func expMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func (s *Server) licSystemID(c *Ctx) (any, error) { return s.License.SystemID(c) }

func (s *Server) licList(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT l.key, l.name, l.type, l.total, l.description, l.expired_at,
			(SELECT count(*) FROM asset.equipment e WHERE e.license_id = l.id)
		FROM core.licenses l ORDER BY lower(l.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key, name, typ string
		var total int
		var desc *string
		var exp *time.Time
		var used int64
		if err := rows.Scan(&key, &name, &typ, &total, &desc, &exp, &used); err != nil {
			return nil, err
		}
		active, status := licenseStatus(exp)
		out = append(out, map[string]any{"key": key, "name": name, "type": typ, "total": total, "used": used, "is_active": active,
			"status_message": status, "description": desc, "expired_at": expMillis(exp)})
	}
	return out, rows.Err()
}

func (s *Server) licInfo(c *Ctx) (any, error) {
	var id int64
	var name, typ string
	var total int
	var desc *string
	var exp *time.Time
	err := s.DB.QueryRow(c, `SELECT id, name, type, total, description, expired_at FROM core.licenses WHERE key = $1`, c.F.StrTrim("license_key")).
		Scan(&id, &name, &typ, &total, &desc, &exp)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_license_key_not_found")
		}
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT id, alias, name FROM asset.equipment WHERE license_id = $1 ORDER BY lower(name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := []map[string]any{}
	for rows.Next() {
		var eid int64
		var alias, ename string
		if err := rows.Scan(&eid, &alias, &ename); err != nil {
			return nil, err
		}
		used = append(used, map[string]any{"id": eid, "alias": alias, "name": ename})
	}
	active, status := licenseStatus(exp)
	return map[string]any{"name": name, "type": typ, "total": total, "used": used, "is_active": active, "status_message": status,
		"description": desc, "expired_at": expMillis(exp)}, rows.Err()
}

func (s *Server) licDelete(c *Ctx) (any, error) {
	var id int64
	var name string
	if err := s.DB.QueryRow(c, `SELECT id, name FROM core.licenses WHERE key = $1`, c.F.StrTrim("license_key")).Scan(&id, &name); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_license_key_not_found")
		}
		return nil, err
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment WHERE license_id = $1`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_equipment_assigned_to_license")
	}
	if _, err := s.DB.Exec(c, `DELETE FROM core.licenses WHERE id = $1`, id); err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("license_success_deleted", name), nil
}

func (s *Server) readLicenseFile(c *Ctx) (*license.Content, string, error) {
	fh := c.F.File("license_file")
	if fh == nil {
		return nil, "", httpx.BadRequest("err_select_license_file")
	}
	if fh.Size > 1<<20 {
		return nil, "", httpx.BadRequest("err_invalid_license_file")
	}
	f, err := fh.Open()
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, "", err
	}
	content, err := s.License.Parse(c, raw)
	if err != nil {
		return content, "", fail(err)
	}
	return content, strings.TrimSpace(string(raw)), nil
}

func expTime(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

func (s *Server) licVerify(c *Ctx) (any, error) {
	content, _, err := s.readLicenseFile(c)
	if err != nil {
		return nil, err
	}
	return map[string]any{"key": content.Key, "type": content.Type, "description": content.Description,
		"asset_keys": content.AssetKeys, "expired_at": content.ExpiredAt}, nil
}

func (s *Server) licUpload(c *Ctx) (any, error) {
	name := c.F.StrTrim("license_name")
	if name == "" {
		return nil, httpx.BadRequest("err_invalid_license_name")
	}
	content, raw, err := s.readLicenseFile(c)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.licenses WHERE key = $1)`, content.Key).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.BadRequest("err_license_same_key")
	}
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.licenses WHERE lower(name) = lower($1))`, name).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.BadRequest("err_license_same_name")
	}
	if content.Type == license.TypeBase {
		if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.licenses WHERE type = 'BASE')`).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, httpx.BadRequest("err_different_base_installed")
		}
	}
	_, err = s.DB.Exec(c, `INSERT INTO core.licenses (key, name, type, description, asset_keys, total, system_id, expired_at, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, content.Key, name, content.Type, nilIfEmpty(content.Description), content.AssetKeys,
		len(content.AssetKeys), nilIfEmpty(content.SystemID), expTime(content.ExpiredAt), raw)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, httpx.BadRequest("err_license_same_name")
		}
		return nil, err
	}
	return httpx.Key("license_success_added", name), nil
}

func (s *Server) licUpdate(c *Ctx) (any, error) {
	key := c.F.StrTrim("license_key")
	name := c.F.StrTrim("license_name")
	if name == "" {
		return nil, httpx.BadRequest("err_invalid_license_name")
	}
	var id int64
	if err := s.DB.QueryRow(c, `SELECT id FROM core.licenses WHERE key = $1`, key).Scan(&id); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_license_key_not_found")
		}
		return nil, err
	}
	var exists bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.licenses WHERE lower(name) = lower($1) AND id <> $2)`, name, id).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.BadRequest("err_license_same_name")
	}
	if c.F.File("license_file") != nil {
		content, raw, err := s.readLicenseFile(c)
		if err != nil {
			return nil, err
		}
		if content.Key != key {
			return nil, httpx.BadRequest("err_license_key_not_match")
		}
		var used int
		if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment WHERE license_id = $1`, id).Scan(&used); err != nil {
			return nil, err
		}
		if content.Type != license.TypeBase && len(content.AssetKeys) < used {
			return nil, httpx.BadRequest("err_equipment_assigned_to_license")
		}
		if _, err := s.DB.Exec(c, `UPDATE core.licenses SET name = $2, type = $3, description = $4, asset_keys = $5, total = $6,
			system_id = $7, expired_at = $8, raw = $9, updated_at = now() WHERE id = $1`, id, name, content.Type, nilIfEmpty(content.Description),
			content.AssetKeys, len(content.AssetKeys), nilIfEmpty(content.SystemID), expTime(content.ExpiredAt), raw); err != nil {
			return nil, err
		}
	} else if _, err := s.DB.Exec(c, `UPDATE core.licenses SET name = $2, updated_at = now() WHERE id = $1`, id, name); err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("license_success_updated", name), nil
}

var _ = errors.New
