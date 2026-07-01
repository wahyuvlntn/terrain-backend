// Package httpx contains request parsing and response helpers shared by all
// HTTP handlers.
package httpx

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxUploadSize bounds multipart request bodies.
const MaxUploadSize = 110 << 20

// Form is a parsed request payload. The frontend sends most requests as
// multipart/form-data built by axios from plain objects, which encodes nested
// values with bracket notation (a[b]=1, a[]=1, a[0][x]=1) and drops null and
// undefined values entirely. Form rebuilds the nested structure and offers
// lenient typed accessors.
type Form struct {
	values map[string]any
	files  map[string][]*multipart.FileHeader
}

// NewForm builds a form from already decoded values (used by tests and JSON).
func NewForm(values map[string]any) *Form {
	if values == nil {
		values = map[string]any{}
	}
	return &Form{values: values, files: map[string][]*multipart.FileHeader{}}
}

// ParseRequest decodes the request body according to its content type.
func ParseRequest(r *http.Request) (*Form, error) {
	f := NewForm(nil)
	ct := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		r.Body = http.MaxBytesReader(nil, r.Body, MaxUploadSize)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		if r.MultipartForm != nil {
			for k, vs := range r.MultipartForm.Value {
				for _, v := range vs {
					f.set(k, v)
				}
			}
			for k, fhs := range r.MultipartForm.File {
				base := baseKey(k)
				f.files[base] = append(f.files[base], fhs...)
				if _, ok := f.values[base]; !ok {
					f.values[base] = "__file__"
				}
			}
		}
	case mediaType == "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return nil, fmt.Errorf("invalid form body: %w", err)
		}
		for k, vs := range r.PostForm {
			for _, v := range vs {
				f.set(k, v)
			}
		}
	default:
		body, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" && trimmed != "null" {
			var m map[string]any
			dec := json.NewDecoder(strings.NewReader(trimmed))
			dec.UseNumber()
			if err := dec.Decode(&m); err != nil {
				return nil, fmt.Errorf("invalid json body: %w", err)
			}
			for k, v := range m {
				if v != nil {
					f.values[k] = v
				}
			}
		}
	}
	// Query string parameters act as defaults.
	for k, vs := range r.URL.Query() {
		if _, ok := f.values[baseKey(k)]; ok {
			continue
		}
		for _, v := range vs {
			f.set(k, v)
		}
	}
	return f, nil
}

func baseKey(k string) string {
	if i := strings.IndexByte(k, '['); i > 0 {
		return k[:i]
	}
	return k
}

// set inserts a value addressed with bracket notation into the nested map.
func (f *Form) set(key, value string) {
	path := splitKey(key)
	if len(path) == 0 {
		return
	}
	f.values[path[0]] = insert(f.values[path[0]], path[1:], value)
}

func splitKey(key string) []string {
	i := strings.IndexByte(key, '[')
	if i < 0 {
		return []string{key}
	}
	parts := []string{key[:i]}
	rest := key[i:]
	for len(rest) > 0 && rest[0] == '[' {
		j := strings.IndexByte(rest, ']')
		if j < 0 {
			break
		}
		parts = append(parts, rest[1:j])
		rest = rest[j+1:]
	}
	return parts
}

func insert(cur any, path []string, value string) any {
	if len(path) == 0 {
		switch c := cur.(type) {
		case nil:
			return value
		case []any:
			return append(c, value)
		case string:
			return []any{c, value}
		default:
			return value
		}
	}
	seg := path[0]
	if seg == "" { // "key[]" append
		arr, _ := cur.([]any)
		if s, ok := cur.(string); ok {
			arr = []any{s}
		}
		return append(arr, insert(nil, path[1:], value))
	}
	m, ok := cur.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	m[seg] = insert(m[seg], path[1:], value)
	return m
}

// normalize converts maps with purely numeric keys into ordered slices.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		numeric := len(t) > 0
		for k, sub := range t {
			t[k] = normalize(sub)
			if _, err := strconv.Atoi(k); err != nil {
				numeric = false
			}
		}
		if numeric {
			keys := make([]int, 0, len(t))
			for k := range t {
				n, _ := strconv.Atoi(k)
				keys = append(keys, n)
			}
			sort.Ints(keys)
			arr := make([]any, 0, len(keys))
			for _, k := range keys {
				arr = append(arr, t[strconv.Itoa(k)])
			}
			return arr
		}
		return t
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	default:
		return v
	}
}

