// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package sim replays historical (or synthetic) data through a battery
// model to compare charging strategies.
package sim

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/series"
)

// Params configures a simulation run.
type Params struct {
	Battery     planner.Battery // SoC = initial SoC
	FeedInCt    float64         // fixed feed-in tariff ct/kWh
	ImportCt    float64         // fixed import price ct/kWh (used if no spot or DynamicImport=false)
	Dynamic     bool            // import price = spot + SurchargeCt
	SurchargeCt float64         // taxes, grid fees etc. on top of spot
	MorningHour int             // hour at which the morning SoC is sampled (default 7)

	PVForecastError float64 // sigma of daily multiplicative PV forecast error (0 = perfect)
	Seed            int64

	Planner  planner.Config
	Forecast forecast.Config
	Calendar *calendar.Calendar
	Location *time.Location
}

// Result holds the key figures of one run.
type Result struct {
	Strategy        string
	Days            float64
	PVKWh           float64
	LoadKWh         float64
	ImportKWh       float64
	ExportKWh       float64
	ExportNegKWh    float64 // exported while spot price < 0
	ChargedNegKWh   float64 // charged into the battery while spot < 0
	ThroughputKWh   float64 // energy charged into the battery
	CostEUR         float64 // import cost minus feed-in revenue
	HoursAbove90    float64 // hours with SoC >= 90 %
	HoursFull       float64 // hours with SoC >= 99 %
	MeanSoC         float64
	MorningSoCMean  float64
	MorningSoCP10   float64
	ShortNights     int // nights in which the battery hit MinSoC before the morning hour
	SelfSufficiency float64
	SelfConsumption float64
}

// Strategy decides the charge limit for the slot i.
type Strategy interface {
	Name() string
	// Limit returns the charge limit in W (planner.Unlimited for none).
	Limit(i int, soc float64) float64
}

// Immediate charges with every surplus watt (what most systems do).
type Immediate struct{}

func (Immediate) Name() string                     { return "sofort laden" }
func (Immediate) Limit(i int, soc float64) float64 { return planner.Unlimited }

// Planned uses the stage 0 forecaster and planner, trained only on data
// before the simulated moment.
type Planned struct {
	s      series.Series
	p      Params
	model  *forecast.LoadModel
	next   int // next record to feed into the model
	plan   planner.Plan
	planAt int
	errs   map[time.Time]float64
	rng    *rand.Rand

	// Plans counts how often each mode was chosen (for reporting).
	Plans map[planner.Mode]int
}

// NewPlanned creates the planner strategy for series s.
func NewPlanned(s series.Series, p Params) *Planned {
	return &Planned{
		s: s, p: p,
		model:  forecast.NewLoadModel(p.Forecast, p.Calendar, p.Location),
		planAt: -1 << 30,
		errs:   map[time.Time]float64{},
		rng:    rand.New(rand.NewSource(p.Seed)),
		Plans:  map[planner.Mode]int{},
	}
}

func (*Planned) Name() string { return "voltibri (spät laden)" }

const horizonSlots = 48 * 4

// replanEvery re-plans hourly in the simulation (live: every slot).
const replanEvery = 4

func (pl *Planned) pvError(t time.Time) float64 {
	if pl.p.PVForecastError == 0 {
		return 1
	}
	y, m, d := t.Date()
	k := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	e, ok := pl.errs[k]
	if !ok {
		e = math.Max(0.2, 1+pl.rng.NormFloat64()*pl.p.PVForecastError)
		pl.errs[k] = e
	}
	return e
}

func (pl *Planned) Limit(i int, soc float64) float64 {
	for ; pl.next < i; pl.next++ {
		pl.model.Add(pl.s[pl.next])
	}
	now := pl.s[i].Time
	if i-pl.planAt >= replanEvery || i < pl.planAt {
		var slots []planner.InputSlot
		for j := i; j < len(pl.s) && j < i+horizonSlots; j++ {
			r := pl.s[j]
			if j > i && r.Time.Sub(pl.s[j-1].Time) != series.Slot {
				break // gap in data
			}
			slots = append(slots, planner.InputSlot{Start: r.Time, PVW: r.PVW * pl.pvError(r.Time), SpotCt: r.SpotCt})
		}
		b := pl.p.Battery
		b.SoC = soc
		pl.plan = planner.Make(planner.Input{Now: now, Battery: b, Slots: slots, TempC: pl.tempTomorrow(i)}, pl.model, pl.p.Planner)
		pl.planAt = i
		pl.Plans[pl.plan.Mode]++
	}
	k := i - pl.planAt
	if k < len(pl.plan.Slots) {
		return pl.plan.Slots[k].ChargeLimitW
	}
	return planner.Unlimited
}

