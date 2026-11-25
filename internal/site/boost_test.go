// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/sdk"
)

type socBat struct{}

func (socBat) SoC(context.Context) (float64, error)          { return 0.5, nil }
func (socBat) BatteryPower(context.Context) (float64, error) { return 0, nil }
func (socBat) CapacityWh() float64                           { return 10000 }

func boostSite(t *testing.T) *Site {
	t.Helper()
	cfg := config.Default()
	cfg.Battery.MaxDischargeW = 5000
	cfg.Devices = []config.Device{{ID: "stab", Mode: "owned", Flexibility: config.FlexSurplus}}
	s := &Site{cfg: cfg, bat: socBat{}, batCache: sdk.NewMemCache(),
		batSet: BatterySettings{BoostReserve: boostReserveDef, BoostStopSoC: boostStopDefault}}
	s.flex = newFlex(cfg, map[string]sdk.Device{"stab": &fakeStager{steps: 3, stepW: 1000}})
	return s
}

func TestBoostPossible(t *testing.T) {
	s := boostSite(t)
	if !s.batteryFeatures().Boost {
		t.Fatal("boost not possible with battery and heating rod")
	}
	s.flex = nil
	if s.batteryFeatures().Boost {
		t.Fatal("boost possible without surplus loads")
	}
	s = boostSite(t)
	s.bat = nil
	if w, _ := s.updateBoost(0.9, time.Now()); w != 0 {
		t.Fatal("boost power without battery")
	}
}

