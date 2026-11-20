// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package forecast implements the "stage 0" household load forecast:
// statistics over comparable past days ("analog days"), weighted by age,
// with an optional linear temperature correction for heat pumps.
//
// For a target day voltibri looks at the most recent days of the same day
// type (workday, Saturday, Sunday/holiday). Each analog day yields one
// possible outcome; the forecast reports the weighted median (P50) and an
// upper quantile (default P80) of those outcomes. The upper quantile is what
// the planner uses for the battery reserve: running short in the night costs
// grid energy, keeping a few percent too much costs almost nothing.
package forecast

import (
	"math"
	"sort"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/series"
)

const slotsPerDay = 96

// Config tunes the load forecast.
type Config struct {
	AnalogDays   int     // number of comparable days used (default 10)
	MaxAgeDays   int     // ignore days older than this (default 70)
	HalfLifeDays float64 // weight halves every N days of age (default 21)
	Quantile     float64 // upper quantile, e.g. 0.8
	HDDBaseC     float64 // heating degree day base temperature (default 15 °C)
	MinDays      int     // minimum history before forecasting (default 7)
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{AnalogDays: 10, MaxAgeDays: 70, HalfLifeDays: 21, Quantile: 0.8, HDDBaseC: 15, MinDays: 7}
}

type day struct {
	date    time.Time // local midnight
	typ     calendar.DayType
	loadW   [slotsPerDay]float64 // NaN = missing
	n       int                  // number of valid slots
	tempSum float64
	tempN   int
}

func (d *day) complete() bool { return d.n >= slotsPerDay-4 } // tolerate 1h of gaps

func (d *day) meanTemp() float64 {
	if d.tempN == 0 {
		return math.NaN()
	}
	return d.tempSum / float64(d.tempN)
}

// LoadModel is the stage 0 load forecaster. It is not safe for concurrent
// use; the caller serialises access.
type LoadModel struct {
	cfg  Config
	cal  *calendar.Calendar
	loc  *time.Location
	days map[time.Time]*day

	// temperature model: dailyWh = a + b*HDD
	tempCoefWh float64 // b, Wh per heating degree day (0 = no correction)
	dirty      bool

	cache map[analogKey][]analog // analog days per target, reset on Add
}

type analogKey struct {
	target, cutoffDay time.Time
	needNext          bool
}

// NewLoadModel creates an empty model.
func NewLoadModel(cfg Config, cal *calendar.Calendar, loc *time.Location) *LoadModel {
	return &LoadModel{cfg: cfg, cal: cal, loc: loc, days: map[time.Time]*day{}}
}

func (m *LoadModel) midnight(t time.Time) time.Time {
	t = t.In(m.loc)
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, m.loc)
}

// slotIndex returns the slot of t within its local day. On DST change days
// the day has 92 or 100 slots; slots beyond 95 are folded onto the last.
func (m *LoadModel) slotIndex(t time.Time) int {
	t = t.In(m.loc)
	i := (t.Hour()*60 + t.Minute()) / 15
	if i >= slotsPerDay {
		i = slotsPerDay - 1
	}
	return i
}

// Add feeds one measured slot into the model.
func (m *LoadModel) Add(r series.Record) {
	key := m.midnight(r.Time)
	d := m.days[key]
	if d == nil {
		d = &day{date: key, typ: m.cal.DayType(key.Add(12 * time.Hour))}
		for i := range d.loadW {
			d.loadW[i] = math.NaN()
		}
		m.days[key] = d
	}
	i := m.slotIndex(r.Time)
	if math.IsNaN(d.loadW[i]) {
		d.n++
	}
	d.loadW[i] = r.LoadW
	if r.HasTemp() {
		d.tempSum += r.TempC
		d.tempN++
	}
	m.dirty = true
	m.cache = nil
}

// Train feeds a whole series.
func (m *LoadModel) Train(s series.Series) {
	for _, r := range s {
		m.Add(r)
	}
}

// Days returns the number of complete days in the history.
func (m *LoadModel) Days() int {
	n := 0
	for _, d := range m.days {
		if d.complete() {
			n++
		}
	}
	return n
}

// Ready reports whether enough history exists for a forecast.
func (m *LoadModel) Ready() bool { return m.Days() >= m.cfg.MinDays }

