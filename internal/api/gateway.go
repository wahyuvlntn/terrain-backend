package api

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/httpx"
)

var aliasRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

func (s *Server) registerGateway() {
	p := "gateway/"
	s.handle(p+"getgatewaylist", AccessSuper, s.gwList)
	s.handle(p+"getgatewaydetails", AccessSuper, s.gwDetails)
	s.handle(p+"getgatewaystatus", AccessSuper, s.gwStatus)
	s.handle(p+"getgatewaytags", AccessSuper, s.gwTags)
	s.handle(p+"getconnectedequipments", AccessSuper, s.gwConnectedEquipments)
	s.handle(p+"browsegatewaytags", AccessSuper, s.gwBrowse)
	s.handle(p+"addgateway", AccessSuper, s.gwAdd)
	s.handle(p+"updategateway", AccessSuper, s.gwUpdate)
	s.handle(p+"deletegateway", AccessSuper, s.gwDelete)
	s.handle(p+"turnongateway", AccessSuper, s.gwTurn(true))
	s.handle(p+"turnoffgateway", AccessSuper, s.gwTurn(false))
	s.handle(p+"addgatewaytags", AccessSuper, s.gwAddTags(false))
	s.handle(p+"importgatewaytags", AccessSuper, s.gwAddTags(true))
	s.handle(p+"updategatewaytag", AccessSuper, s.gwUpdateTag)
	s.handle(p+"deletegatewaytag", AccessSuper, s.gwDeleteTag)
	s.handle(p+"deletegatewaytags", AccessSuper, s.gwDeleteTags)
	s.handle(p+"updatemqttserverpassword", AccessSuper, s.gwNewMQTTPassword)
	s.handle(p+"readtagsondemand", AccessSuper, s.gwReadOnDemand)
	s.handle(p+"executeexpression", AccessSuper, s.gwExecuteExpression)
}

func protocolName(p string) string {
	switch p {
	case datahub.OPCUA:
		return "OPC UA Client"
	case datahub.MQTTServer:
		return "MQTT Server"
	case datahub.MQTTClient:
		return "MQTT Client"
	case datahub.RESTServer:
		return "REST Server"
	case datahub.RESTClient:
		return "REST Client"
	case datahub.ModbusTCP:
		return "Modbus TCP"
	case datahub.ModbusRTU:
		return "Modbus RTU"
	case datahub.S7:
		return "S7"
	case datahub.MTConnect:
		return "MTConnect"
	}
	return p
}

func (s *Server) gatewayID(ctx context.Context, q db.Querier, alias string) (int64, string, error) {
	var id int64
	var real string
	err := q.QueryRow(ctx, `SELECT id, alias FROM dh.gateways WHERE lower(alias) = lower($1)`, strings.TrimSpace(alias)).Scan(&id, &real)
	if err != nil {
		if db.IsNoRows(err) {
			return 0, "", httpx.BadRequest("err_invalid_gateway_alias")
		}
		return 0, "", err
	}
	return id, real, nil
}

func (s *Server) gwList(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT alias, name, type, description, is_active FROM dh.gateways ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var alias, name, typ string
		var desc *string
		var active bool
		if err := rows.Scan(&alias, &name, &typ, &desc, &active); err != nil {
			return nil, err
		}
		st := s.Hub.Status(alias)
		out = append(out, map[string]any{"alias": alias, "name": name, "type": typ, "description": desc, "is_active": active,
			"is_connected": active && st.Connected})
	}
	return out, rows.Err()
}

func (s *Server) gatewayAdditionals(g *datahub.Gateway) map[string]string {
	add := map[string]string{}
	switch g.Type {
	case datahub.MQTTServer:
		port := s.cfg.MQTTPublicPort
		if port == "" {
			if i := strings.LastIndex(s.cfg.MQTTTLSAddr, ":"); i >= 0 {
				port = s.cfg.MQTTTLSAddr[i+1:]
			}
		}
		add["port"] = port
		add["password"] = g.Secrets["mqtt_password"]
		add["username"] = s.Hub.MQTTIdentity(g.Alias)
		add["topic"] = s.Hub.MQTTIdentity(g.Alias)
	case datahub.RESTServer:
		add["url"] = "/api/datahub/rest/" + g.Alias
		add["api_key"] = g.Secrets["api_key"]
	}
	return add
}

