// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/sdk"
)

type fakeStager struct {
	steps  int
	stepW  float64
	step   int
	dry    bool // SetStep switches nothing (dry run)
	failAt int  // SetStep to this step fails after switching one step (0 = never)
}

func (f *fakeStager) ID() string                        { return "x" }
func (f *fakeStager) Steps() int                        { return f.steps }
func (f *fakeStager) StepPowerW() float64               { return f.stepW }
func (f *fakeStager) Step(context.Context) (int, error) { return f.step, nil }
func (f *fakeStager) SetStep(_ context.Context, n int) error {
	switch {
	case f.dry:
	case f.failAt != 0 && n == f.failAt:
		f.step++ // one channel switched, the next failed
		return errors.New("timeout")
	default:
		f.step = n
	}
	return nil
}

func TestFlexSteps(t *testing.T) {
	top, mid := &fakeStager{steps: 3, stepW: 2000}, &fakeStager{steps: 3, stepW: 1500}
	cfg := &config.Config{Devices: []config.Device{
		{ID: "mitte", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHeating},
		{ID: "oben", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHotWater},
		{ID: "obs", Mode: "observed", Flexibility: config.FlexSurplus}, // not controlled
	}}
	f := newFlex(cfg, map[string]sdk.Device{"oben": top, "mitte": mid, "obs": &fakeStager{steps: 1, stepW: 1000}})
	if len(f.loads) != 2 || f.loads[0].id != "oben" {
		t.Fatalf("loads: %+v", f.loads)
	}
	ctx, t0 := context.Background(), time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }
	// 1.6 kW feed-in: not enough for a step of either (2 / 1.5 kW + margin)
	f.update(ctx, at(0), -1600)
	f.update(ctx, at(3), -1600)
	if top.step != 0 || mid.step != 0 {
		t.Fatalf("stepped without enough surplus: %d %d", top.step, mid.step)
	}
	// 2.5 kW feed-in for 2 minutes: hot water first, one step
	f.update(ctx, at(4), -2500)
	f.update(ctx, at(5), -2500)
	if top.step != 0 {
		t.Fatal("stepped before 2 minutes")
	}
	f.update(ctx, at(6), -2500)
	if top.step != 1 || mid.step != 0 {
		t.Fatalf("after 2 min: top %d mid %d", top.step, mid.step)
	}
	// feed-in stays high: top runs up to 3 steps, then heating support
	for m := 6.5; m < 30; m += 0.5 {
		f.update(ctx, at(m), -3000)
	}
	if top.step != 3 || mid.step < 1 {
		t.Fatalf("filling: top %d mid %d", top.step, mid.step)
	}
	// grid import: heating support steps down first, after one minute
	before := mid.step
	f.update(ctx, at(31), 800)
	f.update(ctx, at(31.5), 800)
	if mid.step != before {
		t.Fatal("stepped down too early")
	}
	f.update(ctx, at(32), 800)
	if mid.step != before-1 || top.step != 3 {
		t.Fatalf("down: top %d mid %d", top.step, mid.step)
	}
	if w := f.powerW([]Reading{{ID: "oben", PowerW: 6000}, {ID: "x", PowerW: 99}}); w != 6000 {
		t.Fatalf("power %v", w)
	}
}

func TestFlexReconcile(t *testing.T) {
	rod := &fakeStager{steps: 3, stepW: 1000, step: 2} // already on at the start
	cfg := &config.Config{Devices: []config.Device{{ID: "stab", Mode: "owned", Flexibility: config.FlexSurplus}}}
	f := newFlex(cfg, map[string]sdk.Device{"stab": rod})
	ctx, t0 := context.Background(), time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }

	// first tick takes over the actual step
	f.update(ctx, at(0), 0)
	if f.loads[0].step != 2 {
		t.Fatalf("start: %d", f.loads[0].step)
	}
	// the Shelly restarts: outputs off, the control continues from 0
	rod.step = 0
	f.update(ctx, at(5), 0)
	if f.loads[0].step != 0 {
		t.Fatalf("restart not taken over: %d", f.loads[0].step)
	}
	// switched on by hand: taken over, grid import steps it down again
	rod.step = 3
	f.update(ctx, at(10), 0)
	f.update(ctx, at(10.5), 500)
	f.update(ctx, at(11.5), 500)
	if rod.step != 2 || f.loads[0].step != 2 {
		t.Fatalf("manual: rod %d commanded %d", rod.step, f.loads[0].step)
	}

	// switching 0 -> 3 fails after the first channel: commanded follows (1)
	rod.failAt = 3
	rod.step = 0
	f.update(ctx, at(20), 0) // takes over 0
	f.set(ctx, at(21), f.loads[0], 3, "test")
	if rod.step != 1 || f.loads[0].step != 1 {
		t.Fatalf("after failure: rod %d commanded %d", rod.step, f.loads[0].step)
	}
}

