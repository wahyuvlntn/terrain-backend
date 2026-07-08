package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

func (s *Server) registerMapping() {
	a := "alarm_mapping/"
	s.handle(a+"get_alarm_codes_mapping", AccessSuper, s.amCodes)
	s.handle(a+"get_parameter_list", AccessSuper, s.mapParameters)
	s.handle(a+"get_selected_parameters", AccessSuper, s.amSelected)
	s.handle(a+"save_selected_parameters", AccessSuper, s.amSave)
	s.handle(a+"delete_selected_parameters", AccessSuper, s.amDelete)

	i := "issue_mapping/"
	s.handle(i+"get_models", AccessSuper, s.listTable(`SELECT id, name FROM asset.models ORDER BY lower(name)`))
	s.handle(i+"get_relation_list", AccessSuper, s.imRelations)
	s.handle(i+"get_alarm_codes", AccessSuper, s.imAlarmCodes)
	s.handle(i+"get_parameters", AccessSuper, s.mapParameters)
	s.handle(i+"add_relation", AccessSuper, s.imAdd)
	s.handle(i+"get_issue_relations", AccessSuper, s.imGet)
	s.handle(i+"update_issue_relations", AccessSuper, s.imUpdate)
	s.handle(i+"delete_relation", AccessSuper, s.imDelete)
	s.handle(i+"duplicate_from_model", AccessSuper, s.imDuplicate)
}

func (s *Server) requireModel(c *Ctx, id int64) error {
	var ok bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.models WHERE id = $1)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("err_model_not_found")
	}
	return nil
}

func (s *Server) requireAlarmCode(c *Ctx, id int64) error {
	var ok bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM dm.alarm_codes WHERE id = $1)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("Alarm code not found")
	}
	return nil
}

func (s *Server) amCodes(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	page, limit := c.page(1), c.limit(10, 1000)
	search := ilike(c.F.Str("search"))
	var mapped, unmapped int64
	err := s.DB.QueryRow(c, `SELECT count(*) FILTER (WHERE m.mapped), count(*) FILTER (WHERE NOT m.mapped) FROM (
			SELECT EXISTS (SELECT 1 FROM dm.alarm_mappings am WHERE am.model_id = $1 AND am.alarm_code_id = ac.id) AS mapped
			FROM dm.alarm_codes ac WHERE ac.code ILIKE $2 OR ac.name ILIKE $2) m`, modelID, search).Scan(&mapped, &unmapped)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT ac.id, ac.code, ac.name,
			EXISTS (SELECT 1 FROM dm.alarm_mappings am WHERE am.model_id = $1 AND am.alarm_code_id = ac.id)
		FROM dm.alarm_codes ac WHERE ac.code ILIKE $2 OR ac.name ILIKE $2
		ORDER BY ac.order_id, lower(ac.code), ac.id LIMIT $3 OFFSET $4`, modelID, search, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var id int64
		var code, name string
		var isMapped bool
		if err := rows.Scan(&id, &code, &name, &isMapped); err != nil {
			return nil, err
		}
		data = append(data, map[string]any{"alarm_id": id, "alarm_code": code, "alarm_name": name, "is_mapped": isMapped})
	}
	return map[string]any{"data": data, "mapped_count": mapped, "unmapped_count": unmapped}, rows.Err()
}

// mapParameters lists the variables of a model (alarm and issue mapping).
func (s *Server) mapParameters(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	page, limit := c.page(1), c.limit(10, 1000)
	search := ilike(c.F.Str("search"))
	var count int64
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.model_parameters WHERE model_id = $1 AND (name ILIKE $2 OR alias ILIKE $2)`,
		modelID, search).Scan(&count); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT id, name FROM asset.model_parameters WHERE model_id = $1 AND (name ILIKE $2 OR alias ILIKE $2)
		ORDER BY lower(name), id LIMIT $3 OFFSET $4`, modelID, search, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		data = append(data, map[string]any{"id": id, "name": name})
	}
	return map[string]any{"data": data, "count": count}, rows.Err()
}

func (s *Server) amSelected(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT mp.id, mp.name FROM dm.alarm_mappings am JOIN asset.model_parameters mp ON mp.id = am.model_parameter_id
		WHERE am.model_id = $1 AND am.alarm_code_id = $2 ORDER BY lower(mp.name), mp.id`, c.F.Int("model_id", 0), c.F.Int("alarm_code_id", 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": fmt.Sprint(id), "name": name})
	}
	return out, rows.Err()
}

