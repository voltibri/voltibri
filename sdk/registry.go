// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"fmt"
	"sort"
	"sync"
)

// ControlMode defines who is allowed to control a device.
type ControlMode string

const (
	// ModeOwned: voltibri plans and controls the device.
	ModeOwned ControlMode = "owned"
	// ModeDelegated: an external program (e.g. a utility's VPP) controls the
	// device; voltibri only observes it and plans around it.
	ModeDelegated ControlMode = "delegated"
	// ModeObserved: measurement only, never written to.
	ModeObserved ControlMode = "observed"
)

// DeviceConfig is the configuration passed to a driver factory.
type DeviceConfig struct {
	ID     string
	Type   string
	Mode   ControlMode
	DryRun bool           // log writes instead of executing them
	Params map[string]any // driver specific settings (defaults applied)
	Cache  Cache          // persistent per-device cache (never nil inside a factory)
}

// Factory creates a device from its configuration.
type Factory func(cfg DeviceConfig) (Device, error)

// Capability names a feature a driver offers. Drivers declare them so that
// user interfaces can offer the right roles before a device exists.
type Capability string

const (
	CapGrid           Capability = "grid"            // GridMeter
	CapPV             Capability = "pv"              // PVProducer
	CapBattery        Capability = "battery"         // Battery
	CapBatteryControl Capability = "battery-control" // BatteryController
	CapCharger        Capability = "charger"         // Charger
	CapChargerControl Capability = "charger-control" // ChargerController
	CapSwitch         Capability = "switch"          // Switch
	CapConsumer       Capability = "consumer"        // PowerConsumer
	CapPhases         Capability = "phases"          // PhaseCurrents
	CapPhaseSwitch    Capability = "phase-switch"    // PhaseSwitcher
	CapEnergy         Capability = "energy"          // EnergyMeter
	CapPVForecast     Capability = "pv-forecast"     // PVForecaster
	CapTariff         Capability = "tariff"          // TariffSource
	CapVehicles       Capability = "vehicles"        // VehicleProvider
	CapOAuth          Capability = "oauth"           // OAuthAuthorizer
)

// Category groups drivers in user interfaces.
type Category string

const (
	CatInverter Category = "inverter"
	CatCharger  Category = "charger"
	CatSwitch   Category = "switch"
	CatMeter    Category = "meter"
	CatForecast Category = "forecast"
	CatTariff   Category = "tariff"
	CatVehicle  Category = "vehicle"
	CatDemo     Category = "demo"
	CatNone     Category = "none" // no connection: retired assets, history only
)

// Driver describes a device driver: metadata for user interfaces, the
// parameter schema and the factory.
type Driver struct {
	Type         string       `json:"type"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Category     Category     `json:"category"`
	Capabilities []Capability `json:"capabilities"`
	Params       []Param      `json:"params"`
	SetupURL     string       `json:"setupUrl,omitempty"` // where to create credentials (API client, token)
	Factory      Factory      `json:"-"`
}

var (
	mu      sync.RWMutex
	drivers = map[string]Driver{}
)

// RegisterDriver makes a driver available. It is typically called from a
// driver package's init function.
func RegisterDriver(d Driver) {
	mu.Lock()
	defer mu.Unlock()
	if d.Type == "" || d.Factory == nil {
		panic("sdk: driver needs type and factory")
	}
	if _, dup := drivers[d.Type]; dup {
		panic("sdk: driver registered twice: " + d.Type)
	}
	if d.Name == "" {
		d.Name = d.Type
	}
	drivers[d.Type] = d
}

// Register registers a driver without metadata (kept for simple plugins).
func Register(typ string, f Factory) { RegisterDriver(Driver{Type: typ, Factory: f}) }

// Lookup returns the driver for a type.
func Lookup(typ string) (Driver, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := drivers[typ]
	return d, ok
}

// New validates cfg.Params against the driver schema, applies defaults and
// creates the device.
func New(cfg DeviceConfig) (Device, error) {
	d, ok := Lookup(cfg.Type)
	if !ok {
		return nil, fmt.Errorf("unknown device type %q (available: %v)", cfg.Type, Types())
	}
	params, err := d.Validate(cfg.Params)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.ID, err)
	}
	cfg.Params = params
	if cfg.Cache == nil {
		cfg.Cache = NewMemCache()
	}
	return d.Factory(cfg)
}

// Types returns the names of all registered drivers.
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(drivers))
	for t := range drivers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Drivers returns all registered drivers sorted by category and name.
func Drivers() []Driver {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Driver, 0, len(drivers))
	for _, d := range drivers {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// HasCapability reports whether the driver declares c.
func (d Driver) HasCapability(c Capability) bool {
	for _, x := range d.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}
