package api

import (
	"context"
	"encoding/json"

	"afdd/internal/engine"
)

func (s *Server) alarmCodeIndex(ctx context.Context) (*engine.AlarmCodeIndex, error) {
	return engine.LoadAlarmCodes(ctx, s.DB)
}

// flowTargetCodes returns the alarm codes of the target nodes of a flow.
func flowTargetCodes(raw []byte, ix *engine.AlarmCodeIndex) []int64 {
	var f engine.Flow
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	var out []int64
	seen := map[int64]bool{}
	for i := range f.Nodes {
		n := &f.Nodes[i]
		if n.Type != "target" {
			continue
		}
		if id, ok := ix.NodeCode(n); ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
