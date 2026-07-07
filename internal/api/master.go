package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

// Master data tables editable from the master data page. The frontend
// addresses them by "<schema>.<table>"; the alarm list ("dm.alarms") is the
// alarm code master (dm.alarm_codes).
type masterTable struct {
	table    string // physical table
	order    string // natural order used to number rows
	category string // column scoping rows to a category ("" for none)
	alarm    bool   // dm.alarm_codes (code + name)
	issue    bool   // feedback issues (issue_code)
}

var masterTables = map[string]masterTable{
	"asset.types":                  {table: "asset.types", order: "order_id, lower(name), id"},
	"asset.brands":                 {table: "asset.brands", order: "order_id, lower(name), id"},
	"asset.series":                 {table: "asset.series", order: "order_id, lower(name), id"},
	"dm.alarms":                    {table: "dm.alarm_codes", order: "order_id, lower(code), id", alarm: true},
	"feedback.feedback_categories": {table: "asset.feedback_categories", order: "order_id, lower(name), id"},
	"feedback.feedback_issues":     {table: "asset.feedback_issues", order: "order_id, lower(name), id", category: "category_id", issue: true},
	"asset.power_meter_types":      {table: "asset.power_meter_types", order: "order_id, lower(name), id"},
	"cost.cost_types":              {table: "asset.cost_types", order: "order_id, lower(name), id"},
	"asset.source_types":           {table: "asset.source_types", order: "order_id, lower(name), id", category: "category_id"},
}

// Fixed categories of the equipment group (ids used by the frontend).
var equipmentCategories = []struct {
	id    int64
	name  string
	table string
}{{1, "Types", "asset.types"}, {2, "Brands", "asset.brands"}, {3, "Series", "asset.series"}, {4, "Alarms", "dm.alarms"}}

func (s *Server) registerMaster() {
	p := "master/"
	s.handle(p+"getdatasetcount", AccessUser, s.mdDatasetCount)
	s.handle(p+"getdatacategories", AccessUser, s.mdCategories)
	s.handle(p+"getdatafields", AccessUser, s.mdFields)
	s.handle(p+"getissueidbyid", AccessUser, s.mdIssueCode)
	s.handle(p+"getcategorynamebyid", AccessUser, s.mdCategoryName)
	s.handle(p+"addcategory", AccessUser, s.mdAddCategory)
	s.handle(p+"addfeedbackissue", AccessUser, s.mdAddFeedbackIssue)
	s.handle(p+"addinspectionissue", AccessUser, s.mdAddFeedbackIssue)
	s.handle(p+"addsourcetype", AccessUser, s.mdAddSourceType)
	s.handle(p+"addcosttype", AccessUser, s.mdAddCostType)
	s.handle(p+"adddefect", AccessUser, s.mdAddDefect)
	s.handle(p+"addtypebrandseries", AccessUser, s.mdAddTypeBrandSeries)
	s.handle(p+"updateorder", AccessUser, s.mdUpdateOrder)
	s.handle(p+"updatename", AccessUser, s.mdUpdateName)
	s.handle(p+"deleteentry", AccessUser, s.mdDelete)
	s.handle(p+"bulkimport", AccessUser, s.mdBulkImport)
}

func (c *Ctx) masterTable() (string, masterTable, error) {
	key := strings.ToLower(strings.TrimSpace(c.F.Str("schema")) + "." + strings.TrimSpace(c.F.Str("table")))
	t, ok := masterTables[key]
	if !ok {
		return key, t, httpx.BadRequest("Invalid master data table")
	}
	return key, t, nil
}

// splitAlarmCode splits "CODE - Name" (or "CODE: Name") into code and name.
func splitAlarmCode(text string) (string, string) {
	text = strings.TrimSpace(text)
	for _, sep := range []string{" - ", " – ", ": "} {
		if i := strings.Index(text, sep); i > 0 {
			code := strings.TrimSpace(text[:i])
			name := strings.TrimSpace(text[i+len(sep):])
			if code != "" && name != "" {
				return code, name
			}
		}
	}
	return text, text
}

