// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package charging

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/circuit"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/tariff"
	"github.com/voltibri/voltibri/sdk"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func in(mode Mode, surplus float64, prev State, at time.Duration) Input {
	return Input{Now: t0.Add(at), Mode: mode, Connected: true, SurplusW: surplus, MinA: 6, MaxA: 16, Phases: 3, Prev: prev}
}

func TestDecidePV(t *testing.T) {
	// 3 kW are not enough for 6 A on 3 phases (4.1 kW)
	d := Decide(in(ModePV, 3000, State{}, 0))
	if d.Enabled {
		t.Fatal("started without enough surplus")
	}
	// with phase switching 3 kW are enough on one phase, after the delay
	i := in(ModePV, 3000, State{}, 0)
	i.CanSwitch = true
	d = Decide(i)
	if d.Enabled || d.SurplusSince.IsZero() {
		t.Fatalf("must wait for the enable delay: %+v", d)
	}
	i = in(ModePV, 3000, d.State, EnableDelay)
	i.CanSwitch = true
	d = Decide(i)
	if !d.Enabled || d.Phases != 1 || d.CurrentA != 13 {
		t.Fatalf("want 1p 13 A, got %+v", d)
	}
	// surplus drops: keeps minimum current during the disable delay, then stops
	on := State{Enabled: true, CurrentA: 13, Phases: 1}
	d = Decide(in(ModePV, 500, on, 0))
	if !d.Enabled || d.CurrentA != 6 {
		t.Fatalf("must hold minimum during disable delay: %+v", d)
	}
	d = Decide(in(ModePV, 500, d.State, DisableDelay))
	if d.Enabled {
		t.Fatalf("must stop after disable delay: %+v", d)
	}
}

func TestDecideOtherModes(t *testing.T) {
	if d := Decide(in(ModeMinPV, 0, State{}, 0)); !d.Enabled || d.CurrentA != 6 {
		t.Fatalf("minpv without surplus: %+v", d)
	}
	if d := Decide(in(ModeMinPV, 9200, State{}, 0)); d.CurrentA != 13 {
		t.Fatalf("minpv with 9.2 kW surplus: %+v", d)
	}
	i := in(ModeFast, 0, State{}, 0)
	i.MaxPowerW = 7400 // vehicle limit
	if d := Decide(i); !d.Enabled || math.Abs(d.CurrentA-10.72) > 0.01 {
		t.Fatalf("fast with vehicle limit: %+v", d)
	}
	prev := State{Enabled: true, CurrentA: 8, Phases: 3}
	if d := Decide(in(ModeExternal, 0, prev, 0)); d.State != prev {
		t.Fatalf("external must not change anything: %+v", d)
	}
	i = in(ModePV, 0, State{}, 0)
	i.PlanSlot = true
	if d := Decide(i); !d.Enabled || d.CurrentA != 16 {
		t.Fatalf("plan slot must charge fast: %+v", d)
	}
	i = in(ModeFast, 0, State{}, 0)
	i.Connected = false
	if d := Decide(i); d.Enabled {
		t.Fatal("charging without vehicle")
	}
}

func TestSurplus(t *testing.T) {
	// exporting 1 kW, battery charging 3 kW, EV draws 2 kW
	if s := Surplus(-1000, -3000, 2000, false); s != 6000 {
		t.Fatalf("without battery priority: %v", s)
	}
	if s := Surplus(-1000, -3000, 2000, true); s != 3000 {
		t.Fatalf("with battery priority: %v", s)
	}
	// battery discharging into the car is no surplus
	if s := Surplus(0, 2000, 2000, true); s != 0 {
		t.Fatalf("discharge: %v", s)
	}
}

func TestSchedule(t *testing.T) {
	now := t0.Add(7 * time.Minute) // 12:07
	target := t0.Add(4 * time.Hour)
	// 11 kW, 5.5 kWh -> 2 slots of 15 min
	s := MakeSchedule(now, target, 5500, 11000, nil)
	if len(s.Slots) != 2 || !s.Slots[0].Equal(t0.Add(210*time.Minute)) || s.Cheapest {
		t.Fatalf("late start: %+v", s)
	}
	prices := make([]float64, 16)
	for i := range prices {
		prices[i] = 30
	}
	prices[3], prices[9] = 10, 12
	s = MakeSchedule(now, target, 5500, 11000, prices)
	if !s.Cheapest || len(s.Slots) != 2 || !s.Slots[0].Equal(t0.Add(45*time.Minute)) || !s.Slots[1].Equal(t0.Add(135*time.Minute)) {
		t.Fatalf("cheapest: %+v", s)
	}
	if !s.Active(t0.Add(50*time.Minute)) || s.Active(t0.Add(60*time.Minute)) {
		t.Fatal("Active wrong")
	}
	s = MakeSchedule(now, t0.Add(30*time.Minute), 50000, 11000, nil)
	if !s.Late || len(s.Slots) != 2 {
		t.Fatalf("not enough time: %+v", s)
	}
	if n := NeedWh(0.5, 1, 60000); math.Abs(n-35294) > 1 {
		t.Fatalf("need: %v", n)
	}
}

