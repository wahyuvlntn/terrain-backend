package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type dbTx = pgx.Tx

// EarlyWarning describes an issue detected through an issue relation.
type EarlyWarning struct {
	AssetID      int64
	ModelID      int64
	RelationID   int64
	RelationName string
	TargetCodeID int64
	OtherCodeIDs []int64
	IsCompound   bool
	IsDeduction  bool
	Severity     string // WARNING | CRITICAL
	Parameters   []EWParam
	SourceCodes  []EWCode
	DetectedAt   time.Time
}

// EWParam is an abnormal parameter that contributed to an early warning.
type EWParam struct {
	EPID      int64
	Condition string
	Message   string
}

// EWCode is an active alarm code that contributed to an early warning.
type EWCode struct {
	CodeID   int64
	Severity string
}

type paramHit struct {
	epid     int64
	severity string
	message  string
}

// nodeAlarmCode resolves the alarm code referenced by a graph node.
func nodeAlarmCode(snap *Snapshot, n *FlowNode) (int64, bool) {
	return snap.AlarmCodes.NodeCode(n)
}

// NodeCode resolves the alarm code referenced by an issue relation node.
func (ix *AlarmCodeIndex) NodeCode(n *FlowNode) (int64, bool) {
	if ix == nil || n == nil || n.Data == nil {
		return 0, false
	}
	if raw, ok := n.Data["id"]; ok {
		if id, ok := toInt64(raw); ok {
			if _, exists := ix.Info[id]; exists {
				return id, true
			}
		}
	}
	if code, ok := n.Data["code"].(string); ok && code != "" {
		if id, ok := ix.Resolve(code); ok {
			return id, true
		}
	}
	if name, ok := n.Data["name"].(string); ok && name != "" {
		return ix.Resolve(name)
	}
	return 0, false
}

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (e *Engine) diagnose(ctx context.Context, snap *Snapshot, eq *Equipment, m *ModelDef, res *Result, now time.Time) {
	e.hooksMu.RLock()
	hooks := e.hooks
	e.hooksMu.RUnlock()
	if hooks == nil {
		return
	}
	activeCodes := map[int64]string{}
	for _, a := range res.Alarms {
		if id, ok := snap.AlarmCodes.Resolve(a.Name); ok {
			if prev, exists := activeCodes[id]; !exists || prev != "CRITICAL" {
				activeCodes[id] = a.Severity
			}
		}
	}
	paramsByMP := map[int64]paramHit{}
	paramsByName := map[string]paramHit{}
	mpOfEP := map[int64]*ParamDef{}
	for _, p := range eq.Params {
		mpOfEP[p.EPID] = p
	}
	for _, a := range res.Abnormal {
		p := mpOfEP[a.EPID]
		if p == nil {
			continue
		}
		h := paramHit{epid: a.EPID, severity: a.Severity, message: a.Message}
		paramsByMP[p.MPID] = h
		paramsByName[strings.ToLower(p.Name)] = h
		paramsByName[strings.ToLower(p.Alias)] = h
	}
	if len(activeCodes) == 0 && len(paramsByMP) == 0 {
		return
	}

	for i := range m.Relations {
		rel := &m.Relations[i]
		nodes := map[string]*FlowNode{}
		for j := range rel.Flow.Nodes {
			nodes[rel.Flow.Nodes[j].ID] = &rel.Flow.Nodes[j]
		}
		incoming := map[string][]string{}
		for _, ed := range rel.Flow.Edges {
			if nodes[ed.Source] == nil || nodes[ed.Target] == nil {
				continue
			}
			incoming[ed.Target] = append(incoming[ed.Target], ed.Source)
		}
		memo := map[string]bool{}
		visiting := map[string]bool{}
		var active func(id string) bool
		active = func(id string) bool {
			if v, ok := memo[id]; ok {
				return v
			}
			if visiting[id] {
				return false
			}
			visiting[id] = true
			defer delete(visiting, id)
			n := nodes[id]
			result := false
			switch n.Type {
			case "source":
				if code, ok := nodeAlarmCode(snap, n); ok {
					_, result = activeCodes[code]
				}
			case "parameter":
				result = e.paramNodeHit(n, paramsByMP, paramsByName) != nil
			case "compound":
				ins := incoming[id]
				result = len(ins) > 0
				for _, src := range ins {
					if !active(src) {
						result = false
						break
					}
				}
			case "target":
				for _, src := range incoming[id] {
					if active(src) {
						result = true
						break
					}
				}
			}
			memo[id] = result
			return result
		}

		var targetIDs []string
		for id, n := range nodes {
			if n.Type == "target" {
				targetIDs = append(targetIDs, id)
			}
		}
		sort.Strings(targetIDs)
		allTargetCodes := map[int64]bool{}
		for _, id := range targetIDs {
			if code, ok := nodeAlarmCode(snap, nodes[id]); ok {
				allTargetCodes[code] = true
			}
		}
		for _, tid := range targetIDs {
			if !active(tid) {
				continue
			}
			target, ok := nodeAlarmCode(snap, nodes[tid])
			if !ok {
				continue
			}
			ew := EarlyWarning{AssetID: eq.ID, ModelID: m.ID, RelationID: rel.ID, RelationName: rel.Name, TargetCodeID: target, Severity: "WARNING", DetectedAt: now}
			seenParam := map[int64]bool{}
			seenCode := map[int64]bool{}
			var collect func(id string, depth int)
			collect = func(id string, depth int) {
				if depth > 50 {
					return
				}
				for _, src := range incoming[id] {
					if !active(src) {
						continue
					}
					n := nodes[src]
					switch n.Type {
					case "compound":
						ew.IsCompound = true
						collect(src, depth+1)
					case "parameter":
						ew.IsDeduction = true
						if h := e.paramNodeHit(n, paramsByMP, paramsByName); h != nil && !seenParam[h.epid] {
							seenParam[h.epid] = true
							ew.Parameters = append(ew.Parameters, EWParam{EPID: h.epid, Condition: h.severity, Message: h.message})
							if h.severity == "CRITICAL" {
								ew.Severity = "CRITICAL"
							}
						}
					case "source":
						if code, ok := nodeAlarmCode(snap, n); ok && !seenCode[code] {
							seenCode[code] = true
							sev := activeCodes[code]
							ew.SourceCodes = append(ew.SourceCodes, EWCode{CodeID: code, Severity: sev})
							if sev == "CRITICAL" {
								ew.Severity = "CRITICAL"
							}
						}
					}
				}
			}
			collect(tid, 0)
			for code := range allTargetCodes {
				if code != target {
					ew.OtherCodeIDs = append(ew.OtherCodeIDs, code)
				}
			}
			sort.Slice(ew.OtherCodeIDs, func(a, b int) bool { return ew.OtherCodeIDs[a] < ew.OtherCodeIDs[b] })

			key := fmt.Sprintf("%d:%d:%d", eq.ID, rel.ID, target)
			e.ewMu.Lock()
			last, recent := e.ewRecent[key]
			if recent && now.Sub(last) < e.opts.EarlyWarningCool {
				e.ewMu.Unlock()
				continue
			}
			e.ewRecent[key] = now
			e.ewMu.Unlock()
			if err := hooks.CreateEarlyWarning(ctx, ew); err != nil {
				slog.Error("create early warning", "equipment", eq.Alias, "relation", rel.Name, "err", err)
			}
		}
	}
}

func (e *Engine) paramNodeHit(n *FlowNode, byMP map[int64]paramHit, byName map[string]paramHit) *paramHit {
	if n.Data == nil {
		return nil
	}
	if raw, ok := n.Data["id"]; ok {
		if id, ok := toInt64(raw); ok {
			if h, ok := byMP[id]; ok {
				return &h
			}
		}
	}
	if name, ok := n.Data["name"].(string); ok {
		if h, ok := byName[strings.ToLower(strings.TrimSpace(name))]; ok {
			return &h
		}
	}
	return nil
}
