// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/sdk"
)

// Battery settings changed at runtime (stored directly like the charging
// settings, no configuration version): an upper and a lower charge target,
// a maximum discharge power, the force charging and the boost settings.
// Not every battery supports them: what voltibri can do follows from the
// capabilities of the driver.
//
//	upper target   charge limit 0 at the target   BatteryController
//	lower target   discharge limit 0 at the target BatteryLimiter
//	max. discharge discharge limit                 BatteryLimiter
//	force charging charge (from the grid) now      BatteryForceCharger
//	boost          battery helps the surplus loads any battery (boost.go)
//
// The targets have a small hysteresis so the battery does not toggle around
// them.

const (
	socHyst  = 0.02           // hysteresis around the targets
	forceMax = 12 * time.Hour // a force charging ends after this time at the latest
	batNS    = "battery"      // cache namespace
	setKey   = "settings"     // cache keys
	forceKey = "forceCharge"  // (the old key "boost" held a force charging: not read any more)
)

// BatterySettings are the runtime settings of the home battery. Zero means
// "not set".
type BatterySettings struct {
	// charge targets: automatic from the forecasts or by hand (limits.go)
	LimitMode       string     `json:"limitMode,omitempty"`   // auto, manual ("" = derived)
	ManualFor       string     `json:"manualFor,omitempty"`   // morning (default), always
	ManualUntil     *time.Time `json:"manualUntil,omitempty"` // by hand until (set by voltibri)
	AutoFloorSunny  float64    `json:"autoFloorSunny,omitempty"`
	AutoFloorCloudy float64    `json:"autoFloorCloudy,omitempty"`
	AutoMaxSoC      float64    `json:"autoMaxSoc,omitempty"`    // upper target in auto: 0 = what the night needs, else fixed
	MaxSoC          float64    `json:"maxSoc,omitempty"`        // upper charge target by hand (fraction)
	MinSoC          float64    `json:"minSoc,omitempty"`        // lower charge target by hand (fraction)
	MaxDischargeW   float64    `json:"maxDischargeW,omitempty"` // maximum discharge power
	// PrioritySoC: below it the battery gets the PV surplus before the
	// surplus loads (0 = never, 1 = always; nil = charging.batteryPrioritySoC
	// of the configuration)
	PrioritySoC *float64 `json:"prioritySoc,omitempty"`

	// boost (see boost.go)
	BoostReserve  float64 `json:"boostReserve"`            // share of the discharge power kept back for fluctuations
	BoostStopSoC  float64 `json:"boostStopSoc,omitempty"`  // the boost ends here (0 = default)
	BoostStartSoC float64 `json:"boostStartSoc,omitempty"` // the boost starts by itself here (0 = never)
}

// ForceCharge charges the battery now – from the grid if needed – up to a target.
type ForceCharge struct {
	TargetSoC float64   `json:"targetSoc"`
	Since     time.Time `json:"since"`
}

// BatteryFeatures says which settings the battery supports.
type BatteryFeatures struct {
	MaxSoC       bool `json:"maxSoc"`
	MinSoC       bool `json:"minSoc"`
	MaxDischarge bool `json:"maxDischarge"`
	ForceCharge  bool `json:"forceCharge"`
	Boost        bool `json:"boost"`    // the battery can help the surplus loads
	Priority     bool `json:"priority"` // priority of the battery before the surplus loads
}

// Any reports whether at least one setting is supported.
func (f BatteryFeatures) Any() bool {
	return f.MaxSoC || f.MinSoC || f.MaxDischarge || f.ForceCharge || f.Boost || f.Priority
}

