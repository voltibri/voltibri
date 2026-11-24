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

	"github.com/voltibri/voltibri/sdk"
)

// Battery settings changed at runtime (stored directly like the charging
// settings, no configuration version): an upper and a lower charge target,
// a maximum discharge power and the boost. Not every battery supports them:
// what voltibri can do follows from the capabilities of the driver.
//
//	upper target   charge limit 0 at the target   BatteryController
//	lower target   discharge limit 0 at the target BatteryLimiter
//	max. discharge discharge limit                 BatteryLimiter
//	boost          charge (from the grid) now      BatteryForceCharger
//
// The targets have a small hysteresis so the battery does not toggle around
// them.

const (
	socHyst  = 0.02           // hysteresis around the targets
	boostMax = 12 * time.Hour // a boost ends after this time at the latest
	batNS    = "battery"      // cache namespace
	setKey   = "settings"     // cache keys
	boostKey = "boost"
)

// BatterySettings are the runtime settings of the home battery. Zero means
// "not set".
type BatterySettings struct {
	MaxSoC        float64 `json:"maxSoc,omitempty"`        // upper charge target (fraction)
	MinSoC        float64 `json:"minSoc,omitempty"`        // lower charge target (fraction)
	MaxDischargeW float64 `json:"maxDischargeW,omitempty"` // maximum discharge power
}

// Boost charges the battery now – from the grid if needed – up to a target.
type Boost struct {
	TargetSoC float64   `json:"targetSoc"`
	Since     time.Time `json:"since"`
}

// BatteryFeatures says which settings the battery supports.
type BatteryFeatures struct {
	MaxSoC       bool `json:"maxSoc"`
	MinSoC       bool `json:"minSoc"`
	MaxDischarge bool `json:"maxDischarge"`
	Boost        bool `json:"boost"`
}

// Any reports whether at least one setting is supported.
func (f BatteryFeatures) Any() bool { return f.MaxSoC || f.MinSoC || f.MaxDischarge || f.Boost }

// BatteryInfo is what the web page needs for the battery settings.
type BatteryInfo struct {
	HasBattery bool            `json:"hasBattery"`
	Features   BatteryFeatures `json:"features"`
	Settings   BatterySettings `json:"settings"`
	Boost      *Boost          `json:"boost,omitempty"`
	MaxChargeW float64         `json:"maxChargeW"`
	MaxDisW    float64         `json:"maxDischargeW"`  // configured maximum (for the slider)
	Note       string          `json:"note,omitempty"` // why nothing can be set
}

// batteryFeatures derives the features from the battery driver.
func (s *Site) batteryFeatures() BatteryFeatures {
	if s.batCtl == nil {
		return BatteryFeatures{}
	}
	_, lim := s.batCtl.(sdk.BatteryLimiter)
	return BatteryFeatures{MaxSoC: true, MinSoC: lim, MaxDischarge: lim, Boost: s.forceCh != nil}
}

// BatteryInfo returns features, settings and a running boost.
func (s *Site) BatteryInfo() BatteryInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	in := BatteryInfo{HasBattery: s.bat != nil, Features: s.batteryFeatures(), Settings: s.batSet, Boost: s.boost,
		MaxChargeW: s.cfg.Battery.MaxChargeW, MaxDisW: s.cfg.Battery.MaxDischargeW}
	switch {
	case s.bat == nil:
		in.Note = "Keine Hausbatterie eingerichtet."
	case s.batCtl == nil:
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
	}
	if bs.MaxSoC >= 1 {
		bs.MaxSoC = 0 // 100 % = no upper target
	}
	bs.MaxDischargeW = math.Round(bs.MaxDischargeW/50) * 50
	s.mu.Lock()
	s.batSet = bs
	s.mu.Unlock()
	slog.Info("battery settings", "maxSoC", bs.MaxSoC, "minSoC", bs.MinSoC, "maxDischargeW", bs.MaxDischargeW)
	return s.saveBattery(setKey, bs)
}

// StartBoost charges the battery up to target (fraction) now.
func (s *Site) StartBoost(target float64) error {
	if s.forceCh == nil {
		return errors.New("Diese Batterie kann nicht aus dem Netz geladen werden (kein Boost möglich).")
	}
	if target < 0.1 || target > 1 {
		return errors.New("Das Boost-Ziel muss zwischen 10 und 100 % liegen.")
	}
	s.mu.RLock()
	soc := s.state.SoC
	s.mu.RUnlock()
	if soc >= target {
		return fmt.Errorf("Die Batterie hat das Ziel von %.0f %% schon erreicht.", target*100)
	}
	b := &Boost{TargetSoC: target, Since: time.Now()}
	s.mu.Lock()
	s.boost = b
	s.mu.Unlock()
	slog.Info("battery boost started", "targetSoC", target)
	return s.saveBattery(boostKey, b)
}

// StopBoost ends a running boost.
func (s *Site) StopBoost() error { return s.endBoost("stopped") }

func (s *Site) endBoost(why string) error {
	s.mu.Lock()
	had := s.boost != nil
	s.boost = nil
	s.mu.Unlock()
	if had {
		slog.Info("battery boost ended", "reason", why)
	}
	return s.saveBattery(boostKey, (*Boost)(nil))
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

// loadBattery restores settings and a running boost (the site is rebuilt
// on every configuration change).
func (s *Site) loadBattery() {
	if s.batCache == nil {
		return
	}
	f := s.batteryFeatures()
	if b, _, ok := s.batCache.Get(setKey); ok {
		var bs BatterySettings
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
	if b, _, ok := s.batCache.Get(boostKey); ok && f.Boost {
		var bo *Boost
		if json.Unmarshal(b, &bo) == nil && bo != nil && time.Since(bo.Since) < boostMax {
			s.boost = bo
		}
	}
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
// (forceBattery policy) or a boost. A reached or expired boost ends here.
func (s *Site) forceWant(soc, guestW float64, planLimitW float64) float64 {
	want := 0.0
	if guestW > 50 && soc < 0.99 && planLimitW != 0 {
		want = math.Min(guestW, s.cfg.Battery.MaxChargeW)
	}
	s.mu.RLock()
	b := s.boost
	s.mu.RUnlock()
	if b == nil {
		return want
	}
	switch {
	case !math.IsNaN(soc) && soc >= b.TargetSoC:
		s.endBoost("target reached")
	case time.Since(b.Since) >= boostMax:
		s.endBoost("expired")
	default:
		want = s.cfg.Battery.MaxChargeW
	}
	return want
}