func (s *Server) amSave(c *Ctx) (any, error) {
	modelID, codeID := c.F.Int("model_id", 0), c.F.Int("alarm_code_id", 0)
	if err := s.requireModel(c, modelID); err != nil {
		return nil, err
	}
	if err := s.requireAlarmCode(c, codeID); err != nil {
		return nil, err
	}
	ids := uniqueInt64(httpx.ParseIDList(c.F.Str("parameter_ids")))
	if ids == nil {
		ids = []int64{}
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		if len(ids) > 0 {
			var n int
			if err := tx.QueryRow(c, `SELECT count(*) FROM asset.model_parameters WHERE model_id = $1 AND id = ANY($2)`, modelID, ids).Scan(&n); err != nil {
				return err
			}
			if n != len(ids) {
				return httpx.BadRequest("err_variable_not_found")
			}
		}
		if _, err := tx.Exec(c, `DELETE FROM dm.alarm_mappings WHERE model_id = $1 AND alarm_code_id = $2`, modelID, codeID); err != nil {
			return err
		}
		_, err := tx.Exec(c, `INSERT INTO dm.alarm_mappings (model_id, alarm_code_id, model_parameter_id)
			SELECT $1, $2, unnest($3::bigint[])`, modelID, codeID, ids)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "Alarm mapping successfully saved", nil
}

func (s *Server) amDelete(c *Ctx) (any, error) {
	modelID, codeID := c.F.Int("model_id", 0), c.F.Int("alarm_code_id", 0)
	if _, err := s.DB.Exec(c, `DELETE FROM dm.alarm_mappings WHERE model_id = $1 AND alarm_code_id = $2`, modelID, codeID); err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "Alarm mapping successfully deleted", nil
}

func (s *Server) imRelations(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	page, limit := c.page(1), c.limit(10, 1000)
	search := ilike(c.F.Str("search"))
	var count int64
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM dm.issue_relations WHERE model_id = $1 AND name ILIKE $2`, modelID, search).Scan(&count); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT r.id, r.name, m.name FROM dm.issue_relations r JOIN asset.models m ON m.id = r.model_id
		WHERE r.model_id = $1 AND r.name ILIKE $2 ORDER BY lower(r.name), r.id LIMIT $3 OFFSET $4`, modelID, search, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, model string
		if err := rows.Scan(&id, &name, &model); err != nil {
			return nil, err
		}
		data = append(data, map[string]any{"id": id, "relation_name": name, "model_name": model})
	}
	return map[string]any{"data": data, "count": count}, rows.Err()
}

func (s *Server) imAlarmCodes(c *Ctx) (any, error) {
	page, limit := c.page(1), c.limit(10, 1000)
	search := ilike(c.F.Str("search"))
	var count int64
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM dm.alarm_codes WHERE code ILIKE $1 OR name ILIKE $1`, search).Scan(&count); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT id, code, name FROM dm.alarm_codes WHERE code ILIKE $1 OR name ILIKE $1
		ORDER BY order_id, lower(code), id LIMIT $2 OFFSET $3`, search, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var id int64
		var code, name string
		if err := rows.Scan(&id, &code, &name); err != nil {
			return nil, err
		}
		data = append(data, map[string]any{"id": id, "code": code, "name": name})
	}
	return map[string]any{"data": data, "count": count}, rows.Err()
}

