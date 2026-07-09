package api

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/storage"
)

func (s *Server) registerInstruction() {
	p := "maintenance_instruction/"
	s.handle(p+"get_alarm_codes", AccessUser, s.listTable(`SELECT id, code || ' - ' || name FROM dm.alarm_codes ORDER BY order_id, code`))
	s.handle(p+"get_models", AccessUser, s.listTable(`SELECT id, name FROM asset.models ORDER BY lower(name)`))
	s.handle(p+"get_instruction_types", AccessUser, s.listTable(`SELECT id, name FROM dm.instruction_types ORDER BY id`))
	s.handle(p+"get_instructions", AccessUser, s.miList)
	s.handle(p+"get_instruction_detail", AccessUser, s.miDetail)
	s.handle(p+"create_instruction", AccessUser, s.miCreate)
	s.handle(p+"update_instruction", AccessUser, s.miUpdate)
	s.handle(p+"delete_instruction", AccessUser, s.miDelete)
	s.handle(p+"upload_pdf", AccessUser, s.miUploadPDF)
	s.handle(p+"upload_media", AccessUser, s.miUploadMedia)
}

func (s *Server) miList(c *Ctx) (any, error) {
	var modelFilter []int64
	useModels := false
	if ids, present := c.F.IDs("model_ids"); present {
		modelFilter, useModels = ids, true
	}
	typeIDs, typePresent := c.F.IDs("type_ids")
	locIDs, locPresent := c.F.IDs("location_ids")
	assetIDs, _ := c.F.IDs("asset_ids")
	if locPresent {
		// Restrict to models of the selected equipment.
		assets, err := s.resolveAssets(c, assetFilter{locations: locIDs, locPresent: true, assets: assetIDs})
		if err != nil {
			return nil, err
		}
		models, err := db.Int64s(c, s.DB, `SELECT DISTINCT model_id FROM asset.equipment WHERE id = ANY($1)`, assets)
		if err != nil {
			return nil, err
		}
		if useModels {
			set := int64Set(models)
			modelFilter = filterRows(modelFilter, func(id int64) bool { return set[id] })
		} else {
			modelFilter, useModels = models, true
		}
	}
	if typePresent {
		models, err := db.Int64s(c, s.DB, `SELECT id FROM asset.models WHERE type_id = ANY($1)`, typeIDs)
		if err != nil {
			return nil, err
		}
		if useModels {
			set := int64Set(models)
			modelFilter = filterRows(modelFilter, func(id int64) bool { return set[id] })
		} else {
			modelFilter, useModels = models, true
		}
	}
	var instrTypes any
	if ids, present := c.F.IDs("instruction_type_ids"); present {
		instrTypes = ids
	}
	var models any
	if useModels {
		models = emptyIfNil(modelFilter)
	}
	search := c.F.StrTrim("search")
	where := `NOT i.is_deleted
		AND ($1::bigint[] IS NULL OR EXISTS (SELECT 1 FROM dm.instruction_models im WHERE im.instruction_id = i.id AND im.model_id = ANY($1)))
		AND ($2::bigint[] IS NULL OR i.instruction_type_id = ANY($2))
		AND ($3 = '' OR i.title ILIKE $4 OR i.description ILIKE $4 OR ac.code ILIKE $4 OR ac.name ILIKE $4)`
	var total int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM dm.instructions i LEFT JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id WHERE `+where,
		models, instrTypes, search, ilike(search)).Scan(&total); err != nil {
		return nil, err
	}
	const pageSize = 4
	page := c.page(1)
	rows, err := s.DB.Query(c, `SELECT i.id, i.title, COALESCE(i.updated_at, i.created_at), COALESCE(NULLIF(u.full_name, ''), u.username, ''),
			i.description, t.name
		FROM dm.instructions i JOIN dm.instruction_types t ON t.id = i.instruction_type_id
		LEFT JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id LEFT JOIN core.users u ON u.id = i.created_by
		WHERE `+where+` ORDER BY COALESCE(i.updated_at, i.created_at) DESC, i.id DESC OFFSET $5 LIMIT $6`,
		models, instrTypes, search, ilike(search), (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var id int64
		var title, by, desc, typ string
		var ts time.Time
		if err := rows.Scan(&id, &title, &ts, &by, &desc, &typ); err != nil {
			return nil, err
		}
		data = append(data, map[string]any{"id": id, "name": title, "timestamp": httpx.TS(ts), "created_by": by, "description": desc, "type_name": typ})
	}
	return map[string]any{"data": data, "count": total}, rows.Err()
}

func (s *Server) miDetail(c *Ctx) (any, error) {
	id := c.F.Int("id", 0)
	var (
		codeID                    *int64
		code, codeName            *string
		title, typeName, desc, by string
		typeID, duration          int
		content, pdfURL, pdfName  *string
		created                   time.Time
		updated                   *time.Time
	)
	err := s.DB.QueryRow(c, `SELECT i.alarm_code_id, ac.code, ac.name, i.title, i.instruction_type_id, t.name, i.duration, i.description,
			i.content, i.pdf_url, i.pdf_name, COALESCE(NULLIF(u.full_name, ''), u.username, ''), i.created_at, i.updated_at
		FROM dm.instructions i JOIN dm.instruction_types t ON t.id = i.instruction_type_id
		LEFT JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id LEFT JOIN core.users u ON u.id = i.created_by
		WHERE i.id = $1 AND NOT i.is_deleted`, id).Scan(&codeID, &code, &codeName, &title, &typeID, &typeName, &duration, &desc,
		&content, &pdfURL, &pdfName, &by, &created, &updated)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Instruction not found")
		}
		return nil, err
	}
	models, err := db.Int64s(c, s.DB, `SELECT model_id FROM dm.instruction_models WHERE instruction_id = $1 ORDER BY model_id`, id)
	if err != nil {
		return nil, err
	}
	assets, err := db.Strings(c, s.DB, `SELECT name FROM asset.equipment WHERE model_id = ANY($1) ORDER BY lower(name)`, models)
	if err != nil {
		return nil, err
	}
	full := ""
	if code != nil {
		full = *code + " - " + derefStr(codeName)
	}
	return map[string]any{
		"id": id, "alarm_code_id": codeID, "alarm_code_name": codeName, "alarm_code_full_name": full, "title": title,
		"instruction_type_id": typeID, "instruction_type_name": typeName, "duration": duration, "model_ids": emptyIfNil(models),
		"asset_names": emptyIfNil(assets), "description": desc, "content": content, "pdf_url": pdfURL, "pdf_name": pdfName,
		"is_using_pdf": pdfURL != nil && *pdfURL != "", "created_by": by, "created_at": httpx.TS(created), "updated_at": httpx.TSPtr(updated),
	}, nil
}

type instructionInput struct {
	codeID   *int64
	title    string
	typeID   int64
	duration int64
	models   []int64
	desc     string
	content  *string
	pdfURL   *string
	pdfName  *string
}

func (s *Server) instructionInput(c *Ctx) (*instructionInput, error) {
	in := &instructionInput{
		codeID:   c.F.IntPtr("alarm_code_id"),
		title:    c.F.StrTrim("title"),
		typeID:   c.F.Int("instruction_type_id", 0),
		duration: c.F.Int("duration", 0),
		desc:     c.F.Str("description"),
		content:  c.F.StrPtr("content"),
		pdfURL:   c.F.NonEmptyPtr("pdf_url"),
	}
	in.models, _ = c.F.IDs("model_ids")
	switch {
	case in.title == "":
		return nil, httpx.BadRequest("Title is required")
	case in.typeID < 1 || in.typeID > 3:
		return nil, httpx.BadRequest("err_invalid_type")
	case in.duration < 0:
		return nil, httpx.BadRequest("err_input_valid_number")
	case len(in.models) == 0:
		return nil, httpx.BadRequest("err_invalid_model")
	case len([]rune(in.desc)) > 160:
		return nil, httpx.BadRequest("Description is too long")
	}
	if in.codeID != nil {
		var ok bool
		if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM dm.alarm_codes WHERE id = $1)`, *in.codeID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, httpx.BadRequest("err_alarm_not_found")
		}
	}
	if in.pdfURL != nil {
		key := storage.KeyFromURL(*in.pdfURL)
		if key == "" {
			return nil, httpx.BadRequest("Invalid PDF")
		}
		u := storage.PublicPrefix + key
		in.pdfURL = &u
		var name string
		if err := s.DB.QueryRow(c, `SELECT original_name FROM core.files WHERE object_key = $1`, key).Scan(&name); err == nil {
			in.pdfName = &name
		} else {
			base := key[strings.LastIndex(key, "/")+1:]
			in.pdfName = &base
		}
		in.content = nil
	}
	return in, nil
}

