// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package site runs the control loop: measure every 10 s, store 15-minute
// averages, re-plan every slot and apply the battery charge limit.
package site

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/circuit"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/meter"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/sdk"
)

// CacheProvider offers persistent per-device caches (implemented by store.Store).
type CacheProvider interface {
	Cache(namespace string) sdk.Cache
}

// History persists the 15-minute measurements (implemented by store.Store).
type History interface {
	LoadHistory(ctx context.Context, since time.Time, loc *time.Location) (series.Series, error)
	AppendHistory(ctx context.Context, r series.Record) error
}

// HistoryProvider is an optional device capability that supplies past data
// to pre-train the forecaster (used by the demo driver).
type HistoryProvider interface {
	History(now time.Time) series.Series
}

// State is the latest measurement.
type State struct {
	Time            time.Time            `json:"time"`
	GridW           float64              `json:"gridW"`
	PVW             float64              `json:"pvW"`
	BatteryW        float64              `json:"batteryW"`
	LoadW           float64              `json:"loadW"`
	SoC             float64              `json:"soc"`
	HasBattery      bool                 `json:"hasBattery"`
	LimitW          float64              `json:"chargeLimitW"`
	DischargeLimitW float64              `json:"dischargeLimitW"`       // -1 = battery not held back
	ExcludedW       float64              `json:"excludedW"`             // load the battery must not supply
	Excluded        []string             `json:"excluded,omitempty"`    // devices held back from the battery
	Heat            []HeatReading        `json:"heat,omitempty"`        // heat pumps: heat output, COP, temperatures
	Controlled      bool                 `json:"controlled"`            // voltibri actively controls the battery
	ForceCharge     *ForceCharge         `json:"forceCharge,omitempty"` // the battery charges now up to a target
	Boost           *Boost               `json:"boost,omitempty"`       // the battery helps the surplus loads
	DryRun          bool                 `json:"dryRun"`
	Producers       []Reading            `json:"producers"`
	Consumers       []Reading            `json:"consumers"`
	RestW           float64              `json:"restW"` // house load not covered by a measured consumer
	Loadpoints      []charging.Loadpoint `json:"loadpoints,omitempty"`
	Circuits        []circuit.Status     `json:"circuits,omitempty"` // house connection first
	Errors          []string             `json:"errors,omitempty"`
}

// Reading is the power of a single producer or consumer.
type Reading struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Icon    string  `json:"icon"`
	PowerW  float64 `json:"powerW"`
	Mode    string  `json:"mode"`
	Group   string  `json:"group"`             // overview groups devices with the same group
	Kind    string  `json:"kind,omitempty"`    // asset kind (heat-pump, heating-rod, ...)
	HeatUse string  `json:"heatUse,omitempty"` // heating rods: hotwater or heating
	Step    int     `json:"step,omitempty"`    // loads in steps: current step (phases on)
	Steps   int     `json:"steps,omitempty"`   // loads in steps: number of steps
	// surplus loads in steps (heating rods): runtime mode and priority
	LoadMode LoadMode `json:"loadMode,omitempty"`
	Priority *int     `json:"priority,omitempty"`
	Boosting bool     `json:"boosting,omitempty"` // the home battery adds power (boost)
	Top      bool     `json:"top,omitempty"`      // first before all others for today
	Circuit  string   `json:"circuit,omitempty"`  // sub-distribution the device belongs to
}

type producer struct {
	meta Reading
	dev  sdk.PVProducer
}

type consumer struct {
	meta    Reading
	power   sdk.PowerConsumer // either power ...
	charger sdk.Charger       // ... or a charger
}