func TestBoostSettings(t *testing.T) {
	s := boostSite(t)
	for _, bad := range []BatterySettings{
		{BoostReserve: 0.6, BoostStopSoC: 0.5},
		{BoostStopSoC: 0.97},
		{BoostStopSoC: 0.5, BoostStartSoC: 0.52},
		{MinSoC: 0.5, BoostStopSoC: 0.52},
	} {
		if err := s.SetBatterySettings(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := s.SetBatterySettings(BatterySettings{BoostReserve: 0.3, BoostStopSoC: 0.85, BoostStartSoC: 0.95}); err != nil {
		t.Fatal(err)
	}
	// 5 kW × 70 %; the max. discharge setting narrows it
	if w := s.boostPowerW(s.batSet); w != 3500 {
		t.Fatalf("power %v", w)
	}
	if w := s.boostPowerW(BatterySettings{MaxDischargeW: 2000, BoostReserve: 0.25}); w != 1500 {
		t.Fatalf("narrowed power %v", w)
	}
}

func TestBoostLimit(t *testing.T) {
	s := boostSite(t)
	now := time.Now()
	// above the stop SoC the battery gives boost power (for loads boosted by hand)
	if w, auto := s.updateBoost(0.9, now); w != 4000 || auto {
		t.Fatalf("above stop: %v %v", w, auto)
	}
	// at the stop SoC nothing, until 2 % above it
	if w, _ := s.updateBoost(0.85, now); w != 0 {
		t.Fatal("boost power at the stop SoC")
	}
	if w, _ := s.updateBoost(0.86, now); w != 0 {
		t.Fatal("boost power within the hysteresis")
	}
	if w, _ := s.updateBoost(0.88, now); w != 4000 {
		t.Fatal("no boost power above the hysteresis")
	}
}

func TestBoostAuto(t *testing.T) {
	s := boostSite(t)
	s.batSet.BoostStartSoC = 0.95
	now := time.Now()
	auto := func(soc float64) bool { _, a := s.updateBoost(soc, now); return a }
	// saw-tooth: starts at 95 %, ends at 85 %, starts again at 95 %
	if auto(0.94) {
		t.Fatal("started below the start SoC")
	}
	if !auto(0.95) || s.boost == nil || !s.boost.Auto {
		t.Fatalf("auto start: %+v", s.boost)
	}
	// survives a configuration change
	s2 := boostSite(t)
	s2.batCache = s.batCache
	s2.loadBattery()
	if s2.boost == nil {
		t.Fatal("not restored")
	}
	if !auto(0.9) {
		t.Fatal("ended above the stop SoC")
	}
	if auto(0.85) || s.boost != nil {
		t.Fatal("not ended at the stop SoC")
	}
	if auto(0.9) {
		t.Fatal("restarted below the start SoC")
	}
	if !auto(0.96) {
		t.Fatal("no second start")
	}
	// stopped by hand: no restart until the SoC fell below the start SoC
	s.StopBoost()
	if auto(0.97) {
		t.Fatal("restarted after a manual stop")
	}
	auto(0.93)
	if !auto(0.95) {
		t.Fatal("no start after the SoC fell")
	}
	// a force charging ends it
	bat := &forceBat{}
	s.batCtl, s.forceCh = bat, bat
	s.state.SoC = 0.95
	if err := s.StartForceCharge(0.99); err != nil || s.boost != nil {
		t.Fatalf("force: %v boost %+v", err, s.boost)
	}
	s.StopForceCharge()
	auto(0.93)
	auto(0.95)
	// expired: ends
	s.boost.Since = now.Add(-boostMax)
	if auto(0.95) || s.boost != nil {
		t.Fatal("not expired")
	}
}

func TestBatteryPrioritySoC(t *testing.T) {
	s := boostSite(t)
	st := State{HasBattery: true, SoC: 0.6}
	// default from the configuration (0.5)
	if s.batteryFirst(st) {
		t.Fatal("battery first above the default")
	}
	s.cfg.Charging.BatteryPrioritySoC = 0.7
	if !s.batteryFirst(st) {
		t.Fatal("configured value ignored")
	}
	// the runtime setting wins; settings without it keep it
	p := 0.0
	if err := s.SetBatterySettings(BatterySettings{PrioritySoC: &p, BoostStopSoC: 0.85}); err != nil {
		t.Fatal(err)
	}
	if s.batteryFirst(st) {
		t.Fatal("0 = never")
	}
	if err := s.SetBatterySettings(BatterySettings{BoostStopSoC: 0.85}); err != nil || s.batSet.PrioritySoC == nil || *s.batSet.PrioritySoC != 0 {
		t.Fatalf("not kept: %v %+v", err, s.batSet.PrioritySoC)
	}
	if got := s.BatteryInfo().Settings.PrioritySoC; got == nil || *got != 0 {
		t.Fatalf("info %+v", got)
	}
	bad := 1.2
	if err := s.SetBatterySettings(BatterySettings{PrioritySoC: &bad, BoostStopSoC: 0.85}); err == nil {
		t.Fatal("120 % accepted")
	}
}

func TestChargeTargetsAutoAndManual(t *testing.T) {
	s := boostSite(t)
	bat := &fakeBat{}
	s.batCtl = bat // charge and discharge limits
	s.auto = planner.Targets{MaxSoC: 0.8, MinSoC: 0.05, Reason: "x"}
	// default: automatic
	if bs := s.effective(s.batSet, s.auto); s.batSet.limitMode() != LimitsAuto || bs.MaxSoC != 0.8 || bs.MinSoC != 0.05 {
		t.Fatalf("auto: %+v", bs)
	}
	// pizza evening: 100 % by hand until the next morning
	now := time.Now()
	if err := s.SetBatterySettings(BatterySettings{LimitMode: LimitsManual, MinSoC: 0.1, BoostStopSoC: 0.85}); err != nil {
		t.Fatal(err)
	}
	if bs := s.effective(s.batSet, s.auto); bs.MaxSoC != 0 || bs.MinSoC != 0.1 || s.batSet.ManualUntil == nil {
		t.Fatalf("manual: %+v", s.batSet)
	}
	until := *s.batSet.ManualUntil
	if !until.After(now.Add(time.Hour)) || until.After(now.Add(36*time.Hour)) {
		t.Fatalf("until %v", until)
	}
	// another change by hand keeps the end
	s.SetBatterySettings(BatterySettings{LimitMode: LimitsManual, MinSoC: 0.15, BoostStopSoC: 0.85})
	if !s.batSet.ManualUntil.Equal(until) {
		t.Fatal("end moved")
	}
	// the morning: automatic again
	s.expireManual(until.Add(time.Minute))
	if s.batSet.limitMode() != LimitsAuto || s.batSet.MinSoC != 0.15 {
		t.Fatalf("not expired: %+v", s.batSet)
	}
	// always: no end
	s.SetBatterySettings(BatterySettings{LimitMode: LimitsManual, ManualFor: ManualAlways, MaxSoC: 0.9, BoostStopSoC: 0.85})
	if s.batSet.ManualUntil != nil {
		t.Fatal("end for always")
	}
	s.expireManual(now.Add(100 * time.Hour))
	if s.batSet.limitMode() != LimitsManual {
		t.Fatal("always expired")
	}
	// settings from before the automatic stay by hand
	if (BatterySettings{MaxSoC: 0.9}).limitMode() != LimitsManual {
		t.Fatal("old settings automatic")
	}
	for _, bad := range []BatterySettings{{AutoFloorSunny: 0.3, AutoFloorCloudy: 0.2}, {AutoMaxSoC: 0.4}, {LimitMode: "x"}} {
		bad.BoostStopSoC = 0.85
		if err := s.SetBatterySettings(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