// tempTomorrow returns the mean temperature of the next 24 h (a perfect
// temperature forecast; weather forecasts are good enough for this).
func (pl *Planned) tempTomorrow(i int) float64 {
	var sum float64
	n := 0
	for j := i; j < len(pl.s) && j < i+96; j++ {
		if pl.s[j].HasTemp() {
			sum += pl.s[j].TempC
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return sum / float64(n)
}

// Run simulates the strategy over the series.
func Run(s series.Series, st Strategy, p Params) Result {
	b := p.Battery
	soc := b.SoC
	res := Result{Strategy: st.Name()}
	morningHour := p.MorningHour
	if morningHour == 0 {
		morningHour = 7
	}
	var morning []float64
	var socSum float64
	hitMin := false
	for i, r := range s {
		dt := series.SlotHours
		limit := st.Limit(i, soc)
		net := r.PVW - r.LoadW
		var charge, discharge, imp, exp float64 // W
		if net >= 0 {
			room := (b.MaxSoC - soc) * b.CapacityWh / b.ChargeEff / dt
			charge = math.Min(math.Min(net, b.MaxChargeW), room)
			if limit >= 0 {
				charge = math.Min(charge, limit)
			}
			charge = math.Max(0, charge)
			exp = net - charge
			soc += charge * dt * b.ChargeEff / b.CapacityWh
		} else {
			avail := (soc - b.MinSoC) * b.CapacityWh * b.DischargeEff / dt
			discharge = math.Max(0, math.Min(math.Min(-net, b.MaxDischargeW), avail))
			imp = -net - discharge
			soc -= discharge * dt / b.DischargeEff / b.CapacityWh
		}
		soc = math.Max(b.MinSoC, math.Min(b.MaxSoC, soc))

		res.PVKWh += r.PVW * dt / 1000
		res.LoadKWh += r.LoadW * dt / 1000
		res.ImportKWh += imp * dt / 1000
		res.ExportKWh += exp * dt / 1000
		res.ThroughputKWh += charge * dt / 1000
		if r.HasSpot() && r.SpotCt < 0 {
			res.ExportNegKWh += exp * dt / 1000
			res.ChargedNegKWh += charge * dt / 1000
		}
		price := p.ImportCt
		if p.Dynamic && r.HasSpot() {
			price = r.SpotCt + p.SurchargeCt
		}
		res.CostEUR += (imp*dt/1000*price - exp*dt/1000*p.FeedInCt) / 100
		if soc >= 0.9 {
			res.HoursAbove90 += dt
		}
		if soc >= 0.99 {
			res.HoursFull += dt
		}
		socSum += soc

		lt := r.Time.In(p.Location)
		h := lt.Hour()
		if h >= 18 || h < morningHour {
			if soc <= b.MinSoC+0.001 {
				hitMin = true
			}
		}
		if h == morningHour && lt.Minute() == 0 {
			morning = append(morning, soc)
			if hitMin {
				res.ShortNights++
			}
			hitMin = false
		}
	}
	n := float64(len(s))
	if n > 0 {
		res.Days = n * series.SlotHours / 24
		res.MeanSoC = socSum / n
	}
	if len(morning) > 0 {
		var sum float64
		for _, v := range morning {
			sum += v
		}
		res.MorningSoCMean = sum / float64(len(morning))
		sorted := append([]float64(nil), morning...)
		sort.Float64s(sorted)
		res.MorningSoCP10 = sorted[len(sorted)/10]
	}
	if res.LoadKWh > 0 {
		res.SelfSufficiency = 1 - res.ImportKWh/res.LoadKWh
	}
	if res.PVKWh > 0 {
		res.SelfConsumption = 1 - res.ExportKWh/res.PVKWh
	}
	return res
}
