// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"math"
	"math/rand"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/series"
)

// SynthConfig describes a synthetic household for demos and tests.
type SynthConfig struct {
	KWp       float64 // PV peak power
	Latitude  float64
	Longitude float64
	HeatPump  bool
	Seed      int64
}

// DefaultSynth is a German single family home with ~10 kWp and a heat pump.
func DefaultSynth() SynthConfig {
	return SynthConfig{KWp: 10, Latitude: 51, Longitude: 10, HeatPump: true, Seed: 1}
}

// Synthesize generates plausible data (PV, load, spot price, temperature)
// for the given number of days. It is not meant to be accurate, only to
// exercise forecaster and planner with realistic patterns: seasons, weather
// streaks, workday/weekend behaviour, heat pump demand, negative prices on
// sunny middays.
func Synthesize(start time.Time, days int, c SynthConfig, cal *calendar.Calendar) series.Series {
	rng := rand.New(rand.NewSource(c.Seed))
	loc := start.Location()
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	var out series.Series

	clear := 0.6 // weather persists over days
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		doy := float64(day.YearDay())
		summer := 0.5 - 0.5*math.Cos(2*math.Pi*(doy-10)/365) // 0 in Jan, 1 in Jul
		// weather: AR(1) around a seasonal mean
		mean := 0.45 + 0.3*summer
		clear = 0.55*clear + 0.45*mean + rng.NormFloat64()*0.22
		clear = math.Max(0.08, math.Min(1, clear))
		tempDay := 10 - 10*math.Cos(2*math.Pi*(doy-20)/365) + rng.NormFloat64()*2.5 + 3*(clear-0.5)*summer
		dayFactor := 0.85 + 0.35*rng.Float64()
		typ := cal.DayType(day.Add(12 * time.Hour))
		hddW := 0.0
		if c.HeatPump {
			hddW = math.Max(0, 15-tempDay) * 110
		}
		cloud := 0.0
		for t := day; t.Before(day.AddDate(0, 0, 1)); t = t.Add(series.Slot) {
			lh := float64(t.Hour()) + float64(t.Minute())/60

			// --- PV ---
			utcH := float64(t.UTC().Hour()) + float64(t.UTC().Minute())/60 + 7.5/60
			solarH := utcH + c.Longitude/15
			decl := 23.44 * math.Pi / 180 * math.Sin(2*math.Pi*(284+doy)/365)
			lat := c.Latitude * math.Pi / 180
			ha := (solarH - 12) * 15 * math.Pi / 180
			sinEl := math.Sin(lat)*math.Sin(decl) + math.Cos(lat)*math.Cos(decl)*math.Cos(ha)
			pv := 0.0
			if sinEl > 0 {
				cloud = 0.7*cloud + 0.3*rng.NormFloat64()
				k := clear * (1 - (1-clear)*0.6*math.Abs(cloud))
				pv = c.KWp * 1000 * 0.82 * math.Pow(sinEl, 1.15) * math.Max(0.05, k)
			}

			// --- load ---
			load := 230 + 40*rng.Float64()
			switch typ {
			case calendar.Workday:
				load += bump(lh, 6.5, 8, 700) + bump(lh, 12, 13, 250) + bump(lh, 17, 22, 900)
			default:
				load += bump(lh, 8, 11, 600) + bump(lh, 12, 14, 700) + bump(lh, 17, 22, 950)
			}
			load *= dayFactor * (0.85 + 0.3*rng.Float64())
			if c.HeatPump {
				night := 1.0
				if lh < 7 || lh > 21 {
					night = 1.25 // colder at night
				}
				load += hddW * night * (0.8 + 0.4*rng.Float64())
				load += bump(lh, 13, 13.5, 2400) // domestic hot water
			}

			// --- temperature ---
			temp := tempDay + 4*math.Sin(2*math.Pi*(lh-9)/24)

			// --- spot price (ct/kWh) ---
			solarDip := 0.0
			if sinEl > 0 {
				solarDip = 14 * clear * summer * math.Pow(sinEl, 1.5)
			}
			spot := 9 + bump(lh, 7, 9, 3) + bump(lh, 17.5, 20.5, 7) - solarDip + rng.NormFloat64()*1.2
			if typ != calendar.Workday {
				spot -= 3
			}

			out = append(out, series.Record{
				Time:   t,
				PVW:    math.Round(pv),
				LoadW:  math.Round(load),
				SpotCt: math.Round(spot*100) / 100,
				TempC:  math.Round(temp*10) / 10,
			})
		}
	}
	return out
}

// bump returns w inside [from, to) hours with soft edges, else 0.
func bump(h, from, to, w float64) float64 {
	if h < from-0.25 || h >= to+0.25 {
		return 0
	}
	if h < from || h >= to {
		return w / 2
	}
	return w
}
