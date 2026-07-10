package api

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/storage"
)

const defaultThemeID = 1

var (
	hexColorRe  = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	colorNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	logoTypes   = map[string]bool{"image/jpg": true, "image/jpeg": true, "image/png": true, "image/gif": true}
)

func (s *Server) registerTheme() {
	p := "theme/"
	s.handle(p+"getactivetheme", AccessPublic, s.themeActive)
	s.handle(p+"setactivetheme", AccessUser, s.themeSetActive)
	s.handle(p+"getthemes", AccessUser, s.themeList)
	s.handle(p+"getthemedetails", AccessUser, s.themeDetails)
	s.handle(p+"addtheme", AccessUser, s.themeAdd)
	s.handle(p+"updatetheme", AccessUser, s.themeUpdate)
	s.handle(p+"deletetheme", AccessUser, s.themeDelete)
}

type activeTheme struct {
	ThemeID int64   `json:"theme_id"`
	Logo    *string `json:"logo"`
}

func (s *Server) loadActiveTheme(c *Ctx, q db.Querier) (activeTheme, error) {
	at := activeTheme{ThemeID: defaultThemeID}
	var raw []byte
	err := q.QueryRow(c, `SELECT value FROM core.settings WHERE key = 'active_theme'`).Scan(&raw)
	if err != nil && !db.IsNoRows(err) {
		return at, err
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &at)
	}
	return at, nil
}

func saveActiveTheme(c *Ctx, q db.Querier, at activeTheme) error {
	raw, _ := json.Marshal(at)
	_, err := q.Exec(c, `INSERT INTO core.settings (key, value, updated_at) VALUES ('active_theme', $1, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, raw)
	return err
}

func (s *Server) themeColors(c *Ctx, q db.Querier, id int64) (map[string]string, error) {
	rows, err := q.Query(c, `SELECT name, hex_code FROM core.theme_colors WHERE theme_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, h string
		if err := rows.Scan(&n, &h); err != nil {
			return nil, err
		}
		out[n] = h
	}
	return out, rows.Err()
}

func (s *Server) themeActive(c *Ctx) (any, error) {
	at, err := s.loadActiveTheme(c, s.DB)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.themes WHERE id = $1)`, at.ThemeID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		if err := s.DB.QueryRow(c, `SELECT COALESCE((SELECT id FROM core.themes ORDER BY is_system DESC, id LIMIT 1), $1)`, int64(defaultThemeID)).
			Scan(&at.ThemeID); err != nil {
			return nil, err
		}
	}
	colors, err := s.themeColors(c, s.DB, at.ThemeID)
	if err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(colors))
	for n, h := range colors {
		list = append(list, map[string]any{"name": n, "hex_code": h})
	}
	return map[string]any{"theme_id": at.ThemeID, "logo": imageURL(at.Logo), "colors": list}, nil
}

func (s *Server) themeSetActive(c *Ctx) (any, error) {
	if err := c.Require("themeeditor", "update"); err != nil {
		return nil, err
	}
	id := c.F.Int("theme_id", defaultThemeID)
	var exists bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.themes WHERE id = $1)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, httpx.NotFound("err_theme_not_found")
	}
	at, err := s.loadActiveTheme(c, s.DB)
	if err != nil {
		return nil, err
	}
	old := at.Logo
	at.ThemeID = id
	var uploaded *storage.Saved
	if fh := c.F.File("logo"); fh != nil {
		if fh.Size >= 1<<20 {
			return nil, httpx.BadRequest("err_max_size_file")
		}
		saved, err := s.Store.SaveMultipart(c, "logo", fh, logoTypes)
		if err != nil {
			return nil, fail(err)
		}
		uploaded = saved
		at.Logo = &saved.URL
	} else if c.F.Bool("delete_logo", false) {
		at.Logo = nil
	}
	if err := saveActiveTheme(c, s.DB, at); err != nil {
		if uploaded != nil {
			_ = s.Store.Delete(c, uploaded.Key)
		}
		return nil, err
	}
	if old != nil && (at.Logo == nil || *at.Logo != *old) {
		s.Store.DeleteURL(c, *old)
	}
	return "Active theme successfully updated", nil
}

func previewColors(colors map[string]string, secondary, tertiary bool) []string {
	get := func(k string) string {
		if v, ok := colors[k]; ok {
			return v
		}
		return "#0F94ED"
	}
	primary := get("surfacePrimary")
	sec, ter := primary, primary
	if secondary {
		sec = get("surfaceSecondary")
	}
	if tertiary {
		ter = get("surfaceTertiary")
	}
	return []string{get("base"), primary, sec, ter}
}

func (s *Server) themeList(c *Ctx) (any, error) {
	if err := c.Require("themeeditor", "read"); err != nil {
		return nil, err
	}
	page, limit := c.page(1), c.limit(12, 100)
	rows, err := s.DB.Query(c, `SELECT id, name, is_system, is_surface_secondary_used, is_surface_tertiary_used FROM core.themes
		WHERE name ILIKE $1 ORDER BY is_system DESC, lower(name), id LIMIT $2 OFFSET $3`, ilike(c.F.Str("search")), limit+1, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	type th struct {
		id             int64
		name           string
		system, s2, s3 bool
	}
	var list []th
	for rows.Next() {
		var t th
		if err := rows.Scan(&t.id, &t.name, &t.system, &t.s2, &t.s3); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	extend := int64(len(list)) > limit
	if extend {
		list = list[:limit]
	}
	data := make([]map[string]any, 0, len(list))
	for _, t := range list {
		colors, err := s.themeColors(c, s.DB, t.id)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]any{"id": t.id, "name": t.name, "colors": previewColors(colors, t.s2, t.s3), "is_editable": !t.system})
	}
	return map[string]any{"is_extend": extend, "data": data}, nil
}

func (s *Server) themeDetails(c *Ctx) (any, error) {
	if err := c.Require("themeeditor", "read"); err != nil {
		return nil, err
	}
	id := c.F.Int("theme_id", 0)
	var name string
	var s2, s3 bool
	if err := s.DB.QueryRow(c, `SELECT name, is_surface_secondary_used, is_surface_tertiary_used FROM core.themes WHERE id = $1`, id).
		Scan(&name, &s2, &s3); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_theme_not_found")
		}
		return nil, err
	}
	colors, err := s.themeColors(c, s.DB, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "is_surface_secondary_used": s2, "is_surface_tertiary_used": s3, "colors": colors}, nil
}

type themeInput struct {
	name     string
	s2, s3   bool
	colors   map[string]string
	editedID int64
}

func (c *Ctx) themeInput() (*themeInput, error) {
	in := &themeInput{name: c.F.StrTrim("name"), s2: c.F.Bool("is_surface_secondary_used", false), s3: c.F.Bool("is_surface_tertiary_used", false)}
	if in.name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	if len([]rune(in.name)) > 100 {
		return nil, httpx.BadRequest("Theme name is too long")
	}
	var list []struct {
		Name    string `json:"name"`
		HexCode string `json:"hex_code"`
	}
	if err := json.Unmarshal([]byte(c.F.Str("colors")), &list); err != nil || len(list) == 0 || len(list) > 500 {
		return nil, httpx.BadRequest("Invalid theme colors")
	}
	in.colors = map[string]string{}
	for _, col := range list {
		if !colorNameRe.MatchString(col.Name) || !hexColorRe.MatchString(strings.TrimSpace(col.HexCode)) {
			return nil, httpx.BadRequest("Invalid theme colors")
		}
		in.colors[col.Name] = strings.ToUpper(strings.TrimSpace(col.HexCode))
	}
	return in, nil
}

func saveThemeColors(c *Ctx, tx pgx.Tx, id int64, colors map[string]string) error {
	if _, err := tx.Exec(c, `DELETE FROM core.theme_colors WHERE theme_id = $1`, id); err != nil {
		return err
	}
	names := make([]string, 0, len(colors))
	hexes := make([]string, 0, len(colors))
	for n, h := range colors {
		names = append(names, n)
		hexes = append(hexes, h)
	}
	_, err := tx.Exec(c, `INSERT INTO core.theme_colors (theme_id, name, hex_code) SELECT $1, n, h FROM unnest($2::text[], $3::text[]) AS t(n, h)`,
		id, names, hexes)
	return err
}

func themeNameTaken(c *Ctx, tx pgx.Tx, name string, except int64) error {
	var taken bool
	if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM core.themes WHERE lower(name) = lower($1) AND id <> $2)`, name, except).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return httpx.BadRequest("Theme with the same name already exists")
	}
	return nil
}

