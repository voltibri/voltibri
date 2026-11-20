// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package planner decides how much the battery may charge in each 15-minute
// slot. Stage 0 strategy ("late charging"):
//
//  1. Find the next morning: the first slot after a night (>= 3 h without
//     PV surplus) from which PV covers the load again.
//  2. Forecast the night consumption between the end of today's surplus and
//     that morning (upper quantile, P80 by default).
//  3. Evening target SoC = morning buffer + night consumption + safety margin.
//  4. If today's (pessimistic) PV surplus comfortably exceeds the energy that
//     is still missing, charge only as much as needed – preferably in slots
//     with negative exchange prices, otherwise as late as possible. The rest
//     of the surplus is fed into the grid.
//  5. Otherwise charge immediately with all surplus.
//
// The planner is re-run every slot, so a cloudy afternoon automatically
// pulls charging forward (the surplus estimate shrinks and step 5 kicks in).
package planner

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/series"
)

// Unlimited marks a slot without charge restriction.
const Unlimited = -1.0

// Battery describes the battery state and limits.
type Battery struct {
	CapacityWh    float64
	SoC           float64 // 0..1
	MaxChargeW    float64
	MaxDischargeW float64
	ChargeEff     float64 // AC->battery, e.g. 0.95
	DischargeEff  float64 // battery->AC, e.g. 0.95
	MinSoC        float64 // never discharge below (e.g. 0.05)
	MaxSoC        float64 // usually 1.0
}

// Config tunes the strategy.
type Config struct {
	MorningBufferSoC    float64 `yaml:"morningBufferSoC"`    // SoC to keep when PV takes over again (e.g. 0.25)
	SafetyMarginSoC     float64 `yaml:"safetyMarginSoC"`     // extra reserve (e.g. 0.05)
	PVPessimism         float64 `yaml:"pvPessimism"`         // multiply PV forecast for safe planning (e.g. 0.8)
	Robustness          float64 `yaml:"robustness"`          // delay only if surplus >= need * robustness (e.g. 1.3)
	FillOnNegativePrice bool    `yaml:"fillOnNegativePrice"` // charge without limit while spot < 0
	MinNightHours       float64 `yaml:"minNightHours"`       // deficit length that counts as night (default 3)
}

// DefaultConfig returns the defaults discussed for a summer self-consumption setup.
func DefaultConfig() Config {
	// FillOnNegativePrice is off by default: negative price slots are still
	// preferred for the needed energy, but the battery is not filled to
	// 100 % just because the price is negative (battery health first).
	return Config{MorningBufferSoC: 0.25, SafetyMarginSoC: 0.05, PVPessimism: 0.8, Robustness: 1.3, FillOnNegativePrice: false, MinNightHours: 3}
}

// InputSlot is the PV forecast and price for one future slot.
type InputSlot struct {
	Start  time.Time
	PVW    float64 // forecast mean PV power
	SpotCt float64 // exchange price, NaN if unknown
}

// LoadForecaster is satisfied by forecast.LoadModel.
type LoadForecaster interface {
	Ready() bool
	Slot(t, cutoff time.Time, tempC float64) forecast.Estimate
	Energy(from, to, cutoff time.Time, tempC float64) forecast.Estimate
}

// Input bundles everything the planner needs.
type Input struct {
	Now     time.Time
	Battery Battery
	Slots   []InputSlot // contiguous, starting with the current slot
	TempC   float64     // forecast mean temperature for tomorrow (NaN if unknown)
}

// Mode describes the chosen strategy.
type Mode string

const (
	ModeDelay     Mode = "delay"     // charge late / in negative price slots
	ModeImmediate Mode = "immediate" // charge with all surplus now
	ModeNight     Mode = "night"     // no surplus before next morning
	ModeNoData    Mode = "no-data"   // forecasts unavailable, behave like default
)

// SlotPlan is the decision for one slot.
type SlotPlan struct {
	Start        time.Time `json:"start"`
	PVW          float64   `json:"pvW"`
	LoadW        float64   `json:"loadW"`
	SpotCt       *float64  `json:"spotCt,omitempty"`
	ChargeLimitW float64   `json:"chargeLimitW"` // Unlimited (-1) or watts
	ExpectedSoC  float64   `json:"expectedSoC"`
}

