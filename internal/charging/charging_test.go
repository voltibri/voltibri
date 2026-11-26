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
	"github.com/voltibri/voltibri/internal/pricing"
	"github.com/voltibri/voltibri/internal/run"
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

type memSessions []run.Run

func (m *memSessions) SaveRun(_ context.Context, s run.Run) (int64, error) {
	*m = append(*m, s)
	return int64(len(*m)), nil
}
func (m *memSessions) Runs(context.Context, run.Filter) ([]run.Run, error) { return *m, nil }

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
	if math.Abs(s.EnergyWh-11000) > 1 || math.Abs(s.PVWh-5000) > 1 || math.Abs(s.GridWh-6000) > 1 || s.Asset != "wb" || s.Kind != "charging" || math.Abs(s.CostCt-(6*30+5*8)) > 0.5 {
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
	m, ch, _ := setup(t, config.Loadpoint{Guest: config.GuestBatteryFirst, GuestMode: "fast"}, fakeProvider{"car1": {Connected: false}})
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

	m, ch, _ = setup(t, config.Loadpoint{Guest: config.GuestForceBattery, GuestMode: "fast"}, fakeProvider{})
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 11000}
	r := tick(m, 0, ch, 7000, 0, 5000, 12000) // house 1 kW, PV 5 kW, guest 11 kW
	if r.GuestW != 11000 || r.ForceBatteryW != 4000 {
		t.Fatalf("force battery: %+v", r)
	}
}

func TestPlanInManager(t *testing.T) {
	m, ch := setupVehicle(t, config.Device{}, 60000, fakeProvider{"car1": {SoC: 0.5, Connected: true}})
	m.SetVehicleSettings("car", VehicleSettings{Mode: ModePV,
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

func TestEmptySessionNotStored(t *testing.T) {
	m, ch, ss := setup(t, config.Loadpoint{}, fakeProvider{})
	ch.st = sdk.ChargerStatus{Connected: true}
	for i := 0; i < 30; i++ { // plugged in for 5 minutes, nothing charged
		tick(m, time.Duration(i)*10*time.Second, ch, 0, 0, 0, 500)
	}
	ch.st = sdk.ChargerStatus{}
	tick(m, 5*time.Minute, ch, 0, 0, 0, 500)
	if len(*ss) != 0 {
		t.Fatalf("empty session stored: %+v", *ss)
	}
}

func TestBatteryExclusion(t *testing.T) {
	m, ch := setupVehicle(t, config.Device{NoBatteryModes: []string{"fast"}}, 0, fakeProvider{"car1": {SoC: 0.4, Connected: true}})
	m.SetVehicleSettings("car", VehicleSettings{Mode: ModeFast})
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 11000}
	res := tick(m, 0, ch, 0, 0, 0, 11500)
	if res.ExcludedW != 11000 || !m.Loadpoints()[0].BatteryExcluded {
		t.Fatalf("fast should be excluded: %+v", res)
	}
	m.SetVehicleSettings("car", VehicleSettings{Mode: ModePV})
	if res := tick(m, time.Minute, ch, 0, 0, 0, 11500); res.ExcludedW != 0 || m.Loadpoints()[0].BatteryExcluded {
		t.Fatalf("pv should use the battery: %+v", res)
	}
}

func TestDecideOffAndPV1(t *testing.T) {
	d := Decide(Input{Now: t0, Mode: ModeOff, Connected: true, SurplusW: 9000, MinA: 6, MaxA: 16, Phases: 3, PlanSlot: true})
	if d.Enabled || !strings.Contains(d.Reason, "aus") {
		t.Fatalf("off: %+v", d)
	}
	// 2 kW surplus: one phase with switching, 8 A
	prev := State{Enabled: true, Phases: 1}
	d = Decide(Input{Now: t0, Mode: ModePV1, Connected: true, SurplusW: 2000, MinA: 6, MaxA: 16, Phases: 3, CanSwitch: true, Prev: prev})
	if !d.Enabled || d.Phases != 1 || d.CurrentA != 8 {
		t.Fatalf("pv1: %+v", d)
	}
	// plenty of surplus: stays on one phase (16 A max)
	d = Decide(Input{Now: t0, Mode: ModePV1, Connected: true, SurplusW: 9000, MinA: 6, MaxA: 16, Phases: 3, CanSwitch: true, Prev: prev})
	if d.Phases != 1 || d.CurrentA != 16 {
		t.Fatalf("pv1 high: %+v", d)
	}
}

func TestLoadpointOff(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{}, fakeProvider{"car1": {SoC: 0.4, Connected: true}})
	m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModeFast})
	if err := m.SetOff("wb", true); err != nil {
		t.Fatal(err)
	}
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	lp := m.Loadpoints()[0]
	if !lp.Off || lp.Mode != ModeOff || lp.Target.Enabled {
		t.Fatalf("off: %+v", lp)
	}
	m.SetOff("wb", false)
	tick(m, time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Off || lp.Mode != ModeFast || !lp.Target.Enabled {
		t.Fatalf("on again: %+v", lp)
	}
}

