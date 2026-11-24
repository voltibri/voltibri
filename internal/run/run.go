// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package run defines runs: a continuous activity of an asset – a charging
// session of a wallbox, a heating cycle of a heating rod or heat pump, a run
// of any other load. Charging sessions are runs of kind "charging".
package run

import (
	"context"
	"time"
)

// Run is one run of an asset.
type Run struct {
	ID          int64          `json:"id"`
	Asset       string         `json:"asset"` // device id (stable, also for retired devices)
	Kind        string         `json:"kind"`  // charging, heating, run
	Vehicle     string         `json:"vehicle,omitempty"`
	VehicleName string         `json:"vehicleName,omitempty"`
	Guest       bool           `json:"guest,omitempty"`
	Mode        string         `json:"mode,omitempty"`
	Start       time.Time      `json:"start"`
	End         time.Time      `json:"end,omitzero"`
	EnergyWh    float64        `json:"energyWh"`
	PVWh        float64        `json:"pvWh"`      // from PV
	BatteryWh   float64        `json:"batteryWh"` // from the home battery
	GridWh      float64        `json:"gridWh"`    // from the grid
	CostCt      float64        `json:"costCt"`
	CostUnknown bool           `json:"costUnknown,omitempty"` // e.g. imported without price
	PriceNote   string         `json:"priceNote,omitempty"`   // how the cost was computed
	Attrs       map[string]any `json:"attrs,omitempty"`       // odometer, SoC, raw import data ...
	Source      string         `json:"source,omitempty"`      // "" = measured by voltibri, else e.g. "evcc"
	ExternalID  string         `json:"externalId,omitempty"`  // id in the source system
	ImportID    int64          `json:"importId,omitempty"`    // import that brought it
}

// SolarWh is the energy not taken from the grid (PV and battery).
func (r Run) SolarWh() float64 { return r.PVWh + r.BatteryWh }

// Filter selects runs.
type Filter struct {
	Asset    string
	Vehicle  string   // vehicle name (the same car from different sources counts as one)
	Vehicles []string // further names of that vehicle (aliases); set by the API
	Only     []string // restriction: only runs of these vehicle names (users; never cleared)
	Kind     string
	From     time.Time // start >= From (zero: no limit)
	To       time.Time // start < To (zero: no limit)
	Offset   int
	Limit    int
	All      bool // include voltibri runs hidden by an overlapping import
	// ProfiledFrom skips runs covered by a load profile (device -> first
	// profiled slot); reports take energy and cost of that time from the
	// profile instead.
	ProfiledFrom map[string]time.Time
}

// Store persists runs.
type Store interface {
	SaveRun(ctx context.Context, r Run) (int64, error)
	Runs(ctx context.Context, f Filter) ([]Run, error)
}