func (s *Server) gwDetails(c *Ctx) (any, error) {
	g, err := datahub.LoadGateway(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.BadRequest("err_invalid_gateway_alias")
		}
		return nil, err
	}
	return map[string]any{"alias": g.Alias, "name": g.Name, "type": g.Type, "description": g.Description, "params": g.Params,
		"additionals": s.gatewayAdditionals(g)}, nil
}

func (s *Server) gwStatus(c *Ctx) (any, error) {
	_, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	return s.Hub.Status(alias), nil
}

func (s *Server) gwTags(c *Ctx) (any, error) {
	id, _, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT name, address, description FROM dh.gateway_tags WHERE gateway_id = $1 ORDER BY lower(name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, addr string
		var desc *string
		if err := rows.Scan(&name, &addr, &desc); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "address": addr, "description": desc})
	}
	return out, rows.Err()
}

func (s *Server) gwConnectedEquipments(c *Ctx) (any, error) {
	id, _, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT e.id, e.name, e.model_id, m.name, e.location_id, count(*) FROM asset.equipment_parameters ep
		JOIN asset.equipment e ON e.id = ep.equipment_id JOIN asset.models m ON m.id = e.model_id
		WHERE ep.gateway_id = $1 AND ep.tag_name IS NOT NULL GROUP BY e.id, e.name, e.model_id, m.name, e.location_id ORDER BY lower(e.name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	equipments := []map[string]any{}
	models := map[string]string{}
	locations := map[string][]string{}
	for rows.Next() {
		var eid, mid, lid int64
		var name, mname string
		var n int
		if err := rows.Scan(&eid, &name, &mid, &mname, &lid, &n); err != nil {
			return nil, err
		}
		equipments = append(equipments, map[string]any{"id": eid, "name": name, "model_id": mid, "location_id": lid, "active_tags": n})
		models[strconv.FormatInt(mid, 10)] = mname
		var path []string
		for _, a := range tree.ancestors(lid) {
			path = append(path, tree.name(a))
		}
		locations[strconv.FormatInt(lid, 10)] = emptyIfNil(path)
	}
	return map[string]any{"equipments": equipments, "models": models, "locations": locations}, rows.Err()
}

func (s *Server) gwBrowse(c *Ctx) (any, error) {
	_, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	depth := 1
	if d := c.F.IntPtr("depth"); d != nil {
		depth = int(*d)
	}
	nodes, err := s.Hub.Browse(c, alias, c.F.Str("node_id"), depth)
	if err != nil {
		if strings.Contains(err.Error(), "err_data_source_no_browse") {
			return nil, httpx.BadRequest("err_data_source_no_browse")
		}
		return nil, httpx.BadRequest(sanitizeError(err))
	}
	return nodes, nil
}

// sanitizeError makes driver errors displayable (no ':' which the UI
// would treat as a translation key separator).
func sanitizeError(err error) string {
	return strings.ReplaceAll(err.Error(), ":", " -")
}

func (c *Ctx) gatewayParams() (map[string]string, error) {
	raw := map[string]any{}
	if _, err := c.F.JSON("params", &raw); err != nil {
		return nil, httpx.BadRequest("Invalid parameters")
	}
	out := map[string]string{}
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case nil:
		default:
			b, _ := json.Marshal(t)
			out[k] = string(b)
		}
	}
	return out, nil
}

func (s *Server) gwAdd(c *Ctx) (any, error) {
	alias := c.F.StrTrim("alias")
	name := c.F.StrTrim("name")
	typ := strings.ToUpper(c.F.StrTrim("type"))
	if !aliasRe.MatchString(alias) {
		return nil, httpx.BadRequest("err_invalid_gateway_alias")
	}
	if name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	if !datahub.ValidProtocol(typ) {
		return nil, httpx.BadRequest("err_invalid_data_source_type")
	}
	params, err := c.gatewayParams()
	if err != nil {
		return nil, err
	}
	secrets := map[string]string{}
	switch typ {
	case datahub.MQTTServer:
		secrets["mqtt_password"] = datahub.GeneratePassword()
	case datahub.RESTServer:
		secrets["api_key"] = datahub.GeneratePassword()
	}
	_, err = s.DB.Exec(c, `INSERT INTO dh.gateways (alias, name, type, description, params, secrets, is_active) VALUES ($1,$2,$3,$4,$5,$6,FALSE)`,
		alias, name, typ, c.F.NonEmptyPtr("description"), params, secrets)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, httpx.BadRequest("Data source with the same ID already exists")
		}
		return nil, err
	}
	if err := s.Hub.Reload(c, alias); err != nil {
		return nil, err
	}
	return httpx.Key("data_source_successfully_added", name), nil
}

