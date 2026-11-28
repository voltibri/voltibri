// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/soc"
)

// State of charge of the home battery when the battery does not tell it.
//
// Every good reading is kept as the base; while the reading fails (error
// or NaN) the state of charge is the last reading plus the energy charged
// and discharged since (battery power with the charge and discharge
// efficiencies). A value entered by hand overrides the reading until the
// battery reports something else than when it was entered (the reading
// works again); it survives a restart.

const batSoCKey = "soc"

var errNoBattery = errors.New("Keine Hausbatterie konfiguriert.")

// SoC sources of the home battery (State.SoCSource).
const (
	BatSoCRead      = "api"       // read from the battery
	BatSoCManual    = "manual"    // entered by hand (hochgerechnet)
	BatSoCEstimated = "estimated" // the reading fails: last reading plus energy since
)

// batterySoC returns the effective state of charge and its source; read is
// the reading (NaN or readErr: none), batteryW the battery power (+
// discharge, − charge).
func (s *Site) batterySoC(read float64, readErr error, batteryW float64, now time.Time) (float64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.cfg.Battery
	e := s.batEst
	if e != nil && !s.batTick.IsZero() {
		dt := math.Min(now.Sub(s.batTick).Hours(), 1.0/60)
		if dt > 0 && !math.IsNaN(batteryW) {
			if batteryW < 0 {
				e.Charge(-batteryW*dt, b.ChargeEff, b.CapacityWh)
			} else {
				e.Discharge(batteryW*dt, b.DischargeEff, b.CapacityWh)
			}
		}
	}
	s.batTick = now
	valid := readErr == nil && !math.IsNaN(read)
	switch {
	case e != nil && e.Source == soc.SourceManual:
		if valid && e.APIChanged(read) {
			slog.Info("battery state of charge read again, the value by hand ends", "soc", read, "was", e.Value(b.CapacityWh))
			s.batEst = soc.New(read, now, soc.SourceAPI, read)
			s.saveBatSoC(nil, now)
			return read, BatSoCRead
		}
		// every 5 minutes, and after a clock jump back (saved "in the future")
		if s.batSaved.IsZero() || now.Sub(s.batSaved) >= 5*time.Minute || now.Before(s.batSaved) {
			s.saveBatSoC(e, now)
		}
		return e.Value(b.CapacityWh), BatSoCManual
	case valid:
		s.batEst = soc.New(read, now, soc.SourceAPI, read)
		return read, BatSoCRead
	case e != nil:
		return e.Value(b.CapacityWh), BatSoCEstimated
	}
	return math.NaN(), ""
}

// SetBatterySoC sets the state of charge of the home battery by hand (nil:
// back to the reading).
func (s *Site) SetBatterySoC(v *float64) error {
	if s.bat == nil {
		return errNoBattery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if v == nil {
		if s.batEst != nil && s.batEst.Source == soc.SourceManual {
			s.batEst = nil
		}
		s.saveBatSoC(nil, now)
		return nil
	}
	if err := soc.Validate(*v); err != nil {
		return err
	}
	ref := math.NaN()
	if s.state.SoCSource == BatSoCRead { // the battery reports this now; a stale value must not end the entry
		ref = s.state.SoC
	} else if s.batEst != nil && s.batEst.APIRef != nil {
		ref = *s.batEst.APIRef
	}
	s.batEst = soc.New(*v, now, soc.SourceManual, ref)
	s.saveBatSoC(s.batEst, now)
	s.state.SoC, s.state.SoCSource = *v, BatSoCManual
	slog.Info("battery state of charge by hand", "soc", *v)
	return nil
}

func (s *Site) saveBatSoC(e *soc.Estimate, now time.Time) {
	s.batSaved = now
	soc.Save(s.batCache, batSoCKey, e)
}

// loadBatSoC restores a value entered by hand.
func (s *Site) loadBatSoC() {
	if e := soc.Load(s.batCache, batSoCKey); e != nil && e.Source == soc.SourceManual {
		s.batEst = e
	}
}

// MarshalJSON writes an unknown state of charge (NaN) as null: JSON has no
// NaN, and the web page must still show the rest (and offer the entry).
func (st State) MarshalJSON() ([]byte, error) {
	type plain State
	var v *float64
	if !math.IsNaN(st.SoC) {
		x := st.SoC
		v = &x
	}
	return json.Marshal(struct {
		plain
		SoC *float64 `json:"soc"`
	}{plain(st), v})
}