func (s *Server) imAdd(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	name := c.F.StrTrim("name")
	if name == "" {
		return nil, httpx.BadRequest("Relation name is required")
	}
	if err := s.requireModel(c, modelID); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(c, `INSERT INTO dm.issue_relations (model_id, name) VALUES ($1, $2)`, modelID, name); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, httpx.BadRequest("A relation with the same name already exists")
		}
		return nil, err
	}
	return "Relation successfully added", nil
}

// flowDoc is the stored React Flow document. Unknown node/edge fields are kept
// so the editor gets back exactly what it saved.
type flowDoc struct {
	Nodes []map[string]any `json:"nodes"`
	Edges []map[string]any `json:"edges"`
}

var flowNodeTypes = map[string]bool{"source": true, "target": true, "parameter": true, "compound": true}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asNumber(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

// normalizeFlow validates a flow and fills the fields the editor requires.
func normalizeFlow(raw []byte, strict bool) (*flowDoc, error) {
	doc := &flowDoc{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, doc); err != nil {
			return nil, httpx.BadRequest("Invalid relation flow")
		}
	}
	if doc.Nodes == nil {
		doc.Nodes = []map[string]any{}
	}
	if doc.Edges == nil {
		doc.Edges = []map[string]any{}
	}
	ids := map[string]bool{}
	nodes := doc.Nodes[:0]
	for _, n := range doc.Nodes {
		if n == nil {
			continue
		}
		id := asString(n["id"])
		typ := asString(n["type"])
		if id == "" || ids[id] || !flowNodeTypes[typ] {
			if strict {
				return nil, httpx.BadRequest("Invalid relation flow")
			}
			continue
		}
		ids[id] = true
		pos, _ := n["position"].(map[string]any)
		n["position"] = map[string]any{"x": asNumber(pos["x"]), "y": asNumber(pos["y"])}
		if _, ok := n["data"].(map[string]any); !ok {
			n["data"] = map[string]any{}
		}
		if _, ok := n["draggable"].(bool); !ok {
			n["draggable"] = true
		}
		// Transient editor state is not persisted.
		delete(n, "selected")
		delete(n, "dragging")
		nodes = append(nodes, n)
	}
	doc.Nodes = nodes
	edges := doc.Edges[:0]
	seen := map[string]bool{}
	for _, e := range doc.Edges {
		if e == nil {
			continue
		}
		id, src, dst := asString(e["id"]), asString(e["source"]), asString(e["target"])
		if id == "" || seen[id] || !ids[src] || !ids[dst] {
			if strict {
				return nil, httpx.BadRequest("Invalid relation flow")
			}
			continue
		}
		seen[id] = true
		if asString(e["type"]) == "" {
			e["type"] = "mapping"
		}
		delete(e, "selected")
		edges = append(edges, e)
	}
	doc.Edges = edges
	return doc, nil
}

func (s *Server) imGet(c *Ctx) (any, error) {
	var raw []byte
	if err := s.DB.QueryRow(c, `SELECT flow FROM dm.issue_relations WHERE id = $1`, c.F.Int("relation_id", 0)).Scan(&raw); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Relation not found")
		}
		return nil, err
	}
	doc, err := normalizeFlow(raw, false)
	if err != nil {
		return map[string]any{"nodes": []any{}, "edges": []any{}}, nil
	}
	return doc, nil
}

func (s *Server) imUpdate(c *Ctx) (any, error) {
	id := c.F.Int("relation_id", 0)
	name := c.F.StrTrim("relation_name")
	if name == "" {
		return nil, httpx.BadRequest("Relation name is required")
	}
	doc, err := normalizeFlow([]byte(c.F.Str("flow")), true)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	tag, err := s.DB.Exec(c, `UPDATE dm.issue_relations SET name = $2, flow = $3, updated_at = now() WHERE id = $1`, id, name, raw)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, httpx.BadRequest("A relation with the same name already exists")
		}
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("Relation not found")
	}
	s.Engine.Invalidate()
	return "Relation successfully updated", nil
}