// BatteryInfo is what the web page needs for the battery settings.
type BatteryInfo struct {
	HasBattery bool            `json:"hasBattery"`
	Features   BatteryFeatures `json:"features"`
	Settings   BatterySettings `json:"settings"`
	Force      *ForceCharge    `json:"forceCharge,omitempty"`
	Boost      *Boost          `json:"boost,omitempty"`
	MaxChargeW float64         `json:"maxChargeW"`
	MaxDisW    float64         `json:"maxDischargeW"` // configured maximum (for the slider)
	BoostMaxW  float64         `json:"boostMaxW"`     // battery power a boost can release (before the reserve)
	Auto       planner.Targets `json:"auto"`          // automatic targets now
	MaxSoCNow  float64         `json:"maxSocNow"`     // targets that apply now (0 = none)
	MinSoCNow  float64         `json:"minSocNow"`
	Note       string          `json:"note,omitempty"` // why nothing can be set
}

// batteryFeatures derives the features from the battery driver. The boost
// needs no control of the battery: it steers the loads.
func (s *Site) batteryFeatures() BatteryFeatures {
	f := BatteryFeatures{Boost: s.boostPossible(), Priority: s.bat != nil}
	if s.batCtl == nil {
		return f
	}
	_, lim := s.batCtl.(sdk.BatteryLimiter)
	f.MaxSoC, f.MinSoC, f.MaxDischarge, f.ForceCharge = true, lim, lim, s.forceCh != nil
	return f
}

// BatteryInfo returns features, settings, a running force charging and boost.
func (s *Site) BatteryInfo() BatteryInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	in := BatteryInfo{HasBattery: s.bat != nil, Features: s.batteryFeatures(), Settings: s.batSet, Force: s.force, Boost: s.boost,
		MaxChargeW: s.cfg.Battery.MaxChargeW, MaxDisW: s.cfg.Battery.MaxDischargeW, BoostMaxW: s.boostMaxW(s.batSet)}
	in.Settings.BoostStopSoC = s.batSet.boostStop()
	p := s.prioritySoC(s.batSet)
	in.Settings.PrioritySoC = &p
	in.Settings.LimitMode = s.batSet.limitMode()
	ac := s.batSet.autoConfig()
	in.Settings.AutoFloorSunny, in.Settings.AutoFloorCloudy, in.Settings.AutoMaxSoC = ac.FloorSunny, ac.FloorCloudy, ac.MaxSoC
	in.Auto = s.auto
	if in.Auto.Reason == "" {
		in.Auto.Reason = "Wird beim nächsten Planlauf berechnet (alle 15 Minuten)."
	}
	eff := s.effective(s.batSet, s.auto)
	in.MaxSoCNow, in.MinSoCNow = eff.MaxSoC, eff.MinSoC
	switch {
	case s.bat == nil:
		in.Note = "Keine Hausbatterie eingerichtet."
	case s.batCtl == nil && !in.Features.Boost:
		in.Note = "voltibri steuert die Batterie nicht (Steuerung im Gerät nicht auf „voltibri steuert“ gestellt oder vom Treiber nicht unterstützt)."
	}
	return in
}

