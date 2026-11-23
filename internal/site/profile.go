// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/store"
)

// ProfileWriter stores the load profile (implemented by store.Store).
type ProfileWriter interface {
	AppendProfile(ctx context.Context, slot time.Time, values []store.ProfileValue) error
}

// MinRunWh: shorter runs (a blip of a device) are not stored.
const MinRunWh = 20.0

// sources is how the house load is covered at one moment.
type sources struct {
	pv, battery, grid float64 // shares, sum 1
	gridCt, feedinCt  float64 // prices (0 if unknown)
}

func sourcesOf(st State, gridCt, feedinCt float64) sources {
	s := sources{gridCt: nz(gridCt), feedinCt: nz(feedinCt)}
	if st.LoadW <= 0 {
		s.pv = 1
		return s
	}
	s.grid = math.Min(1, math.Max(0, st.GridW)/st.LoadW)
	s.battery = math.Min(1-s.grid, math.Max(0, st.BatteryW)/st.LoadW) // positive = discharging
	s.pv = 1 - s.grid - s.battery
	return s
}

func nz(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// add books energy drawn by a device: shares and cost (PV and battery at
// the lost feed-in tariff, grid at the grid price).
func (src sources) add(v *store.ProfileValue, wh float64) {
	v.ImportWh += wh
	v.PVWh += wh * src.pv
	v.BatteryWh += wh * src.battery
	v.GridWh += wh * src.grid
	v.CostCt += wh / 1000 * (src.grid*src.gridCt + (src.pv+src.battery)*src.feedinCt)
}

// profile accumulates the energy of all devices in the current slot.
type profile struct {
	last   time.Time
	values map[string]*store.ProfileValue
}

func (p *profile) value(id string) *store.ProfileValue {
	if p.values == nil {
		p.values = map[string]*store.ProfileValue{}
	}
	v, ok := p.values[id]
	if !ok {
		v = &store.ProfileValue{Asset: id}
		p.values[id] = v
	}
	return v
}

// sample integrates one measurement; returns the energy factor dt in hours.
func (s *Site) sampleProfile(st State, src sources) {
	p := &s.prof
	dt := st.Time.Sub(p.last).Hours()
	p.last = st.Time
	if dt <= 0 || dt > 2.0/60 { // first sample or a gap: nothing to integrate
		return
	}
	for _, c := range st.Consumers {
		src.add(p.value(c.ID), c.PowerW*dt)
	}
	for _, pr := range st.Producers {
		p.value(pr.ID).ExportWh += math.Max(0, pr.PowerW) * dt
	}
	// grid and battery are channels of their device (a hybrid inverter is
	// PV, battery and grid meter at once): stored as <device>.grid and
	// <device>.battery so the values do not mix
	if id := s.cfg.Roles.Grid; id != "" {
		id += ".grid"
		if st.GridW >= 0 {
			p.value(id).ImportWh += st.GridW * dt
		} else {
			p.value(id).ExportWh += -st.GridW * dt
		}
	}
	if id := s.cfg.Roles.Battery; id != "" && st.HasBattery {
		id += ".battery"
		if st.BatteryW >= 0 {
			p.value(id).ExportWh += st.BatteryW * dt
		} else {
			p.value(id).ImportWh += -st.BatteryW * dt
		}
	}
	s.detectRuns(st, src, dt)
}

// flushProfile stores the finished slot.
func (s *Site) flushProfile(ctx context.Context, slot time.Time) {
	p := &s.prof
	if len(p.values) == 0 {
		return
	}
	vals := make([]store.ProfileValue, 0, len(p.values))
	for _, v := range p.values {
		vals = append(vals, *v)
	}
	p.values = nil
	if w, ok := s.hist.(ProfileWriter); ok {
		if err := w.AppendProfile(ctx, slot, vals); err != nil {
			slog.Error("writing load profile failed", "err", err)
		}
	}
}

// runState detects runs of one device from its power.
type runState struct {
	run        *run.Run
	belowSince time.Time
}

// runSettings of a device: whether runs are detected, threshold and pause.
func runSettings(d config.Device) (on bool, thresholdW float64, pause time.Duration) {
	kind := config.KindOf(d)
	switch kind {
	case config.KindEVCharger, config.KindVehicleAccount, config.KindForecast, config.KindTariff,
		config.KindPV, config.KindBattery, config.KindInverter, config.KindGridMeter:
		return false, 0, 0 // wallboxes have charging sessions; producers and meters no runs
	}
	on = d.Runs == nil || *d.Runs
	thresholdW = d.RunThresholdW
	if thresholdW <= 0 {
		thresholdW = 25
	}
	pause = 5 * time.Minute // e.g. a washing machine pauses between washing and spinning
	if kind == config.KindHeatPump {
		pause = 15 * time.Minute
	}
	return on, thresholdW, pause
}

// detectRuns starts a run when a device draws more than its threshold and
// ends it after the pause below the threshold.
func (s *Site) detectRuns(st State, src sources, dt float64) {
	if s.runs == nil {
		s.runs = map[string]*runState{}
	}
	for _, c := range st.Consumers {
		d, ok := s.device(c.ID)
		if !ok {
			continue
		}
		on, threshold, pause := runSettings(d)
		if !on {
			continue
		}
		rs := s.runs[c.ID]
		if rs == nil {
			rs = &runState{}
			s.runs[c.ID] = rs
		}
		active := c.PowerW > threshold
		if rs.run == nil {
			if !active {
				continue
			}
			rs.run = &run.Run{Asset: c.ID, Kind: config.RunKind(config.KindOf(d)), Start: st.Time,
				PriceNote: "PV und Akku zur Einspeisevergütung, Netz zum Tarif"}
		}
		r := rs.run
		var v store.ProfileValue
		src.add(&v, c.PowerW*dt)
		r.EnergyWh += v.ImportWh
		r.PVWh += v.PVWh
		r.BatteryWh += v.BatteryWh
		r.GridWh += v.GridWh
		r.CostCt += v.CostCt
		if active {
			rs.belowSince = time.Time{}
			continue
		}
		if rs.belowSince.IsZero() {
			rs.belowSince = st.Time
		}
		if st.Time.Sub(rs.belowSince) >= pause {
			r.End = rs.belowSince
			rs.run, rs.belowSince = nil, time.Time{}
			s.saveRun(*r)
		}
	}
}

func (s *Site) saveRun(r run.Run) {
	if r.EnergyWh < MinRunWh {
		return
	}
	if rs, ok := s.hist.(run.Store); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := rs.SaveRun(ctx, r); err != nil {
			slog.Error("saving run failed", "asset", r.Asset, "err", err)
		}
	}
}

// finishRuns ends open runs when the site stops (a restart splits a run;
// that is acceptable for consumers without sessions).
func (s *Site) finishRuns(now time.Time) {
	for _, rs := range s.runs {
		if rs.run != nil {
			rs.run.End = now
			s.saveRun(*rs.run)
			rs.run = nil
		}
	}
}

func (s *Site) device(id string) (config.Device, bool) {
	for _, d := range s.cfg.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return config.Device{}, false
}