func (t masterTable) nameExpr() string {
	if t.alarm {
		return "CASE WHEN name = '' OR lower(name) = lower(code) THEN code ELSE code || ' - ' || name END"
	}
	return "name"
}

func (t masterTable) scope(categoryID int64) (string, []any) {
	if t.category == "" {
		return "TRUE", nil
	}
	return t.category + " = $1", []any{categoryID}
}

// renumber compacts order_id to 1..n following the natural order.
func renumber(c *Ctx, tx pgx.Tx, t masterTable, categoryID int64) error {
	where, args := t.scope(categoryID)
	_, err := tx.Exec(c, `UPDATE `+t.table+` x SET order_id = r.rn FROM (
		SELECT id, row_number() OVER (ORDER BY `+t.order+`) AS rn FROM `+t.table+` WHERE `+where+`) r
		WHERE x.id = r.id AND x.order_id <> r.rn`, args...)
	return err
}

func nextOrder(c *Ctx, q db.Querier, t masterTable, categoryID int64) (int, error) {
	where, args := t.scope(categoryID)
	var n int
	err := q.QueryRow(c, `SELECT COALESCE(max(order_id), 0) + 1 FROM `+t.table+` WHERE `+where, args...).Scan(&n)
	return n, err
}

func (s *Server) mdDatasetCount(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "read"); err != nil {
		return nil, err
	}
	var equipment, feedback int64
	if err := s.DB.QueryRow(c, `SELECT (SELECT count(*) FROM asset.types) + (SELECT count(*) FROM asset.brands) +
		(SELECT count(*) FROM asset.series) + (SELECT count(*) FROM dm.alarm_codes), (SELECT count(*) FROM asset.feedback_issues)`).
		Scan(&equipment, &feedback); err != nil {
		return nil, err
	}
	return []map[string]any{{"group": "equipment", "count": equipment}, {"group": "feedback", "count": feedback}}, nil
}

func (s *Server) mdList(c *Ctx, t masterTable, categoryID int64, search string) ([]map[string]any, error) {
	where, args := t.scope(categoryID)
	args = append(args, ilike(search))
	n := len(args)
	rows, err := s.DB.Query(c, fmt.Sprintf(`SELECT id, label, rn FROM (
			SELECT id, %s AS label, row_number() OVER (ORDER BY %s) AS rn FROM %s WHERE %s) x
		WHERE label ILIKE $%d ORDER BY rn`, t.nameExpr(), t.order, t.table, where, n), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, rn int64
		var name string
		if err := rows.Scan(&id, &name, &rn); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name, "order_id": rn})
	}
	return out, rows.Err()
}

func (s *Server) mdCategories(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "read"); err != nil {
		return nil, err
	}
	schema := strings.ToLower(strings.TrimSpace(c.F.Str("schema")))
	search := strings.ToLower(strings.TrimSpace(c.F.Str("search_filter")))
	switch schema {
	case "asset":
		out := []map[string]any{}
		for _, cat := range equipmentCategories {
			if search == "" || strings.Contains(strings.ToLower(cat.name), search) {
				out = append(out, map[string]any{"id": cat.id, "name": cat.name, "order_id": cat.id})
			}
		}
		return out, nil
	case "feedback":
		return s.mdList(c, masterTables["feedback.feedback_categories"], 0, search)
	}
	return []map[string]any{}, nil
}

func (s *Server) mdFields(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "read"); err != nil {
		return nil, err
	}
	_, t, err := c.masterTable()
	if err != nil {
		return []map[string]any{}, nil
	}
	return s.mdList(c, t, c.F.Int("category_id", 0), c.F.Str("search_filter"))
}

func (s *Server) mdIssueCode(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "read"); err != nil {
		return nil, err
	}
	var code string
	if err := s.DB.QueryRow(c, `SELECT issue_code FROM asset.feedback_issues WHERE id = $1`, c.F.Int("id", 0)).Scan(&code); err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return code, nil
}

