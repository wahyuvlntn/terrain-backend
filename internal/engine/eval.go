package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"afdd/internal/script"
)

// Value is a typed parameter value exposed to scripts.
type Value struct {
	V   any // float64 | bool | string | nil
	Raw string
	T   int64 // ms
	Q   bool  // good quality
}

// Num returns the numeric value when available.
func (v Value) Num() (float64, bool) {
	switch t := v.V.(type) {
	case float64:
		return t, !math.IsNaN(t) && !math.IsInf(t, 0)
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// ActiveAlarm is an alarm condition that is currently true.
type ActiveAlarm struct {
	Key      string // stable identity of the alarm source
	Type     string // ALARM or ABNORMAL
	DefID    int64  // model alarm / dynamic alarm / equipment parameter id
	Name     string
	Severity string // WARNING or CRITICAL
	Message  string
	EPID     int64 // for ABNORMAL
	DynName  string
}

// Result is the outcome of an evaluation.
type Result struct {
	Status   int
	Alarms   []ActiveAlarm // model + dynamic alarms
	Abnormal []ActiveAlarm // parameter threshold breaches
	Errors   []string
}

// Condition derives the equipment condition.
func (r *Result) Condition() string {
	if r.Status == StatusOffline {
		return CondOffline
	}
	cond := CondNormal
	for _, lists := range [][]ActiveAlarm{r.Alarms, r.Abnormal} {
		for _, a := range lists {
			if a.Severity == "CRITICAL" {
				return CondCritical
			}
			cond = CondWarning
		}
	}
	return cond
}

// ParamCondition evaluates the threshold condition of a numeric value.
func ParamCondition(p *ParamDef, v float64) string {
	if p.CritType != "" && thresholdBreached(v, p.CritType, p.CritLower, p.CritUpper) {
		return CondCritical
	}
	if p.WarnType != "" && thresholdBreached(v, p.WarnType, p.WarnLower, p.WarnUpper) {
		return CondWarning
	}
	return CondNormal
}

// thresholdBreached interprets threshold expressions such as "X > _",
// "X >= _ or X < _". The first operand of '>' parts is the upper limit and of
// '<' parts the lower limit.
func thresholdBreached(v float64, typ string, lower, upper *float64) bool {
	for _, part := range strings.Split(typ, " or ") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "X"))
		switch {
		case strings.HasPrefix(part, ">="):
			if upper != nil && v >= *upper {
				return true
			}
		case strings.HasPrefix(part, ">"):
			if upper != nil && v > *upper {
				return true
			}
		case strings.HasPrefix(part, "<="):
			if lower != nil && v <= *lower {
				return true
			}
		case strings.HasPrefix(part, "<"):
			if lower != nil && v < *lower {
				return true
			}
		}
	}
	return false
}

// ValidThresholdType reports whether t is an accepted threshold expression.
func ValidThresholdType(t string) bool {
	switch t {
	case "", "X < _", "X <= _", "X > _", "X >= _", "X > _ or X < _", "X >= _ or X < _", "X > _ or X <= _", "X >= _ or X <= _":
		return true
	}
	return false
}

// Evaluator runs model scripts.
type Evaluator struct {
	Timeout time.Duration
}

// newRuntime prepares a VM with every parameter exposed as a global
// variable {v, t, q} plus a `params` object keyed by alias.
func (ev *Evaluator) newRuntime(values map[string]Value) *script.Runtime {
	rt := script.NewRuntime(ev.Timeout)
	all := map[string]any{}
	for alias, v := range values {
		obj := map[string]any{"v": v.V, "t": v.T, "q": v.Q}
		all[alias] = obj
		if script.ValidIdentifier(alias) {
			_ = rt.Set(alias, obj)
		}
	}
	_ = rt.Set("params", all)
	return rt
}

