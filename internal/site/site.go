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
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/sdk"
)

// HistoryProvider is an optional device capability that supplies past data
// to pre-train the forecaster (used by the demo driver).
type HistoryProvider interface {
	History(now time.Time) series.Series
}

// State is the latest measurement.
type State struct {
	Time       time.Time `json:"time"`
	GridW      float64   `json:"gridW"`
	PVW        float64   `json:"pvW"`
	BatteryW   float64   `json:"batteryW"`
	LoadW      float64   `json:"loadW"`
	SoC        float64   `json:"soc"`
	LimitW     float64   `json:"chargeLimitW"`
	Controlled bool      `json:"controlled"` // voltibri actively controls the battery
	DryRun     bool      `json:"dryRun"`
	Errors     []string  `json:"errors,omitempty"`
}

// Site wires devices, forecaster and planner together.
type Site struct {
	cfg *config.Config
	loc *time.Location

	devices map[string]sdk.Device
	grid    sdk.GridMeter
	pvs     []sdk.PVProducer
	bat     sdk.Battery
	batCtl  sdk.BatteryController
	pvf     sdk.PVForecaster
	tariff  sdk.TariffSource

	model *forecast.LoadModel
	hist  *os.File

	mu    sync.RWMutex
	state State
	plan  planner.Plan

	// accumulator for the current slot
	slot           time.Time
	sumPV, sumLoad float64
	nSamples       int
}

// New creates the site from configuration.
func New(cfg *config.Config) (*Site, error) {
	loc, err := time.LoadLocation(cfg.Site.Timezone)
	if err != nil {
		return nil, err
	}
	cal, err := calendar.New(cfg.Site.Holidays...)
	if err != nil {
		return nil, err
	}
	s := &Site{cfg: cfg, loc: loc, devices: map[string]sdk.Device{}, model: forecast.NewLoadModel(forecast.DefaultConfig(), cal, loc)}
	for _, dc := range cfg.Devices {
		dev, err := sdk.New(sdk.DeviceConfig{ID: dc.ID, Type: dc.Type, Mode: sdk.ControlMode(dc.Mode), DryRun: cfg.Site.DryRun, Params: dc.Params})
		if err != nil {
			return nil, fmt.Errorf("device %s: %w", dc.ID, err)
		}
		s.devices[dc.ID] = dev
	}
	var ok bool
	if s.grid, ok = s.devices[cfg.Roles.Grid].(sdk.GridMeter); !ok {
		return nil, fmt.Errorf("roles.grid: device %q cannot measure grid power", cfg.Roles.Grid)
	}
	for _, id := range cfg.Roles.PV {
		pv, ok := s.devices[id].(sdk.PVProducer)
		if !ok {
			return nil, fmt.Errorf("roles.pv: device %q cannot measure PV power", id)
		}
		s.pvs = append(s.pvs, pv)
	}
	if id := cfg.Roles.Battery; id != "" {
		if s.bat, ok = s.devices[id].(sdk.Battery); !ok {
			return nil, fmt.Errorf("roles.battery: device %q is not a battery", id)
		}
		if ctl, ok := s.devices[id].(sdk.BatteryController); ok && modeOf(cfg, id) == sdk.ModeOwned {
			s.batCtl = ctl
		}
	}
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
	return s, nil
}

func modeOf(cfg *config.Config, id string) sdk.ControlMode {
	for _, d := range cfg.Devices {
		if d.ID == id {
			return sdk.ControlMode(d.Mode)
		}
	}
	return sdk.ModeObserved
}