func (s *Server) imDelete(c *Ctx) (any, error) {
	tag, err := s.DB.Exec(c, `DELETE FROM dm.issue_relations WHERE id = $1`, c.F.Int("relation_id", 0))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("Relation not found")
	}
	s.Engine.Invalidate()
	return "Relation successfully deleted", nil
}

// imDuplicate copies every relation of a model to another model. Parameter
// nodes are re-pointed to the target model variables with the same name
// (or alias); nodes without a counterpart are dropped with their edges.
func (s *Server) imDuplicate(c *Ctx) (any, error) {
	src, dst := c.F.Int("source_model_id", 0), c.F.Int("target_model_id", 0)
	if src == dst {
		return nil, httpx.BadRequest("Source and target model must be different")
	}
	if err := s.requireModel(c, src); err != nil {
		return nil, err
	}
	if err := s.requireModel(c, dst); err != nil {
		return nil, err
	}
	count := 0
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		// Map source parameter ids to target parameters.
		type tp struct {
			id   int64
			name string
		}
		byAlias, byName := map[string]tp{}, map[string]tp{}
		rows, err := tx.Query(c, `SELECT id, alias, name FROM asset.model_parameters WHERE model_id = $1`, dst)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p tp
			var alias string
			if err := rows.Scan(&p.id, &alias, &p.name); err != nil {
				rows.Close()
				return err
			}
			byAlias[alias] = p
			byName[strings.ToLower(p.name)] = p
		}
		rows.Close()
		paramMap := map[int64]tp{}
		rows, err = tx.Query(c, `SELECT id, alias, name FROM asset.model_parameters WHERE model_id = $1`, src)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var alias, name string
			if err := rows.Scan(&id, &alias, &name); err != nil {
				rows.Close()
				return err
			}
			if p, ok := byAlias[alias]; ok {
				paramMap[id] = p
			} else if p, ok := byName[strings.ToLower(name)]; ok {
				paramMap[id] = p
			}
		}
		rows.Close()

		existing, err := db.Strings(c, tx, `SELECT lower(name) FROM dm.issue_relations WHERE model_id = $1`, dst)
		if err != nil {
			return err
		}
		taken := map[string]bool{}
		for _, n := range existing {
			taken[n] = true
		}
		type rel struct {
			name string
			flow []byte
		}
		var rels []rel
		rows, err = tx.Query(c, `SELECT name, flow FROM dm.issue_relations WHERE model_id = $1 ORDER BY id`, src)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r rel
			if err := rows.Scan(&r.name, &r.flow); err != nil {
				rows.Close()
				return err
			}
			rels = append(rels, r)
		}
		rows.Close()
		for _, r := range rels {
			doc, err := normalizeFlow(r.flow, false)
			if err != nil {
				continue
			}
			nodes := doc.Nodes[:0]
			for _, n := range doc.Nodes {
				if asString(n["type"]) == "parameter" {
					data := n["data"].(map[string]any)
					pid, _ := toInt(data["id"])
					p, ok := paramMap[pid]
					if !ok {
						continue
					}
					data["id"] = p.id
					data["name"] = p.name
				}
				nodes = append(nodes, n)
			}
			doc.Nodes = nodes
			raw, _ := json.Marshal(doc)
			fixed, _ := normalizeFlow(raw, false)
			raw, err = json.Marshal(fixed)
			if err != nil {
				return err
			}
			name := r.name
			for i := 2; taken[strings.ToLower(name)]; i++ {
				name = fmt.Sprintf("%s (%d)", r.name, i)
			}
			taken[strings.ToLower(name)] = true
			if _, err := tx.Exec(c, `INSERT INTO dm.issue_relations (model_id, name, flow) VALUES ($1, $2, $3)`, dst, name, raw); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return fmt.Sprintf("%d relations successfully duplicated", count), nil
}

func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		if ids := httpx.ParseIDList(t); len(ids) == 1 {
			return ids[0], true
		}
	}
	return 0, false
}