func (s *Server) themeAdd(c *Ctx) (any, error) {
	if err := c.Require("themeeditor", "create"); err != nil {
		return nil, err
	}
	in, err := c.themeInput()
	if err != nil {
		return nil, err
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := themeNameTaken(c, tx, in.name, 0); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(c, `INSERT INTO core.themes (name, is_surface_secondary_used, is_surface_tertiary_used, created_by)
			VALUES ($1,$2,$3,$4) RETURNING id`, in.name, in.s2, in.s3, c.User.ID).Scan(&id); err != nil {
			return err
		}
		return saveThemeColors(c, tx, id, in.colors)
	})
	if err != nil {
		return nil, err
	}
	return "Theme successfully added", nil
}

func (s *Server) themeUpdate(c *Ctx) (any, error) {
	if err := c.Require("themeeditor", "update"); err != nil {
		return nil, err
	}
	id := c.F.Int("theme_id", 0)
	in, err := c.themeInput()
	if err != nil {
		return nil, err
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		var system bool
		if err := tx.QueryRow(c, `SELECT is_system FROM core.themes WHERE id = $1 FOR UPDATE`, id).Scan(&system); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("err_theme_not_found")
			}
			return err
		}
		if system {
			return httpx.BadRequest("System theme can't be edited")
		}
		if err := themeNameTaken(c, tx, in.name, id); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `UPDATE core.themes SET name = $2, is_surface_secondary_used = $3, is_surface_tertiary_used = $4, updated_at = now()
			WHERE id = $1`, id, in.name, in.s2, in.s3); err != nil {
			return err
		}
		return saveThemeColors(c, tx, id, in.colors)
	})
	if err != nil {
		return nil, err
	}
	return "Theme successfully updated", nil
}

func (s *Server) themeDelete(c *Ctx) (any, error) {
	if err := c.Require("themeeditor", "delete"); err != nil {
		return nil, err
	}
	id := c.F.Int("theme_id", 0)
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		var system bool
		if err := tx.QueryRow(c, `SELECT is_system FROM core.themes WHERE id = $1 FOR UPDATE`, id).Scan(&system); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("err_theme_not_found")
			}
			return err
		}
		if system {
			return httpx.BadRequest("err_system_theme_can't_deleted")
		}
		if _, err := tx.Exec(c, `DELETE FROM core.themes WHERE id = $1`, id); err != nil {
			return err
		}
		at, err := s.loadActiveTheme(c, tx)
		if err != nil {
			return err
		}
		if at.ThemeID == id {
			at.ThemeID = defaultThemeID
			return saveActiveTheme(c, tx, at)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return "Theme successfully deleted", nil
}