// Site wires devices, forecaster and planner together.
type Site struct {
	cfg *config.Config
	loc *time.Location

	devices   map[string]sdk.Device
	grid      sdk.GridMeter
	pvs       []producer
	consumers []consumer
	bat       sdk.Battery
	batCtl    sdk.BatteryController
	pvf       sdk.PVForecaster
	tariff    sdk.TariffSource

	charge     *charging.Manager
	flex       *flexState              // surplus loads (heating rods in steps)
	forceCh    sdk.BatteryForceCharger // battery can be force-charged (guest policy forceBattery)
	forcing    bool
	forceErr   bool
	batCache   sdk.Cache              // battery settings, force charging and boost (nil without store)
	batSet     BatterySettings        // runtime settings (targets, max. discharge, boost)
	force      *ForceCharge           // running force charging
	boost      *Boost                 // running boost
	boostHold  bool                   // automatic boost stopped by hand: no new start until the SoC fell below the start
	boostEmpty bool                   // the battery is at the boost stop SoC (with hysteresis)
	guard      socGuard               // charge targets with hysteresis
	loadCache  sdk.Cache              // load settings (priority, mode)
	loadSet    map[string]LoadSetting // load settings per device
	auto       planner.Targets        // automatic charge targets (from the last plan)

	model     *forecast.LoadModel
	hist      History
	corr      map[string]meter.Correction // per device id
	circuits  []circuit.Node              // installation tree, nil if not configured
	prof      profile                     // load profile of the current slot
	discharge float64                     // wanted discharge limit, -1 = none
	limits    limitState                  // last written battery limits
	runs      map[string]*runState        // run detection per device

	mu    sync.RWMutex
	state State
	plan  planner.Plan

	// accumulator for the current slot
	slot           time.Time
	sumPV, sumLoad float64
	nSamples       int
}

// New creates the site from configuration.
func New(cfg *config.Config, hist History) (*Site, error) {
	if err := cfg.Ready(); err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.Site.Timezone)
	if err != nil {
		return nil, err
	}
	cal, err := calendar.New(cfg.Site.Holidays...)
	if err != nil {
		return nil, err
	}
	s := &Site{cfg: cfg, loc: loc, hist: hist, devices: map[string]sdk.Device{}, corr: map[string]meter.Correction{},
		model: forecast.NewLoadModel(forecast.DefaultConfig(), cal, loc)}
	built := false
	defer func() {
		if !built { // a failed configuration must not leave connections open
			s.closeDevices()
		}
	}()
	s.circuits = circuitNodes(cfg)
	s.discharge = -1
	s.batSet = BatterySettings{BoostReserve: boostReserveDef, BoostStopSoC: boostStopDefault}
	now := time.Now()
	for _, dc := range cfg.Devices {
		if dc.Retired(now) {
			continue // decommissioned: history only
		}
		s.corr[dc.ID] = dc.Correction
		dcfg := sdk.DeviceConfig{ID: dc.ID, Name: dc.Name, Type: dc.Type, Mode: sdk.ControlMode(dc.Mode), DryRun: cfg.Site.DryRun, Params: dc.Params}
		if cp, ok := hist.(CacheProvider); ok {
			dcfg.Cache = cp.Cache(dc.ID)
		}
		dev, err := sdk.New(dcfg)
		if err != nil {
			return nil, fmt.Errorf("device %s: %w", dc.ID, err)
		}
		s.devices[dc.ID] = dev
	}
	var ok bool
	if s.grid, ok = s.devices[cfg.Roles.Grid].(sdk.GridMeter); !ok {
		return nil, fmt.Errorf("roles.grid: device %q cannot measure grid power", cfg.Roles.Grid)
	}
	isPV := map[string]bool{}
	for _, id := range cfg.Roles.PV {
		pv, ok := s.devices[id].(sdk.PVProducer)
		if !ok {
			return nil, fmt.Errorf("roles.pv: device %q cannot measure PV power", id)
		}
		s.pvs = append(s.pvs, producer{meta: meta(cfg, id, "solar", true), dev: pv})
		isPV[id] = true
	}
	// every other device that measures its own consumption is a consumer
	for _, dc := range cfg.Devices {
		if dc.ID == cfg.Roles.Grid || dc.ID == cfg.Roles.Battery || isPV[dc.ID] {
			continue
		}
		dev := s.devices[dc.ID]
		if ch, ok := dev.(sdk.Charger); ok {
			s.consumers = append(s.consumers, consumer{meta: meta(cfg, dc.ID, "car", false), charger: ch})
		} else if pc, ok := dev.(sdk.PowerConsumer); ok {
			s.consumers = append(s.consumers, consumer{meta: meta(cfg, dc.ID, "plug", false), power: pc})
		}
	}
	if id := cfg.Roles.Battery; id != "" {
		if s.bat, ok = s.devices[id].(sdk.Battery); !ok {
			return nil, fmt.Errorf("roles.battery: device %q is not a battery", id)
		}
		if ctl, ok := s.devices[id].(sdk.BatteryController); ok && modeOf(cfg, id) == sdk.ModeOwned {
			s.batCtl = ctl
		}
	}
	if ctl, ok := s.batCtl.(sdk.BatteryForceCharger); ok {
		s.forceCh = ctl
	}
	var settings sdk.Cache
	if cp, ok := hist.(CacheProvider); ok {
		settings = cp.Cache("charging")
		s.batCache = cp.Cache(batNS)
		s.loadCache = cp.Cache(loadsNS)
	}
	s.loadLoads()
	sessions, _ := hist.(run.Store)
	s.flex = newFlex(cfg, s.devices)
	if s.charge, err = charging.New(cfg, loc, s.devices, settings, sessions); err != nil {
		return nil, err
	}
	s.loadBattery() // after the loads: whether a boost is possible depends on them
	if id := cfg.Roles.PVForecast; id != "" {
		if s.pvf, ok = s.devices[id].(sdk.PVForecaster); !ok {
			return nil, fmt.Errorf("roles.pvForecast: device %q provides no PV forecast", id)
		}
	}
	if id := cfg.Roles.Tariff; id != "" {
		if s.tariff, ok = s.devices[id].(sdk.TariffSource); !ok {
			return nil, fmt.Errorf("roles.tariff: device %q provides no prices", id)
		}
	}
	built = true
	return s, nil
}

