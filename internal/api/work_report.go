package api

import (
	"context"
	"strings"
	"time"

	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/maint"
	"afdd/internal/storage"
)

func (s *Server) registerWorkReport() {
	p := "maintenance_order_work_report/"
	s.handle(p+"get_work_report", AccessUser, s.wrGet)
	s.handle(p+"get_work_report_status", AccessUser, s.wrStatus)
	s.handle(p+"get_start_stop_ts", AccessUser, s.wrStartStop)
	s.handle(p+"start_work_report", AccessUser, s.wrStart)
	s.handle(p+"pause_work_report", AccessUser, s.wrPause)
	s.handle(p+"resume_work_report", AccessUser, s.wrResume)
	s.handle(p+"set_work_report_status_to_done", AccessUser, s.wrDone)
	s.handle(p+"close_work_report", AccessUser, s.wrClose)
	s.handle(p+"upload_image", AccessUser, s.wrUploadImage)
	s.handle(p+"delete_image", AccessUser, s.wrDeleteImage)
	s.handle(p+"add_work_update", AccessUser, s.wrAddUpdate)
	s.handle(p+"get_asset_status", AccessUser, s.wrAssetStatus)
	s.handle(p+"update_issue_validity", AccessUser, s.wrUpdateValidity)
}

// instructionItem is an instruction of an order issue.
type instructionItem struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	TypeName    string   `json:"type_name"`
	Content     *string  `json:"content"`
	IsUsingPdf  bool     `json:"is_using_pdf"`
	PdfURL      *string  `json:"pdf_url"`
	PdfName     *string  `json:"pdf_name"`
	Description string   `json:"description"`
	Duration    int      `json:"duration"`
	CreatedBy   string   `json:"created_by"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   any      `json:"updated_at"`
	ModelNames  []string `json:"model_names"`
	CodeName    string   `json:"code_name"`
	IsOther     bool     `json:"is_other"`
	Order       int      `json:"order"`
	TypeID      int      `json:"type_id"`
	CodeID      int64    `json:"code_id"`
	IsValid     *bool    `json:"is_valid"`
}

type orderIssue struct {
	ID        int64
	Code      string
	Name      string
	IsOther   bool
	ValidDiag *bool
	ValidRep  *bool
	Status    string
	Detected  *time.Time
}

func (s *Server) orderIssues(ctx context.Context, orderID int64) ([]orderIssue, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id, c.code, c.name, i.is_other, i.is_valid_diagnostic, i.is_valid_repair, i.alarm_status, i.detected_at
		FROM dm.maintenance_order_issues i JOIN dm.alarm_codes c ON c.id = i.alarm_code_id WHERE i.order_id = $1 ORDER BY i.position, c.code`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orderIssue
	for rows.Next() {
		var it orderIssue
		if err := rows.Scan(&it.ID, &it.Code, &it.Name, &it.IsOther, &it.ValidDiag, &it.ValidRep, &it.Status, &it.Detected); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// orderInstructions returns the instructions (of the order's equipment model)
// for every issue of the order.
func (s *Server) orderInstructions(ctx context.Context, o *maint.Order, issues []orderIssue) ([]instructionItem, error) {
	out := []instructionItem{}
	for pos, is := range issues {
		rows, err := s.DB.Query(ctx, `SELECT i.id, i.title, t.name, i.content, i.pdf_url, i.pdf_name, i.description, i.duration,
				COALESCE(NULLIF(u.full_name, ''), u.username, ''), i.created_at, i.updated_at, i.instruction_type_id,
				COALESCE((SELECT array_agg(m.name ORDER BY m.name) FROM dm.instruction_models x JOIN asset.models m ON m.id = x.model_id
					WHERE x.instruction_id = i.id), '{}')
			FROM dm.instructions i JOIN dm.instruction_types t ON t.id = i.instruction_type_id
			JOIN dm.instruction_models im ON im.instruction_id = i.id AND im.model_id = $2
			LEFT JOIN core.users u ON u.id = i.created_by
			WHERE i.alarm_code_id = $1 AND NOT i.is_deleted ORDER BY i.instruction_type_id, i.title`, is.ID, o.ModelID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var it instructionItem
			var created time.Time
			var updated *time.Time
			if err := rows.Scan(&it.ID, &it.Name, &it.TypeName, &it.Content, &it.PdfURL, &it.PdfName, &it.Description, &it.Duration,
				&it.CreatedBy, &created, &updated, &it.TypeID, &it.ModelNames); err != nil {
				rows.Close()
				return nil, err
			}
			it.CreatedAt = httpx.TS(created)
			it.UpdatedAt = httpx.TSPtr(updated)
			it.IsUsingPdf = it.PdfURL != nil && *it.PdfURL != ""
			it.CodeName = is.Code + " - " + is.Name
			it.CodeID = is.ID
			it.IsOther = is.IsOther
			it.Order = pos + 1
			switch it.TypeID {
			case 1:
				it.IsValid = is.ValidDiag
			case 2:
				it.IsValid = is.ValidRep
			}
			if it.ModelNames == nil {
				it.ModelNames = []string{}
			}
			out = append(out, it)
		}
		rows.Close()
	}
	return out, nil
}

func (s *Server) wrGet(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	issues, err := s.orderIssues(c, o.ID)
	if err != nil {
		return nil, err
	}
	instructions, err := s.orderInstructions(c, o, issues)
	if err != nil {
		return nil, err
	}
	techs, err := maint.Technicians(c, s.DB, o.ID)
	if err != nil {
		return nil, err
	}
	var minutes int64
	for _, it := range instructions {
		if !it.IsOther {
			minutes += int64(it.Duration)
		}
	}
	if minutes == 0 {
		minutes = maint.EstimatedSeconds(c, s.DB, o) / 60
	}
	typeName := "Preventive"
	if o.TypeID == maint.TypeCorrective {
		typeName = "Corrective"
	}
	var createdBy *string
	if o.RequestedBy != nil {
		v := userDisplay(c, s.DB, o.RequestedBy)
		createdBy = &v
	} else if o.FromSystem {
		v := "System"
		createdBy = &v
	}
	issueRows := []map[string]any{}
	for _, is := range issues {
		issueRows = append(issueRows, map[string]any{"id": is.ID, "name": is.Code + " - " + is.Name, "is_other": is.IsOther,
			"is_valid_diagnostic": is.ValidDiag, "is_valid_repair": is.ValidRep})
	}
	return map[string]any{
		"maintenance_name": o.Name, "maintenance_type_name": typeName, "created_at": httpx.TS(o.CreatedAt), "created_by": createdBy,
		"estimated_time_minute": minutes, "instructions": instructions, "personnels": emptyIfNil(techs),
		"is_degradation": o.IsDeduction, "degradation_name": o.DegradationName, "issues": issueRows,
	}, nil
}

func (s *Server) wrStatus(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM dm.work_updates WHERE order_id = $1`, o.ID).Scan(&n); err != nil {
		return nil, err
	}
	var updated any
	if o.UpdatedAt != nil {
		updated = httpx.TS(*o.UpdatedAt)
	}
	return map[string]any{"status": maint.StatusLabel(o.StatusID), "updated_at": updated, "has_work_update": n > 0}, nil
}

func (s *Server) wrStartStop(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	stopped := o.CompletedAt
	if stopped == nil {
		stopped = o.ClosedAt
	}
	return map[string]any{"started_at": httpx.TSPtr(o.StartedAt), "stopped_at": httpx.TSPtr(stopped)}, nil
}

// requireTechnician checks the user may report on an order.
func (s *Server) requireTechnician(c *Ctx, o *maint.Order) error {
	if c.User.IsSuperuser() {
		return nil
	}
	if !c.User.Can("maintenanceorder", "report") {
		return httpx.Forbidden("")
	}
	if !maint.IsAssigned(c, s.DB, o.ID, c.User.ID) && !c.User.IsAdmin() {
		return httpx.Forbidden("Only assigned technicians can update this maintenance order")
	}
	return nil
}

func (s *Server) wrOrder(c *Ctx, technician bool) (*maint.Order, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	if technician {
		if err := s.requireTechnician(c, o); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (s *Server) wrStart(c *Ctx) (any, error) {
	o, err := s.wrOrder(c, true)
	if err != nil {
		return nil, err
	}
	if err := s.Maint.Start(c, c.actor(), o.ID, c.F.Bool("is_status_change", false)); err != nil {
		return nil, fail(err)
	}
	return "Maintenance order started", nil
}

func (s *Server) wrPause(c *Ctx) (any, error) {
	o, err := s.wrOrder(c, true)
	if err != nil {
		return nil, err
	}
	if err := s.Maint.Pause(c, c.actor(), o.ID, c.F.Str("reason")); err != nil {
		return nil, fail(err)
	}
	return "Maintenance order paused", nil
}

func (s *Server) wrResume(c *Ctx) (any, error) {
	o, err := s.wrOrder(c, true)
	if err != nil {
		return nil, err
	}
	if err := s.Maint.Resume(c, c.actor(), o.ID); err != nil {
		return nil, fail(err)
	}
	return "Maintenance order resumed", nil
}

func (c *Ctx) validities() ([]maint.IssueValidity, error) {
	var items []maint.IssueValidity
	if _, err := c.F.JSON("issues", &items); err != nil {
		return nil, httpx.BadRequest("Invalid issues")
	}
	return items, nil
}

func (s *Server) wrDone(c *Ctx) (any, error) {
	o, err := s.wrOrder(c, true)
	if err != nil {
		return nil, err
	}
	items, err := c.validities()
	if err != nil {
		return nil, err
	}
	if err := s.Maint.Done(c, c.actor(), o.ID, items); err != nil {
		return nil, fail(err)
	}
	return "Maintenance order completed", nil
}

func (s *Server) wrClose(c *Ctx) (any, error) {
	if err := c.Require("maintenanceorder", "approve"); err != nil {
		return nil, err
	}
	o, err := s.wrOrder(c, false)
	if err != nil {
		return nil, err
	}
	items, err := c.validities()
	if err != nil {
		return nil, err
	}
	if err := s.Maint.Close(c, c.actor(), o.ID, items); err != nil {
		return nil, fail(err)
	}
	return "Maintenance order closed", nil
}

const workReportFolder = "work_reports"

func (s *Server) wrUploadImage(c *Ctx) (any, error) {
	if !c.User.Can("maintenanceorder", "report") && !c.User.Can("maintenanceorder", "approve") {
		return nil, httpx.Forbidden("")
	}
	fh := c.F.File("image")
	if fh == nil {
		return nil, httpx.BadRequest("err_image_required")
	}
	if fh.Size > 20<<20 {
		return nil, httpx.BadRequest("File size is too large")
	}
	saved, err := s.Store.SaveMultipart(c, workReportFolder, fh, storage.AllowedImageTypes)
	if err != nil {
		return nil, fail(err)
	}
	_, _ = s.DB.Exec(c, `INSERT INTO core.files (object_key, original_name, content_type, size, created_by) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (object_key) DO NOTHING`, saved.Key, saved.Name, saved.ContentType, saved.Size, c.User.ID)
	return map[string]any{"url": saved.URL, "location": saved.Key}, nil
}

func (s *Server) wrDeleteImage(c *Ctx) (any, error) {
	key := strings.TrimSpace(c.F.Str("image_location"))
	if k := storage.KeyFromURL(key); k != "" {
		key = k
	}
	if !strings.HasPrefix(key, workReportFolder+"/") || strings.Contains(key, "..") {
		return nil, httpx.BadRequest("Invalid image location")
	}
	var used bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM dm.work_updates WHERE $1 = ANY(image_urls))`, storage.PublicPrefix+key).Scan(&used); err != nil {
		return nil, err
	}
	if used {
		return nil, httpx.BadRequest("Image is already attached to a work update")
	}
	var owner *int64
	_ = s.DB.QueryRow(c, `SELECT created_by FROM core.files WHERE object_key = $1`, key).Scan(&owner)
	if owner != nil && *owner != c.User.ID && !c.User.IsAdmin() {
		return nil, httpx.Forbidden("")
	}
	if err := s.Store.Delete(c, key); err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(c, `DELETE FROM core.files WHERE object_key = $1`, key)
	return "Image deleted", nil
}