func setInstructionModels(ctx context.Context, tx pgx.Tx, id int64, models []int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM dm.instruction_models WHERE instruction_id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO dm.instruction_models (instruction_id, model_id)
		SELECT $1, m.id FROM asset.models m WHERE m.id = ANY($2) ON CONFLICT DO NOTHING`, id, models)
	return err
}

func (s *Server) miCreate(c *Ctx) (any, error) {
	if err := c.Require("maintenanceinstruction", "create"); err != nil {
		return nil, err
	}
	in, err := s.instructionInput(c)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c, `INSERT INTO dm.instructions (alarm_code_id, title, instruction_type_id, duration, description, content, pdf_url, pdf_name, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, in.codeID, in.title, in.typeID, in.duration, in.desc, in.content, in.pdfURL, in.pdfName, c.User.ID).Scan(&id); err != nil {
			return err
		}
		return setInstructionModels(c, tx, id, in.models)
	})
	if err != nil {
		return nil, err
	}
	return id, nil
}

func (s *Server) miUpdate(c *Ctx) (any, error) {
	if err := c.Require("maintenanceinstruction", "update"); err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	in, err := s.instructionInput(c)
	if err != nil {
		return nil, err
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		tag, err := tx.Exec(c, `UPDATE dm.instructions SET alarm_code_id = $2, title = $3, instruction_type_id = $4, duration = $5, description = $6,
			content = $7, pdf_url = $8, pdf_name = $9, updated_at = now() WHERE id = $1 AND NOT is_deleted`,
			id, in.codeID, in.title, in.typeID, in.duration, in.desc, in.content, in.pdfURL, in.pdfName)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("Instruction not found")
		}
		return setInstructionModels(c, tx, id, in.models)
	})
	if err != nil {
		return nil, err
	}
	return "Instruction successfully updated", nil
}

