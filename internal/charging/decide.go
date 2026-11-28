// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package charging controls EV chargers (loadpoints): charge modes, charge
// plans, vehicle detection, guest vehicles and charging sessions.
//
// Modes (per vehicle; guests use the loadpoint's guest mode):
//   - external: another system controls car and charger (e.g. a utility
//     VPP). voltibri never writes, it only measures and logs sessions.
//   - pv:       charge with PV surplus only
//   - minpv:    always charge with at least the minimum power, more with
//     surplus
//   - fast:     charge with maximum power
//   - boost:    PV surplus plus power from the home battery (down to the
//     boost limit of the battery, never from the grid); a boosted vehicle
//     comes before all other surplus loads
//
// A charge plan (target SoC by a time) overlays pv and minpv: in the slots
// the plan needs, the vehicle charges fast.
package charging

import (
	"fmt"
	"math"
	"time"
)

// Mode is a charge mode.
type Mode string

const (
	ModeOff      Mode = "off" // charging disabled
	ModeExternal Mode = "external"
	ModePV       Mode = "pv"
	ModePV1      Mode = "pv1" // PV surplus on one phase only (starts at 1.4 kW)
	ModeMinPV    Mode = "minpv"
	ModeFast     Mode = "fast"
	ModeBoost    Mode = "boost" // PV surplus + home battery
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeOff, ModeExternal, ModePV, ModePV1, ModeMinPV, ModeFast, ModeBoost:
		return true
	}
	return false
}

// Surplus reports whether the mode follows the PV surplus (and takes part
// in the priority order of the surplus loads).
func (m Mode) Surplus() bool {
	return m == ModePV || m == ModePV1 || m == ModeMinPV || m == ModeBoost
}

// Voltage per phase used to convert power and current.
const Voltage = 230.0

// Delays before starting / stopping PV charging (as in evcc).
var (
	EnableDelay  = time.Minute
	DisableDelay = 3 * time.Minute
)

// Input is everything decide needs for one loadpoint.
type Input struct {
	Now       time.Time
	Mode      Mode
	PlanSlot  bool    // the current slot belongs to an active charge plan
	Connected bool    // a vehicle is plugged in
	SurplusW  float64 // power available for this loadpoint incl. what it draws now
	MinA      float64
	MaxA      float64
	Phases    int     // installed phases (1 or 3)
	CarPhases int     // phases the car draws on (0 = unknown: all released)
	CanSwitch bool    // charger can switch between 1 and 3 phases
	MinPowerW float64 // minpv: minimum power (0 = minimum current)
	MaxPowerW float64 // vehicle limit (0 = none)
	Prev      State   // state after the previous decision
}

// State is kept between decisions.
type State struct {
	Enabled      bool      `json:"enabled"`
	CurrentA     float64   `json:"currentA"`
	Phases       int       `json:"phases"`
	SurplusSince time.Time `json:"-"` // enough surplus to start since
	DeficitSince time.Time `json:"-"` // too little surplus to continue since
}