func (s *Server) mdCategoryName(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "read"); err != nil {
		return nil, err
	}
	key, t, err := c.masterTable()
	if err != nil {
		return nil, nil
	}
	id := c.F.Int("id", 0)
	if key == "asset.power_meter_types" || key == "cost.cost_types" || key == "asset.source_types" {
		return nil, nil
	}
	var name string
	if err := s.DB.QueryRow(c, `SELECT `+t.nameExpr()+` FROM `+t.table+` WHERE id = $1`, id).Scan(&name); err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return name, nil
}

func duplicateName() error { return httpx.BadRequest("Data with the same name already exists") }

// insertSimple inserts a named row at the end of the list.
func (s *Server) insertSimple(c *Ctx, t masterTable, categoryID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return httpx.BadRequest("err_name_is_required")
	}
	return s.DB.Tx(c, func(tx pgx.Tx) error { return insertMaster(c, tx, t, categoryID, name, "") })
}

func insertMaster(c *Ctx, tx pgx.Tx, t masterTable, categoryID int64, name, issueCode string) error {
	order, err := nextOrder(c, tx, t, categoryID)
	if err != nil {
		return err
	}
	switch {
	case t.alarm:
		code, label := splitAlarmCode(name)
		_, err = tx.Exec(c, `INSERT INTO dm.alarm_codes (code, name, order_id) VALUES ($1, $2, $3)`, code, label, order)
	case t.issue:
		var exists bool
		if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.feedback_issues WHERE category_id = $1 AND lower(name) = lower($2))`,
			categoryID, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return duplicateName()
		}
		_, err = tx.Exec(c, `INSERT INTO asset.feedback_issues (category_id, name, issue_code, order_id) VALUES ($1, $2, $3, $4)`,
			categoryID, name, strings.TrimSpace(issueCode), order)
		if db.IsForeignKeyViolation(err) {
			return httpx.BadRequest("err_category_not_found")
		}
	case t.category != "":
		var exists bool
		if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM `+t.table+` WHERE `+t.category+` = $1 AND lower(name) = lower($2))`,
			categoryID, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return duplicateName()
		}
		_, err = tx.Exec(c, `INSERT INTO `+t.table+` (`+t.category+`, name, order_id) VALUES ($1, $2, $3)`, categoryID, name, order)
	default:
		var exists bool
		if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM `+t.table+` WHERE lower(name) = lower($1))`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return duplicateName()
		}
		_, err = tx.Exec(c, `INSERT INTO `+t.table+` (name, order_id) VALUES ($1, $2)`, name, order)
	}
	if db.IsUniqueViolation(err) {
		return duplicateName()
	}
	return err
}

func (s *Server) mdAddCategory(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	key, t, err := c.masterTable()
	if err != nil {
		return nil, err
	}
	if key != "feedback.feedback_categories" {
		return nil, httpx.BadRequest("Categories of this group cannot be changed")
	}
	if err := s.insertSimple(c, t, 0, c.F.Str("category_name")); err != nil {
		return nil, err
	}
	return "success", nil
}

func (s *Server) mdAddFeedbackIssue(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	name := c.F.StrTrim("name")
	if name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	t := masterTables["feedback.feedback_issues"]
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		return insertMaster(c, tx, t, c.F.Int("category_id", 0), name, c.F.Str("issue_id"))
	})
	if err != nil {
		return nil, err
	}
	return "success", nil
}

func (s *Server) mdAddSourceType(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	if err := s.insertSimple(c, masterTables["asset.source_types"], c.F.Int("category_id", 0), c.F.Str("name")); err != nil {
		return nil, err
	}
	return "success", nil
}

func (s *Server) mdAddCostType(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	if err := s.insertSimple(c, masterTables["cost.cost_types"], 0, c.F.Str("name")); err != nil {
		return nil, err
	}
	return "success", nil
}

func (s *Server) mdAddDefect(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	return nil, httpx.BadRequest("Production master data is not available")
}

func (s *Server) mdAddTypeBrandSeries(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	table := strings.ToLower(strings.TrimSpace(c.F.Str("table")))
	key := "asset." + table
	if table == "alarms" {
		key = "dm.alarms"
	}
	t, ok := masterTables[key]
	if !ok || (key != "asset.types" && key != "asset.brands" && key != "asset.series" && key != "dm.alarms") {
		return nil, httpx.BadRequest("Invalid master data table")
	}
	if err := s.insertSimple(c, t, 0, c.F.Str("field_name")); err != nil {
		return nil, err
	}
	if t.alarm {
		s.Engine.Invalidate()
	}
	return "success", nil
}

func (s *Server) masterCategoryOf(c *Ctx, q db.Querier, t masterTable, id int64) (int64, error) {
	if t.category == "" {
		var ok bool
		if err := q.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM `+t.table+` WHERE id = $1)`, id).Scan(&ok); err != nil {
			return 0, err
		}
		if !ok {
			return 0, httpx.NotFound("Data not found")
		}
		return 0, nil
	}
	var cat int64
	if err := q.QueryRow(c, `SELECT `+t.category+` FROM `+t.table+` WHERE id = $1`, id).Scan(&cat); err != nil {
		if db.IsNoRows(err) {
			return 0, httpx.NotFound("Data not found")
		}
		return 0, err
	}
	return cat, nil
}