// loadHistory trains the model from the history file (or a history
// provider) and opens the file for appending.
func (s *Site) loadHistory() error {
	path := s.cfg.Site.History
	if f, err := os.Open(path); err == nil {
		hist, err := series.ReadCSV(bufio.NewReader(f), s.loc)
		f.Close()
		if err != nil {
			return fmt.Errorf("history %s: %w", path, err)
		}
		s.model.Train(hist)
		slog.Info("history loaded", "file", path, "slots", len(hist), "days", s.model.Days())
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.model.Days() == 0 {
		for _, d := range s.devices {
			if hp, ok := d.(HistoryProvider); ok {
				h := hp.History(time.Now())
				s.model.Train(h)
				slog.Info("forecaster pre-trained from device history", "device", d.ID(), "days", s.model.Days())
				break
			}
		}
	}
	newFile := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		newFile = true
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if newFile {
		if err := series.WriteHeader(f); err != nil {
			return err
		}
	}
	s.hist = f
	return nil
}

// Run executes the control loop until ctx is cancelled.
func (s *Site) Run(ctx context.Context) error {
	if err := s.loadHistory(); err != nil {
		return err
	}
	defer s.hist.Close()

	measure := time.NewTicker(10 * time.Second)
	defer measure.Stop()
	keepAlive := time.NewTicker(time.Minute)
	defer keepAlive.Stop()

	s.measure(ctx)
	s.replan(ctx)
	s.apply(ctx)
	for {
		select {
		case <-ctx.Done():
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
	st := State{Time: time.Now(), DryRun: s.cfg.Site.DryRun, Controlled: s.batCtl != nil}
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	var err error
	st.GridW, err = s.grid.GridPower(ctx)
	note(err)
	for _, pv := range s.pvs {
		w, err := pv.PVPower(ctx)
		note(err)
		st.PVW += w
	}
	if s.bat != nil {
		st.SoC, err = s.bat.SoC(ctx)
		note(err)
		st.BatteryW, err = s.bat.BatteryPower(ctx)
		note(err)
	}
	st.LoadW = math.Max(0, st.GridW+st.PVW+st.BatteryW)
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
	if newSlot && s.nSamples > 0 {
		r := series.Record{Time: s.slot, PVW: s.sumPV / float64(s.nSamples), LoadW: s.sumLoad / float64(s.nSamples), SpotCt: math.NaN(), TempC: math.NaN()}
		s.model.Add(r)
		if err := series.WriteRecord(s.hist, r); err != nil {
			slog.Error("writing history failed", "err", err)
		}
		s.sumPV, s.sumLoad, s.nSamples = 0, 0, 0
	}
	if s.slot.IsZero() || newSlot {
		s.slot = slot
	}
	if len(errs) == 0 {
		s.sumPV += st.PVW
		s.sumLoad += st.LoadW
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

	s.mu.RLock()
	soc := s.state.SoC
	s.mu.RUnlock()

	b := s.cfg.Battery
	in := planner.Input{Now: now, TempC: math.NaN(), Battery: planner.Battery{
		CapacityWh: b.CapacityWh, SoC: soc, MaxChargeW: b.MaxChargeW, MaxDischargeW: b.MaxDischargeW,
		ChargeEff: b.ChargeEff, DischargeEff: b.DischargeEff, MinSoC: b.MinSoC, MaxSoC: 1,
	}}
	var plan planner.Plan
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
		plan = planner.Make(in, s.model, s.cfg.Planner)
	}
	s.mu.Lock()
	s.plan = plan
	s.mu.Unlock()
	slog.Info("plan", "mode", plan.Mode, "targetSoC", fmt.Sprintf("%.0f%%", 100*plan.TargetSoC), "limitW", plan.CurrentLimitW(), "reason", first(plan.Reasons))
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func (s *Site) apply(ctx context.Context) {
	s.mu.RLock()
	limit := s.plan.CurrentLimitW()
	s.mu.RUnlock()
	if s.batCtl == nil {
		return
	}
	var err error
	if limit < 0 {
		err = s.batCtl.ResetLimits(ctx)
	} else {
		err = s.batCtl.SetChargeLimit(ctx, limit)
	}
	if err != nil {
		slog.Error("applying battery limit failed", "err", err)
		return
	}
	s.mu.Lock()
	s.state.LimitW = limit
	s.mu.Unlock()
}

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