func (s *Server) gwUpdate(c *Ctx) (any, error) {
	id, alias, err := s.gatewayID(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	name := c.F.StrTrim("name")
	typ := strings.ToUpper(c.F.StrTrim("type"))
	if name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	if !datahub.ValidProtocol(typ) {
		return nil, httpx.BadRequest("err_invalid_data_source_type")
	}
	params, err := c.gatewayParams()
	if err != nil {
		return nil, err
	}
	g, err := datahub.LoadGateway(c, s.DB, alias)
	if err != nil {
		return nil, err
	}
	secrets := g.Secrets
	if typ == datahub.MQTTServer && secrets["mqtt_password"] == "" {
		secrets["mqtt_password"] = datahub.GeneratePassword()
	}
	if typ == datahub.RESTServer && secrets["api_key"] == "" {
		secrets["api_key"] = datahub.GeneratePassword()
	}
	if _, err := s.DB.Exec(c, `UPDATE dh.gateways SET name = $2, type = $3, description = $4, params = $5, secrets = $6, updated_at = now() WHERE id = $1`,
		id, name, typ, c.F.NonEmptyPtr("description"), params, secrets); err != nil {
		return nil, err
	}
	if err := s.Hub.Reload(c, alias); err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("data_source_successfully_updated", name), nil
}

func (s *Server) gwDelete(c *Ctx) (any, error) {
	id, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment_parameters WHERE gateway_id = $1 AND tag_name IS NOT NULL`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_tags_have_parameters")
	}
	var name string
	_ = s.DB.QueryRow(c, `SELECT name FROM dh.gateways WHERE id = $1`, id).Scan(&name)
	if _, err := s.DB.Exec(c, `DELETE FROM dh.gateways WHERE id = $1`, id); err != nil {
		return nil, err
	}
	s.Hub.Remove(alias)
	s.Engine.Invalidate()
	return httpx.Key("data_source_successfully_deleted", name), nil
}

func (s *Server) gwTurn(on bool) HandlerFunc {
	return func(c *Ctx) (any, error) {
		id, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
		if err != nil {
			return nil, err
		}
		var name string
		if err := s.DB.QueryRow(c, `UPDATE dh.gateways SET is_active = $2, updated_at = now() WHERE id = $1 RETURNING name`, id, on).Scan(&name); err != nil {
			return nil, err
		}
		if err := s.Hub.Reload(c, alias); err != nil {
			return nil, err
		}
		if on {
			return httpx.Key("data_source_successfully_turned_on", name), nil
		}
		return httpx.Key("data_source_successfully_turned_off", name), nil
	}
}

type tagInput struct {
	Name        string  `json:"name"`
	Address     string  `json:"address"`
	Description *string `json:"description"`
}

func (s *Server) gwAddTags(upsert bool) HandlerFunc {
	return func(c *Ctx) (any, error) {
		id, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
		if err != nil {
			return nil, err
		}
		var tags []tagInput
		if _, err := c.F.JSON("gateway_tags", &tags); err != nil || len(tags) == 0 {
			return nil, httpx.BadRequest("err_invalid_gateway_tags")
		}
		seen := map[string]bool{}
		for i := range tags {
			tags[i].Name = strings.TrimSpace(tags[i].Name)
			tags[i].Address = strings.TrimSpace(tags[i].Address)
			if tags[i].Name == "" || tags[i].Address == "" {
				return nil, httpx.BadRequest("err_invalid_gateway_tags")
			}
			if seen[strings.ToLower(tags[i].Name)] {
				return nil, httpx.BadRequest("Duplicated tag name " + tags[i].Name)
			}
			seen[strings.ToLower(tags[i].Name)] = true
		}
		err = s.DB.Tx(c, func(tx pgx.Tx) error {
			for _, t := range tags {
				if upsert {
					if _, err := tx.Exec(c, `INSERT INTO dh.gateway_tags (gateway_id, name, address, description) VALUES ($1,$2,$3,$4)
						ON CONFLICT (gateway_id, name) DO UPDATE SET address = EXCLUDED.address, description = EXCLUDED.description`,
						id, t.Name, t.Address, t.Description); err != nil {
						return err
					}
				} else if _, err := tx.Exec(c, `INSERT INTO dh.gateway_tags (gateway_id, name, address, description) VALUES ($1,$2,$3,$4)`,
					id, t.Name, t.Address, t.Description); err != nil {
					if db.IsUniqueViolation(err) {
						return httpx.BadRequest("Tag " + t.Name + " already exists")
					}
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := s.Hub.Reload(c, alias); err != nil {
			return nil, err
		}
		s.Engine.Invalidate()
		if upsert {
			return httpx.Key("tags_successfully_imported", len(tags)), nil
		}
		return httpx.Key("successfully_added_tags", len(tags)), nil
	}
}

func (s *Server) gwUpdateTag(c *Ctx) (any, error) {
	id, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	cur := c.F.StrTrim("current_tag_name")
	name := c.F.StrTrim("tag_name")
	addr := c.F.StrTrim("tag_address")
	if name == "" || addr == "" {
		return nil, httpx.BadRequest("err_invalid_tag_name")
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		tag, err := tx.Exec(c, `UPDATE dh.gateway_tags SET name = $3, address = $4, description = $5 WHERE gateway_id = $1 AND name = $2`,
			id, cur, name, addr, c.F.NonEmptyPtr("tag_description"))
		if err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("Tag " + name + " already exists")
			}
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.BadRequest("err_invalid_tag_name")
		}
		if cur != name {
			_, err = tx.Exec(c, `UPDATE asset.equipment_parameters SET tag_name = $3 WHERE gateway_id = $1 AND tag_name = $2`, id, cur, name)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := s.Hub.Reload(c, alias); err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("tag_successfully_updated", name), nil
}

func (s *Server) deleteTags(c *Ctx, names []string) (any, error) {
	id, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, httpx.BadRequest("err_invalid_tag_names")
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment_parameters WHERE gateway_id = $1 AND tag_name = ANY($2)`, id, names).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		if len(names) == 1 {
			return nil, httpx.BadRequest("err_tag_has_parameters")
		}
		return nil, httpx.BadRequest("err_tags_have_parameters")
	}
	if _, err := s.DB.Exec(c, `DELETE FROM dh.gateway_tags WHERE gateway_id = $1 AND name = ANY($2)`, id, names); err != nil {
		return nil, err
	}
	if err := s.Hub.Reload(c, alias); err != nil {
		return nil, err
	}
	return "Tags successfully deleted", nil
}

