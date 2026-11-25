// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package shelly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/voltibri/voltibri/sdk"
)

// fakeShelly is a Shelly 2PM: two channels with output, power and current.
type fakeShelly struct {
	mu  sync.Mutex
	on  [2]bool
	w   [2]float64
	e   [2]float64 // energy counter, Wh
	set int        // Switch.Set calls
}

func (f *fakeShelly) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var id int
	fmt.Sscan(r.URL.Query().Get("id"), &id)
	switch r.URL.Path {
	case "/rpc/Switch.GetStatus":
		p := 0.0
		if f.on[id] {
			p = f.w[id]
		}
		json.NewEncoder(w).Encode(map[string]any{"output": f.on[id], "apower": p, "current": p / 230, "aenergy": map[string]float64{"total": 1000 + f.e[id]}})
	case "/rpc/Switch.Set":
		f.on[id] = r.URL.Query().Get("on") == "true"
		f.set++
		w.Write([]byte(`{"was_on":false}`))
	}
}

func TestRod(t *testing.T) {
	// shelly A switches L1 of both rods, shelly B L2, shelly C L3; rod "oben" is channel 1
	var fs [3]*fakeShelly
	var hosts [3]string
	for i := range fs {
		fs[i] = &fakeShelly{w: [2]float64{1500, 2000}}
		srv := httptest.NewServer(http.HandlerFunc(fs[i].handler))
		defer srv.Close()
		hosts[i] = strings.TrimPrefix(srv.URL, "http://")
	}
	cfg := sdk.DeviceConfig{ID: "oben", Mode: sdk.ModeOwned, Params: map[string]any{
		"l1": hosts[0] + ":1", "l2": hosts[1] + ":1", "l3": hosts[2] + ":1", "powerKW": 6.0}}
	d, err := NewRod(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rod := d.(*Rod)
	ctx := context.Background()
	if rod.Steps() != 3 || rod.StepPowerW() != 2000 {
		t.Fatalf("steps %d, %v W", rod.Steps(), rod.StepPowerW())
	}
	if err := rod.SetStep(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if !fs[0].on[1] || !fs[1].on[1] || fs[2].on[1] || fs[0].on[0] {
		t.Fatalf("step 2 switched %+v %+v %+v", fs[0].on, fs[1].on, fs[2].on)
	}
	if n, _ := rod.Step(ctx); n != 2 {
		t.Fatalf("step %d", n)
	}
	if w, _ := rod.ConsumerPower(ctx); w != 4000 {
		t.Fatalf("power %v", w)
	}
	if p, _ := rod.Currents(ctx); p[0] < 8 || p[1] < 8 || p[2] != 0 {
		t.Fatalf("currents %v", p)
	}
	// rotation: L3 has run least, so step 3 -> 2 drops L1 or L2 (the one with
	// more energy), and a later step up picks the phase with the least
	if err := rod.SetStep(ctx, 3); err != nil {
		t.Fatal(err)
	}
	fs[0].e[1], fs[1].e[1], fs[2].e[1] = 500, 800, 100
	rod.last = nil
	calls := fs[0].set + fs[1].set + fs[2].set
	if err := rod.SetStep(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if fs[1].on[1] || !fs[0].on[1] || !fs[2].on[1] {
		t.Fatalf("step down should drop L2 (most energy): %v %v %v", fs[0].on[1], fs[1].on[1], fs[2].on[1])
	}
	if c := fs[0].set + fs[1].set + fs[2].set - calls; c != 1 {
		t.Fatalf("switched %d channels for one step", c)
	}
	rod.last = nil
	rod.SetStep(ctx, 0)
	rod.last = nil
	rod.SetStep(ctx, 1)
	if !fs[2].on[1] || fs[0].on[1] || fs[1].on[1] {
		t.Fatalf("step 1 should be L3 (least energy): %v %v %v", fs[0].on[1], fs[1].on[1], fs[2].on[1])
	}
	// correction: L3 gets +1 kWh, so step 1 now goes to L1 (500 Wh)
	corr := map[string]any{}
	for k, v := range cfg.Params {
		corr[k] = v
	}
	corr["l3OffsetKWh"] = 1.0
	rc, _ := NewRod(sdk.DeviceConfig{ID: "oben", Mode: sdk.ModeOwned, Params: corr})
	rc.(*Rod).SetStep(ctx, 0)
	rc.(*Rod).last = nil
	rc.(*Rod).SetStep(ctx, 1)
	if !fs[0].on[1] || fs[1].on[1] || fs[2].on[1] {
		t.Fatalf("offset ignored: %v %v %v", fs[0].on[1], fs[1].on[1], fs[2].on[1])
	}
	// fixed: L1 first, L2 next; down takes L2 back first (despite energy)
	fixed := map[string]any{"phaseOrder": OrderFixed}
	for k, v := range cfg.Params {
		fixed[k] = v
	}
	fs[0].e[1] = 5000
	rf, _ := NewRod(sdk.DeviceConfig{ID: "oben", Mode: sdk.ModeOwned, Params: fixed})
	for _, n := range []int{0, 2, 1} {
		rf.(*Rod).last = nil
		rf.(*Rod).SetStep(ctx, n)
	}
	if !fs[0].on[1] || fs[1].on[1] || fs[2].on[1] {
		t.Fatalf("fixed: %v %v %v", fs[0].on[1], fs[1].on[1], fs[2].on[1])
	}
	// random: the choice comes from randN among the phases that are off
	random := map[string]any{"phaseOrder": OrderRandom}
	for k, v := range cfg.Params {
		random[k] = v
	}
	rr, _ := NewRod(sdk.DeviceConfig{ID: "oben", Mode: sdk.ModeOwned, Params: random})
	rr.(*Rod).randN = func(n int) int { return n - 1 } // always the last candidate
	rr.(*Rod).last = nil
	rr.(*Rod).SetStep(ctx, 0)
	rr.(*Rod).last = nil
	rr.(*Rod).SetStep(ctx, 1)
	if fs[0].on[1] || fs[1].on[1] || !fs[2].on[1] {
		t.Fatalf("random: %v %v %v", fs[0].on[1], fs[1].on[1], fs[2].on[1])
	}
	if _, err := NewRod(sdk.DeviceConfig{ID: "u", Params: map[string]any{"l1": hosts[0], "powerKW": 2.0, "phaseOrder": "lifo"}}); err == nil {
		t.Fatal("unknown phaseOrder accepted")
	}
	// single phase rod: one step
	one, _ := NewRod(sdk.DeviceConfig{ID: "x", Params: map[string]any{"l1": hosts[0], "powerKW": 2.0}})
	if one.(*Rod).Steps() != 1 || one.(*Rod).phases[0].dev.channel != 0 {
		t.Fatalf("single phase: %+v", one)
	}
	// observed: no writes; dry run: logged only
	obs, _ := NewRod(sdk.DeviceConfig{ID: "o", Mode: sdk.ModeObserved, Params: cfg.Params})
	if err := obs.(*Rod).SetStep(ctx, 3); err == nil {
		t.Fatal("observed rod was switched")
	}
	before := fs[2].set
	dry, _ := NewRod(sdk.DeviceConfig{ID: "d", Mode: sdk.ModeOwned, DryRun: true, Params: cfg.Params})
	if err := dry.(*Rod).SetStep(ctx, 3); err != nil || fs[2].set != before {
		t.Fatalf("dry run wrote: %v", err)
	}
	if _, err := NewRod(sdk.DeviceConfig{ID: "e", Params: map[string]any{"powerKW": 6.0}}); err == nil {
		t.Fatal("rod without channel accepted")
	}
}