// Decision is the target state plus a human readable reason.
type Decision struct {
	State
	Reason string
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// amps converts power to current per phase.
func amps(w float64, phases int) float64 { return w / (Voltage * float64(phases)) }

// DrawPhases are the phases a car draws on when the charger releases
// released phases: a car with a 1- or 2-phase charger takes fewer.
func DrawPhases(released, car int) int {
	if car > 0 && car < released {
		return car
	}
	return released
}

// Decide computes the target charger state. It never decides for
// ModeExternal (the caller must not write in that case).
func Decide(in Input) Decision {
	d := Decision{State: in.Prev}
	if in.Phases != 1 {
		in.Phases = 3
	}
	if d.Phases == 0 {
		d.Phases = in.Phases
	}
	// power and current per phase convert over the phases the car draws on
	draw := func(phases int) int { return DrawPhases(phases, in.CarPhases) }
	maxAt := func(phases int) float64 {
		a := in.MaxA
		if in.MaxPowerW > 0 {
			a = math.Min(a, amps(in.MaxPowerW, draw(phases)))
		}
		return math.Max(a, in.MinA)
	}
	off := func(reason string) Decision {
		d.Enabled, d.CurrentA = false, 0
		d.SurplusSince, d.DeficitSince = time.Time{}, time.Time{}
		d.Reason = reason
		return d
	}
	if in.Mode == ModeExternal {
		d.Reason = "extern gesteuert – voltibri greift nicht ein"
		return d
	}
	if in.Mode == ModeBoost { // the surplus includes the battery's share
		in.Mode = ModePV
	}
	if in.Mode == ModeOff {
		return off("Laden aus")
	}
	if !in.Connected {
		return off("kein Fahrzeug angesteckt")
	}
	if in.Mode == ModeFast || in.PlanSlot {
		// the wallbox's maximum (load management included) on all its
		// phases: the car takes what its onboard charger can
		d.Enabled, d.CurrentA, d.Phases = true, math.Max(in.MaxA, in.MinA), in.Phases
		d.SurplusSince, d.DeficitSince = time.Time{}, time.Time{}
		d.Reason = "Schnellladen mit maximaler Leistung"
		if in.PlanSlot && in.Mode != ModeFast {
			d.Reason = "Ladeplan: lädt jetzt mit maximaler Leistung"
		}
		return d
	}

	// pv / minpv: pick the phase count the surplus allows
	phases := in.Phases
	switch {
	case in.Mode == ModePV1 && (in.CanSwitch || in.Phases == 1):
		phases = 1 // always one phase: starts earlier, finer steps
	case in.CanSwitch && in.Phases == 3 && in.SurplusW < in.MinA*Voltage*float64(draw(3)):
		phases = 1
	}
	maxA := maxAt(phases)
	target := amps(in.SurplusW, draw(phases))
	minA := in.MinA
	if in.Mode == ModeMinPV && in.MinPowerW > 0 {
		minA = clamp(amps(in.MinPowerW, draw(phases)), in.MinA, maxA)
	}

	if in.Mode == ModeMinPV {
		d.Enabled, d.Phases = true, phases
		d.CurrentA = clamp(math.Floor(target), minA, maxA)
		d.SurplusSince, d.DeficitSince = time.Time{}, time.Time{}
		if target < minA {
			d.Reason = fmt.Sprintf("Mindestleistung %.1f kW, Rest aus Netz/Batterie", minA*Voltage*float64(draw(phases))/1000)
		} else {
			d.Reason = "lädt mit PV-Überschuss (mindestens Mindestleistung)"
		}
		return d
	}

	// pv only, with start/stop delays
	enough := target >= in.MinA
	if !d.Enabled {
		if !enough {
			return off(fmt.Sprintf("wartet auf Überschuss (ab %.1f kW)", in.MinA*Voltage*float64(draw(phases))/1000))
		}
		if d.SurplusSince.IsZero() {
			d.SurplusSince = in.Now
		}
		if in.Now.Sub(d.SurplusSince) < EnableDelay {
			d.Reason = "Überschuss vorhanden, startet gleich"
			return d
		}
		d.Enabled, d.Phases, d.SurplusSince = true, phases, time.Time{}
	}
	if !enough {
		if d.DeficitSince.IsZero() {
			d.DeficitSince = in.Now
		}
		if in.Now.Sub(d.DeficitSince) >= DisableDelay {
			return off("zu wenig Überschuss, Laden pausiert")
		}
		d.CurrentA = in.MinA
		d.Reason = "Überschuss zu gering, pausiert gleich"
		return d
	}
	d.DeficitSince = time.Time{}
	d.Phases = phases
	d.CurrentA = clamp(math.Floor(target), in.MinA, maxA)
	d.Reason = "lädt mit PV-Überschuss"
	return d
}

// Surplus returns the power available for EV charging.
//
// gridW: + import / − export, batteryW: + discharge / − charge, evW: what
// the loadpoint draws now. With batteryPriority the battery's charging
// power is not offered to the vehicle.
func Surplus(gridW, batteryW, evW float64, batteryPriority bool) float64 {
	s := -gridW + evW
	if batteryPriority {
		return s - math.Max(batteryW, 0) // battery charge stays with the battery
	}
	return s - batteryW // battery charge counts as available, discharge does not
}