// Evaluate computes status and alarms of an equipment. forced overrides the
// status (maintenance work); offline short-circuits script evaluation.
func (ev *Evaluator) Evaluate(m *ModelDef, params []*ParamDef, values map[string]Value, forced *int, offline bool) Result {
	res := Result{Status: StatusIdle}
	if offline {
		res.Status = StatusOffline
	}
	if m == nil {
		if forced != nil {
			res.Status = *forced
		}
		return res
	}
	if !offline {
		rt := ev.newRuntime(values)
		run := func(label, src string) (bool, bool) {
			if strings.TrimSpace(src) == "" {
				return false, false
			}
			v, err := rt.Run(src)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", label, err))
				return false, true
			}
			return script.Truthy(v), true
		}
		hasScripts := false
		status := 0
		if ok, defined := run("unplanned down", m.UnplannedScript); defined {
			hasScripts = true
			if ok {
				status = StatusUnplannedDown
			}
		}
		if status == 0 {
			if ok, defined := run("planned down", m.PlannedScript); defined {
				hasScripts = true
				if ok {
					status = StatusPlannedDown
				}
			}
		}
		if status == 0 {
			if ok, defined := run("running", m.RunningScript); defined {
				hasScripts = true
				if ok {
					status = StatusRunning
				}
			}
		}
		switch {
		case status != 0:
			res.Status = status
		case hasScripts:
			res.Status = StatusIdle
		default:
			// No status definition: online equipment is considered running.
			res.Status = StatusRunning
		}

		for _, a := range m.Alarms {
			ok, defined := run("alarm "+a.Name, a.Script)
			if !defined || !ok {
				continue
			}
			sev := "WARNING"
			if a.Criticality == 2 {
				sev = "CRITICAL"
			}
			res.Alarms = append(res.Alarms, ActiveAlarm{
				Key: fmt.Sprintf("alarm:%d", a.ID), Type: "ALARM", DefID: a.ID, Name: a.Name, Severity: sev, Message: a.Message,
			})
		}
		for _, d := range m.DynAlarms {
			if strings.TrimSpace(d.Script) == "" {
				continue
			}
			v, err := rt.Run(d.Script)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("dynamic alarm %s: %v", d.Name, err))
				continue
			}
			for _, item := range dynamicResults(v) {
				name := item.name
				if name == "" {
					name = d.Name
				}
				res.Alarms = append(res.Alarms, ActiveAlarm{
					Key: fmt.Sprintf("dyn:%d:%s", d.ID, strings.ToLower(name)), Type: "ALARM", DefID: d.ID,
					Name: name, Severity: item.severity, Message: item.message, DynName: d.Name,
				})
			}
		}
	}

	if !offline {
		for _, p := range params {
			if p.WarnType == "" && p.CritType == "" {
				continue
			}
			v, ok := values[p.Alias]
			if !ok || !v.Q {
				continue
			}
			num, ok := v.Num()
			if !ok {
				continue
			}
			cond := ParamCondition(p, num)
			if cond == CondNormal {
				continue
			}
			res.Abnormal = append(res.Abnormal, ActiveAlarm{
				Key: fmt.Sprintf("abn:%d", p.EPID), Type: "ABNORMAL", DefID: p.EPID, EPID: p.EPID,
				Name: p.Name, Severity: cond,
				Message: fmt.Sprintf("%s is %s %s", p.Name, strings.ToLower(cond), formatValue(num, p)),
			})
		}
	}
	if forced != nil && res.Status != StatusOffline {
		res.Status = *forced
	}
	return res
}

func formatValue(v float64, p *ParamDef) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if p.Scale != nil && *p.Scale >= 0 && *p.Scale <= 10 {
		s = strconv.FormatFloat(v, 'f', *p.Scale, 64)
	}
	if p.Unit != "" {
		s += " " + p.Unit
	}
	return s
}

type dynItem struct {
	name     string
	severity string
	message  string
}

func dynamicResults(v any) []dynItem {
	var out []dynItem
	var visit func(x any)
	visit = func(x any) {
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				visit(e)
			}
		case map[string]any:
			it := dynItem{severity: "WARNING"}
			it.name = fmt.Sprint(valueOr(t["name"], ""))
			it.message = fmt.Sprint(valueOr(t["message"], ""))
			switch c := t["criticality"].(type) {
			case string:
				if strings.EqualFold(strings.TrimSpace(c), "CRITICAL") || c == "2" {
					it.severity = "CRITICAL"
				}
			case int64:
				if c == 2 {
					it.severity = "CRITICAL"
				}
			case float64:
				if c == 2 {
					it.severity = "CRITICAL"
				}
			}
			out = append(out, it)
		case bool:
			// `false` means no alarm; a bare `true` raises the definition itself.
			if t {
				out = append(out, dynItem{severity: "WARNING"})
			}
		}
	}
	visit(v)
	return out
}

func valueOr(v any, def any) any {
	if v == nil {
		return def
	}
	return v
}

// ToValue converts a raw tag value into a typed script value.
func ToValue(raw string, ts int64, good bool, dataType string, scale *int) Value {
	v := script.CoerceValue(raw, dataType)
	if f, ok := v.(float64); ok && scale != nil && *scale >= 0 && *scale <= 10 {
		p := math.Pow(10, float64(*scale))
		v = math.Round(f*p) / p
	}
	if v == nil {
		good = false
	}
	return Value{V: v, Raw: raw, T: ts, Q: good}
}
