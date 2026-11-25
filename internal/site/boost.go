// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/config"
)

// Boost: the home battery helps the surplus loads. A boosted load gets the
// PV surplus plus a share of the battery's discharge power, never power
// from the grid:
//
//	boost power = max. discharge power × (1 − reserve)
//
// The reserve stays with the battery to absorb small fluctuations (house
// load, clouds), so they do not end up in the grid. The battery gives
// boost power down to the stop SoC (global battery setting).
//
// A load is boosted by hand (charge mode "boost" of a vehicle, mode
// "boost" of a heating rod: before all other loads, e.g. a car shortly
// before it leaves) or by the automatic boost: from the start SoC down to
// the stop SoC every load in a surplus mode gets the boost power, in the
// order of their priority. With start 95 % and stop 85 % a heating rod
// that can only be switched in coarse steps runs in a saw-tooth on days
// with moderate but steady PV: the battery fills up from the surplus, the
// boost empties it into the rod, and so on.
//
// The boost does not need control of the battery: a battery that keeps the
// grid at zero covers the extra load by itself. Loads the battery must not
// supply (battery use "never", excluded charge modes) get no boost.

const (
	boostMax         = 12 * time.Hour // a boost ends after this time at the latest
	boostKey         = "loadBoost"    // cache key of a running automatic boost
	boostStopDefault = 0.85
	boostReserveDef  = 0.2
)

// Boost is a running automatic boost.
type Boost struct {
	Since   time.Time `json:"since"`
	Auto    bool      `json:"auto,omitempty"` // always true (kept for stored boosts)
	StopSoC float64   `json:"stopSoc"`        // ends here
	PowerW  float64   `json:"powerW"`         // battery power released for the loads now
}

// boostStop is the stop SoC (default when not set).
func (bs BatterySettings) boostStop() float64 {
	if bs.BoostStopSoC > 0 {
		return bs.BoostStopSoC
	}
	return boostStopDefault
}

// checkBoost validates the boost settings.
func checkBoost(bs BatterySettings, possible bool) error {
	if !possible {
		return nil
	}
	stop := bs.boostStop()
	switch {
	case bs.BoostReserve < 0 || bs.BoostReserve > 0.5:
		return errors.New("Die Boost-Reserve muss zwischen 0 und 50 % liegen.")
	case stop < 0.1 || stop > 0.95:
		return errors.New("Der Boost muss zwischen 10 und 95 % Ladestand enden.")
	case bs.MinSoC > 0 && stop < bs.MinSoC+0.05:
		return errors.New("Der Boost muss mindestens 5 % über dem unteren Ladeziel enden.")
	case bs.BoostStartSoC != 0 && (bs.BoostStartSoC > 1 || bs.BoostStartSoC < stop+0.05):
		return errors.New("Der automatische Boost muss mindestens 5 % über dem Stopp-Ladestand starten.")
	}
	return nil
}

// boostMaxW is the discharge power a boost can release before the reserve:
// the configured maximum, narrowed by the max. discharge setting.
func (s *Site) boostMaxW(bs BatterySettings) float64 {
	w := s.cfg.Battery.MaxDischargeW
	if bs.MaxDischargeW > 0 && (w <= 0 || bs.MaxDischargeW < w) {
		w = bs.MaxDischargeW
	}
	return w
}

// boostPossible reports whether a boost makes sense: a battery with a known
// discharge power and at least one load that uses surplus.
func (s *Site) boostPossible() bool {
	if s.bat == nil || s.cfg.Battery.MaxDischargeW <= 0 {
		return false
	}
	return (s.charge != nil && s.charge.HasLoadpoints()) || (s.flex != nil && len(s.flex.loads) > 0)
}

// boostPowerW is the battery power a boost releases with the settings.
func (s *Site) boostPowerW(bs BatterySettings) float64 {
	return math.Round(s.boostMaxW(bs)*(1-bs.BoostReserve)/50) * 50
}

func (s *Site) beginBoost(auto bool, stop float64, bs BatterySettings) error {
	b := &Boost{Since: time.Now(), Auto: auto, StopSoC: stop, PowerW: s.boostPowerW(bs)}
	s.mu.Lock()
	s.boost, s.boostHold = b, false
	s.state.Boost = b // shown right away, not only after the next measurement
	s.mu.Unlock()
	slog.Info("boost started", "auto", auto, "stopSoC", stop, "powerW", b.PowerW)
	return s.saveBattery(boostKey, b)
}

// StopBoost ends a running automatic boost. It does not start again until
// the SoC has dropped below the start SoC.
func (s *Site) StopBoost() error { return s.endBoost("stopped", true) }

// endBoost ends a running boost; hold keeps the automatic start off until
// the SoC has dropped below the start SoC.
func (s *Site) endBoost(why string, hold bool) error {
	s.mu.Lock()
	had := s.boost != nil
	s.boost, s.state.Boost = nil, nil
	if had && hold {
		s.boostHold = true
	}
	s.mu.Unlock()
	if !had {
		return nil
	}
	slog.Info("boost ended", "reason", why)
	return s.saveBattery(boostKey, (*Boost)(nil))
}

// updateBoost starts the automatic boost, ends it at the stop SoC and
// returns the boost power of the battery (0 = at the stop SoC: no boost,
// also not by hand) and whether the automatic boost runs.
func (s *Site) updateBoost(soc float64, now time.Time) (float64, bool) {
	s.mu.RLock()
	b, bs, hold, empty := s.boost, s.batSet, s.boostHold, s.boostEmpty
	s.mu.RUnlock()
	if !s.boostPossible() {
		if b != nil {
			s.endBoost("not possible any more", false)
		}
		return 0, false
	}
	start, stop := bs.BoostStartSoC, bs.boostStop()
	if !math.IsNaN(soc) { // unknown SoC: keep what runs
		switch {
		case soc <= stop:
			empty = true
		case soc > stop+socHyst:
			empty = false
		}
		if hold && (start == 0 || soc < start) {
			hold = false
		}
		s.mu.Lock()
		s.boostEmpty, s.boostHold = empty, hold
		s.mu.Unlock()
		if b == nil && start > 0 && soc >= start && !hold && !empty {
			if err := s.beginBoost(true, stop, bs); err != nil {
				slog.Error("storing the boost failed", "err", err)
			}
			s.mu.RLock()
			b = s.boost
			s.mu.RUnlock()
		}
	}
	w := s.boostPowerW(bs)
	if empty {
		w = 0
	}
	if b == nil {
		return w, false
	}
	switch {
	case empty:
		s.endBoost("stop SoC reached", false)
		return 0, false
	case now.Sub(b.Since) >= boostMax:
		s.endBoost("expired", true)
		return w, false
	}
	if w != b.PowerW || stop != b.StopSoC { // the settings changed while it runs
		nb := *b
		nb.PowerW, nb.StopSoC = w, stop
		s.mu.Lock()
		s.boost = &nb
		s.mu.Unlock()
	}
	return w, true
}

// noBattery reports whether the battery must not supply the device.
func noBattery(cfg *config.Config, id string) bool {
	for _, d := range cfg.Devices {
		if d.ID == id {
			return d.BatteryUse == config.BatteryNever
		}
	}
	return false
}