// TempCoefWh returns the fitted heating sensitivity in Wh per degree day.
func (m *LoadModel) TempCoefWh() float64 {
	m.refit()
	return m.tempCoefWh
}

func (m *LoadModel) hdd(tempC float64) float64 {
	return math.Max(0, m.cfg.HDDBaseC-tempC)
}

// refit estimates dailyWh = a + b*HDD by least squares over complete days
// with temperature data (recomputed lazily after new data arrived).
func (m *LoadModel) refit() {
	if !m.dirty {
		return
	}
	m.dirty = false
	var xs, ys []float64
	for _, d := range m.days {
		if !d.complete() || d.tempN == 0 {
			continue
		}
		var wh float64
		for _, v := range d.loadW {
			if !math.IsNaN(v) {
				wh += v * series.SlotHours
			}
		}
		wh *= slotsPerDay / float64(d.n)
		xs = append(xs, m.hdd(d.meanTemp()))
		ys = append(ys, wh)
	}
	m.tempCoefWh = 0
	if len(xs) < 14 {
		return
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(len(xs))
	my /= float64(len(ys))
	var sxx, sxy float64
	for i := range xs {
		sxx += (xs[i] - mx) * (xs[i] - mx)
		sxy += (xs[i] - mx) * (ys[i] - my)
	}
	// require a meaningful spread of heating degrees (variance > 4 K²)
	if sxx/float64(len(xs)) < 4 {
		return
	}
	if b := sxy / sxx; b > 0 {
		m.tempCoefWh = b
	}
}

type analog struct {
	offset time.Duration // analog day - target day (negative)
	days   int           // age in days
	weight float64
	hdd    float64 // NaN if unknown
}

// analogs returns comparable past days for the target day starting at
// targetMidnight. Only days strictly before the target day and before now
// (cutoff) are used.
func (m *LoadModel) analogs(targetMidnight, cutoff time.Time, needNext bool) []analog {
	key := analogKey{targetMidnight, m.midnight(cutoff), needNext}
	if a, ok := m.cache[key]; ok {
		return a
	}
	a := m.findAnalogs(targetMidnight, cutoff, needNext)
	if m.cache == nil {
		m.cache = map[analogKey][]analog{}
	}
	m.cache[key] = a
	return a
}

func (m *LoadModel) findAnalogs(targetMidnight, cutoff time.Time, needNext bool) []analog {
	// all days before the cutoff day are complete or not; days on the cutoff
	// day itself are never complete before midnight, so caching per cutoff
	// day is exact.
	cutoff = m.midnight(cutoff).Add(24 * time.Hour)
	typ := m.cal.DayType(targetMidnight.Add(12 * time.Hour))
	collect := func(sameType bool) []analog {
		var out []analog
		for age := 1; age <= m.cfg.MaxAgeDays && len(out) < m.cfg.AnalogDays; age++ {
			dd := targetMidnight.AddDate(0, 0, -age)
			d := m.days[dd]
			if d == nil || !d.complete() || !dd.Before(cutoff) {
				continue
			}
			if needNext {
				// the window continues into the following day
				next := m.days[dd.AddDate(0, 0, 1)]
				if next == nil || next.n < slotsPerDay/2 {
					continue
				}
			}
			if sameType && d.typ != typ {
				continue
			}
			out = append(out, analog{
				offset: dd.Sub(targetMidnight),
				days:   age,
				weight: math.Pow(0.5, float64(age)/m.cfg.HalfLifeDays),
				hdd:    m.hdd(d.meanTemp()),
			})
		}
		return out
	}
	a := collect(true)
	if len(a) < 3 { // not enough days of this type yet: use any day
		a = collect(false)
	}
	return a
}

// valueAt returns the stored load for t, or NaN.
func (m *LoadModel) valueAt(t time.Time) float64 {
	d := m.days[m.midnight(t)]
	if d == nil {
		return math.NaN()
	}
	return d.loadW[m.slotIndex(t)]
}

// shift moves t by whole calendar days, keeping the local wall clock time
// (robust against DST changes).
func (m *LoadModel) shift(t time.Time, days int) time.Time {
	t = t.In(m.loc)
	return time.Date(t.Year(), t.Month(), t.Day()+days, t.Hour(), t.Minute(), 0, 0, m.loc)
}

// Estimate is a forecast with median and upper quantile.
type Estimate struct {
	P50 float64
	PQ  float64 // upper quantile (Config.Quantile)
	N   int     // number of analog days used (0 = no data)
}

// Slot forecasts the mean load in W for the slot starting at t.
// tempC is the forecast mean temperature of that day (NaN if unknown).
func (m *LoadModel) Slot(t, cutoff time.Time, tempC float64) Estimate {
	m.refit()
	target := m.midnight(t)
	var vals, ws []float64
	for _, a := range m.analogs(target, cutoff, false) {
		days := int(math.Round(-a.offset.Hours() / 24))
		v := m.valueAt(m.shift(t, -days))
		if math.IsNaN(v) {
			continue
		}
		v += m.tempAdjust(a, tempC) / 24 // spread daily correction evenly (Wh/day -> W)
		vals = append(vals, math.Max(0, v))
		ws = append(ws, a.weight)
	}
	return m.estimate(vals, ws)
}

// Energy forecasts the energy in Wh consumed in [from, to). The window may
// span midnight (e.g. the night). tempC is the forecast mean temperature.
func (m *LoadModel) Energy(from, to, cutoff time.Time, tempC float64) Estimate {
	m.refit()
	from, to = series.SlotStart(from), series.SlotStart(to)
	if !to.After(from) {
		return Estimate{}
	}
	target := m.midnight(from)
	needNext := !m.midnight(to.Add(-time.Second)).Equal(target)
	hours := to.Sub(from).Hours()
	var vals, ws []float64
	for _, a := range m.analogs(target, cutoff, needNext) {
		days := int(math.Round(-a.offset.Hours() / 24))
		var wh float64
		missing := 0
		total := 0
		for t := from; t.Before(to); t = t.Add(series.Slot) {
			total++
			v := m.valueAt(m.shift(t, -days))
			if math.IsNaN(v) {
				missing++
				continue
			}
			wh += v * series.SlotHours
		}
		if missing*4 > total { // more than 25% missing: unusable
			continue
		}
		if missing > 0 {
			wh *= float64(total) / float64(total-missing)
		}
		wh += m.tempAdjust(a, tempC) * hours / 24
		vals = append(vals, math.Max(0, wh))
		ws = append(ws, a.weight)
	}
	return m.estimate(vals, ws)
}

// tempAdjust returns the daily energy correction in Wh for using analog day
// a on a day with forecast temperature tempC.
func (m *LoadModel) tempAdjust(a analog, tempC float64) float64 {
	if m.tempCoefWh == 0 || math.IsNaN(tempC) || math.IsNaN(a.hdd) {
		return 0
	}
	return m.tempCoefWh * (m.hdd(tempC) - a.hdd)
}

func (m *LoadModel) estimate(vals, ws []float64) Estimate {
	if len(vals) == 0 {
		return Estimate{}
	}
	return Estimate{
		P50: weightedQuantile(vals, ws, 0.5),
		PQ:  weightedQuantile(vals, ws, m.cfg.Quantile),
		N:   len(vals),
	}
}

// weightedQuantile returns the q-quantile of vals with weights ws using
// linear interpolation between the weighted midpoints.
func weightedQuantile(vals, ws []float64, q float64) float64 {
	type vw struct{ v, w float64 }
	xs := make([]vw, len(vals))
	var total float64
	for i := range vals {
		xs[i] = vw{vals[i], ws[i]}
		total += ws[i]
	}
	if len(xs) == 1 {
		return xs[0].v
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].v < xs[j].v })
	// cumulative position of each value's weight midpoint, normalised to 0..1
	pos := make([]float64, len(xs))
	var cum float64
	for i, x := range xs {
		pos[i] = (cum + x.w/2) / total
		cum += x.w
	}
	if q <= pos[0] {
		return xs[0].v
	}
	for i := 1; i < len(xs); i++ {
		if q <= pos[i] {
			f := (q - pos[i-1]) / (pos[i] - pos[i-1])
			return xs[i-1].v + f*(xs[i].v-xs[i-1].v)
		}
	}
	return xs[len(xs)-1].v
}