// meta returns display information for a device.
func meta(cfg *config.Config, id, defIcon string, producer bool) Reading {
	r := Reading{ID: id, Name: id, Icon: defIcon}
	kind := ""
	for _, d := range cfg.Devices {
		if d.ID == id {
			kind = config.KindOf(d)
			if d.Name != "" {
				r.Name = d.Name
			}
			if d.Icon != "" {
				r.Icon = d.Icon
			}
			r.Mode = d.Mode
			r.Group = d.Group
			r.HeatUse = d.HeatUse
		}
	}
	if r.Group == "" {
		r.Group = defaultGroup(r, kind, producer)
	}
	r.Kind = kind
	return r
}

// defaultGroup derives the overview group: all producers form "pv", consumers
// are grouped by kind or icon (all cars, all heating, ...), anything else
// stands alone. Heat pump and heating rods heat the same buffer: "heizung".
func defaultGroup(r Reading, kind string, producer bool) string {
	if producer {
		return "pv"
	}
	switch kind {
	case config.KindEVCharger:
		return "wallboxen"
	case config.KindHeatPump, config.KindHeatingRod:
		return "heizung"
	case config.KindAppliance, config.KindClimate:
		if r.Icon == "plug" || r.Icon == "" {
			return "haushalt" // household appliances share one bubble
		}
	}
	switch r.Icon {
	case "car":
		return "wallboxen"
	case "heatpump", "water":
		return "heizung"
	}
	return r.ID
}

func modeOf(cfg *config.Config, id string) sdk.ControlMode {
	for _, d := range cfg.Devices {
		if d.ID == id {
			return sdk.ControlMode(d.Mode)
		}
	}
	return sdk.ModeObserved
}