// SetBatterySettings validates and stores the settings; they take effect
// with the next measurement.
func (s *Site) SetBatterySettings(bs BatterySettings) error {
	f := s.batteryFeatures()
	switch {
	case bs.MaxSoC != 0 && !f.MaxSoC:
		return errors.New("Diese Batterie unterstützt kein oberes Ladeziel.")
	case (bs.MinSoC != 0) && !f.MinSoC:
		return errors.New("Diese Batterie unterstützt kein unteres Ladeziel.")
	case bs.MaxDischargeW != 0 && !f.MaxDischarge:
		return errors.New("Diese Batterie unterstützt keine Begrenzung der Entladeleistung.")
	case bs.MaxSoC < 0 || bs.MaxSoC > 1 || bs.MinSoC < 0 || bs.MinSoC > 1:
		return errors.New("Ladeziele müssen zwischen 0 und 100 % liegen.")
	case bs.MaxSoC != 0 && bs.MaxSoC < 0.2:
		return errors.New("Das obere Ladeziel muss mindestens 20 % sein.")
	case bs.MinSoC > 0.9:
		return errors.New("Das untere Ladeziel darf höchstens 90 % sein.")
	case bs.MaxSoC != 0 && bs.MinSoC != 0 && bs.MinSoC > bs.MaxSoC-0.1:
		return errors.New("Zwischen unterem und oberem Ladeziel müssen mindestens 10 % liegen.")
	case bs.MaxDischargeW < 0:
		return errors.New("Die Entladeleistung darf nicht negativ sein.")
	case bs.PrioritySoC != nil && (*bs.PrioritySoC < 0 || *bs.PrioritySoC > 1):
		return errors.New("Der Vorrang der Batterie muss zwischen 0 und 100 % liegen.")
	}
	if err := checkBoost(bs, f.Boost); err != nil {
		return err
	}
	if err := checkLimits(bs); err != nil {
		return err
	}
	s.mu.RLock()
	prev := s.batSet
	s.mu.RUnlock()
	if bs.LimitMode == "" {
		bs.LimitMode = prev.limitMode()
	}
	s.manualUntil(&bs, prev, time.Now())
	if bs.PrioritySoC == nil || !f.Priority { // not sent: keep it
		s.mu.RLock()
		bs.PrioritySoC = s.batSet.PrioritySoC
		s.mu.RUnlock()
	}
	if !f.Boost { // keep what was set for a battery that can boost again later
		s.mu.RLock()
		bs.BoostReserve, bs.BoostStopSoC, bs.BoostStartSoC = s.batSet.BoostReserve, s.batSet.BoostStopSoC, s.batSet.BoostStartSoC
		s.mu.RUnlock()
	}
	if bs.MaxSoC >= 1 {
		bs.MaxSoC = 0 // 100 % = no upper target
	}
	bs.MaxDischargeW = math.Round(bs.MaxDischargeW/50) * 50
	s.mu.Lock()
	s.batSet = bs
	s.mu.Unlock()
	slog.Info("battery settings", "limits", bs.limitMode(), "manualUntil", bs.ManualUntil, "maxSoC", bs.MaxSoC, "minSoC", bs.MinSoC, "maxDischargeW", bs.MaxDischargeW,
		"prioritySoC", s.prioritySoC(bs), "boostReserve", bs.BoostReserve, "boostStopSoC", bs.BoostStopSoC, "boostStartSoC", bs.BoostStartSoC)
	return s.saveBattery(setKey, bs)
}

// StartForceCharge charges the battery up to target (fraction) now, from
// the grid if needed. A running boost ends: both at once make no sense.
func (s *Site) StartForceCharge(target float64) error {
	if s.forceCh == nil {
		return errors.New("Diese Batterie kann nicht aus dem Netz geladen werden (keine Zwangsladung möglich).")
	}
	if target < 0.1 || target > 1 {
		return errors.New("Das Ziel der Zwangsladung muss zwischen 10 und 100 % liegen.")
	}
	s.mu.RLock()
	soc := s.state.SoC
	s.mu.RUnlock()
	if soc >= target {
		return fmt.Errorf("Die Batterie hat das Ziel von %.0f %% schon erreicht.", target*100)
	}
	if err := s.endBoost("force charging started", true); err != nil {
		return err
	}
	fc := &ForceCharge{TargetSoC: target, Since: time.Now()}
	s.mu.Lock()
	s.force, s.state.ForceCharge = fc, fc
	s.mu.Unlock()
	slog.Info("battery force charging started", "targetSoC", target)
	return s.saveBattery(forceKey, fc)
}

// StopForceCharge ends a running force charging.
func (s *Site) StopForceCharge() error { return s.endForce("stopped") }

func (s *Site) endForce(why string) error {
	s.mu.Lock()
	had := s.force != nil
	s.force, s.state.ForceCharge = nil, nil
	s.mu.Unlock()
	if had {
		slog.Info("battery force charging ended", "reason", why)
	}
	return s.saveBattery(forceKey, (*ForceCharge)(nil))
}

