// Package script runs user supplied JavaScript (model status / alarm
// definitions and gateway mapping expressions) in a sandboxed goja runtime.
//
// Scripts are written as function bodies: they may use `return` and top
// level `await`. Scripts without a `return` statement yield the value of
// their last expression.
package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

var returnRe = regexp.MustCompile(`(^|[^A-Za-z0-9_$.])return([^A-Za-z0-9_$]|$)`)
var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

var reserved = map[string]bool{
	"break": true, "case": true, "catch": true, "class": true, "const": true, "continue": true, "debugger": true,
	"default": true, "delete": true, "do": true, "else": true, "export": true, "extends": true, "finally": true,
	"for": true, "function": true, "if": true, "import": true, "in": true, "instanceof": true, "new": true,
	"return": true, "super": true, "switch": true, "this": true, "throw": true, "try": true, "typeof": true,
	"var": true, "void": true, "while": true, "with": true, "yield": true, "let": true, "static": true,
	"enum": true, "await": true, "implements": true, "package": true, "protected": true, "interface": true,
	"private": true, "public": true, "null": true, "true": true, "false": true, "undefined": true, "NaN": true,
	"Infinity": true, "eval": true, "arguments": true, "Math": true, "JSON": true, "Date": true, "Object": true,
	"Array": true, "String": true, "Number": true, "Boolean": true, "Promise": true, "console": true,
}

// ValidIdentifier reports whether name can be exposed as a global variable.
func ValidIdentifier(name string) bool {
	return identRe.MatchString(name) && !reserved[name]
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*goja.Program{}
)

// compile wraps and compiles a script body (cached by source).
func compile(src string) (*goja.Program, error) {
	cacheMu.Lock()
	if p, ok := cache[src]; ok {
		cacheMu.Unlock()
		return p, nil
	}
	cacheMu.Unlock()

	var wrapped string
	if returnRe.MatchString(src) {
		wrapped = "(async function(){\n" + src + "\n})()"
	} else {
		// The body is evaluated through eval() to get its completion value;
		// compile it on its own first so syntax errors surface here.
		if _, err := goja.Compile("script.js", src, false); err != nil {
			return nil, err
		}
		lit, _ := json.Marshal(src)
		wrapped = "(function(){ return eval(" + string(lit) + "); })()"
	}
	p, err := goja.Compile("script.js", wrapped, false)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	if len(cache) > 5000 {
		cache = map[string]*goja.Program{}
	}
	cache[src] = p
	cacheMu.Unlock()
	return p, nil
}

// Check compiles a script and returns a syntax error, if any.
func Check(src string) error {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	_, err := compile(src)
	return err
}

// Runtime is a JavaScript VM with a set of global variables. It is not safe
// for concurrent use.
type Runtime struct {
	vm      *goja.Runtime
	timeout time.Duration
}

// NewRuntime creates a fresh VM.
func NewRuntime(timeout time.Duration) *Runtime {
	if timeout <= 0 {
		timeout = time.Second
	}
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	noop := func(goja.FunctionCall) goja.Value { return goja.Undefined() }
	console := vm.NewObject()
	for _, n := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(n, noop)
	}
	_ = vm.Set("console", console)
	return &Runtime{vm: vm, timeout: timeout}
}

// Set defines a global variable.
func (r *Runtime) Set(name string, v any) error { return r.vm.Set(name, v) }

// SetJSON defines a global variable holding a native JavaScript copy of v
// (objects and arrays behave exactly like JSON.parse results).
func (r *Runtime) SetJSON(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	parse, ok := goja.AssertFunction(r.vm.Get("JSON").ToObject(r.vm).Get("parse"))
	if !ok {
		return errors.New("JSON.parse unavailable")
	}
	val, err := parse(goja.Undefined(), r.vm.ToValue(string(b)))
	if err != nil {
		return err
	}
	return r.vm.Set(name, val)
}

// ErrTimeout is returned when a script exceeds its time budget.
var ErrTimeout = errors.New("script timed out")

// Run executes a script body and returns its (awaited) result exported to Go.
func (r *Runtime) Run(src string) (val any, err error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	prog, err := compile(src)
	if err != nil {
		return nil, fmt.Errorf("syntax error: %w", err)
	}
	timer := time.AfterFunc(r.timeout, func() { r.vm.Interrupt(ErrTimeout) })
	defer func() {
		timer.Stop()
		r.vm.ClearInterrupt()
		if rec := recover(); rec != nil {
			err = fmt.Errorf("script panic: %v", rec)
		}
	}()
	v, err := r.vm.RunProgram(prog)
	if err != nil {
		var ie *goja.InterruptedError
		if errors.As(err, &ie) {
			return nil, ErrTimeout
		}
		return nil, err
	}
	return exportValue(v)
}

func exportValue(v goja.Value) (any, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	if p, ok := v.Export().(*goja.Promise); ok {
		switch p.State() {
		case goja.PromiseStateFulfilled:
			return exportValue(p.Result())
		case goja.PromiseStateRejected:
			res := p.Result()
			if res != nil {
				return nil, fmt.Errorf("script error: %s", res.String())
			}
			return nil, errors.New("script error")
		default:
			return nil, errors.New("script did not complete (pending promise)")
		}
	}
	return v.Export(), nil
}

// Truthy converts a script result to a boolean using JavaScript semantics.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	default:
		return true
	}
}

// VTQ is the value/timestamp/quality triple exposed to scripts.
type VTQ struct {
	V any   `json:"v"`
	T int64 `json:"t"`
	Q bool  `json:"q"`
}

// ParamObject converts a VTQ into the object shape scripts expect.
func ParamObject(v VTQ) map[string]any {
	return map[string]any{"v": v.V, "t": v.T, "q": v.Q}
}

// CoerceValue converts a raw string value to the declared data type.
func CoerceValue(raw string, dataType string) any {
	switch dataType {
	case "number":
		s := strings.TrimSpace(raw)
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		switch strings.ToLower(s) {
		case "true":
			return 1.0
		case "false":
			return 0.0
		}
		return nil
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1", "on", "yes":
			return true
		case "false", "0", "off", "no", "":
			return false
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			return f != 0
		}
		return nil
	default:
		return raw
	}
}