func TestFlexReconcileDryRun(t *testing.T) {
	rod := &fakeStager{steps: 3, stepW: 1000, dry: true}
	cfg := &config.Config{Site: config.Site{DryRun: true},
		Devices: []config.Device{{ID: "stab", Mode: "owned", Flexibility: config.FlexSurplus}}}
	f := newFlex(cfg, map[string]sdk.Device{"stab": rod})
	ctx, t0 := context.Background(), time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }
	for m := 0.0; m <= 10; m += 0.5 {
		f.update(ctx, at(m), -5000)
	}
	// nothing switched, yet the simulated steps are kept (no reset to 0)
	if rod.step != 0 || f.loads[0].step != 3 {
		t.Fatalf("dry run: rod %d commanded %d", rod.step, f.loads[0].step)
	}
}

func TestSurplusOrder(t *testing.T) {
	hot, heat := &fakeStager{steps: 3, stepW: 1000}, &fakeStager{steps: 2, stepW: 1500}
	never := &fakeStager{steps: 1, stepW: 1000}
	cfg := config.Default()
	cfg.Battery.MaxDischargeW = 5000
	cfg.Devices = []config.Device{
		{ID: "wb", Mode: "owned"},
		{ID: "heiz", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHeating},
		{ID: "ww", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHotWater},
		{ID: "nie", Mode: "owned", Flexibility: config.FlexSurplus, BatteryUse: config.BatteryNever},
	}
	cfg.Charging.Loadpoints = []config.Loadpoint{{Charger: "wb"}}
	s := &Site{cfg: cfg}
	s.flex = newFlex(cfg, map[string]sdk.Device{"heiz": heat, "ww": hot, "nie": never})
	ctx, now := context.Background(), time.Now()
	order := func(a *allocator) (ids []string) {
		for _, e := range a.entries {
			ids = append(ids, e.id)
		}
		return
	}
	lp := charging.Loadpoint{ID: "wb", Connected: true, Mode: charging.ModePV}
	in := surplusInput{loadpoints: []charging.Loadpoint{lp}}
	// equal priority: wallbox, hot water, heating, other
	if got := fmt.Sprint(order(s.newAllocator(ctx, now, in))); got != "[wb ww heiz nie]" {
		t.Fatalf("default order %s", got)
	}
	// priorities: higher first
	s.SetLoadPriority("heiz", 8)
	s.SetLoadPriority("wb", 2)
	if got := fmt.Sprint(order(s.newAllocator(ctx, now, in))); got != "[heiz ww nie wb]" {
		t.Fatalf("priority order %s", got)
	}
	// a boost by hand comes first, a car charging fast is booked before everything
	s.SetLoadMode("nie", LoadBoost)
	if got := fmt.Sprint(order(s.newAllocator(ctx, now, in))); got != "[nie heiz ww wb]" {
		t.Fatalf("boost order %s", got)
	}
	in.loadpoints[0].Mode = charging.ModeFast
	if got := fmt.Sprint(order(s.newAllocator(ctx, now, in))); got != "[wb nie heiz ww]" {
		t.Fatalf("fast order %s", got)
	}
	if err := s.SetLoadMode("wb", LoadBoost); err == nil {
		t.Fatal("mode for a wallbox accepted")
	}
	if err := s.SetLoadPriority("ww", 11); err == nil {
		t.Fatal("priority 11 accepted")
	}
}