func (s *Server) mdUpdateOrder(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "update"); err != nil {
		return nil, err
	}
	_, t, err := c.masterTable()
	if err != nil {
		return nil, err
	}
	src, dst := c.F.Int("source_id", 0), c.F.Int("target_id", 0)
	if src == dst {
		return "success", nil
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		cat, err := s.masterCategoryOf(c, tx, t, src)
		if err != nil {
			return err
		}
		cat2, err := s.masterCategoryOf(c, tx, t, dst)
		if err != nil {
			return err
		}
		if cat != cat2 {
			return httpx.BadRequest("Entries belong to different categories")
		}
		if _, err := tx.Exec(c, `LOCK TABLE `+t.table+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		if err := renumber(c, tx, t, cat); err != nil {
			return err
		}
		var from, to int
		if err := tx.QueryRow(c, `SELECT order_id FROM `+t.table+` WHERE id = $1`, src).Scan(&from); err != nil {
			return err
		}
		if err := tx.QueryRow(c, `SELECT order_id FROM `+t.table+` WHERE id = $1`, dst).Scan(&to); err != nil {
			return err
		}
		where, args := t.scope(cat)
		n := len(args)
		args = append(args, from, to)
		if from < to {
			_, err = tx.Exec(c, fmt.Sprintf(`UPDATE %s SET order_id = order_id - 1 WHERE %s AND order_id > $%d AND order_id <= $%d`,
				t.table, where, n+1, n+2), args...)
		} else {
			_, err = tx.Exec(c, fmt.Sprintf(`UPDATE %s SET order_id = order_id + 1 WHERE %s AND order_id >= $%d AND order_id < $%d`,
				t.table, where, n+2, n+1), args...)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(c, `UPDATE `+t.table+` SET order_id = $2 WHERE id = $1`, src, to)
		return err
	})
	if err != nil {
		return nil, err
	}
	return "success", nil
}

func (s *Server) mdUpdateName(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "update"); err != nil {
		return nil, err
	}
	_, t, err := c.masterTable()
	if err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	name := c.F.StrTrim("updated_name")
	if name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		cat, err := s.masterCategoryOf(c, tx, t, id)
		if err != nil {
			return err
		}
		switch {
		case t.alarm:
			code, label := splitAlarmCode(name)
			_, err = tx.Exec(c, `UPDATE dm.alarm_codes SET code = $2, name = $3 WHERE id = $1`, id, code, label)
		case t.issue:
			var exists bool
			if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.feedback_issues WHERE category_id = $1 AND lower(name) = lower($2) AND id <> $3)`,
				cat, name, id).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return duplicateName()
			}
			_, err = tx.Exec(c, `UPDATE asset.feedback_issues SET name = $2, issue_code = $3 WHERE id = $1`, id, name, c.F.StrTrim("updated_issue_id"))
		default:
			where, args := t.scope(cat)
			args = append(args, name, id)
			var exists bool
			if err := tx.QueryRow(c, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s AND lower(name) = lower($%d) AND id <> $%d)`,
				t.table, where, len(args)-1, len(args)), args...).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return duplicateName()
			}
			_, err = tx.Exec(c, `UPDATE `+t.table+` SET name = $2 WHERE id = $1`, id, name)
		}
		if db.IsUniqueViolation(err) {
			return duplicateName()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "success", nil
}

