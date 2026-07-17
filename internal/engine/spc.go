package engine

import (
	"context"
	"math"
	"sync"
	"time"

	"afdd/internal/notify"
)

const spcHistory = 20

// spcTracker keeps the last logged values of every SPC enabled parameter and
// throttles repeated notifications.
type spcTracker struct {
	mu       sync.Mutex
	history  map[int64][]float64
	lastSent map[string]time.Time
}

func newSPCTracker() *spcTracker {
	return &spcTracker{history: map[int64][]float64{}, lastSent: map[string]time.Time{}}
}

func (t *spcTracker) push(epid int64, v float64) []float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := append(t.history[epid], v)
	if len(h) > spcHistory {
		h = h[len(h)-spcHistory:]
	}
	t.history[epid] = h
	return append([]float64(nil), h...)
}

func (t *spcTracker) allow(key string, now time.Time, cooldown time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.lastSent[key]; ok && now.Sub(last) < cooldown {
		return false
	}
	t.lastSent[key] = now
	return true
}

// SPCViolations evaluates the Western Electric / Nelson rules on the most
// recent points (last element is the newest) and returns violated categories.
func SPCViolations(points []float64, p *ParamDef) []string {
	if len(points) == 0 {
		return nil
	}
	last := points[len(points)-1]
	var out []string
	if (p.USL != nil && last > *p.USL) || (p.LSL != nil && last < *p.LSL) {
		out = append(out, "SPC_OUT_OF_SPEC")
	}
	if (p.UCL != nil && last > *p.UCL) || (p.LCL != nil && last < *p.LCL) {
		out = append(out, "SPC_OUT_OF_CONTROL")
	}
	cl, sigma, ok := centerAndSigma(p)
	if !ok {
		return out
	}
	z := func(v float64) float64 { return (v - cl) / sigma }
	tail := func(n int) []float64 {
		if len(points) < n {
			return nil
		}
		return points[len(points)-n:]
	}
	// 2 of 3 points beyond 2 sigma on the same side.
	if w := tail(3); w != nil {
		up, down := 0, 0
		for _, v := range w {
			if z(v) > 2 {
				up++
			} else if z(v) < -2 {
				down++
			}
		}
		if up >= 2 || down >= 2 {
			out = append(out, "SPC_2_OUT_OF_3_POINTS_ZONE_A_OR_BEYOND")
		}
	}
	// 4 of 5 points beyond 1 sigma on the same side.
	if w := tail(5); w != nil {
		up, down := 0, 0
		for _, v := range w {
			if z(v) > 1 {
				up++
			} else if z(v) < -1 {
				down++
			}
		}
		if up >= 4 || down >= 4 {
			out = append(out, "SPC_4_OUT_OF_5_POINTS_ZONE_B_OR_BEYOND")
		}
	}
	// 8 consecutive points outside zone C (beyond 1 sigma, either side).
	if w := tail(8); w != nil {
		all := true
		for _, v := range w {
			if math.Abs(z(v)) <= 1 {
				all = false
				break
			}
		}
		if all {
			out = append(out, "SPC_8_POINTS_ZONE_A")
		}
	}
	// 7 consecutive points steadily increasing or decreasing.
	if w := tail(7); w != nil {
		inc, dec := true, true
		for i := 1; i < len(w); i++ {
			if !(w[i] > w[i-1]) {
				inc = false
			}
			if !(w[i] < w[i-1]) {
				dec = false
			}
		}
		if inc || dec {
			out = append(out, "SPC_7_POINTS_MONOTONIC")
		}
	}
	// 14 consecutive points alternating up and down.
	if w := tail(14); w != nil {
		alt := true
		for i := 2; i < len(w); i++ {
			d1 := w[i-1] - w[i-2]
			d2 := w[i] - w[i-1]
			if d1 == 0 || d2 == 0 || (d1 > 0) == (d2 > 0) {
				alt = false
				break
			}
		}
		if alt {
			out = append(out, "SPC_14_POINTS_ALTERNATING")
		}
	}
	// 15 consecutive points within 1 sigma (stratification).
	if w := tail(15); w != nil {
		all := true
		for _, v := range w {
			if math.Abs(z(v)) > 1 {
				all = false
				break
			}
		}
		if all {
			out = append(out, "SPC_15_POINTS_ZONE_C")
		}
	}
	return out
}

func centerAndSigma(p *ParamDef) (float64, float64, bool) {
	if p.CenterLine != nil && p.Sigma != nil && *p.Sigma > 0 {
		return *p.CenterLine, *p.Sigma, true
	}
	if p.UCL != nil && p.LCL != nil && *p.UCL > *p.LCL {
		cl := (*p.UCL + *p.LCL) / 2
		if p.CenterLine != nil {
			cl = *p.CenterLine
		}
		return cl, (*p.UCL - *p.LCL) / 6, true
	}
	return 0, 0, false
}

func (e *Engine) checkSPC(ctx context.Context, eq *Equipment, p *ParamDef, v float64, at time.Time) {
	if p.USL == nil && p.LSL == nil && p.UCL == nil && p.LCL == nil && p.CenterLine == nil {
		return
	}
	points := e.spc.push(p.EPID, v)
	for _, cat := range SPCViolations(points, p) {
		key := cat + ":" + itoa(p.EPID)
		if !e.spc.allow(key, at, time.Hour) {
			continue
		}
		msg := notify.SPCControlMessage(eq.Name+" "+p.Name, at)
		if cat == "SPC_OUT_OF_SPEC" {
			msg = notify.SPCSpecMessage(eq.Name+" "+p.Name, at)
		}
		assetID := eq.ID
		loc := eq.LocationID
		_ = e.notify.Send(ctx, notify.Notification{
			Category: cat, Message: msg, AssetID: &assetID, LocationID: &loc, Timestamp: at,
			AdditionalData: map[string]any{"parameter_id": p.EPID, "parameter_name": p.Name, "value": v},
		})
	}
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
