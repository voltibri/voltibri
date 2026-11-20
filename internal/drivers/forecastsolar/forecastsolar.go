// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package forecastsolar fetches PV forecasts from the free forecast.solar
// API (https://forecast.solar). The free tier allows 12 requests per hour
// per IP; results are cached for 30 minutes.
package forecastsolar

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:         "forecast.solar",
		Name:         "forecast.solar",
		Description:  "Kostenlose PV-Prognose (max. 12 Abfragen pro Stunde), mehrere Dachflächen möglich.",
		Category:     sdk.CatForecast,
		Capabilities: []sdk.Capability{sdk.CapPVForecast},
		Params: []sdk.Param{
			{Name: "lat", Label: "Breitengrad", Type: sdk.TypeFloat, Required: true, Min: sdk.Ptr(-90), Max: sdk.Ptr(90)},
			{Name: "lon", Label: "Längengrad", Type: sdk.TypeFloat, Required: true, Min: sdk.Ptr(-180), Max: sdk.Ptr(180)},
			{Name: "planes", Label: "Dachflächen", Type: sdk.TypeList, Required: true, Items: []sdk.Param{
				{Name: "kwp", Label: "Leistung", Type: sdk.TypeFloat, Unit: "kWp", Required: true, Min: sdk.Ptr(0)},
				{Name: "dec", Label: "Neigung", Type: sdk.TypeFloat, Unit: "°", Default: 30.0, Min: sdk.Ptr(0), Max: sdk.Ptr(90)},
				{Name: "az", Label: "Ausrichtung", Type: sdk.TypeFloat, Unit: "°", Default: 0.0, Min: sdk.Ptr(-180), Max: sdk.Ptr(180), Description: "0 = Süd, -90 = Ost, 90 = West"},
			}},
			{Name: "timezone", Label: "Zeitzone", Type: sdk.TypeString, Default: "Europe/Berlin", Advanced: true},
			{Name: "url", Label: "API-URL", Type: sdk.TypeURL, Advanced: true},
		},
		Factory: New,
	})
}

type plane struct {
	dec, az, kwp float64
}

// Device is a forecast.solar PV forecast for one or more roof planes.
type Device struct {
	id       string
	baseURL  string
	lat, lon float64
	planes   []plane
	loc      *time.Location

	mu      sync.Mutex
	points  []point
	fetched time.Time
}

type point struct {
	t time.Time
	w float64
}

// New creates the device. Parameters:
//
//	lat, lon: location (required)
//	planes:   list of {dec: tilt°, az: azimuth° (0=south, -90=east), kwp}
//	timezone: default Europe/Berlin
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	lat, err := cfg.Float("lat")
	if err != nil {
		return nil, err
	}
	lon, err := cfg.Float("lon")
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.StringOr("timezone", "Europe/Berlin"))
	if err != nil {
		return nil, err
	}
	d := &Device{id: cfg.ID, baseURL: cfg.StringOr("url", "https://api.forecast.solar"), lat: lat, lon: lon, loc: loc}
	for _, p := range cfg.Slice("planes") {
		m, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: planes must be a list of maps", cfg.ID)
		}
		pc := sdk.DeviceConfig{ID: cfg.ID, Params: m}
		kwp, err := pc.Float("kwp")
		if err != nil {
			return nil, err
		}
		d.planes = append(d.planes, plane{dec: pc.FloatOr("dec", 30), az: pc.FloatOr("az", 0), kwp: kwp})
	}
	if len(d.planes) == 0 {
		return nil, fmt.Errorf("%s: at least one plane required", cfg.ID)
	}
	return d, nil
}

func (d *Device) ID() string { return d.id }

type response struct {
	Result struct {
		Watts map[string]float64 `json:"watts"`
	} `json:"result"`
}

func (d *Device) fetch(ctx context.Context) ([]point, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.points != nil && time.Since(d.fetched) < 30*time.Minute {
		return d.points, nil
	}
	sum := map[time.Time]float64{}
	for _, p := range d.planes {
		var r response
		u := fmt.Sprintf("%s/estimate/%.4f/%.4f/%.0f/%.0f/%.2f", d.baseURL, d.lat, d.lon, p.dec, p.az, p.kwp)
		if err := httpx.GetJSON(ctx, u, &r); err != nil {
			if d.points != nil {
				return d.points, nil // keep stale forecast on errors / rate limit
			}
			return nil, err
		}
		for ts, w := range r.Result.Watts {
			t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, d.loc)
			if err != nil {
				continue
			}
			sum[t] += w
		}
	}
	pts := make([]point, 0, len(sum))
	for t, w := range sum {
		pts = append(pts, point{t, w})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
	d.points, d.fetched = pts, time.Now()
	return pts, nil
}

// interpolate returns the instantaneous power at t (0 outside the data,
// which only covers daylight hours).
func interpolate(pts []point, t time.Time) float64 {
	i := sort.Search(len(pts), func(i int) bool { return !pts[i].t.Before(t) })
	if i < len(pts) && pts[i].t.Equal(t) {
		return pts[i].w
	}
	if i == 0 || i == len(pts) {
		return 0
	}
	a, b := pts[i-1], pts[i]
	if b.t.Sub(a.t) > 3*time.Hour { // night gap between days
		return 0
	}
	f := float64(t.Sub(a.t)) / float64(b.t.Sub(a.t))
	return a.w + f*(b.w-a.w)
}

// PVForecast implements sdk.PVForecaster with mean power per 15 min slot.
func (d *Device) PVForecast(ctx context.Context, from, to time.Time) ([]sdk.Slot, error) {
	pts, err := d.fetch(ctx)
	if err != nil {
		return nil, err
	}
	var out []sdk.Slot
	for t := series.SlotStart(from); t.Before(to); t = t.Add(series.Slot) {
		// mean of start, middle and end of the slot
		w := (interpolate(pts, t) + 2*interpolate(pts, t.Add(series.Slot/2)) + interpolate(pts, t.Add(series.Slot))) / 4
		out = append(out, sdk.Slot{Start: t, Duration: series.Slot, Value: math.Max(0, w)})
	}
	return out, nil
}