func (s *Server) gwDeleteTag(c *Ctx) (any, error) {
	return s.deleteTags(c, []string{c.F.StrTrim("tag_name")})
}

func (s *Server) gwDeleteTags(c *Ctx) (any, error) {
	var names []string
	if _, err := c.F.JSON("tag_names", &names); err != nil {
		return nil, httpx.BadRequest("err_invalid_tag_names")
	}
	return s.deleteTags(c, names)
}

func (s *Server) gwNewMQTTPassword(c *Ctx) (any, error) {
	id, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	pw := datahub.GeneratePassword()
	if _, err := s.DB.Exec(c, `UPDATE dh.gateways SET secrets = secrets || jsonb_build_object('mqtt_password', $2::text), updated_at = now() WHERE id = $1`, id, pw); err != nil {
		return nil, err
	}
	if err := s.Hub.Reload(c, alias); err != nil {
		return nil, err
	}
	return pw, nil
}

func (s *Server) gwReadOnDemand(c *Ctx) (any, error) {
	_, alias, err := s.gatewayID(c, s.DB, c.F.Str("gateway_alias"))
	if err != nil {
		return nil, err
	}
	var addrs []string
	if _, err := c.F.JSON("gateway_tag_addresses", &addrs); err != nil || len(addrs) == 0 {
		return nil, httpx.BadRequest("err_invalid_tag_addresses")
	}
	res, err := s.Hub.Read(c, alias, addrs)
	if err != nil {
		var he *httpx.Error
		if errorsAs(err, &he) {
			return nil, he
		}
		return nil, httpx.BadRequest(sanitizeError(err))
	}
	return res, nil
}

func (s *Server) gwExecuteExpression(c *Ctx) (any, error) {
	out, err := s.Hub.ExecuteExpression(c.F.Str("expression"), c.F.Str("input_response"))
	if err != nil {
		return nil, httpx.BadRequest(sanitizeError(err))
	}
	return out, nil
}

// gatewayTagMap lists tags by gateway alias (used by the equipment manager).
func (s *Server) gatewayTagNames(ctx context.Context, gatewayID int64) ([]string, error) {
	names, err := db.Strings(ctx, s.DB, `SELECT name FROM dh.gateway_tags WHERE gateway_id = $1`, gatewayID)
	sort.Strings(names)
	return names, err
}
