// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package demo simulates a complete site (PV, house load, battery, grid,
// PV forecast and spot prices) in real time, based on the synthetic data
// generator. It lets you try voltibri without any hardware.
package demo

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/internal/sim"
	"github.com/voltibri/voltibri/sdk"
)

func init() { sdk.Register("demo", New) }

// Device is a simulated site.
type Device struct {
	id    string
	synth sim.SynthConfig
	cal   *calendar.Calendar
	loc   *time.Location
	data  map[int64]series.Record // keyed by Unix seconds of slot start

	mu       sync.Mutex
	cap      float64
	maxW     float64
	soc      float64
	limit    float64
	last     time.Time
	batteryW float64
}

// New creates the demo site. Parameters: capacityWh (13800), kwp (10),
// maxChargeW (6000), timezone (Europe/Berlin).
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	loc, err := time.LoadLocation(cfg.StringOr("timezone", "Europe/Berlin"))
	if err != nil {
		return nil, err
	}
	cal, _ := calendar.New()
	sc := sim.DefaultSynth()
	sc.KWp = cfg.FloatOr("kwp", 10)
	d := &Device{
		id: cfg.ID, synth: sc, cal: cal, loc: loc,
		cap: cfg.FloatOr("capacityWh", 13800), maxW: cfg.FloatOr("maxChargeW", 6000),
		soc: 0.4, limit: planner.Unlimited,
	}
	// 60 days of history plus 3 days of "future" for forecasts. shiftDays
	// moves the season (e.g. 200 shows June data in November).
	shift := int(cfg.FloatOr("shiftDays", 0))
	start := time.Now().In(loc).AddDate(0, 0, -60)
	d.data = map[int64]series.Record{}
	for _, r := range sim.Synthesize(start.AddDate(0, 0, shift), 63, sc, cal) {
		r.Time = r.Time.AddDate(0, 0, -shift)
		d.data[r.Time.Unix()] = r
	}
	return d, nil
}

func (d *Device) ID() string { return d.id }

func (d *Device) at(t time.Time) series.Record {
	return d.data[series.SlotStart(t).Unix()]
}

// History returns the synthetic past, used to pre-train the forecaster.
func (d *Device) History(now time.Time) series.Series {
	var out series.Series
	for t := series.SlotStart(now.AddDate(0, 0, -60)); t.Before(series.SlotStart(now)); t = t.Add(series.Slot) {
		if r, ok := d.data[t.Unix()]; ok {
			out = append(out, r)
		}
	}
	return out
}

// step integrates the battery up to now.
func (d *Device) step() series.Record {
	now := time.Now()
	r := d.at(now)
	if !d.last.IsZero() {
		dt := now.Sub(d.last).Hours()
		net := r.PVW - r.LoadW
		if net >= 0 {
			c := math.Min(net, d.maxW)
			if d.limit >= 0 {
				c = math.Min(c, d.limit)
			}
			c = math.Min(c, (1-d.soc)*d.cap/0.95/math.Max(dt, 1e-9))
			d.soc += c * dt * 0.95 / d.cap
			d.batteryW = -c
		} else {
			dis := math.Min(math.Min(-net, d.maxW), (d.soc-0.05)*d.cap*0.95/math.Max(dt, 1e-9))
			dis = math.Max(0, dis)
			d.soc -= dis * dt / 0.95 / d.cap
			d.batteryW = dis
		}
	}
	d.last = now
	return r
}

func (d *Device) GridPower(ctx context.Context) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.step()
	return r.LoadW - r.PVW - d.batteryW, nil
}

func (d *Device) PVPower(ctx context.Context) (float64, error) {
	return d.at(time.Now()).PVW, nil
}

func (d *Device) SoC(ctx context.Context) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.step()
	return d.soc, nil
}

func (d *Device) BatteryPower(ctx context.Context) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.step()
	return d.batteryW, nil
}

func (d *Device) CapacityWh() float64 { return d.cap }

func (d *Device) SetChargeLimit(ctx context.Context, w float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.step()
	d.limit = w
	return nil
}

func (d *Device) ResetLimits(ctx context.Context) error {
	return d.SetChargeLimit(ctx, planner.Unlimited)
}

func (d *Device) PVForecast(ctx context.Context, from, to time.Time) ([]sdk.Slot, error) {
	var out []sdk.Slot
	for t := series.SlotStart(from); t.Before(to); t = t.Add(series.Slot) {
		out = append(out, sdk.Slot{Start: t, Duration: series.Slot, Value: d.at(t).PVW})
	}
	return out, nil
}

func (d *Device) Prices(ctx context.Context, from, to time.Time) ([]sdk.Slot, error) {
	var out []sdk.Slot
	for t := series.SlotStart(from); t.Before(to); t = t.Add(series.Slot) {
		r, ok := d.data[t.Unix()]
		if !ok {
			continue
		}
		out = append(out, sdk.Slot{Start: t, Duration: series.Slot, Value: r.SpotCt})
	}
	return out, nil
}