func TestSurplusShare(t *testing.T) {
	hot, heat := &fakeStager{steps: 3, stepW: 1000}, &fakeStager{steps: 2, stepW: 1500}
	cfg := config.Default()
	cfg.Battery.MaxDischargeW = 5000
	cfg.Devices = []config.Device{
		{ID: "heiz", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHeating},
		{ID: "ww", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHotWater},
	}
	s := &Site{cfg: cfg}
	s.flex = newFlex(cfg, map[string]sdk.Device{"heiz": heat, "ww": hot})
	ctx, t0 := context.Background(), time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }
	// a plant with 4.6 kW surplus beyond the house; the battery takes what is left
	tick := func(m float64, boostW float64, auto bool) {
		draw := float64(hot.step)*1000 + float64(heat.step)*1500
		bat := draw - 4600 // > 0 discharge
		a := s.newAllocator(ctx, at(m), surplusInput{batteryW: bat, boostW: boostW, autoBoost: auto,
			rodPowers: map[string]float64{"ww": float64(hot.step) * 1000, "heiz": float64(heat.step) * 1500}})
		a.finish()
	}
	for m := 0.0; m <= 20; m += 0.5 {
		tick(m, 0, false)
	}
	// hot water full (3 kW), heating support one step (1.5 kW): 4.5 of 4.6 kW
	if hot.step != 3 || heat.step != 1 {
		t.Fatalf("surplus: ww %d heiz %d", hot.step, heat.step)
	}
	// heating support first
	s.SetLoadPriority("heiz", 7)
	for m := 20.5; m <= 40; m += 0.5 {
		tick(m, 0, false)
	}
	if heat.step != 2 || hot.step != 1 {
		t.Fatalf("priority: ww %d heiz %d", hot.step, heat.step)
	}
	// automatic boost with 4 kW: everything runs from PV + battery
	for m := 40.5; m <= 60; m += 0.5 {
		tick(m, 4000, true)
	}
	if heat.step != 2 || hot.step != 3 {
		t.Fatalf("boost: ww %d heiz %d", hot.step, heat.step)
	}
	// boost over: the battery must not feed the rods, the lowest steps down
	for m := 60.5; m <= 75; m += 0.5 {
		tick(m, 4000, false)
	}
	if heat.step != 2 || hot.step != 1 {
		t.Fatalf("after boost: ww %d heiz %d", hot.step, heat.step)
	}
	// a rod switched off by the user
	s.SetLoadMode("heiz", LoadOff)
	tick(76, 0, false)
	if heat.step != 0 {
		t.Fatalf("off: %d", heat.step)
	}
}

func TestSurplusTopToday(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "wb", Mode: "owned"},
		{ID: "ww", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHotWater},
		{ID: "heiz", Mode: "owned", Flexibility: config.FlexSurplus, HeatUse: config.HeatUseHeating},
	}
	cfg.Charging.Loadpoints = []config.Loadpoint{{Charger: "wb"}}
	s := &Site{cfg: cfg, loc: time.UTC}
	s.flex = newFlex(cfg, map[string]sdk.Device{"ww": &fakeStager{steps: 3, stepW: 1000}, "heiz": &fakeStager{steps: 2, stepW: 1500}})
	ctx := context.Background()
	in := surplusInput{loadpoints: []charging.Loadpoint{{ID: "wb", Connected: true, Mode: charging.ModePV}}}
	order := func(now time.Time) string {
		var ids []string
		for _, e := range s.newAllocator(ctx, now, in).entries {
			ids = append(ids, e.id)
		}
		return fmt.Sprint(ids)
	}
	s.SetLoadPriority("wb", 0)
	s.SetLoadPriority("ww", 9)
	// first for today: before every priority, a boost by hand still before it
	s.SetLoadTop("heiz", true)
	now := time.Now()
	if got := order(now); got != "[heiz ww wb]" {
		t.Fatalf("top: %s", got)
	}
	s.SetLoadMode("ww", LoadBoost)
	if got := order(now); got != "[ww heiz wb]" {
		t.Fatalf("boost before top: %s", got)
	}
	s.SetLoadMode("ww", LoadSurplus)
	// tomorrow: back to the priorities
	if got := order(now.Add(25 * time.Hour)); got != "[ww heiz wb]" {
		t.Fatalf("next day: %s", got)
	}
	if u := s.setting("heiz").TopUntil; u == nil || u.Hour() != 0 || !u.After(now) || u.After(now.Add(24*time.Hour)) {
		t.Fatalf("until %v", u)
	}
	s.SetLoadTop("heiz", false)
	if s.setting("heiz").TopUntil != nil {
		t.Fatal("not ended")
	}
}
