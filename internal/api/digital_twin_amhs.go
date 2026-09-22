package api

import (
	"encoding/json"
	"fmt"
	"time"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

// twinAMHS is the live overhead transport of FAB1 for the 3D twin: the
// simulator's state (core.settings demo_amhs_state, cmd/simulator
// AmhsSnapshot) with the vehicles' plans, the FOUPs on the load ports and the
// stocker counts, and the server time, so the app aligns its clock. It needs
// the digitaltwin "read" permission and a visible process tool; lot codes and
// products are left out without the production "read" permission.
func (s *Server) twinAMHS(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	res := map[string]any{"served_at": httpx.TS(time.Now()), "state": nil}
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return nil, err
	}
	visible := false
	for _, r := range eq {
		visible = visible || r.Visible && prodToolTypes[r.TypeName] != ""
	}
	if !visible {
		return res, nil
	}
	var raw []byte
	var at time.Time
	err = s.DB.QueryRow(c, `SELECT value, updated_at FROM core.settings WHERE key = 'demo_amhs_state'`).Scan(&raw, &at)
	if db.IsNoRows(err) {
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("transport state: %w", err)
	}
	if !c.User.Can("production", "read") {
		stripLots(st)
	}
	// the server time as late as possible: the app's clock offset is this minus the arrival time
	res["state"], res["updated_at"], res["served_at"] = st, httpx.TS(at), httpx.TS(time.Now())
	return res, nil
}

// stripLots removes the lot codes and products from a transport state.
func stripLots(st map[string]any) {
	if vs, ok := st["vehicles"].([]any); ok {
		for _, v := range vs {
			if segs, ok := v.(map[string]any)["segs"].([]any); ok {
				for _, s := range segs {
					if m, ok := s.(map[string]any); ok && m["lot"] != nil {
						m["lot"] = "FOUP"
					}
				}
			}
		}
	}
	if ps, ok := st["ports"].([]any); ok {
		for _, p := range ps {
			if m, ok := p.(map[string]any); ok {
				m["lot"] = "FOUP"
				delete(m, "product")
				delete(m, "hot")
			}
		}
	}
}