func TestPlanNext(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 11, 20, 21, 0, 0, 0, loc) // Friday 21:00
	p := Plan{SoC: 1, Days: []int{1, 2, 3, 4, 5}, Time: "08:00"}
	if n := p.Next(now, loc); n.Weekday() != time.Monday || n.Hour() != 8 {
		t.Fatalf("next weekday plan: %v", n)
	}
	p = Plan{SoC: 1, Days: []int{6}, Time: "08:00"}
	if n := p.Next(now, loc); n.Day() != 21 {
		t.Fatalf("tomorrow: %v", n)
	}
	if (Plan{SoC: 1, At: now.Add(-time.Hour)}).Next(now, loc) != (time.Time{}) {
		t.Fatal("expired one-off plan")
	}
	if (Plan{SoC: 1.2, At: now}).Validate() == nil || (Plan{SoC: 1, Days: []int{1}, Time: "25:00"}).Validate() == nil {
		t.Fatal("invalid plan accepted")
	}
}

// --- manager ---

type fakeCharger struct {
	st      sdk.ChargerStatus
	enabled []bool
	current []float64
}

func (c *fakeCharger) ID() string { return "wb" }
func (c *fakeCharger) ChargerStatus(context.Context) (sdk.ChargerStatus, error) {
	return c.st, nil
}
func (c *fakeCharger) EnableCharging(_ context.Context, on bool) error {
	c.enabled = append(c.enabled, on)
	return nil
}
func (c *fakeCharger) SetCurrent(_ context.Context, a float64) error {
	c.current = append(c.current, a)
	return nil
}

type fakeProvider map[string]sdk.VehicleStatus

func (p fakeProvider) ID() string { return "tibber" }
func (p fakeProvider) Vehicles(context.Context) ([]sdk.VehicleInfo, error) {
	var out []sdk.VehicleInfo
	for id := range p {
		out = append(out, sdk.VehicleInfo{ID: id, Name: "Auto " + id})
	}
	return out, nil
}
func (p fakeProvider) VehicleStatus(_ context.Context, id string) (sdk.VehicleStatus, error) {
	return p[id], nil
}

type memSessions []Session

func (m *memSessions) SaveSession(_ context.Context, s Session) (int64, error) {
	*m = append(*m, s)
	return int64(len(*m)), nil
}
func (m *memSessions) Sessions(context.Context, int) ([]Session, error) { return *m, nil }

func setup(t *testing.T, lp config.Loadpoint, prov fakeProvider) (*Manager, *fakeCharger, *memSessions) {
	t.Helper()
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: "wb", Type: "x", Mode: "owned", Name: "Carport"}, {ID: "tibber", Type: "y"}}
	cfg.Tariffs.Grid = &tariff.Config{Type: "fixed", Price: 30}
	cfg.Tariffs.FeedIn = &tariff.Config{Type: "fixed", Price: 8}
	lp.Charger = "wb"
	cfg.Charging.Loadpoints = []config.Loadpoint{lp}
	ch := &fakeCharger{}
	devs := map[string]sdk.Device{"wb": ch, "tibber": prov}
	ss := &memSessions{}
	m, err := New(cfg, time.UTC, devs, nil, ss)
	if err != nil {
		t.Fatal(err)
	}
	m.refreshVehicles(context.Background())
	return m, ch, ss
}

func tick(m *Manager, at time.Duration, ch *fakeCharger, gridW, batW, pvW, loadW float64) Result {
	return m.Update(context.Background(), SiteInput{Now: t0.Add(at), GridW: gridW, BatteryW: batW, PVW: pvW, LoadW: loadW,
		Chargers: map[string]sdk.ChargerStatus{"wb": ch.st}})
}

func TestManagerDetectsVehicleAndCharges(t *testing.T) {
	m, ch, ss := setup(t, config.Loadpoint{}, fakeProvider{"car1": {SoC: 0.4, Connected: true}})
	if err := m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModeFast, CapacityWh: 60000}); err != nil {
		t.Fatal(err)
	}
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	lp := m.Loadpoints()[0]
	if lp.Vehicle != "tibber/car1" || lp.Mode != ModeFast || !lp.Target.Enabled {
		t.Fatalf("loadpoint: %+v", lp)
	}
	if len(ch.enabled) != 1 || !ch.enabled[0] || ch.current[0] != 16 {
		t.Fatalf("charger writes: %+v %+v", ch.enabled, ch.current)
	}
	// 11 kW for one hour, 6 kW from the grid
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 11000}
	for i := 1; i <= 360; i++ {
		tick(m, time.Duration(i)*10*time.Second, ch, 6000, 0, 5500, 11500)
	}
	ch.st = sdk.ChargerStatus{}
	tick(m, time.Hour+10*time.Second, ch, 0, 0, 0, 500)
	if len(*ss) != 1 {
		t.Fatalf("sessions: %+v", *ss)
	}
	s := (*ss)[0]
	if math.Abs(s.EnergyWh-11000) > 1 || math.Abs(s.SolarWh-5000) > 1 || math.Abs(s.CostCt-(6*30+5*8)) > 0.5 {
		t.Fatalf("session: %+v", s)
	}
}