func TestGuestDefaultOffAndCards(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{}, fakeProvider{"car1": {Connected: false}})
	m.cfg.Charging.Cards = []config.Card{{Card: "RFID-10-Dräger", Vehicle: "tibber/car1", Mode: "fast"}, {Card: "Gast PV", Vehicle: Guest, Mode: "pv"}}
	// a guest plugs in: default off
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Mode != ModeOff || lp.Target.Enabled {
		t.Fatalf("guest default: %+v", lp)
	}
	// the card starts the e-Up fast for this session
	ch.st = sdk.ChargerStatus{Connected: true, Card: "RFID-10-Dräger"}
	tick(m, time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Vehicle != "tibber/car1" || lp.Mode != ModeFast || !lp.SessionMode || !lp.Target.Enabled || lp.Card != "RFID-10-Dräger" {
		t.Fatalf("card: %+v", lp)
	}
	// unplug: session settings are gone, next guest is off again
	ch.st = sdk.ChargerStatus{}
	tick(m, 2*time.Minute, ch, 0, 0, 0, 500)
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 3*time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Vehicle != Guest || lp.Mode != ModeOff || lp.SessionMode {
		t.Fatalf("after unplug: %+v", lp)
	}
	// guest card: guest with PV surplus
	ch.st = sdk.ChargerStatus{Connected: true, Card: "gast pv"}
	tick(m, 4*time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Vehicle != Guest || lp.Mode != ModePV {
		t.Fatalf("guest card: %+v", lp)
	}
	// button during the session overrides it, also only for the session
	m.SetSessionMode("wb", ModeFast)
	tick(m, 5*time.Minute, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Mode != ModeFast {
		t.Fatalf("button: %+v", lp)
	}
}

// fakeOwn is a vehicle asset (driver "vehicle").
type fakeOwn struct {
	id    string
	capWh float64
}

func (f fakeOwn) ID() string { return f.id }
func (f fakeOwn) Vehicles(context.Context) ([]sdk.VehicleInfo, error) {
	capWh := f.capWh
	if capWh == 0 {
		capWh = 77000
	}
	return []sdk.VehicleInfo{{ID: f.id, Name: "Tavascan", CapacityWh: capWh}}, nil
}
func (f fakeOwn) VehicleStatus(context.Context, string) (sdk.VehicleStatus, error) {
	return sdk.VehicleStatus{SoC: math.NaN(), LimitSoC: math.NaN(), RangeKm: math.NaN()}, nil
}

func TestVehicleAssetWithDataSource(t *testing.T) {
	cache := sdk.NewMemCache()
	// settings saved while the car was only known as tibber/car1
	_ = cache.Put("vehicle:tibber/car1", []byte(`{"mode":"fast"}`))
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: "wb", Type: "x", Mode: "owned"}, {ID: "tibber", Type: "y"},
		{ID: "tavascan", Type: "vehicle", Name: "Tavascan", Params: map[string]any{"source": "tibber", "sourceVehicle": "car1"}}}
	cfg.Tariffs.Grid = &tariff.Config{Type: "fixed", Price: 30}
	cfg.Charging.Loadpoints = []config.Loadpoint{{Charger: "wb"}}
	cfg.Charging.Cards = []config.Card{{Card: "EWE", Vehicle: "tibber/car1"}}
	ch := &fakeCharger{}
	prov := fakeProvider{"car1": {SoC: 0.33, Connected: true}, "car2": {SoC: 0.5}}
	m, err := New(cfg, time.UTC, map[string]sdk.Device{"wb": ch, "tibber": prov, "tavascan": fakeOwn{id: "tavascan"}}, cache, &memSessions{})
	if err != nil {
		t.Fatal(err)
	}
	m.refreshVehicles(context.Background())
	keys := map[string]Vehicle{}
	for _, v := range m.Vehicles() {
		keys[v.Key] = v
	}
	v, ok := keys["tavascan"]
	if !ok || len(keys) != 2 || keys["tibber/car2"].Key == "" {
		t.Fatalf("vehicles: %+v", keys)
	}
	if !v.Asset || v.Provider != "tibber" || v.SourceID != "car1" || v.SoC == nil || *v.SoC != 0.33 || v.Settings.Mode != ModeFast || v.Settings.CapacityWh != 77000 {
		t.Fatalf("asset: %+v", v)
	}
	if c, _ := m.cardFor("ewe"); c.Vehicle != "tavascan" {
		t.Fatalf("card: %+v", c)
	}
	if err := m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModePV}); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := cache.Get("vehicle:tavascan"); !strings.Contains(string(b), `"pv"`) {
		t.Fatalf("settings stored under %s", b)
	}
	// plugged in: detected as the asset
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	if lp := m.Loadpoints()[0]; lp.Vehicle != "tavascan" || lp.VehicleName != "Tavascan" {
		t.Fatalf("loadpoint: %+v", lp)
	}
}