func (s *Server) mdDelete(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "delete"); err != nil {
		return nil, err
	}
	_, t, err := c.masterTable()
	if err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		cat, err := s.masterCategoryOf(c, tx, t, id)
		if err != nil {
			return err
		}
		if t.alarm {
			var used bool
			if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM dm.maintenance_order_issues WHERE alarm_code_id = $1)
				OR EXISTS (SELECT 1 FROM dm.instructions WHERE alarm_code_id = $1 AND NOT is_deleted)`, id).Scan(&used); err != nil {
				return err
			}
			if used {
				return httpx.BadRequest("Data is still in use and cannot be deleted")
			}
		}
		if _, err := tx.Exec(c, `DELETE FROM `+t.table+` WHERE id = $1`, id); err != nil {
			if db.IsForeignKeyViolation(err) {
				return httpx.BadRequest("Data is still in use and cannot be deleted")
			}
			return err
		}
		return renumber(c, tx, t, cat)
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "success", nil
}

type bulkSheet struct {
	Categories []string `json:"categories"`
	Fields     []struct {
		Category string `json:"category"`
		Name     string `json:"name"`
		FieldID  string `json:"field_id"`
	} `json:"fields"`
}

func (s *Server) mdBulkImport(c *Ctx) (any, error) {
	if err := c.Require("masterdata", "create"); err != nil {
		return nil, err
	}
	var payload map[string]*bulkSheet
	if err := json.Unmarshal([]byte(c.F.Str("payload")), &payload); err != nil {
		return nil, httpx.BadRequest("Invalid file")
	}
	added := 0
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		insert := func(t masterTable, cat int64, name, code string) error {
			name = strings.TrimSpace(name)
			if name == "" {
				return nil
			}
			sp, err := tx.Begin(c)
			if err != nil {
				return err
			}
			if err := insertMaster(c, sp, t, cat, name, code); err != nil {
				_ = sp.Rollback(c)
				var he *httpx.Error
				if errorsAs(err, &he) {
					return nil // duplicates are skipped
				}
				return err
			}
			added++
			return sp.Commit(c)
		}
		if sheet := payload["equipment"]; sheet != nil {
			for _, f := range sheet.Fields {
				key := ""
				switch strings.ToLower(strings.TrimSpace(f.Category)) {
				case "type", "types", "1":
					key = "asset.types"
				case "brand", "brands", "2":
					key = "asset.brands"
				case "series", "3":
					key = "asset.series"
				case "alarm", "alarms", "alarm code", "alarm codes", "4":
					key = "dm.alarms"
				default:
					continue
				}
				if err := insert(masterTables[key], 0, f.Name, ""); err != nil {
					return err
				}
			}
		}
		if sheet := payload["feedback"]; sheet != nil {
			cats := masterTables["feedback.feedback_categories"]
			for _, name := range sheet.Categories {
				if err := insert(cats, 0, name, ""); err != nil {
					return err
				}
			}
			for _, f := range sheet.Fields {
				var catID int64
				if err := tx.QueryRow(c, `SELECT id FROM asset.feedback_categories WHERE lower(name) = lower($1)`, strings.TrimSpace(f.Category)).Scan(&catID); err != nil {
					if db.IsNoRows(err) {
						continue
					}
					return err
				}
				if err := insert(masterTables["feedback.feedback_issues"], catID, f.Name, f.FieldID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return fmt.Sprintf("%d entries imported", added), nil
}