// Raw returns the decoded value for key (nested structures normalised).
func (f *Form) Raw(key string) any {
	v, ok := f.values[key]
	if !ok {
		return nil
	}
	return normalize(v)
}

// Has reports whether the key was sent at all (even if empty).
func (f *Form) Has(key string) bool {
	_, ok := f.values[key]
	return ok
}

// Keys returns all top level keys.
func (f *Form) Keys() []string {
	keys := make([]string, 0, len(f.values))
	for k := range f.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isNullish(s string) bool {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "null", "undefined", "nan":
		return true
	}
	return false
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		if t == "__file__" {
			return "", false
		}
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case []any:
		if len(t) == 1 {
			return scalarString(t[0])
		}
		parts := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := scalarString(x); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ","), true
	case map[string]any:
		b, _ := json.Marshal(t)
		return string(b), true
	default:
		return fmt.Sprint(t), true
	}
}

// Str returns the value as a string ("" when absent).
func (f *Form) Str(key string) string {
	s, _ := scalarString(f.values[key])
	return s
}

// StrTrim returns the trimmed string value.
func (f *Form) StrTrim(key string) string { return strings.TrimSpace(f.Str(key)) }

// StrPtr returns nil when the key is absent or "null"/"undefined".
func (f *Form) StrPtr(key string) *string {
	s, ok := scalarString(f.values[key])
	if !ok {
		return nil
	}
	if l := strings.ToLower(strings.TrimSpace(s)); l == "null" || l == "undefined" {
		return nil
	}
	return &s
}

// NonEmptyPtr returns nil for absent, nullish or blank values.
func (f *Form) NonEmptyPtr(key string) *string {
	s, ok := scalarString(f.values[key])
	if !ok || isNullish(s) {
		return nil
	}
	s = strings.TrimSpace(s)
	return &s
}

// IntPtr parses an integer; nil when absent, blank, null or invalid.
func (f *Form) IntPtr(key string) *int64 {
	s, ok := scalarString(f.values[key])
	if !ok || isNullish(s) {
		return nil
	}
	n, err := parseInt(s)
	if err != nil {
		return nil
	}
	return &n
}

// Int returns the integer value or def.
func (f *Form) Int(key string, def int64) int64 {
	if p := f.IntPtr(key); p != nil {
		return *p
	}
	return def
}

func parseInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(fl) || math.IsInf(fl, 0) {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	return int64(fl), nil
}

// FloatPtr parses a float; nil when absent or invalid.
func (f *Form) FloatPtr(key string) *float64 {
	s, ok := scalarString(f.values[key])
	if !ok || isNullish(s) {
		return nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// Float returns the float value or def.
func (f *Form) Float(key string, def float64) float64 {
	if p := f.FloatPtr(key); p != nil {
		return *p
	}
	return def
}

// BoolPtr parses a boolean ("true"/"1"/"yes"); nil when absent/null.
func (f *Form) BoolPtr(key string) *bool {
	s, ok := scalarString(f.values[key])
	if !ok || isNullish(s) {
		return nil
	}
	b := ParseBool(s)
	return &b
}

// Bool returns the boolean value or def.
func (f *Form) Bool(key string, def bool) bool {
	if p := f.BoolPtr(key); p != nil {
		return *p
	}
	return def
}

// ParseBool interprets common truthy spellings.
func ParseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y", "on", "t":
		return true
	}
	return false
}

// IDs parses a list of integers. Accepted encodings: "1,2,3", "{1,2,3}",
// "[1,2,3]", repeated key[] fields and JSON arrays. present is false when the
// key was not sent or was null (meaning "no filter").
func (f *Form) IDs(key string) (ids []int64, present bool) {
	v, ok := f.values[key]
	if !ok || v == nil {
		return nil, false
	}
	ids = []int64{}
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range normalizeMapValues(t) {
				walk(e)
			}
		default:
			s, ok := scalarString(t)
			if !ok {
				return
			}
			for _, n := range ParseIDList(s) {
				ids = append(ids, n)
			}
		}
	}
	if s, isStr := v.(string); isStr {
		trimmed := strings.TrimSpace(s)
		if l := strings.ToLower(trimmed); l == "null" || l == "undefined" {
			return nil, false
		}
	}
	walk(v)
	return dedupeIDs(ids), true
}