func (s *Server) wrAddUpdate(c *Ctx) (any, error) {
	o, err := s.wrOrder(c, true)
	if err != nil {
		return nil, err
	}
	var images []string
	if list, ok := c.F.StringList("image_urls"); ok {
		for _, u := range list {
			if k := storage.KeyFromURL(u); k != "" && strings.HasPrefix(k, workReportFolder+"/") {
				images = append(images, storage.PublicPrefix+k)
			}
		}
	}
	issueIDs, _ := c.F.IDs("issue_ids")
	if err := s.Maint.AddWorkUpdate(c, c.actor(), o.ID, c.F.Str("note"), images, issueIDs); err != nil {
		return nil, fail(err)
	}
	return "Your work update has been saved", nil
}

func (s *Server) wrAssetStatus(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, []int64{o.AssetID})
	if err != nil {
		return nil, err
	}
	status := engine.StatusName(engine.StatusOffline)
	if st := states[o.AssetID]; st != nil && st.Status != 0 {
		status = engine.StatusName(st.Status)
	}
	return map[string]any{"asset_status": status}, nil
}

func (s *Server) wrUpdateValidity(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	if !c.User.Can("maintenanceorder", "approve") {
		if err := s.requireTechnician(c, o); err != nil {
			return nil, err
		}
	}
	iv := maint.IssueValidity{AlarmCodeID: c.F.Int("alarm_code_id", 0), InstructionTypeID: c.F.Int("instruction_type_id", 0), IsValid: c.F.BoolPtr("is_valid")}
	if err := s.Maint.UpdateIssueValidity(c, c.actor(), o.ID, iv); err != nil {
		return nil, fail(err)
	}
	return "Issue validity updated", nil
}