// loadHistory trains the model from the stored history. On first start a
// legacy CSV history (site.history) is imported once; without any history
// a device may supply synthetic data (demo).
func (s *Site) loadHistory(ctx context.Context) error {
	since := time.Now().AddDate(0, 0, -100)
	hist, err := s.hist.LoadHistory(ctx, since, s.loc)
	if err != nil {
		return err
	}
	if len(hist) == 0 && s.cfg.Site.History != "" {
		if hist, err = s.importCSV(ctx, s.cfg.Site.History); err != nil {
			return err
		}
	}
	s.model.Train(hist)
	slog.Info("history loaded", "slots", len(hist), "days", s.model.Days())
	if s.model.Days() == 0 {
		for _, d := range s.devices {
			if hp, ok := d.(HistoryProvider); ok {
				s.model.Train(hp.History(time.Now()))
				slog.Info("forecaster pre-trained from device history", "device", d.ID(), "days", s.model.Days())
				break
			}
		}
	}
	return nil
}

func (s *Site) importCSV(ctx context.Context, path string) (series.Series, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hist, err := series.ReadCSV(bufio.NewReader(f), s.loc)
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", path, err)
	}
	for _, r := range hist {
		if err := s.hist.AppendHistory(ctx, r); err != nil {
			return nil, err
		}
	}
	slog.Info("legacy CSV history imported into the database", "file", path, "slots", len(hist))
	return hist, nil
}

// power applies the device's correction to a power reading.
func (s *Site) power(id string, raw float64) float64 { return s.corr[id].Power(raw) }

// Run executes the control loop until ctx is cancelled.
func (s *Site) Run(ctx context.Context) error {
	if err := s.loadHistory(ctx); err != nil {
		return err
	}

	measure := time.NewTicker(10 * time.Second)
	defer measure.Stop()
	keepAlive := time.NewTicker(time.Minute)
	defer keepAlive.Stop()

	go s.charge.Run(ctx)
	defer s.charge.Close()

	s.measure(ctx)
	s.replan(ctx)
	s.apply(ctx)
	s.measure(ctx) // show the effect of the first limit right away
	for {
		select {
		case <-ctx.Done():
			defer s.closeDevices()
			// keep what was measured: the started slot (added to on the
			// next start) and open runs
			s.finishRuns(time.Now())
			fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.flushProfile(fctx, s.slot)
			fcancel()
			if s.batCtl != nil {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := s.batCtl.ResetLimits(rctx); err != nil {
					slog.Error("resetting battery limits failed", "err", err)
				} else {
					slog.Info("battery limits reset")
				}
				cancel()
			}
			return nil
		case <-measure.C:
			if s.measure(ctx) { // a new slot started
				s.replan(ctx)
				s.apply(ctx)
			}
		case <-keepAlive.C:
			s.apply(ctx) // refresh before the device side revert timeout
		}
	}
}

