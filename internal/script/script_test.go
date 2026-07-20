package script

import (
	"errors"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	for _, ok := range []string{"", "x.v > 1", "if (a.v) { return 1 } return 0", "const y = 2; y * 3"} {
		if err := Check(ok); err != nil {
			t.Errorf("Check(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"x.v ===", "return (", "if (a { }"} {
		if err := Check(bad); err == nil {
			t.Errorf("Check(%q) accepted a syntax error", bad)
		}
	}
}

func TestRun(t *testing.T) {
	r := NewRuntime(time.Second)
	_ = r.SetJSON("temp", map[string]any{"v": 81.5, "t": 1, "q": true})
	cases := []struct {
		src  string
		want any
	}{
		{"temp.v > 80", true},
		{"temp.v * 2", 163.0},
		{"if (temp.v > 100) { return 'hot' } return 'ok'", "ok"},
		{"const limits = [70, 80]; temp.v > Math.max(...limits)", true},
	}
	for _, c := range cases {
		got, err := r.Run(c.src)
		if err != nil {
			t.Fatalf("Run(%q): %v", c.src, err)
		}
		switch w := c.want.(type) {
		case float64:
			var f float64
			switch g := got.(type) {
			case float64:
				f = g
			case int64:
				f = float64(g)
			}
			if f != w {
				t.Errorf("Run(%q) = %v (%T), want %v", c.src, got, got, w)
			}
		default:
			if got != c.want {
				t.Errorf("Run(%q) = %v, want %v", c.src, got, c.want)
			}
		}
	}
	if _, err := r.Run("undefinedThing.v > 1"); err == nil {
		t.Error("reference error not reported")
	}
}

func TestTimeout(t *testing.T) {
	r := NewRuntime(100 * time.Millisecond)
	start := time.Now()
	_, err := r.Run("while (true) {}")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
	// The runtime is usable after an interrupt.
	if v, err := r.Run("1 + 1"); err != nil || v != int64(2) && v != 2.0 {
		t.Fatalf("after timeout: %v %v", v, err)
	}
}

func TestTruthyAndCoerce(t *testing.T) {
	for v, want := range map[any]bool{nil: false, true: true, false: false, 0.0: false, 2.0: true, int64(0): false, "": false, "x": true} {
		if Truthy(v) != want {
			t.Errorf("Truthy(%v) != %v", v, want)
		}
	}
	if CoerceValue("12.5", "number") != 12.5 || CoerceValue("true", "boolean") != true || CoerceValue("abc", "string") != "abc" {
		t.Error("CoerceValue basic types")
	}
	if CoerceValue("1", "boolean") != true || CoerceValue("0", "boolean") != false {
		t.Error("numeric booleans")
	}
	if CoerceValue("n/a", "number") != nil {
		t.Error("invalid numbers must become nil")
	}
	if !ValidIdentifier("temp_1") || ValidIdentifier("1temp") || ValidIdentifier("return") || ValidIdentifier("Math") {
		t.Error("ValidIdentifier")
	}
}
