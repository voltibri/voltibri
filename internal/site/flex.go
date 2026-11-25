// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/sdk"
)

// Surplus loads in steps: devices with flexibility "surplus" that run in
// steps (a heating rod switched phase by phase) use the surplus the
// priority order leaves them (surplus.go). Each load has a mode: off,
// surplus (default) or boost (surplus plus power from the home battery,
// before all other loads).
//
// A step is added after the surplus has covered it (plus a margin) for
// flexUp, and the last one is taken back after a deficit for flexDown.
// While a load waits for its next step, that step is reserved: the loads
// after it do not take the same surplus at the same time.
//
// Every tick the commanded step is compared with the switched channels: a
// Shelly that restarted (outputs off) or was switched by hand is taken
// over as it is, and the control continues from there. In a dry run
// nothing is switched, so only the first reading is taken over.

const (
	flexUp      = 2 * time.Minute
	flexDown    = time.Minute
	flexMargin  = 200.0            // W of surplus kept beyond the next step
	flexImportW = 100.0            // deficit that counts as "too much"
	flexHold    = 2 * time.Minute  // minimum time between two changes of a load
	flexSettle  = 10 * time.Second // after a change the channels may still report the old state
)

// LoadMode is the mode of a surplus load in steps.
type LoadMode string

const (
	LoadOff     LoadMode = "off"
	LoadSurplus LoadMode = "surplus" // default
	LoadBoost   LoadMode = "boost"   // surplus + home battery
)

// Valid reports whether m is a known mode.
func (m LoadMode) Valid() bool { return m == LoadOff || m == LoadSurplus || m == LoadBoost }

// flexLoad is one surplus load.
type flexLoad struct {
	id        string
	dev       sdk.Stager
	noBat     bool      // the battery must not supply it: no boost
	tie       int       // order at equal priority: hot water before heating support
	index     int       // position in the configuration
	mode      LoadMode  // set by the site every tick
	step      int       // commanded step (also in dry run)
	changed   time.Time // last change
	upSince   time.Time
	downSince time.Time
}

// flexState holds the surplus loads in steps.
type flexState struct {
	dryRun bool
	synced bool // the actual steps were taken over once
	loads  []*flexLoad
}

// newFlex collects the surplus loads of the configuration.
func newFlex(cfg *config.Config, devices map[string]sdk.Device) *flexState {
	f := &flexState{dryRun: cfg.Site.DryRun}
	for i, d := range cfg.Devices {
		if d.Flexibility != config.FlexSurplus || d.Retired(time.Now()) {
			continue
		}
		st, ok := devices[d.ID].(sdk.Stager)
		if !ok || sdk.ControlMode(d.Mode) != sdk.ModeOwned {
			continue
		}
		tie := 2
		switch d.HeatUse {
		case config.HeatUseHotWater:
			tie = 0
		case config.HeatUseHeating:
			tie = 1
		}
		f.loads = append(f.loads, &flexLoad{id: d.ID, dev: st, tie: tie, index: i, mode: LoadSurplus,
			noBat: d.BatteryUse == config.BatteryNever})
	}
	sort.SliceStable(f.loads, func(i, j int) bool { return f.loads[i].tie < f.loads[j].tie })
	return f
}

// load returns the load with the id.
func (f *flexState) load(id string) *flexLoad {
	if f == nil {
		return nil
	}
	for _, l := range f.loads {
		if l.id == id {
			return l
		}
	}
	return nil
}

// update runs one tick for all loads in their own order, without battery
// and without other surplus loads: gridW > 0 is import, < 0 feed-in.
func (f *flexState) update(ctx context.Context, now time.Time, gridW float64) {
	if f == nil || len(f.loads) == 0 {
		return
	}
	f.reconcile(ctx, now)
	avail := -gridW
	for _, l := range f.loads {
		avail += float64(l.step) * l.dev.StepPowerW()
	}
	for _, l := range f.loads {
		avail -= f.control(ctx, now, l, avail, "PV-Überschuss")
	}
}

// control steps one load with the surplus it may use (including what it
// draws now) and returns the power it takes from now on.
func (f *flexState) control(ctx context.Context, now time.Time, l *flexLoad, avail float64, why string) float64 {
	stepW := l.dev.StepPowerW()
	if l.mode == LoadOff {
		l.upSince, l.downSince = time.Time{}, time.Time{}
		if l.step > 0 {
			f.set(ctx, now, l, 0, "ausgeschaltet")
		}
		return float64(l.step) * stepW
	}
	own := float64(l.step) * stepW
	switch {
	case l.step > 0 && avail < own-flexImportW:
		l.upSince = time.Time{}
		if l.downSince.IsZero() {
			l.downSince = now
		}
		if now.Sub(l.downSince) >= flexDown {
			f.set(ctx, now, l, l.step-1, "Überschuss reicht nicht")
			l.downSince = time.Time{}
		}
	case l.step < l.dev.Steps() && avail >= own+stepW+flexMargin:
		l.downSince = time.Time{}
		if l.upSince.IsZero() {
			l.upSince = now
		}
		if now.Sub(l.upSince) >= flexUp && now.Sub(l.changed) >= flexHold {
			f.set(ctx, now, l, l.step+1, why)
			l.upSince = time.Time{}
		}
	default:
		l.upSince, l.downSince = time.Time{}, time.Time{}
	}
	take := float64(l.step) * stepW
	if !l.upSince.IsZero() && l.step < l.dev.Steps() {
		take += stepW // reserved for the next step
	}
	return take
}

func (f *flexState) set(ctx context.Context, now time.Time, l *flexLoad, step int, why string) {
	if err := l.dev.SetStep(ctx, step); err != nil {
		// some channels may have switched: go on from what is switched now
		slog.Error("switching surplus load failed", "device", l.id, "step", step, "err", err)
		if n, e := l.dev.Step(ctx); e == nil && n != l.step {
			l.step, l.changed = n, now
		}
		return
	}
	slog.Info("surplus load", "device", l.id, "step", step, "of", l.dev.Steps(), "reason", why)
	l.step, l.changed = step, now
}

// reconcile takes over the actual steps where they differ from the
// commanded ones (restart of voltibri or of a Shelly, switched by hand).
// A load that cannot be read keeps its commanded step.
func (f *flexState) reconcile(ctx context.Context, now time.Time) {
	if f == nil || (f.synced && f.dryRun) {
		return // nothing is switched: the commanded step is the simulated one
	}
	first := !f.synced
	f.synced = true
	for _, l := range f.loads {
		if !first && now.Sub(l.changed) < flexSettle {
			continue
		}
		n, err := l.dev.Step(ctx)
		if err != nil || n == l.step {
			continue
		}
		if !first {
			slog.Warn("surplus load switched outside voltibri", "device", l.id, "commanded", l.step, "actual", n)
		}
		l.step, l.changed = n, now
	}
}

// powerW is the power the surplus loads draw now (from the readings).
func (f *flexState) powerW(cs []Reading) float64 {
	if f == nil {
		return 0
	}
	w := 0.0
	for _, l := range f.loads {
		for _, c := range cs {
			if c.ID == l.id {
				w += c.PowerW
			}
		}
	}
	return w
}