// measure reads all meters; returns true when a new slot began.
func (s *Site) measure(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	st := State{Time: time.Now(), DryRun: s.cfg.Site.DryRun, Controlled: s.batCtl != nil, HasBattery: s.bat != nil,
		Producers: []Reading{}, Consumers: []Reading{}}
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	var err error
	st.GridW, err = s.grid.GridPower(ctx)
	note(err)
	st.GridW = s.power(s.cfg.Roles.Grid, st.GridW)
	for _, pv := range s.pvs {
		w, err := pv.dev.PVPower(ctx)
		note(err)
		w = s.power(pv.meta.ID, w)
		st.PVW += w
		r := pv.meta
		r.PowerW = w
		st.Producers = append(st.Producers, r)
	}
	if s.bat != nil {
		st.SoC, err = s.bat.SoC(ctx)
		note(err)
		st.BatteryW, err = s.bat.BatteryPower(ctx)
		note(err)
		st.BatteryW = s.power(s.cfg.Roles.Battery, st.BatteryW)
	}
	st.LoadW = math.Max(0, st.GridW+st.PVW+st.BatteryW)
	known := 0.0
	chargers := map[string]sdk.ChargerStatus{}
	for _, c := range s.consumers {
		var w float64
		if c.charger != nil {
			cs, err := c.charger.ChargerStatus(ctx)
			note(err)
			w = cs.PowerW
			cs.PowerW = math.Max(0, s.power(c.meta.ID, w))
			chargers[c.meta.ID] = cs
		} else {
			w, err = c.power.ConsumerPower(ctx)
			note(err)
		}
		r := c.meta
		r.PowerW = math.Max(0, s.power(c.meta.ID, w))
		if sg, ok := s.devices[c.meta.ID].(sdk.Stager); ok {
			r.Steps = sg.Steps()
			r.Step, _ = sg.Step(ctx) // cached by the driver
		}
		known += r.PowerW
		st.Consumers = append(st.Consumers, r)
	}
	st.RestW = math.Max(0, st.LoadW-known)
	var tree *circuit.Tree
	if s.circuits != nil {
		powers, phases := map[string]float64{}, map[string]int{}
		for _, c := range st.Consumers {
			powers[c.ID] = c.PowerW
			if _, ok := chargers[c.ID]; ok {
				phases[c.ID] = s.cfg.LoadpointFor(c.ID).Phases
			}
		}
		tree = s.measureCircuits(ctx, powers, phases, note)
		st.Circuits = tree.Statuses() // before load management books planned changes
		for i := range st.Consumers {
			if c := tree.CircuitOf(st.Consumers[i].ID); c != circuit.Root {
				st.Consumers[i].Circuit = c
			}
		}
	}
	// the surplus loads (wallboxes, heating rods) share the surplus by
	// priority; the battery adds boost power for boosted loads
	boostW, autoBoost := 0.0, false
	if s.bat != nil {
		boostW, autoBoost = s.updateBoost(st.SoC, st.Time)
	}
	alloc := s.newAllocator(ctx, st.Time, surplusInput{gridW: st.GridW, batteryW: st.BatteryW,
		batteryFirst: s.batteryFirst(st) && !autoBoost, boostW: boostW, autoBoost: autoBoost,
		loadpoints: s.charge.Loadpoints(), chargers: chargerPowers(chargers), rodPowers: readingPowers(st.Consumers)})
	guestW, excludedW, forceW := s.updateCharging(ctx, st, chargers, tree, alloc, boostW, autoBoost)
	alloc.finish()
	for i, c := range st.Consumers {
		if l := s.flex.load(c.ID); l != nil {
			ls := s.setting(c.ID)
			st.Consumers[i].LoadMode, st.Consumers[i].Priority, st.Consumers[i].Top = ls.Mode, &ls.Priority, ls.top(st.Time)
			st.Consumers[i].Boosting = alloc.rodBoosting(c.ID) && l.step > 0
			st.Consumers[i].Step = l.step
		}
	}
	if s.bat != nil {
		s.updateForce(ctx, st.SoC, forceW)
		s.mu.RLock()
		st.ForceCharge, st.Boost = s.force, s.boost
		s.mu.RUnlock()
	}
	st.Heat = s.readHeat(ctx)
	st.Loadpoints = s.charge.Loadpoints()
	s.updateDischargeLimit(ctx, &st, excludedW)
	st.Errors = errs
	for _, e := range errs {
		slog.Warn("measurement error", "err", e)
	}

	s.mu.Lock()
	st.LimitW = s.state.LimitW
	s.state = st
	s.mu.Unlock()

	slot := series.SlotStart(st.Time)
	newSlot := !s.slot.IsZero() && !slot.Equal(s.slot)
	// load profile per device and run detection
	if newSlot {
		s.flushProfile(ctx, s.slot)
	}
	gridCt, feedinCt := s.charge.Prices(ctx)
	s.sampleProfile(st, sourcesOf(st, gridCt, feedinCt))
	if newSlot && s.nSamples > 0 {
		r := series.Record{Time: s.slot, PVW: s.sumPV / float64(s.nSamples), LoadW: s.sumLoad / float64(s.nSamples), SpotCt: math.NaN(), TempC: math.NaN()}
		s.model.Add(r)
		if err := s.hist.AppendHistory(ctx, r); err != nil {
			slog.Error("writing history failed", "err", err)
		}
		if ps, ok := s.hist.(PlanStore); ok && st.HasBattery && !math.IsNaN(st.SoC) {
			if err := ps.SaveSlotSoC(ctx, s.slot, st.SoC); err != nil {
				slog.Error("writing state of charge failed", "err", err)
			}
		}
		s.sumPV, s.sumLoad, s.nSamples = 0, 0, 0
	}
	if s.slot.IsZero() || newSlot {
		s.slot = slot
	}
	if len(errs) == 0 {
		s.sumPV += st.PVW
		s.sumLoad += math.Max(0, st.LoadW-guestW) // guests stay out of the load forecast
		s.nSamples++
	}
	return newSlot
}

