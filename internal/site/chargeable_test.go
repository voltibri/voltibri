// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/sdk"
)

var t0 = time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

type fakePlug struct {
	on  bool
	set []bool
}

func (p *fakePlug) ID() string                                     { return "ebike" }
func (p *fakePlug) ConsumerPower(context.Context) (float64, error) { return 0, nil }
func (p *fakePlug) SwitchState(context.Context) (bool, error)      { return p.on, nil }
func (p *fakePlug) SetSwitch(_ context.Context, on bool) error {
	p.on = on
	p.set = append(p.set, on)
	return nil
}

func plugSetup(t *testing.T, dryRun bool, bat config.DeviceBattery) (*Site, *fakePlug) {
	t.Helper()
	cfg := config.Default()
	cfg.Site.DryRun = dryRun
	cfg.Devices = []config.Device{{ID: "ebike", Type: "shelly", Mode: "owned", Kind: config.KindAppliance,
		Flexibility: config.FlexInterruptible, Battery: &bat}}
	plug := &fakePlug{on: true}
	s := &Site{cfg: cfg}
	s.chargers = newChargeables(cfg, map[string]sdk.Device{"ebike": plug}, sdk.NewMemCache())
	return s, plug
}

// charge runs the e-bike charger with w watts from minute a to b.
func charge(s *Site, a, b int, w float64) *ChargeView {
	var v *ChargeView
	for m := a; m <= b; m++ {
		v = s.chargers.update(context.Background(), map[string]float64{"ebike": w}, t0.Add(time.Duration(m)*time.Minute))["ebike"]
	}
	return v
}

func TestChargeableLimitSwitchesOff(t *testing.T) {
	// 500 Wh e-bike battery, typical 20 % when charging starts, efficiency 1 for round numbers
	s, plug := plugSetup(t, false, config.DeviceBattery{CapacityWh: 500, DefaultSoC: 0.2, Efficiency: 1, LimitSoC: 0.8})
	v := charge(s, 0, 30, 200) // 30 min at 200 W = 100 Wh = +20 %
	if v.SoC == nil || math.Abs(*v.SoC-0.4) > 0.01 || v.SoCSource != "default" || !v.Active || v.Off {
		t.Fatalf("charging: %+v", v)
	}
	// by hand: 70 %; 15 min later (+10 %) the limit of 80 % switches the plug off
	if err := s.SetLoadSoC("ebike", ptrF(0.7)); err != nil {
		t.Fatal(err)
	}
	v = charge(s, 31, 46, 200)
	if !v.Off || len(plug.set) != 1 || plug.set[0] || !strings.Contains(v.Reason, "80 %") || !v.Limit.Reached {
		t.Fatalf("at the limit: %+v %v", v, plug.set)
	}
	// off: no power, the charge waits (does not end after the pause)
	v = charge(s, 47, 70, 0)
	if !v.Active || !v.Off {
		t.Fatalf("waiting: %+v", v)
	}
	// +x % does not fit 5 % steps; a higher limit switches on again
	if err := s.SetLoadLimit("ebike", &charging.Limit{Add: 0.07}); err == nil {
		t.Fatal("+7 % accepted")
	}
	if err := s.SetLoadLimit("ebike", &charging.Limit{Add: 0.1}); err != nil {
		t.Fatal(err)
	}
	if !plug.on {
		t.Fatal("not switched on again")
	}
	v = charge(s, 71, 86, 200) // +10 % = 50 Wh = 15 min
	if !v.Off || len(plug.set) != 3 {
		t.Fatalf("+10 %%: %+v %v", v, plug.set)
	}
	// switched on for a new charge: unplugged after the pause, the estimate ends
	if err := s.SwitchLoadOn("ebike"); err != nil {
		t.Fatal(err)
	}
	v = charge(s, 87, 95, 0)
	if v.Active || v.SoC != nil || v.Limit == nil || v.Limit.Session {
		t.Fatalf("after the charge: %+v", v)
	}
}

func TestChargeableDryRunAndUnknownSoC(t *testing.T) {
	s, plug := plugSetup(t, true, config.DeviceBattery{CapacityWh: 500, Efficiency: 1})
	_ = s.SetLoadLimit("ebike", &charging.Limit{SoC: 0.8})
	v := charge(s, 0, 5, 200)
	if v.SoC != nil || v.Limit == nil || v.Limit.Note == "" {
		t.Fatalf("unknown soc: %+v", v)
	}
	_ = s.SetLoadLimit("ebike", &charging.Limit{Add: 0.2}) // 100 Wh from now
	v = charge(s, 6, 40, 200)
	if v.Off || len(plug.set) != 0 || !strings.Contains(v.Reason, "Probelauf") {
		t.Fatalf("dry run: %+v %v", v, plug.set)
	}
}
