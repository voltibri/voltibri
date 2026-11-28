// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package charging

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/tariff"
	"github.com/voltibri/voltibri/sdk"
)

// setupOwn: a wallbox and the vehicle asset "tavascan" (optionally bound
// to tibber/car1).
func setupOwn(t *testing.T, own fakeOwn, prov fakeProvider) (*Manager, *fakeCharger) {
	t.Helper()
	cfg := config.Default()
	veh := config.Device{ID: own.id, Type: "vehicle", Name: "Tavascan"}
	if prov != nil {
		veh.Params = map[string]any{"source": "tibber", "sourceVehicle": "car1"}
	}
	cfg.Devices = []config.Device{{ID: "wb", Type: "x", Mode: "owned"}, {ID: "tibber", Type: "y"}, veh}
	cfg.Tariffs.Grid = &tariff.Config{Type: "fixed", Price: 30}
	cfg.Charging.Loadpoints = []config.Loadpoint{{Charger: "wb"}}
	ch := &fakeCharger{}
	devs := map[string]sdk.Device{"wb": ch, own.id: own}
	if prov != nil {
		devs["tibber"] = prov
	}
	m, err := New(cfg, time.UTC, devs, sdk.NewMemCache(), &memSessions{})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return t0 }
	m.refreshVehicles(context.Background())
	return m, ch
}

func vehicle(m *Manager, key string) Vehicle {
	for _, v := range m.Vehicles() {
		if v.Key == key {
			return v
		}
	}
	return Vehicle{}
}

func socOfV(v Vehicle) float64 {
	if v.SoC == nil {
		return math.NaN()
	}
	return *v.SoC
}

func TestManualSoCInterpolatedUntilUnplugged(t *testing.T) {
	m, ch := setupOwn(t, fakeOwn{id: "tavascan", capWh: 50000, def: 0.3}, nil)
	if err := m.SetVehicleSettings("tavascan", VehicleSettings{Mode: ModeFast}); err != nil {
		t.Fatal(err)
	}
	// chosen and state of charge entered before plugging in
	if err := m.SetLoadpointVehicle("wb", "tavascan"); err != nil {
		t.Fatal(err)
	}
	v := 0.5
	if err := m.SetVehicleSoC("tavascan", &v); err != nil {
		t.Fatal(err)
	}
	if err := m.SetVehicleSoC("tavascan", ptr(1.2)); err == nil {
		t.Fatal("120 % accepted")
	}
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 11000}
	for i := 1; i <= 61; i++ {
		tick(m, time.Duration(i)*time.Minute, ch, 11000, 0, 0, 11500)
	}
	// 60 min at 11 kW = 11 kWh, 85 % into 50 kWh: +18.7 %
	got := vehicle(m, "tavascan")
	if s := socOfV(got); math.Abs(s-0.687) > 0.002 || got.SoCSource != "manual" || !got.SoCEstimated {
		t.Fatalf("interpolated: %v %+v", s, got)
	}
	lp := m.Loadpoints()[0]
	if lp.Finish == nil || lp.Finish.Basis != "full" || lp.Finish.NeedWh < 18000 {
		t.Fatalf("finish with the estimate: %+v", lp.Finish)
	}
	// +10 % for this session: 5.9 kWh more, about 32 min
	if err := m.SetLoadpointLimit("wb", &Limit{Add: 0.1}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetLoadpointLimit("wb", &Limit{Add: 0.07}); err == nil {
		t.Fatal("+7 % accepted")
	}
	for i := 62; i <= 92; i++ {
		tick(m, time.Duration(i)*time.Minute, ch, 11000, 0, 0, 11500)
	}
	if lp := m.Loadpoints()[0]; !lp.Target.Enabled || lp.Limit == nil || lp.Limit.Reached || lp.Finish.Basis != "own-limit" {
		t.Fatalf("before the limit: %+v %+v", lp.Limit, lp.Finish)
	}
	for i := 93; i <= 96; i++ {
		tick(m, time.Duration(i)*time.Minute, ch, 11000, 0, 0, 11500)
	}
	lp = m.Loadpoints()[0]
	if lp.Target.Enabled || !lp.Limit.Reached || !strings.Contains(lp.Reason, "+10 % geladen") {
		t.Fatalf("at the limit: %+v %q", lp.Limit, lp.Reason)
	}
	if s := socOfV(vehicle(m, "tavascan")); math.Abs(s-0.79) > 0.01 {
		t.Fatalf("soc at the limit: %v", s)
	}
	// unplugged: estimate and session limit end
	ch.st = sdk.ChargerStatus{}
	tick(m, 100*time.Minute, ch, 0, 0, 0, 500)
	if got := vehicle(m, "tavascan"); got.SoC != nil {
		t.Fatalf("after unplug: %+v", got)
	}
	// plugged in again: the typical state of charge
	_ = m.SetLoadpointVehicle("wb", "tavascan")
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 110*time.Minute, ch, 0, 0, 0, 500)
	if got := vehicle(m, "tavascan"); socOfV(got) != 0.3 || got.SoCSource != "default" {
		t.Fatalf("default: %+v", got)
	}
	if lp := m.Loadpoints()[0]; lp.Limit != nil || !lp.Target.Enabled {
		t.Fatalf("limit of the last session: %+v", lp.Limit)
	}
}