func (s *Site) replan(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	now := time.Now().In(s.loc)
	from := series.SlotStart(now)
	to := from.Add(48 * time.Hour)

	s.expireManual(now)
	s.mu.RLock()
	soc, bs := s.state.SoC, s.batSet
	s.mu.RUnlock()

	b := s.cfg.Battery
	in := planner.Input{Now: now, TempC: math.NaN(), Battery: planner.Battery{
		CapacityWh: b.CapacityWh, SoC: soc, MaxChargeW: b.MaxChargeW, MaxDischargeW: b.MaxDischargeW,
		ChargeEff: b.ChargeEff, DischargeEff: b.DischargeEff, MinSoC: math.Max(b.MinSoC, bs.MinSoC), MaxSoC: 1,
	}}
	// the charge targets and the discharge limit of the battery settings
	if bs.MaxSoC > 0 {
		in.Battery.MaxSoC = math.Max(in.Battery.MinSoC, bs.MaxSoC)
	}
	if bs.MaxDischargeW > 0 && (b.MaxDischargeW <= 0 || bs.MaxDischargeW < b.MaxDischargeW) {
		in.Battery.MaxDischargeW = bs.MaxDischargeW
	}
	var plan planner.Plan
	var auto planner.Targets
	if s.bat == nil || s.pvf == nil {
		plan = planner.Make(planner.Input{Now: now}, nil, s.cfg.Planner)
		plan.Reasons = append(plan.Reasons, "Batterie oder PV-Prognose nicht konfiguriert (roles.battery / roles.pvForecast).")
	} else {
		pv, err := s.pvf.PVForecast(ctx, from, to)
		if err != nil {
			slog.Warn("PV forecast unavailable", "err", err)
		}
		prices := map[int64]float64{} // keyed by Unix seconds: sources use different time zones
		if s.tariff != nil {
			ps, err := s.tariff.Prices(ctx, from, to)
			if err != nil {
				slog.Warn("prices unavailable", "err", err)
			}
			for _, p := range ps {
				for t := p.Start; t.Before(p.Start.Add(p.Duration)); t = t.Add(series.Slot) {
					prices[t.Unix()] = p.Value
				}
			}
		}
		for _, p := range pv {
			spot, ok := prices[p.Start.Unix()]
			if !ok {
				spot = math.NaN()
			}
			in.Slots = append(in.Slots, planner.InputSlot{Start: p.Start, PVW: p.Value, SpotCt: spot})
		}
		// automatic charge targets first: the plan works with the targets that apply
		auto = planner.AutoTargets(in, s.model, s.cfg.Planner, bs.autoConfig())
		eff := s.effective(bs, auto)
		in.Battery.MinSoC, in.Battery.MaxSoC = math.Max(b.MinSoC, eff.MinSoC), 1
		if eff.MaxSoC > 0 {
			in.Battery.MaxSoC = math.Max(in.Battery.MinSoC, eff.MaxSoC)
		}
		plan = planner.Make(in, s.model, s.cfg.Planner)
	}
	s.mu.Lock()
	s.plan, s.auto = plan, auto
	s.mu.Unlock()
	s.savePlan(ctx, plan)
	slog.Info("plan", "mode", plan.Mode, "targetSoC", fmt.Sprintf("%.0f%%", 100*plan.TargetSoC), "limitW", plan.CurrentLimitW(), "reason", first(plan.Reasons))
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func (s *Site) apply(ctx context.Context) { s.writeLimits(ctx, true) }

// updateCharging runs the loadpoints and returns the guest charging power.
func (s *Site) updateCharging(ctx context.Context, st State, chargers map[string]sdk.ChargerStatus, tree *circuit.Tree,
	alloc *allocator, boostW float64, autoBoost bool) (guestW, excludedW, forceW float64) {
	if !s.charge.HasLoadpoints() {
		return 0, 0, 0
	}
	prios, top := map[string]int{}, map[string]bool{}
	for _, lp := range s.cfg.Charging.Loadpoints {
		ls := s.setting(lp.Charger)
		prios[lp.Charger], top[lp.Charger] = ls.Priority, ls.top(st.Time)
	}
	res := s.charge.Update(ctx, charging.SiteInput{Now: st.Time, GridW: st.GridW, BatteryW: st.BatteryW, PVW: st.PVW, LoadW: st.LoadW,
		Chargers: chargers, BatteryPriority: s.batteryFirst(st) && !autoBoost, BatteryFirst: s.batteryFirst(st), Circuits: tree, Alloc: alloc,
		BoostW: boostW, AutoBoost: autoBoost, Priority: prios, Top: top})

	return res.GuestW, res.ExcludedW, res.ForceBatteryW
}

// batteryFirst: the battery keeps PV first while it is low, unless the
// battery plan expects enough surplus later anyway.
func (s *Site) batteryFirst(st State) bool {
	s.mu.RLock()
	plan, bs := s.plan, s.batSet
	s.mu.RUnlock()
	return st.HasBattery && st.SoC < s.prioritySoC(bs) && plan.Mode != planner.ModeDelay
}

func chargerPowers(cs map[string]sdk.ChargerStatus) map[string]float64 {
	m := make(map[string]float64, len(cs))
	for id, c := range cs {
		m[id] = c.PowerW
	}
	return m
}

func readingPowers(rs []Reading) map[string]float64 {
	m := make(map[string]float64, len(rs))
	for _, r := range rs {
		m[r.ID] = r.PowerW
	}
	return m
}

// updateForce force-charges the battery for a guest (policy forceBattery:
// charge as if the guest was not there) or a force charging, and returns to the
// battery plan afterwards.
func (s *Site) updateForce(ctx context.Context, soc, guestForceW float64) {
	s.mu.RLock()
	planLimit := s.plan.CurrentLimitW()
	s.mu.RUnlock()
	want := s.forceWant(soc, guestForceW, planLimit)
	switch {
	case want > 0 && s.forceCh == nil:
		if !s.forceErr {
			slog.Warn("guest policy forceBattery: the battery cannot be force-charged (no control or not supported); the battery charges from real surplus only")
			s.forceErr = true
		}
	case want > 0:
		if err := s.forceCh.ForceCharge(ctx, want); err != nil {
			slog.Error("force-charging the battery failed", "err", err)
		} else {
			s.forcing = true
		}
	case s.forcing:
		s.forcing = false
		s.apply(ctx) // back to the battery plan
	}
}

// Charging returns the loadpoint manager.
func (s *Site) Charging() *charging.Manager { return s.charge }

// State returns the latest measurement.
func (s *Site) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Plan returns the current plan.
func (s *Site) Plan() planner.Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.plan
}