func (s *Site) saveBattery(key string, v any) error {
	if s.batCache == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.batCache.Put(key, b)
}

// loadBattery restores settings, a running force charging and boost (the
// site is rebuilt on every configuration change).
func (s *Site) loadBattery() {
	if s.batCache == nil {
		return
	}
	f := s.batteryFeatures()
	if b, _, ok := s.batCache.Get(setKey); ok {
		bs := s.batSet // defaults for what was never stored
		if json.Unmarshal(b, &bs) == nil {
			// drop what the (possibly new) battery does not support
			if !f.MaxSoC {
				bs.MaxSoC = 0
			}
			if !f.MinSoC {
				bs.MinSoC = 0
			}
			if !f.MaxDischarge {
				bs.MaxDischargeW = 0
			}
			s.batSet = bs
		}
	}
	if b, _, ok := s.batCache.Get(forceKey); ok && f.ForceCharge {
		var fc *ForceCharge
		if json.Unmarshal(b, &fc) == nil && fc != nil && time.Since(fc.Since) < forceMax {
			s.force = fc
		}
	}
	if b, _, ok := s.batCache.Get(boostKey); ok && f.Boost {
		var bo *Boost
		if json.Unmarshal(b, &bo) == nil && bo != nil && time.Since(bo.Since) < boostMax {
			s.boost = bo
		}
	}
}

// prioritySoC is the SoC below which the battery gets the PV surplus first.
func (s *Site) prioritySoC(bs BatterySettings) float64 {
	if bs.PrioritySoC != nil {
		return *bs.PrioritySoC
	}
	if p := s.cfg.Charging.BatteryPrioritySoC; p > 0 {
		return p
	}
	return 0.5
}

// socGuard enforces the charge targets with hysteresis.
type socGuard struct {
	full  bool // at the upper target: no charging
	empty bool // at the lower target: no discharging
}

// apply narrows the charge and discharge limit (-1 = none) by the settings.
func (g *socGuard) apply(bs BatterySettings, soc, charge, discharge float64) (float64, float64) {
	// an unknown SoC (NaN) matches no case: the last decision stays
	if bs.MaxSoC > 0 && bs.MaxSoC < 1 {
		switch {
		case soc >= bs.MaxSoC:
			g.full = true
		case soc < bs.MaxSoC-socHyst:
			g.full = false
		}
	} else {
		g.full = false
	}
	if bs.MinSoC > 0 {
		switch {
		case soc <= bs.MinSoC:
			g.empty = true
		case soc > bs.MinSoC+socHyst:
			g.empty = false
		}
	} else {
		g.empty = false
	}
	if g.full {
		charge = 0
	}
	if bs.MaxDischargeW > 0 && (discharge < 0 || discharge > bs.MaxDischargeW) {
		discharge = bs.MaxDischargeW
	}
	if g.empty {
		discharge = 0
	}
	return charge, discharge
}

// forceWant is the force charging power wanted now: for a guest
// (forceBattery policy, not while a boost empties the battery on purpose)
// or a force charging. A reached or expired force charging ends here.
func (s *Site) forceWant(soc, guestW float64, planLimitW float64) float64 {
	want := 0.0
	s.mu.RLock()
	fc, boosting := s.force, s.boost != nil
	s.mu.RUnlock()
	if guestW > 50 && soc < 0.99 && planLimitW != 0 && !boosting {
		want = math.Min(guestW, s.cfg.Battery.MaxChargeW)
	}
	if fc == nil {
		return want
	}
	switch {
	case !math.IsNaN(soc) && soc >= fc.TargetSoC:
		s.endForce("target reached")
	case time.Since(fc.Since) >= forceMax:
		s.endForce("expired")
	default:
		want = s.cfg.Battery.MaxChargeW
	}
	return want
}
