// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package demo simulates a complete site (PV, house load, battery, grid,
// PV forecast and spot prices) in real time, based on the synthetic data
// generator. It lets you try voltibri without any hardware.
//
// Additional demo devices attach to a site:
//
//	demo-producer: a share of the site's PV (e.g. a balcony plant)
//	demo-consumer: kind "share" (fixed share of the house load, e.g. heat
//	               pump) or "wallbox" (charges an EV with PV surplus,
//	               adding to the house load)
package demo

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/internal/sim"
	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.Register("demo", New)
	sdk.Register("demo-producer", NewProducer)
	sdk.Register("demo-consumer", NewConsumer)
}

var (
	sitesMu sync.Mutex
	sites   = map[string]*Device{}
)

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
	pvShare  float64 // share of PV reported by the site itself
	soc      float64
	limit    float64
	last     time.Time
	batteryW float64
	wallbox  []*Consumer
}

// New creates the demo site. Parameters: capacityWh (13800), kwp (10),
// maxChargeW (6000), pvShare (1), shiftDays (0), timezone (Europe/Berlin).
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
		pvShare: cfg.FloatOr("pvShare", 1),
		soc:     0.4, limit: planner.Unlimited,
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
	sitesMu.Lock()
	sites[cfg.ID] = d
	sitesMu.Unlock()
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

// flows returns PV, total house load (incl. wallbox) and the battery power
// that results from the current charge limit.
func (d *Device) flows(now time.Time) (pv, load, battery float64) {
	r := d.at(now)
	pv, load = r.PVW, r.LoadW
	for _, w := range d.wallbox {
		load += w.wallboxPower(pv, load, now)
	}
	net := pv - load
	if net >= 0 {
		c := math.Min(net, d.maxW)
		if d.limit >= 0 {
			c = math.Min(c, d.limit)
		}
		if d.soc >= 0.999 {
			c = 0
		}
		return pv, load, -c
	}
	dis := math.Min(-net, d.maxW)
	if d.soc <= 0.05 {
		dis = 0
	}
	return pv, load, dis
}

// step integrates the SoC with the battery power of the previous interval
// and recomputes the current flows (so all readings are consistent).
func (d *Device) step() (pv, load float64) {
	now := time.Now()
	if !d.last.IsZero() {
		dt := now.Sub(d.last).Hours()
		if d.batteryW < 0 {
			d.soc += -d.batteryW * dt * 0.95 / d.cap
		} else {
			d.soc -= d.batteryW * dt / 0.95 / d.cap
		}
		d.soc = math.Max(0.05, math.Min(1, d.soc))
	}
	d.last = now
	pv, load, d.batteryW = d.flows(now)
	return pv, load
}

func (d *Device) GridPower(ctx context.Context) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pv, load := d.step()
	return load - pv - d.batteryW, nil
}

func (d *Device) PVPower(ctx context.Context) (float64, error) {
	return d.at(time.Now()).PVW * d.pvShare, nil
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
	d.step()
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

func siteOf(cfg sdk.DeviceConfig) (*Device, error) {
	id := cfg.StringOr("site", "demo")
	sitesMu.Lock()
	defer sitesMu.Unlock()
	s, ok := sites[id]
	if !ok {
		return nil, fmt.Errorf("%s: demo site %q not found (declare it before this device)", cfg.ID, id)
	}
	return s, nil
}

// Producer reports a share of the demo site's PV.
type Producer struct {
	id    string
	site  *Device
	share float64
}

// NewProducer creates a demo producer. Parameters: site, share.
func NewProducer(cfg sdk.DeviceConfig) (sdk.Device, error) {
	s, err := siteOf(cfg)
	if err != nil {
		return nil, err
	}
	return &Producer{id: cfg.ID, site: s, share: cfg.FloatOr("share", 0.1)}, nil
}

func (p *Producer) ID() string { return p.id }

func (p *Producer) PVPower(ctx context.Context) (float64, error) {
	return p.site.at(time.Now()).PVW * p.share, nil
}

// Consumer is a simulated consumer of the demo site.
type Consumer struct {
	id    string
	site  *Device
	kind  string
	share float64
	maxW  float64
}

// NewConsumer creates a demo consumer. Parameters: site, kind ("share" or
// "wallbox"), share (for kind share), maxW (for wallbox, default 11000).
func NewConsumer(cfg sdk.DeviceConfig) (sdk.Device, error) {
	s, err := siteOf(cfg)
	if err != nil {
		return nil, err
	}
	c := &Consumer{id: cfg.ID, site: s, kind: cfg.StringOr("kind", "share"), share: cfg.FloatOr("share", 0.2), maxW: cfg.FloatOr("maxW", 11000)}
	if c.kind == "wallbox" {
		s.mu.Lock()
		s.wallbox = append(s.wallbox, c)
		s.mu.Unlock()
	}
	return c, nil
}

func (c *Consumer) ID() string { return c.id }

// wallboxPower: the car is plugged in on weekdays 9:00–12:45 and charges
// with PV surplus above 500 W (min 1.4 kW, max maxW).
func (c *Consumer) wallboxPower(pv, load float64, now time.Time) float64 {
	t := now.In(c.site.loc)
	h := float64(t.Hour()) + float64(t.Minute())/60
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday || h < 9 || h >= 12.75 {
		return 0
	}
	surplus := pv - load - 500
	if surplus < 1400 {
		return 0
	}
	return math.Min(surplus, c.maxW)
}

func (c *Consumer) ConsumerPower(ctx context.Context) (float64, error) {
	s := c.site
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	r := s.at(now)
	if c.kind == "wallbox" {
		return c.wallboxPower(r.PVW, r.LoadW, now), nil
	}
	return r.LoadW * c.share, nil
}