// Plan is the planner result.
type Plan struct {
	Created       time.Time  `json:"created"`
	Mode          Mode       `json:"mode"`
	Evening       *time.Time `json:"evening,omitempty"` // end of PV surplus
	Morning       *time.Time `json:"morning,omitempty"` // PV covers load again
	NightEnergyWh float64    `json:"nightEnergyWh"`
	TargetSoC     float64    `json:"targetSoC"`
	NeedWh        float64    `json:"needWh"`
	SurplusWh     float64    `json:"surplusWh"`
	Reasons       []string   `json:"reasons"`
	Slots         []SlotPlan `json:"slots"`
}

// CurrentLimitW returns the charge limit for the current slot.
func (p Plan) CurrentLimitW() float64 {
	if len(p.Slots) == 0 {
		return Unlimited
	}
	return p.Slots[0].ChargeLimitW
}

// Make computes a plan.
func Make(in Input, lf LoadForecaster, cfg Config) Plan {
	p := Plan{Created: in.Now, Mode: ModeNoData}
	n := len(in.Slots)
	b := in.Battery
	if n == 0 || lf == nil || !lf.Ready() {
		p.Reasons = append(p.Reasons, "Noch keine Prognose möglich (zu wenig Verlaufsdaten oder keine PV-Prognose): Batterie lädt normal.")
		p.Slots = unlimitedSlots(in)
		return p
	}

	pvP := make([]float64, n)  // pessimistic PV
	load := make([]float64, n) // P50 load
	for i, s := range in.Slots {
		pvP[i] = s.PVW * cfg.PVPessimism
		load[i] = lf.Slot(s.Start, in.Now, in.TempC).P50
	}

	// 1. find next morning after a night
	nightSlots := int(math.Max(1, cfg.MinNightHours*4))
	morning, evening := -1, -1
	run := 0
	for i := 0; i < n; i++ {
		if pvP[i] < load[i] {
			run++
			continue
		}
		if run >= nightSlots {
			morning = i
			evening = i - run - 1 // last surplus slot before the night (-1 if night already running)
			break
		}
		run = 0
	}
	if morning < 0 {
		p.Mode = ModeImmediate
		p.Reasons = append(p.Reasons, "Kein PV-Überschuss am nächsten Morgen erkennbar (Prognosehorizont oder Winter): Batterie lädt mit allem Überschuss.")
		p.Slots = buildSlots(in, pvP, load, cfg, nil)
		simulateSoC(&p, in, load)
		return p
	}
	mt := in.Slots[morning].Start
	p.Morning = &mt
	nightStart := 0
	if evening >= 0 {
		et := in.Slots[evening].Start.Add(series.Slot)
		p.Evening = &et
		nightStart = evening + 1
	}

	// 2./3. night consumption and target
	night := lf.Energy(in.Slots[nightStart].Start, mt, in.Now, in.TempC)
	p.NightEnergyWh = night.PQ
	target := cfg.MorningBufferSoC + night.PQ/(b.CapacityWh*b.DischargeEff) + cfg.SafetyMarginSoC
	target = math.Max(b.MinSoC, math.Min(b.MaxSoC, target))
	p.TargetSoC = target

	if evening < 0 {
		p.Mode = ModeNight
		p.Reasons = append(p.Reasons, fmt.Sprintf("Nacht: kein Überschuss bis %s. Erwarteter Verbrauch bis dahin %.1f kWh (vorsichtig geschätzt).",
			mt.Format("15:04"), night.PQ/1000))
		p.Slots = unlimitedSlots(in)
		fillForecast(&p, in, load)
		simulateSoC(&p, in, load)
		return p
	}

	// 4. surplus vs. need
	need := math.Max(0, (target-b.SoC)*b.CapacityWh/b.ChargeEff)
	p.NeedWh = need
	surplus := make([]float64, evening+1)
	var total float64
	for i := 0; i <= evening; i++ {
		surplus[i] = math.Min(math.Max(0, pvP[i]-load[i]), b.MaxChargeW) * series.SlotHours
		total += surplus[i]
	}
	p.SurplusWh = total

	if total < need*cfg.Robustness {
		p.Mode = ModeImmediate
		p.Reasons = append(p.Reasons, fmt.Sprintf(
			"Bis %s werden vorsichtig gerechnet nur %.1f kWh Überschuss erwartet, benötigt sind %.1f kWh (Ziel %.0f %% Ladestand): Batterie lädt sofort.",
			p.Evening.Format("15:04"), total/1000, need/1000, 100*target))
		p.Slots = buildSlots(in, pvP, load, cfg, nil)
		simulateSoC(&p, in, load)
		return p
	}

	// allocate the needed energy: negative price slots first, then latest
	alloc := make([]float64, evening+1)
	order := make([]int, 0, evening+1)
	for i := 0; i <= evening; i++ {
		order = append(order, i)
	}
	neg := func(i int) bool { return in.Slots[i].SpotCt < 0 } // NaN compares false
	sort.SliceStable(order, func(a, c int) bool {
		ia, ic := order[a], order[c]
		if neg(ia) != neg(ic) {
			return neg(ia)
		}
		if neg(ia) {
			return in.Slots[ia].SpotCt < in.Slots[ic].SpotCt
		}
		return ia > ic // later first
	})
	remaining := need * cfg.Robustness
	for _, i := range order {
		if remaining <= 0 {
			break
		}
		e := math.Min(surplus[i], remaining)
		alloc[i] = e
		remaining -= e
	}
	p.Mode = ModeDelay
	negCount := 0
	for i := 0; i <= evening; i++ {
		if neg(i) {
			negCount++
		}
	}
	p.Reasons = append(p.Reasons, fmt.Sprintf(
		"Genug Sonne: %.1f kWh Überschuss bis %s erwartet, benötigt %.1f kWh. Ziel am Abend %.0f %% (Nacht %.1f kWh bis %s + %.0f %% Puffer + %.0f %% Sicherheit).",
		total/1000, p.Evening.Format("15:04"), need/1000, 100*target, night.PQ/1000, mt.Format("15:04"), 100*cfg.MorningBufferSoC, 100*cfg.SafetyMarginSoC))
	if negCount > 0 {
		p.Reasons = append(p.Reasons, fmt.Sprintf("%d Viertelstunden mit negativem Börsenpreis: dort wird bevorzugt geladen (netzdienlich).", negCount))
	} else {
		p.Reasons = append(p.Reasons, "Geladen wird möglichst spät, damit die Batterie nicht stundenlang voll steht; der Rest wird eingespeist.")
	}
	p.Slots = buildSlots(in, pvP, load, cfg, alloc)
	simulateSoC(&p, in, load)
	return p
}