// setupVehicle is setup with a vehicle asset "car" (configuration dev)
// whose live data comes from tibber/car1.
func setupVehicle(t *testing.T, dev config.Device, capWh float64, prov fakeProvider) (*Manager, *fakeCharger) {
	t.Helper()
	cfg := config.Default()
	dev.ID, dev.Type, dev.Params = "car", "vehicle", map[string]any{"source": "tibber", "sourceVehicle": "car1"}
	cfg.Devices = []config.Device{{ID: "wb", Type: "x", Mode: "owned"}, {ID: "tibber", Type: "y"}, dev}
	cfg.Tariffs.Grid = &tariff.Config{Type: "fixed", Price: 30}
	cfg.Tariffs.FeedIn = &tariff.Config{Type: "fixed", Price: 8}
	cfg.Charging.Loadpoints = []config.Loadpoint{{Charger: "wb"}}
	ch := &fakeCharger{}
	m, err := New(cfg, time.UTC, map[string]sdk.Device{"wb": ch, "tibber": prov, "car": fakeOwn{id: "car", capWh: capWh}}, nil, &memSessions{})
	if err != nil {
		t.Fatal(err)
	}
	m.refreshVehicles(context.Background())
	return m, ch
}

func TestFinishAndPhases(t *testing.T) {
	now := t0
	nan := math.NaN()
	// 40 % -> limit 80 % of 60 kWh = 24 kWh / 0.85 at 11 kW: about 2 h 34 min
	f := finish(sdk.VehicleStatus{SoC: 0.4, LimitSoC: 0.8}, 60000, nil, true, 11000, now)
	if f == nil || f.Basis != "limit" || f.At.Sub(now) < 2*time.Hour+30*time.Minute || f.At.Sub(now) > 2*time.Hour+40*time.Minute {
		t.Fatalf("limit: %+v", f)
	}
	// plan wins; not charging: no time, but the need
	if f := finish(sdk.VehicleStatus{SoC: 0.4, LimitSoC: 0.8}, 60000, &Schedule{SoC: 0.6}, false, 0, now); f.Basis != "plan" || !f.At.IsZero() || f.NeedWh < 13000 {
		t.Fatalf("plan: %+v", f)
	}
	if f := finish(sdk.VehicleStatus{SoC: 0.8, LimitSoC: 0.8}, 60000, nil, false, 0, now); !f.Reached {
		t.Fatalf("reached: %+v", f)
	}
	if f := finish(sdk.VehicleStatus{SoC: nan, LimitSoC: nan}, 60000, nil, true, 11000, now); f != nil {
		t.Fatalf("unknown SoC: %+v", f)
	}
	for _, c := range []struct {
		st   sdk.ChargerStatus
		on   bool
		want int
	}{{sdk.ChargerStatus{PowerW: 11000, Phases: 3}, true, 3}, {sdk.ChargerStatus{PowerW: 3600}, true, 1}, {sdk.ChargerStatus{PowerW: 9000}, true, 3}, {sdk.ChargerStatus{PowerW: 5000, Phases: 1}, false, 0}} {
		if got := livePhases(c.st, c.on); got != c.want {
			t.Errorf("phases %+v: %d", c.st, got)
		}
	}
}

func TestOffReason(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{GuestMode: "off"}, fakeProvider{})
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	if r := m.Loadpoints()[0].Reason; !strings.Contains(r, "Gäste laden nicht automatisch") {
		t.Fatalf("guest default off: %q", r)
	}
	m.SetOff("wb", true)
	tick(m, time.Minute, ch, 0, 0, 0, 500)
	if r := m.Loadpoints()[0].Reason; !strings.Contains(r, "Wallbox ausgeschaltet") {
		t.Fatalf("wallbox off: %q", r)
	}
}

