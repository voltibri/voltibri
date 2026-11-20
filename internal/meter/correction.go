// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package meter applies user defined corrections to device measurements.
//
// Many devices measure slightly off. A go-e charger, for example, may report
// 200 W less than it actually draws while charging. A correction
//
//	corrected = raw * factor + offset   (offset only while |raw| > activeAbove)
//
// is applied by the core to every value a device reports, so it works the
// same for all drivers and plugins.
package meter

import (
	"math"

	"github.com/voltibri/voltibri/sdk"
)

// Voltage used to convert power offsets into currents.
const Voltage = 230.0

// Correction describes how to adjust a device's readings.
type Correction struct {
	// Factor scales all values (1 = unchanged, 1.03 = +3 %). 0 means 1.
	Factor float64 `yaml:"factor,omitempty" json:"factor,omitempty"`
	// OffsetW is added to power readings while the device is active.
	OffsetW float64 `yaml:"offsetW,omitempty" json:"offsetW,omitempty"`
	// ActiveAboveW: the offset is only applied while |raw power| exceeds
	// this threshold, so an idle device keeps reading 0 W.
	ActiveAboveW float64 `yaml:"activeAboveW,omitempty" json:"activeAboveW,omitempty"`
}

// IsZero reports whether the correction changes nothing.
func (c Correction) IsZero() bool {
	return (c.Factor == 0 || c.Factor == 1) && c.OffsetW == 0
}

func (c Correction) factor() float64 {
	if c.Factor == 0 {
		return 1
	}
	return c.Factor
}

// active reports whether the offset applies for the raw power.
func (c Correction) active(raw float64) bool {
	return math.Abs(raw) > c.ActiveAboveW && raw != 0
}

// Power corrects a power reading. For bidirectional values (grid, battery)
// the offset follows the sign of the reading.
func (c Correction) Power(raw float64) float64 {
	v := raw * c.factor()
	if c.OffsetW != 0 && c.active(raw) {
		v += math.Copysign(c.OffsetW, raw)
	}
	return v
}

// Currents corrects phase currents. The power offset is converted into a
// current and spread over the phases currently carrying load.
func (c Correction) Currents(raw sdk.Phases, rawPowerW float64) sdk.Phases {
	f := c.factor()
	out := sdk.Phases{raw[0] * f, raw[1] * f, raw[2] * f}
	if c.OffsetW == 0 || !c.active(rawPowerW) {
		return out
	}
	n := 0
	for _, i := range raw {
		if i > 0.5 {
			n++
		}
	}
	if n == 0 {
		return out
	}
	add := c.OffsetW / Voltage / float64(n)
	for k, i := range raw {
		if i > 0.5 {
			out[k] += add
		}
	}
	return out
}

// Energy corrects a cumulative energy counter. Only the factor applies:
// the power offset is accounted for by voltibri's own integration of the
// corrected power (charging sessions, history), not by the device counter.
func (c Correction) Energy(rawWh float64) float64 { return rawWh * c.factor() }
