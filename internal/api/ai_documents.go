package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"afdd/internal/auth"
	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/storage"
)

// RAG document library (DESIGN §4.4, §6.2). Originals are PDFs in the private
// bucket (MINIO_DOCS_BUCKET, key rag/<yyyy>/<uuid>.pdf) and are only served
// by GET /api/ai/documents/{id}/file after the session, permission and scope
// checks, never under /public. ai-analytics indexes PENDING documents
// (NOTIFY rag_documents): PENDING -> INDEXING -> READY | FAILED.

const (
	docMaxBytes  = 50 << 20
	docMaxTitle  = 300
	docMaxName   = 200
	docFileRoute = "/api/ai/documents/%d/file"
)

var docTypes = []string{"manual", "sop", "maintenance_report", "setpoint_log", "fdd_reference", "other"}
var docStatuses = []string{"PENDING", "INDEXING", "READY", "FAILED"}

func (s *Server) registerAIDocuments() {
	s.handle("ai_documents/list", AccessUser, s.docList)
	s.handle("ai_documents/get", AccessUser, s.docGet)
	s.handle("ai_documents/upload", AccessUser, s.docUpload)
	s.handle("ai_documents/delete", AccessUser, s.docDelete)
	s.handle("ai_documents/reindex", AccessUser, s.docReindex)
}

const docColumns = `d.id, d.title, d.doc_type, d.source, d.object_key, d.file_name, d.content_type, d.size_bytes, d.sha256,
	d.site_code, d.location_id, d.asset_id, d.unit, d.asset_type, d.model_id, d.alarm_codes, d.doc_date, d.status, d.error,
	d.pages, d.chunks, d.metadata, d.uploaded_by, d.created_at, d.updated_at, d.indexed_at, e.alias, e.name, u.full_name, m.name`

const docFrom = ` FROM rag.documents d LEFT JOIN asset.equipment e ON e.id = d.asset_id LEFT JOIN core.users u ON u.id = d.uploaded_by
	LEFT JOIN asset.models m ON m.id = d.model_id`

func scanDoc(row pgx.Row) (map[string]any, error) {
	var id, size int64
	var title, docType, source, key, fileName, contentType, siteCode, status string
	var sha, unit, assetType, errText, assetAlias, assetName, uploaderName, modelName *string
	var locationID, assetID, modelID, uploadedBy *int64
	var codes []string
	var docDate, indexedAt *time.Time
	var pages, chunks *int
	var metadata json.RawMessage
	var created, updated time.Time
	if err := row.Scan(&id, &title, &docType, &source, &key, &fileName, &contentType, &size, &sha, &siteCode, &locationID, &assetID,
		&unit, &assetType, &modelID, &codes, &docDate, &status, &errText, &pages, &chunks, &metadata, &uploadedBy, &created, &updated,
		&indexedAt, &assetAlias, &assetName, &uploaderName, &modelName); err != nil {
		return nil, err
	}
	var date any
	if docDate != nil {
		date = docDate.Format("2006-01-02")
	}
	return map[string]any{
		"id": id, "title": title, "doc_type": docType, "source": source, "object_key": key, "file_name": fileName,
		"content_type": contentType, "size_bytes": size, "sha256": sha, "site_code": siteCode, "location_id": locationID,
		"asset_id": assetID, "unit": unit, "asset_type": assetType, "model_id": modelID, "alarm_codes": emptyIfNil(codes),
		"doc_date": date, "status": status, "error": errText, "pages": pages, "chunks": chunks, "metadata": metadata,
		"uploaded_by": uploadedBy, "created_at": httpx.TS(created), "updated_at": httpx.TS(updated), "indexed_at": httpx.TSPtr(indexedAt),
		"asset_alias": assetAlias, "asset_name": assetName, "uploaded_by_name": uploaderName, "model_name": modelName,
		"file_url": fmt.Sprintf(docFileRoute, id),
	}, nil
}

// docScopeClause restricts documents (alias d) to the user's scope: documents
// tied to a location, an equipment or a unit outside the scope are hidden;
// general documents (manuals, references) are visible to everyone. "" for
// unrestricted users.
func (s *Server) docScopeClause(c *Ctx, args *sqlArgs) (string, error) {
	scope, err := c.Scope()
	if err != nil {
		return "", err
	}
	if scope == nil || scope.Unrestricted {
		return "", nil
	}
	locs := emptyIfNil(scope.LocationIDs())
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return "", err
	}
	keys, err := s.visibleAliasKeys(c, eq)
	if err != nil {
		return "", err
	}
	l, k := args.add(locs), args.add(emptyIfNil(keys))
	return fmt.Sprintf(`(d.location_id IS NULL OR d.location_id = ANY(%[1]s))
		AND (d.asset_id IS NULL OR EXISTS (SELECT 1 FROM asset.equipment se WHERE se.id = d.asset_id AND se.location_id = ANY(%[1]s)))
		AND (d.unit IS NULL OR lower(d.unit) = ANY(%[2]s))`, l, k), nil
}