func normalizeMapValues(m map[string]any) []any {
	n := normalize(m)
	if arr, ok := n.([]any); ok {
		return arr
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// ParseIDList parses "1,2", "{1,2}", "[1,2]" or JSON arrays of numbers or
// numeric strings. Invalid entries are skipped.
func ParseIDList(s string) []int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var arr []any
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			out := make([]int64, 0, len(arr))
			for _, e := range arr {
				if es, ok := scalarString(e); ok {
					if n, err := parseInt(es); err == nil {
						out = append(out, n)
					}
				}
			}
			return out
		}
	}
	s = strings.Trim(s, "{}[]")
	out := []int64{}
	for _, p := range strings.Split(s, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"'`)
		if p == "" {
			continue
		}
		if n, err := parseInt(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func dedupeIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// StringList parses "a,b", JSON arrays or repeated fields into strings.
func (f *Form) StringList(key string) (list []string, present bool) {
	v, ok := f.values[key]
	if !ok || v == nil {
		return nil, false
	}
	list = []string{}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if s, ok := scalarString(e); ok && s != "" {
				list = append(list, s)
			}
		}
		return list, true
	}
	s, _ := scalarString(v)
	s = strings.TrimSpace(s)
	if l := strings.ToLower(s); l == "null" || l == "undefined" {
		return nil, false
	}
	if strings.HasPrefix(s, "[") {
		var arr []any
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			for _, e := range arr {
				if es, ok := scalarString(e); ok && es != "" {
					list = append(list, es)
				}
			}
			return list, true
		}
	}
	s = strings.Trim(s, "{}")
	for _, p := range strings.Split(s, ",") {
		if p = strings.Trim(strings.TrimSpace(p), `"`); p != "" {
			list = append(list, p)
		}
	}
	return list, true
}

// JSON decodes a value that was sent as a JSON string (or as nested form
// fields) into dst. Returns false when the key is absent.
func (f *Form) JSON(key string, dst any) (bool, error) {
	v, ok := f.values[key]
	if !ok || v == nil {
		return false, nil
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" || s == "null" || s == "undefined" {
			return false, nil
		}
		raw = []byte(s)
	default:
		b, err := json.Marshal(normalize(t))
		if err != nil {
			return true, err
		}
		raw = b
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return true, fmt.Errorf("invalid %s: %w", key, err)
	}
	return true, nil
}

// File returns the first uploaded file for key.
func (f *Form) File(key string) *multipart.FileHeader {
	if fhs := f.files[key]; len(fhs) > 0 {
		return fhs[0]
	}
	return nil
}

// Files returns all uploaded files for key.
func (f *Form) Files(key string) []*multipart.FileHeader { return f.files[key] }

// TimePtr parses timestamps sent as ISO strings, "YYYY-MM-DD" or epoch
// milliseconds. Date-only strings are interpreted in loc.
func (f *Form) TimePtr(key string, loc *time.Location) *time.Time {
	s, ok := scalarString(f.values[key])
	if !ok || isNullish(s) {
		return nil
	}
	t, err := ParseTime(s, loc)
	if err != nil {
		return nil
	}
	return &t
}

// ParseTime parses the timestamp formats produced by the frontend.
func ParseTime(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if loc == nil {
		loc = time.UTC
	}
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	// epoch milliseconds
	allDigits := true
	for i, r := range s {
		if (r < '0' || r > '9') && !(i == 0 && r == '-') {
			allDigits = false
			break
		}
	}
	if allDigits && len(s) >= 9 {
		ms, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return time.UnixMilli(ms).UTC(), nil
		}
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999Z0700",
		"2006-01-02T15:04:05.999Z",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999 -0700",
		"2006-01-02 15:04:05Z07:00",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	localLayouts := []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, l := range localLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t.UTC(), nil
		}
	}
	// JavaScript Date.toString() style is not supported; give up.
	return time.Time{}, fmt.Errorf("invalid time %q", s)
}