func ptr(f float64) *float64 { return &f }

func TestSoCFromDataSourceAndByHand(t *testing.T) {
	prov := fakeProvider{"car1": {SoC: 0.4, LimitSoC: math.NaN(), Connected: true, Updated: t0}}
	m, ch := setupOwn(t, fakeOwn{id: "tavascan", capWh: 77000}, prov)
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, time.Minute, ch, 0, 0, 0, 500)
	if got := vehicle(m, "tavascan"); socOfV(got) != 0.4 || got.SoCSource != "api" {
		t.Fatalf("api: %+v", got)
	}
	// by hand: a stale value of the cloud (still 40 %) does not undo it
	m.now = func() time.Time { return t0.Add(2 * time.Minute) }
	if err := m.SetVehicleSoC("tavascan", ptr(0.6)); err != nil {
		t.Fatal(err)
	}
	prov["car1"] = sdk.VehicleStatus{SoC: 0.4, LimitSoC: math.NaN(), Connected: true, Updated: t0.Add(3 * time.Minute)}
	m.refreshVehicles(context.Background())
	tick(m, 4*time.Minute, ch, 0, 0, 0, 500)
	if got := vehicle(m, "tavascan"); socOfV(got) != 0.6 || got.SoCSource != "manual" {
		t.Fatalf("manual after a stale value: %+v", got)
	}
	// a new value of the cloud replaces it
	prov["car1"] = sdk.VehicleStatus{SoC: 0.62, LimitSoC: math.NaN(), Connected: true, Updated: t0.Add(5 * time.Minute)}
	m.refreshVehicles(context.Background())
	tick(m, 6*time.Minute, ch, 0, 0, 0, 500)
	if got := vehicle(m, "tavascan"); socOfV(got) != 0.62 || got.SoCSource != "api" {
		t.Fatalf("new api value: %+v", got)
	}
	// the vehicle's limit stops at 60 %
	if err := m.SetVehicleSettings("tavascan", VehicleSettings{Mode: ModeFast, Limit: &Limit{SoC: 0.6}}); err != nil {
		t.Fatal(err)
	}
	tick(m, 7*time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Target.Enabled || lp.Limit == nil || lp.Limit.Session || !strings.Contains(lp.Reason, "Ladelimit 60 %") {
		t.Fatalf("vehicle limit: %+v %q", lp.Limit, lp.Reason)
	}
	// a higher limit for this session starts again
	if err := m.SetLoadpointLimit("wb", &Limit{SoC: 0.8}); err != nil {
		t.Fatal(err)
	}
	tick(m, 8*time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; !lp.Target.Enabled || !lp.Limit.Session {
		t.Fatalf("session limit: %+v", lp.Limit)
	}
}

func TestPlanAddWithoutSoC(t *testing.T) {
	m, ch := setupOwn(t, fakeOwn{id: "tavascan", capWh: 50000}, nil)
	plans := []Plan{{SoC: 0.8, At: t0.Add(5 * time.Hour)}}
	if err := m.SetVehicleSettings("tavascan", VehicleSettings{Plans: plans}); err != nil {
		t.Fatal(err)
	}
	_ = m.SetLoadpointVehicle("wb", "tavascan")
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Plan == nil || !strings.Contains(lp.Plan.Reason, "unbekannt") {
		t.Fatalf("soc plan without soc: %+v", lp.Plan)
	}
	plans = []Plan{{Add: 0.2, At: t0.Add(5 * time.Hour)}}
	if err := m.SetVehicleSettings("tavascan", VehicleSettings{Plans: plans}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetVehicleSettings("tavascan", VehicleSettings{Plans: []Plan{{Add: 0.2, SoC: 0.5, At: t0.Add(time.Hour)}}}); err == nil {
		t.Fatal("plan with both accepted")
	}
	tick(m, 2*time.Minute, ch, 0, 0, 0, 500)
	lp := m.Loadpoints()[0]
	if lp.Plan == nil || math.Abs(lp.Plan.NeedWh-0.2*50000/Efficiency) > 1 || len(lp.Plan.Slots) == 0 || lp.Plan.Add != 0.2 {
		t.Fatalf("+20 %% plan: %+v", lp.Plan)
	}
	if lp.Finish == nil || lp.Finish.Basis != "plan" || lp.Finish.Add != 0.2 {
		t.Fatalf("finish: %+v", lp.Finish)
	}
}