// docLoad returns a visible document (404 otherwise).
func (s *Server) docLoad(c *Ctx, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, httpx.BadRequest("A document id is required")
	}
	var args sqlArgs
	where := "d.id = " + args.add(id)
	scope, err := s.docScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		where += " AND " + scope
	}
	doc, err := scanDoc(s.DB.QueryRow(c, `SELECT `+docColumns+docFrom+` WHERE `+where, args...))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Document not found")
		}
		return nil, err
	}
	return doc, nil
}

func (c *Ctx) requireDocsRead() error {
	if c.User.Can("aiinsights", "read") {
		return nil
	}
	return httpx.Forbidden("")
}

// docList lists the library. Input: doc_type?, status? (CSV), asset_type?,
// unit?, search?, limit (1..100, 20), offset -> {total, documents}.
func (s *Server) docList(c *Ctx) (any, error) {
	if err := c.requireDocsRead(); err != nil {
		return nil, err
	}
	var args sqlArgs
	conds := []string{"TRUE"}
	if list, ok := c.F.StringList("doc_type"); ok && len(list) > 0 {
		for _, t := range list {
			if !contains(docTypes, strings.ToLower(strings.TrimSpace(t))) {
				return nil, httpx.BadRequestf("Unknown document type %q", t)
			}
		}
		conds = append(conds, "d.doc_type = ANY("+args.add(lowerAll(list))+")")
	}
	if list, ok := c.F.StringList("status"); ok && len(list) > 0 {
		up := make([]string, len(list))
		for i, st := range list {
			up[i] = strings.ToUpper(strings.TrimSpace(st))
			if !contains(docStatuses, up[i]) {
				return nil, httpx.BadRequestf("Unknown document status %q", st)
			}
		}
		conds = append(conds, "d.status = ANY("+args.add(up)+")")
	}
	if v := c.F.StrTrim("asset_type"); v != "" {
		conds = append(conds, "lower(d.asset_type) = lower("+args.add(v)+")")
	}
	if v := c.F.StrTrim("unit"); v != "" {
		conds = append(conds, "lower(d.unit) = lower("+args.add(v)+")")
	}
	if v := c.F.StrTrim("search"); v != "" {
		p := args.add(ilike(v))
		conds = append(conds, "(d.title ILIKE "+p+" OR d.file_name ILIKE "+p+" OR d.unit ILIKE "+p+")")
	}
	scope, err := s.docScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		conds = append(conds, scope)
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM rag.documents d WHERE `+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	offset := c.F.Int("offset", 0)
	if offset < 0 {
		offset = 0
	}
	// (the query text is built before args is passed: the evaluation order of
	// args.add and args... within one call is unspecified)
	q := `SELECT ` + docColumns + docFrom + ` WHERE ` + where + ` ORDER BY d.created_at DESC, d.id DESC LIMIT ` +
		args.add(c.limit(20, 100)) + ` OFFSET ` + args.add(offset)
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := []map[string]any{}
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"total": total, "documents": docs}, nil
}

// docGet returns one document. Input: id.
func (s *Server) docGet(c *Ctx) (any, error) {
	if err := c.requireDocsRead(); err != nil {
		return nil, err
	}
	return s.docLoad(c, c.F.Int("id", 0))
}

// docUpload adds a PDF to the library (a person only). Input: multipart file
// (PDF, at most 50 MB), title, doc_type, asset_type?, unit? (equipment alias),
// doc_date? (YYYY-MM-DD), alarm_codes? (CSV) -> Doc (PENDING).
func (s *Server) docUpload(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "update"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	fh := c.F.File("file")
	if fh == nil {
		return nil, httpx.BadRequest("Choose a PDF file to upload")
	}
	if s.DocsStore == nil {
		return nil, &httpx.Error{Status: http.StatusServiceUnavailable, Msg: "Document storage is not configured"}
	}
	if fh.Size > docMaxBytes {
		return nil, httpx.BadRequest("The PDF is larger than 50 MB")
	}
	if fh.Size <= 0 {
		return nil, httpx.BadRequest("The file is empty")
	}
	docType := strings.ToLower(c.F.StrTrim("doc_type"))
	if docType == "" {
		docType = "other"
	}
	if !contains(docTypes, docType) {
		return nil, httpx.BadRequestf("Unknown document type %q", docType)
	}
	fileName := docFileName(fh.Filename)
	title := strings.TrimSpace(c.F.Str("title"))
	if title == "" {
		title = strings.TrimSuffix(fileName, path.Ext(fileName))
	}
	if len([]rune(title)) > docMaxTitle {
		return nil, httpx.BadRequestf("The title is too long (at most %d characters)", docMaxTitle)
	}
	var docDate *time.Time
	if v := c.F.StrTrim("doc_date"); v != "" && !strings.EqualFold(v, "null") {
		d, err := time.Parse("2006-01-02", v[:min(len(v), 10)])
		if err != nil {
			return nil, httpx.BadRequest("doc_date must be a date (YYYY-MM-DD)")
		}
		docDate = &d
	}

	// Equipment / type metadata (filters for retrieval).
	var assetID, locationID, modelID *int64
	var unit, assetType *string
	if v := c.F.StrTrim("asset_type"); v != "" {
		var name string
		if err := s.DB.QueryRow(c, `SELECT name FROM asset.types WHERE lower(name) = lower($1)`, v).Scan(&name); err != nil {
			if db.IsNoRows(err) {
				return nil, httpx.BadRequestf("Unknown equipment type %q", v)
			}
			return nil, err
		}
		assetType = &name
	}
	siteCode := ""
	if v := c.F.StrTrim("unit"); v != "" {
		var id, loc, model int64
		var alias, typeName string
		var site *string
		err := s.DB.QueryRow(c, `SELECT e.id, e.alias, e.location_id, m.id, t.name, v.site_code FROM asset.equipment e
			JOIN asset.models m ON m.id = e.model_id JOIN asset.types t ON t.id = m.type_id
			LEFT JOIN analytics.v_assets v ON v.id = e.id WHERE lower(e.alias) = lower($1)`, v).Scan(&id, &alias, &loc, &model, &typeName, &site)
		if err != nil {
			if db.IsNoRows(err) {
				return nil, httpx.BadRequestf("Unknown equipment %q", v)
			}
			return nil, err
		}
		if err := s.visibleAsset(c, id); err != nil {
			return nil, err
		}
		if assetType != nil && !strings.EqualFold(*assetType, typeName) {
			return nil, httpx.BadRequestf("%s is a %s, not a %s", alias, typeName, *assetType)
		}
		assetID, locationID, modelID, unit, assetType = &id, &loc, &model, &alias, &typeName
		if site != nil {
			siteCode = *site
		}
	}
	codes := []string{}
	if list, ok := c.F.StringList("alarm_codes"); ok && len(list) > 0 {
		for _, code := range list {
			var canonical string
			if err := s.DB.QueryRow(c, `SELECT code FROM dm.alarm_codes WHERE lower(code) = lower($1)`, strings.TrimSpace(code)).
				Scan(&canonical); err != nil {
				if db.IsNoRows(err) {
					return nil, httpx.BadRequestf("Unknown alarm code %q", code)
				}
				return nil, err
			}
			if !contains(codes, canonical) {
				codes = append(codes, canonical)
			}
		}
	}

	// Content: a real PDF (magic within the first KiB, as PDF readers accept),
	// hashed to refuse duplicates.
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	head := make([]byte, 1024)
	n, _ := io.ReadFull(f, head)
	if !bytes.Contains(head[:n], []byte("%PDF-")) {
		f.Close()
		return nil, httpx.BadRequest("The file is not a PDF document")
	}
	h := sha256.New()
	h.Write(head[:n])
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	sum := hex.EncodeToString(h.Sum(nil))
	var dup int64
	switch err := s.DB.QueryRow(c, `SELECT id FROM rag.documents WHERE sha256 = $1 LIMIT 1`, sum).Scan(&dup); {
	case err == nil:
		return nil, httpx.Conflict(fmt.Sprintf("This PDF is already in the library (document %d)", dup))
	case !db.IsNoRows(err):
		return nil, err
	}

	key := fmt.Sprintf("rag/%04d/%s.pdf", time.Now().UTC().Year(), newUUID())
	f, err = fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	saved, err := s.DocsStore.Put(c, key, "application/pdf", f, fh.Size)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c, `INSERT INTO rag.documents (title, doc_type, source, object_key, file_name, content_type, size_bytes,
				sha256, site_code, location_id, asset_id, unit, asset_type, model_id, alarm_codes, doc_date, status, uploaded_by)
			VALUES ($1, $2, 'upload', $3, $4, 'application/pdf', $5, $6,
				COALESCE(NULLIF($7, ''), (SELECT code FROM core.sites WHERE is_default), 'SGF1'),
				$8, $9, $10, $11, $12, $13, $14, 'PENDING', $15) RETURNING id`,
			title, docType, saved.Key, fileName, saved.Size, sum, siteCode, locationID, assetID, unit, assetType, modelID, codes,
			docDate, c.User.ID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(c, `SELECT pg_notify('rag_documents', $1::text)`, strconv.FormatInt(id, 10))
		return err
	})
	if err != nil {
		if derr := s.DocsStore.Delete(context.WithoutCancel(c), saved.Key); derr != nil {
			slog.Warn("remove orphan document object", "key", saved.Key, "err", derr)
		}
		return nil, fail(err)
	}
	slog.Info("document uploaded", "id", id, "title", title, "size", saved.Size, "user", c.User.Username)
	return s.docLoad(c, id)
}

// docDelete removes a document, its chunks and its PDF (a person only).
// Input: id.
func (s *Server) docDelete(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "update"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	doc, err := s.docLoad(c, c.F.Int("id", 0))
	if err != nil {
		return nil, err
	}
	id := doc["id"].(int64)
	var key string
	if err := s.DB.QueryRow(c, `DELETE FROM rag.documents WHERE id = $1 RETURNING object_key`, id).Scan(&key); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Document not found")
		}
		return nil, err
	}
	if s.DocsStore != nil {
		if err := s.DocsStore.Delete(context.WithoutCancel(c), key); err != nil {
			slog.Warn("delete document object", "key", key, "err", err)
		}
	}
	slog.Info("document deleted", "id", id, "user", c.User.Username)
	return map[string]any{"id": id, "deleted": true}, nil
}

// docReindex queues a document for indexing again. Input: id.
func (s *Server) docReindex(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "update"); err != nil {
		return nil, err
	}
	doc, err := s.docLoad(c, c.F.Int("id", 0))
	if err != nil {
		return nil, err
	}
	id := doc["id"].(int64)
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		tag, err := tx.Exec(c, `UPDATE rag.documents SET status = 'PENDING', error = NULL, updated_at = now()
			WHERE id = $1 AND status <> 'INDEXING'`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.Conflict("The document is being indexed, try again when it is done")
		}
		_, err = tx.Exec(c, `SELECT pg_notify('rag_documents', $1::text)`, strconv.FormatInt(id, 10))
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.docLoad(c, id)
}

// serveDocumentFile streams a document's PDF from the private bucket:
// GET /api/ai/documents/{id}/file with the session header, for users with
// aiinsights.read or aiassistant.read who can see the document. Range
// requests are supported (progressive PDF viewing).
func (s *Server) serveDocumentFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpx.WriteError(w, r, &httpx.Error{Status: http.StatusMethodNotAllowed, Msg: "Method not allowed"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteError(w, r, httpx.NotFound("Document not found"))
		return
	}
	u, err := s.Auth.Authenticate(r.Context(), r.Header.Get(auth.SessionHeader))
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal(err))
		return
	}
	if u == nil {
		httpx.WriteError(w, r, httpx.Unauthorized())
		return
	}
	if !u.Can("aiinsights", "read") && !u.Can("aiassistant", "read") {
		httpx.WriteError(w, r, httpx.Forbidden(""))
		return
	}
	c := &Ctx{Context: r.Context(), R: r, W: w, F: httpx.NewForm(nil), User: u, s: s}
	doc, err := s.docLoad(c, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if s.DocsStore == nil {
		httpx.WriteError(w, r, &httpx.Error{Status: http.StatusServiceUnavailable, Msg: "Document storage is not configured"})
		return
	}
	obj, err := s.DocsStore.Open(r.Context(), doc["object_key"].(string))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.WriteError(w, r, httpx.NotFound("The document file is missing"))
			return
		}
		httpx.WriteError(w, r, httpx.Internal(err))
		return
	}
	defer obj.Close()
	name, _ := doc["file_name"].(string)
	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	disposition := mime.FormatMediaType("inline", map[string]string{"filename": name})
	if disposition == "" {
		disposition = "inline"
	}
	h.Set("Content-Disposition", disposition)
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if obj.ETag != "" {
		h.Set("ETag", strconv.Quote(obj.ETag))
	}
	http.ServeContent(w, r, "", obj.ModTime, obj)
}

// docFileName returns the base name of an uploaded file, without control
// characters, at most docMaxName characters, ending in .pdf.
func docFileName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" {
		name = "document.pdf"
	}
	if !strings.EqualFold(path.Ext(name), ".pdf") {
		name += ".pdf"
	}
	if rs := []rune(name); len(rs) > docMaxName {
		name = string(rs[:docMaxName-4]) + ".pdf"
	}
	return name
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b[:])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func lowerAll(list []string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}
