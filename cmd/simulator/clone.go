package main

import (
	"maps"
	"math/rand/v2"
	"reflect"
)

// Clone returns a deep copy of the plant for a simulation job: every asset's
// state (duty, trips, maintenance, communication, faults, controls, plant
// controller commands, lags, loop and PI states, noise processes, last true
// and measured values), the weather processes and the environment. The
// random sources are new ones derived from seed, so clones made with the
// same seed evolve identically until their actions differ. Model and
// equipment definitions are read-only and shared; EnvHook is a setting of
// the job using the clone and is not copied.
func (p *Plant) Clone(seed uint64) *Plant {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	c := &Plant{ByAlias: make(map[string]*Asset, len(p.Assets)), Models: p.Models, Env: p.Env, loc: p.loc, anchor: p.anchor, rng: r,
		defs: p.defs, groups: make(map[string][]*Asset, len(p.groups))}
	c.weather = p.weather.clone(rand.New(rand.NewPCG(r.Uint64(), r.Uint64())))
	byOld := make(map[*Asset]*Asset, len(p.Assets))
	for _, a := range p.Assets {
		na := a.clone(rand.New(rand.NewPCG(r.Uint64(), r.Uint64())))
		byOld[a] = na
		c.addAsset(na)
	}
	if p.Truth != nil {
		c.Truth = make(map[*Asset]map[string]float64, len(p.Truth))
		for a, v := range p.Truth {
			if na := byOld[a]; na != nil {
				c.Truth[na] = na.last
				if na.last == nil {
					c.Truth[na] = maps.Clone(v)
				}
			}
		}
	}
	return c
}

// clone deep-copies an asset with a new random source.
func (a *Asset) clone(r *rand.Rand) *Asset {
	na := *a
	na.rng = r
	if a.TripSpec != nil {
		ts := *a.TripSpec
		na.TripSpec = &ts
	}
	na.Faults = make(map[string]*Fault, len(a.Faults))
	for k, f := range a.Faults {
		nf := *f
		na.Faults[k] = &nf
	}
	na.Ctl = maps.Clone(a.Ctl)
	na.Cmd = maps.Clone(a.Cmd)
	na.smooth = maps.Clone(a.smooth)
	na.noise = make(map[string]*ar, len(a.noise))
	for k, n := range a.noise {
		nn := *n
		na.noise[k] = &nn
	}
	if na.Ctl == nil {
		na.Ctl = map[string]float64{}
	}
	if na.Cmd == nil {
		na.Cmd = map[string]float64{}
	}
	if na.smooth == nil {
		na.smooth = map[string]float64{}
	}
	na.last = maps.Clone(a.last)
	switch {
	case a.meas == nil:
		na.meas = nil
	case sameMap(a.meas, a.last):
		na.meas = na.last // no sensor fault: measured = true
	default:
		na.meas = maps.Clone(a.meas)
	}
	return &na
}

// sameMap reports whether two maps are the same map object (not just equal).
func sameMap(a, b map[string]float64) bool {
	if a == nil || b == nil {
		return false
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// clone copies the weather processes with a new random source.
func (w *Weather) clone(r *rand.Rand) *Weather {
	db, wb, load := *w.db, *w.wb, *w.load
	return &Weather{db: &db, wb: &wb, load: &load, rng: r}
}