func TestBoostSurplus(t *testing.T) {
	m, ch, _ := setup(t, config.Loadpoint{}, fakeProvider{"car1": {SoC: 0.4, Connected: true}})
	m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModeMinPV})
	ch.st = sdk.ChargerStatus{Connected: true, Charging: true, PowerW: 4140}
	// running at 6 A, 3 phases; battery already covers 1 kW of it
	boost := func(at time.Duration, boostW float64) Loadpoint {
		m.Update(context.Background(), SiteInput{Now: t0.Add(at), GridW: 0, BatteryW: 1000, PVW: 4000, LoadW: 5000,
			Chargers: map[string]sdk.ChargerStatus{"wb": ch.st}, BatteryPriority: true, BoostW: boostW, AutoBoost: true})
		return m.Loadpoints()[0]
	}
	// without boost: 3.1 kW surplus -> minimum current
	if lp := boost(0, 0); lp.Target.CurrentA != 6 {
		t.Fatalf("no boost: %+v", lp.Target)
	}
	// boost 4 kW: surplus 4.14 - 1 + 4 = 7.14 kW -> 10 A, battery priority ignored
	lp := boost(10*time.Second, 4000)
	if lp.Target.CurrentA != 10 || !strings.Contains(lp.Reason, "Boost") {
		t.Fatalf("boost: %+v %q", lp.Target, lp.Reason)
	}
	// a vehicle the battery must not supply gets no boost
	m.mu.Lock()
	m.batteryNever["wb"] = true
	m.mu.Unlock()
	if lp := boost(20*time.Second, 4000); lp.Target.CurrentA != 6 {
		t.Fatalf("excluded: %+v", lp.Target)
	}
	m.mu.Lock()
	m.batteryNever["wb"] = false
	m.mu.Unlock()
	// boost by hand (charge mode) without the automatic boost
	m.SetVehicleSettings("tibber/car1", VehicleSettings{Mode: ModeBoost})
	manual := func(at time.Duration, boostW float64) Loadpoint {
		m.Update(context.Background(), SiteInput{Now: t0.Add(at), GridW: 0, BatteryW: 1000, PVW: 4000, LoadW: 5000,
			Chargers: map[string]sdk.ChargerStatus{"wb": ch.st}, BoostW: boostW})
		return m.Loadpoints()[0]
	}
	if lp := manual(30*time.Second, 4000); lp.Target.CurrentA != 10 || !lp.Boosting {
		t.Fatalf("boost mode: %+v", lp)
	}
	// the battery at its boost limit: surplus only
	if lp := manual(40*time.Second, 0); lp.Boosting || !strings.Contains(lp.Reason, "Boost-Grenze") {
		t.Fatalf("at the limit: %+v", lp)
	}
}

func TestVehiclePriceRule(t *testing.T) {
	cfg := config.Default()
	rule := &pricing.Rule{PV: &pricing.Share{Mode: pricing.Replace, Base: pricing.BaseFixed, Ct: 15},
		Grid: &pricing.Share{Mode: pricing.Adjust, Percent: 10}}
	cfg.Devices = []config.Device{{ID: "wb", Type: "x", Mode: "owned"}, {ID: "tibber", Type: "y"},
		{ID: "eup", Type: "vehicle", Name: "Dräger e-Up", Price: rule, Params: map[string]any{"source": "tibber", "sourceVehicle": "car1"}}}
	cfg.Tariffs.Grid = &tariff.Config{Type: "fixed", Price: 30}
	cfg.Tariffs.FeedIn = &tariff.Config{Type: "fixed", Price: 8}
	cfg.Charging.Loadpoints = []config.Loadpoint{{Charger: "wb"}}
	ch := &fakeCharger{}
	ss := &memSessions{}
	m, err := New(cfg, time.UTC, map[string]sdk.Device{"wb": ch, "tibber": fakeProvider{"car1": {SoC: 0.4, Connected: true}},
		"eup": fakeOwn{id: "eup"}}, nil, ss)
	if err != nil {
		t.Fatal(err)
	}
	m.refreshVehicles(context.Background())
	ch.st = sdk.ChargerStatus{Connected: true}
	tick(m, 0, ch, 0, 0, 0, 500)
	// 11 kW for one hour: 6 kW from the grid, 5 kW PV
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
	if want := 5*15 + 6*30*1.1; math.Abs(s.CostCt-want) > 0.5 {
		t.Fatalf("cost %.2f, want %.2f", s.CostCt, want)
	}
	if !strings.HasPrefix(s.PriceNote, "Dräger e-Up: ") || !strings.Contains(s.PriceNote, "Netz Tarif +10 %") {
		t.Fatalf("note %q", s.PriceNote)
	}
	sh, ok := pricing.FromAttrs(s.Attrs)
	if !ok || math.Abs(sh.Grid.Wh-6000) > 1 || math.Abs(sh.PV.FeedInCt-5*8) > 0.5 {
		t.Fatalf("shares: %+v", sh)
	}
	// without a rule the same session costs PV at feed-in, grid at the tariff
	if c, _ := (*pricing.Rule)(nil).Cost(sh, ""); math.Abs(c-(6*30+5*8)) > 0.5 {
		t.Fatalf("default cost %.2f", c)
	}
}