func TestManagerExternalNeverWrites(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{}, fakeProvider{"car1": {Connected: true}})
	m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModeExternal})
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 7000}
	for i := 0; i < 30; i++ {
		tick(m, time.Duration(i)*10*time.Second, ch, 7000, 0, 0, 7500)
	}
	if len(ch.enabled)+len(ch.current) != 0 {
		t.Fatalf("external mode wrote to the charger: %v %v", ch.enabled, ch.current)
	}
	if s := m.Loadpoints()[0].Session; s == nil || s.EnergyWh <= 0 {
		t.Fatal("external mode must still log energy")
	}
}

func TestGuestPolicies(t *testing.T) {
	// no vehicle cloud reports a plugged-in car -> guest after the timeout
	m, ch, _ := setup(t, config.Loadpoint{Guest: config.GuestBatteryFirst}, fakeProvider{"car1": {Connected: false}})
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 0)
	if lp := m.Loadpoints()[0]; !lp.Detecting || lp.Vehicle != Guest {
		t.Fatalf("must be detecting: %+v", lp)
	}
	// battery charging 4 kW from PV, nothing exported: battery first -> guest gets only the minimum
	tick(m, DetectTimeout+time.Second, ch, 0, -4000, 5000, 1000)
	lp := m.Loadpoints()[0]
	if lp.Detecting || lp.Mode != ModeFast || lp.Target.CurrentA != 6 {
		t.Fatalf("battery first: %+v", lp)
	}

	m, ch, _ = setup(t, config.Loadpoint{Guest: config.GuestForceBattery}, fakeProvider{})
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 11000}
	r := tick(m, 0, ch, 7000, 0, 5000, 12000) // house 1 kW, PV 5 kW, guest 11 kW
	if r.GuestW != 11000 || r.ForceBatteryW != 4000 {
		t.Fatalf("force battery: %+v", r)
	}
}

func TestPlanInManager(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{}, fakeProvider{"car1": {SoC: 0.5, Connected: true}})
	m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModePV, CapacityWh: 60000,
		Plans: []Plan{{SoC: 1, At: t0.Add(10 * time.Hour)}}})
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	lp := m.Loadpoints()[0]
	// 30 kWh / 0.85 = 35.3 kWh from the grid at 11 kW -> 13 slots; same price everywhere -> the latest ones
	if lp.Plan == nil || len(lp.Plan.Slots) != 13 || !lp.Plan.Slots[12].Equal(t0.Add(9*time.Hour+45*time.Minute)) || lp.Target.Enabled {
		t.Fatalf("plan: %+v", lp.Plan)
	}
	tick(m, lp.Plan.Start.Sub(t0)+time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; !lp.Target.Enabled || lp.Target.CurrentA != 16 {
		t.Fatalf("plan slot must charge: %+v", lp)
	}
}

func TestLoadManagement(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{}, fakeProvider{"car1": {SoC: 0.4, Connected: true}})
	m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModeFast})
	ch.st = sdk.ChargerStatus{Connected: true}
	// the other wallbox already draws 20 A of the shared 32 A
	tree := func(other float64) *circuit.Tree {
		return circuit.Compute([]circuit.Node{{ID: "wbs", Name: "Wallboxen", MaxA: 32, Devices: []string{"wb", "zaun"}}},
			map[string]circuit.Reading{"zaun": {Currents: circuit.Phases{other, other, other}, HasCurrents: true}, "wb": {HasCurrents: true}}, nil)
	}
	run := func(at time.Duration, other float64) Loadpoint {
		m.Update(context.Background(), SiteInput{Now: t0.Add(at), LoadW: 500, Chargers: map[string]sdk.ChargerStatus{"wb": ch.st}, Circuits: tree(other)})
		return m.Loadpoints()[0]
	}
	lp := run(0, 20)
	if !lp.Target.Enabled || lp.Target.CurrentA != 12 || lp.LimitBy != "Wallboxen" || *lp.LimitA != 12 {
		t.Fatalf("limited: %+v", lp)
	}
	if ch.current[len(ch.current)-1] != 12 {
		t.Fatalf("charger current %v", ch.current)
	}
	// only 2 A left: pause instead of exceeding the fuse with the 6 A minimum
	lp = run(time.Minute, 30)
	if lp.Target.Enabled || !strings.Contains(lp.Reason, "ausgelastet") {
		t.Fatalf("paused: %+v", lp)
	}
	// headroom again: full current
	lp = run(2*time.Minute, 0)
	if lp.Target.CurrentA != 16 || lp.LimitA != nil {
		t.Fatalf("free: %+v", lp)
	}
}
