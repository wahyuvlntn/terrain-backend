package datahub

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"afdd/internal/script"
)

// ExecuteExpression runs a mapping expression against a sample response and
// returns the produced output (used by the "test expression" button).
func (h *Hub) ExecuteExpression(expression, input string) (any, error) {
	return runExpression(expression, parseResponse([]byte(input)), h.scriptTimeout)
}

func parseResponse(b []byte) any {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err == nil {
		return jsonNumbersToFloat(v)
	}
	return s
}

func jsonNumbersToFloat(v any) any {
	switch t := v.(type) {
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case map[string]any:
		for k, x := range t {
			t[k] = jsonNumbersToFloat(x)
		}
		return t
	case []any:
		for i := range t {
			t[i] = jsonNumbersToFloat(t[i])
		}
		return t
	}
	return v
}

func runExpression(expression string, response any, timeout time.Duration) (any, error) {
	rt := script.NewRuntime(timeout)
	if err := rt.SetJSON("response", response); err != nil {
		return nil, err
	}
	if err := rt.SetJSON("output", map[string]any{}); err != nil {
		return nil, err
	}
	// The expression assigns to `output`; evaluate it then read the global.
	src := expression + "\n;return (typeof output === 'undefined') ? {} : output;"
	res, err := rt.Run(src)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// mapPayload converts a received payload into address -> value pairs.
func (h *Hub) mapPayload(g *Gateway, payload []byte) (map[string]VTQC, error) {
	resp := parseResponse(payload)
	var out any = resp
	if g.ParamBool("CUSTOM_MAPPING", false) {
		expr := g.Param("EXPRESSION", "output = response")
		res, err := runExpression(expr, resp, h.scriptTimeout)
		if err != nil {
			return nil, fmt.Errorf("expression error: %w", err)
		}
		out = res
	}
	now := time.Now().UnixMilli()
	values := map[string]VTQC{}
	switch t := out.(type) {
	case map[string]any:
		flatten("", t, now, values)
	case nil:
	default:
		values["value"] = toVTQC(t, now)
	}
	return values, nil
}

func isVTQ(m map[string]any) bool {
	_, hasV := m["v"]
	if !hasV {
		return false
	}
	for k := range m {
		if k != "v" && k != "t" && k != "q" {
			return false
		}
	}
	return true
}

func flatten(prefix string, m map[string]any, now int64, out map[string]VTQC) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		addr := k
		if prefix != "" {
			addr = prefix + "." + k
		}
		switch v := m[k].(type) {
		case map[string]any:
			if isVTQ(v) {
				out[addr] = toVTQC(v, now)
				if prefix != "" {
					out[k] = out[addr]
				}
			} else {
				flatten(addr, v, now, out)
			}
		default:
			out[addr] = toVTQC(v, now)
		}
	}
}

// toVTQC converts a scalar, a VTQ object or an array of those (latest wins).
func toVTQC(v any, now int64) VTQC {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return VTQC{V: "", T: now, Q: QualityBad}
		}
		best := toVTQC(t[0], now)
		for _, e := range t[1:] {
			c := toVTQC(e, now)
			if c.T >= best.T {
				best = c
			}
		}
		return best
	case map[string]any:
		if isVTQ(t) {
			r := VTQC{V: scalar(t["v"]), T: now, Q: QualityGood}
			if ts, ok := toFloat(t["t"]); ok && ts > 0 {
				r.T = int64(ts)
			}
			if q, ok := t["q"]; ok {
				switch qq := q.(type) {
				case bool:
					if !qq {
						r.Q = QualityBad
					}
				default:
					if f, ok := toFloat(qq); ok && f != 0 {
						r.Q = QualityBad
					}
				}
			}
			return r
		}
		b, _ := json.Marshal(t)
		return VTQC{V: string(b), T: now}
	default:
		return VTQC{V: scalar(t), T: now, Q: QualityGood}
	}
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if math.Trunc(t) == t && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return strings.Trim(string(b), `"`)
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}