func (s *Server) miDelete(c *Ctx) (any, error) {
	if err := c.Require("maintenanceinstruction", "delete"); err != nil {
		return nil, err
	}
	id := c.F.Int("maintenance_id", 0)
	if id == 0 {
		id = c.F.Int("id", 0)
	}
	tag, err := s.DB.Exec(c, `UPDATE dm.instructions SET is_deleted = TRUE, updated_at = now() WHERE id = $1 AND NOT is_deleted`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("Instruction not found")
	}
	return "Instruction successfully deleted", nil
}

func (s *Server) miUploadPDF(c *Ctx) (any, error) {
	if !c.User.Can("maintenanceinstruction", "create") && !c.User.Can("maintenanceinstruction", "update") {
		return nil, httpx.Forbidden("")
	}
	fh := c.F.File("pdf")
	if fh == nil {
		return nil, httpx.BadRequest("err_file_required")
	}
	saved, err := s.Store.SaveMultipart(c, "instructions/pdf", fh, map[string]bool{"application/pdf": true})
	if err != nil {
		var ute *storage.UnsupportedTypeError
		if asUnsupported(err, &ute) {
			return nil, httpx.BadRequest("Only PDF files are supported")
		}
		return nil, err
	}
	_, _ = s.DB.Exec(c, `INSERT INTO core.files (object_key, original_name, content_type, size, created_by) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (object_key) DO NOTHING`, saved.Key, saved.Name, saved.ContentType, saved.Size, c.User.ID)
	return map[string]any{"url": saved.URL, "name": saved.Name}, nil
}

func (s *Server) miUploadMedia(c *Ctx) (any, error) {
	if !c.User.Can("maintenanceinstruction", "create") && !c.User.Can("maintenanceinstruction", "update") {
		return nil, httpx.Forbidden("")
	}
	fh := c.F.File("media")
	if fh == nil {
		return nil, httpx.BadRequest("err_file_required")
	}
	saved, err := s.Store.SaveMultipart(c, "instructions/media", fh, storage.AllowedImageTypes)
	if err != nil {
		return nil, fail(err)
	}
	_, _ = s.DB.Exec(c, `INSERT INTO core.files (object_key, original_name, content_type, size, created_by) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (object_key) DO NOTHING`, saved.Key, saved.Name, saved.ContentType, saved.Size, c.User.ID)
	return saved.URL, nil
}