func unlimitedSlots(in Input) []SlotPlan {
	out := make([]SlotPlan, len(in.Slots))
	for i, s := range in.Slots {
		out[i] = SlotPlan{Start: s.Start, PVW: s.PVW, ChargeLimitW: Unlimited, SpotCt: optPrice(s.SpotCt)}
	}
	return out
}

func fillForecast(p *Plan, in Input, load []float64) {
	for i := range p.Slots {
		p.Slots[i].LoadW = load[i]
	}
}

func optPrice(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// buildSlots converts allocations into charge limits. alloc == nil means no
// restriction at all.
func buildSlots(in Input, pvP, load []float64, cfg Config, alloc []float64) []SlotPlan {
	out := make([]SlotPlan, len(in.Slots))
	for i, s := range in.Slots {
		sp := SlotPlan{Start: s.Start, PVW: s.PVW, LoadW: load[i], ChargeLimitW: Unlimited, SpotCt: optPrice(s.SpotCt)}
		if alloc != nil && i < len(alloc) {
			switch {
			case cfg.FillOnNegativePrice && s.SpotCt < 0:
				sp.ChargeLimitW = Unlimited
			default:
				sp.ChargeLimitW = math.Round(alloc[i] / series.SlotHours)
			}
		}
		out[i] = sp
	}
	return out
}

// simulateSoC fills ExpectedSoC using the nominal PV forecast and P50 load.
func simulateSoC(p *Plan, in Input, load []float64) {
	b := in.Battery
	soc := b.SoC
	for i := range p.Slots {
		s := &p.Slots[i]
		net := s.PVW - load[i]
		if net > 0 {
			c := math.Min(net, b.MaxChargeW)
			if s.ChargeLimitW >= 0 {
				c = math.Min(c, s.ChargeLimitW)
			}
			soc = math.Min(b.MaxSoC, soc+c*series.SlotHours*b.ChargeEff/b.CapacityWh)
		} else {
			d := math.Min(-net, b.MaxDischargeW)
			soc = math.Max(b.MinSoC, soc-d*series.SlotHours/b.DischargeEff/b.CapacityWh)
		}
		s.ExpectedSoC = math.Round(soc*1000) / 1000
	}
}
