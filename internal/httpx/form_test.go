package httpx

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// multipartRequest builds a request the way axios toFormData does.
func multipartRequest(t *testing.T, fields [][2]string) *Form {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = w.WriteField(f[0], f[1])
	}
	fw, _ := w.CreateFormFile("photo", "a.png")
	_, _ = fw.Write([]byte("png"))
	_ = w.Close()
	r := httptest.NewRequest("POST", "/", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	f, err := ParseRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseAxiosMultipart(t *testing.T) {
	f := multipartRequest(t, [][2]string{
		{"asset_id", "12"}, {"name", "  Chiller  "}, {"flag", "true"}, {"ratio", "1.5"},
		{"location_ids[]", "3"}, {"location_ids[]", "4"},
		{"filter[severity]", "2"}, {"filter[types][]", "1"}, {"filter[types][]", "2"},
		{"empty_ids", "{}"}, {"pg_ids", "{5,6}"}, {"csv", "7, 8"}, {"json_ids", "[9,10]"},
		{"techs", "a,b"}, {"null_value", "null"}, {"payload", `{"x":[1,2]}`},
	})
	if f.Int("asset_id", 0) != 12 || f.StrTrim("name") != "Chiller" || !f.Bool("flag", false) || f.Float("ratio", 0) != 1.5 {
		t.Fatalf("scalars: %v %q %v %v", f.Int("asset_id", 0), f.StrTrim("name"), f.Bool("flag", false), f.Float("ratio", 0))
	}
	check := func(key string, want []int64, wantPresent bool) {
		t.Helper()
		got, present := f.IDs(key)
		if present != wantPresent || !reflect.DeepEqual(got, want) {
			t.Errorf("IDs(%s) = %v,%v want %v,%v", key, got, present, want, wantPresent)
		}
	}
	check("location_ids", []int64{3, 4}, true)
	check("empty_ids", []int64{}, true)
	check("pg_ids", []int64{5, 6}, true)
	check("csv", []int64{7, 8}, true)
	check("json_ids", []int64{9, 10}, true)
	check("missing", nil, false)
	check("null_value", nil, false)

	var filter struct {
		Severity string   `json:"severity"`
		Types    []string `json:"types"`
	}
	if ok, err := f.JSON("filter", &filter); !ok || err != nil || filter.Severity != "2" || len(filter.Types) != 2 {
		t.Fatalf("nested object: %+v %v %v", filter, ok, err)
	}
	var payload map[string][]int
	if ok, err := f.JSON("payload", &payload); !ok || err != nil || len(payload["x"]) != 2 {
		t.Fatalf("json string: %v %v", payload, err)
	}
	if l, ok := f.StringList("techs"); !ok || !reflect.DeepEqual(l, []string{"a", "b"}) {
		t.Fatalf("string list: %v", l)
	}
	if f.File("photo") == nil || f.File("photo").Filename != "a.png" {
		t.Fatal("file missing")
	}
	if f.IntPtr("missing") != nil || f.NonEmptyPtr("missing") != nil {
		t.Fatal("missing keys must be nil")
	}
}

func TestTimeParsing(t *testing.T) {
	sg := time.FixedZone("SGT", 8*3600)
	f := NewForm(map[string]any{
		"iso":      "2026-10-06T07:00:00.000Z",
		"offset":   "2026-10-06T00:00:00+08:00",
		"date":     "2026-10-06",
		"datetime": "2026-10-06 15:04",
		"ms":       "1791274155929",
	})
	want := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)
	if got := f.TimePtr("iso", sg); got == nil || !got.Equal(want) {
		t.Errorf("iso: %v", got)
	}
	if got := f.TimePtr("offset", time.UTC); got == nil || !got.Equal(time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("offset: %v", got)
	}
	// Plain dates are interpreted in the client time zone.
	if got := f.TimePtr("date", sg); got == nil || !got.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, sg)) {
		t.Errorf("date: %v", got)
	}
	if got := f.TimePtr("datetime", sg); got == nil || !got.Equal(time.Date(2026, 10, 6, 15, 4, 0, 0, sg)) {
		t.Errorf("datetime: %v", got)
	}
	if got := f.TimePtr("ms", sg); got == nil || got.UnixMilli() != 1791274155929 {
		t.Errorf("unix ms: %v", got)
	}
}

func TestKeyMessages(t *testing.T) {
	if got := Key("alarm_successfully_updated", "E01"); got != "MSG:alarm_successfully_updated:E01" {
		t.Fatalf("Key = %q", got)
	}
	if got := TS(time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.FixedZone("x", 3600))); got != "2026-01-02T02:04:05.006Z" {
		t.Fatalf("TS = %q", got)
	}
}
