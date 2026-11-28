// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package soc estimates the state of charge of a battery (vehicle, home
// battery, e-bike, power bank) from a known value and the energy charged or
// discharged since: a value from the device's API, one entered by hand or a
// typical value. A newer value from the API replaces the base.
package soc

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

// Source of the base value.
type Source string

const (
	SourceAPI     Source = "api"     // read from the device or its cloud
	SourceManual  Source = "manual"  // entered by hand
	SourceDefault Source = "default" // typical state of charge when plugging in (configured)
)

// Estimate is a state of charge: a base value plus the energy stored in the
// battery since then (negative: taken out).
type Estimate struct {
	Base     float64   `json:"base"`               // 0..1
	At       time.Time `json:"at"`                 // when the base was taken
	Source   Source    `json:"source"`             // where the base comes from
	StoredWh float64   `json:"storedWh,omitempty"` // energy into the battery since At
	// APIRef is the API value seen when the base was taken (NaN stored as
	// nil): the API replaces a base only once it reports something else, so
	// a stale value (car asleep, cloud down) does not undo an entry by hand.
	APIRef *float64 `json:"apiRef,omitempty"`
	// InUse: the battery is being charged with this estimate (vehicle
	// plugged in, device running); unset for a value entered beforehand.
	InUse bool `json:"inUse,omitempty"`
}

// New returns an estimate with the base value.
func New(base float64, at time.Time, src Source, apiRef float64) *Estimate {
	e := &Estimate{}
	e.Rebase(base, at, src, apiRef)
	return e
}

// Rebase starts again from a known value.
func (e *Estimate) Rebase(base float64, at time.Time, src Source, apiRef float64) {
	e.Base, e.At, e.Source, e.StoredWh, e.APIRef = clamp(base), at, src, 0, nil
	if !math.IsNaN(apiRef) {
		e.APIRef = &apiRef
	}
}

// Charge adds energy taken in by the charger (wh at the input, eff the
// efficiency to the battery). With a capacity the stored energy stops at
// full: a later discharge starts from 100 %.
func (e *Estimate) Charge(wh, eff, capacityWh float64) {
	if wh > 0 && eff > 0 {
		e.StoredWh += wh * eff
		e.limit(capacityWh)
	}
}

// Discharge takes energy delivered by the battery (wh at the output).
func (e *Estimate) Discharge(wh, eff, capacityWh float64) {
	if wh > 0 && eff > 0 {
		e.StoredWh -= wh / eff
		e.limit(capacityWh)
	}
}

func (e *Estimate) limit(capacityWh float64) {
	if capacityWh > 0 {
		e.StoredWh = math.Max(-e.Base*capacityWh, math.Min((1-e.Base)*capacityWh, e.StoredWh))
	}
}

// Value is the estimated state of charge (0..1). Without a capacity the
// charged energy cannot be converted: the base stays.
func (e *Estimate) Value(capacityWh float64) float64 {
	if capacityWh <= 0 {
		return e.Base
	}
	return clamp(e.Base + e.StoredWh/capacityWh)
}

// Interpolated reports whether the value is more than the base (energy was
// counted since).
func (e *Estimate) Interpolated() bool { return math.Abs(e.StoredWh) >= 1 }

// APIChanged reports whether an API value differs from the one seen when
// the base was taken (by more than half a percent).
func (e *Estimate) APIChanged(v float64) bool {
	if math.IsNaN(v) {
		return false
	}
	return e.APIRef == nil || math.Abs(*e.APIRef-v) > 0.005
}

// Validate checks a value entered by hand.
func Validate(v float64) error {
	if math.IsNaN(v) || v < 0 || v > 1 {
		return fmt.Errorf("Ladestand muss zwischen 0 und 100 %% liegen")
	}
	return nil
}

func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// Load reads an estimate from the cache (nil: none).
func Load(c sdk.Cache, key string) *Estimate {
	if c == nil {
		return nil
	}
	b, _, ok := c.Get(key)
	if !ok || len(b) == 0 {
		return nil
	}
	var e Estimate
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	return &e
}

// Save stores an estimate (nil: removes it).
func Save(c sdk.Cache, key string, e *Estimate) {
	if c == nil {
		return
	}
	var b []byte
	if e != nil {
		b, _ = json.Marshal(e)
	}
	_ = c.Put(key, b)
}
