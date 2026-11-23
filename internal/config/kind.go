// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

// Asset kinds. They decide the area in the UI and the kind of runs.
const (
	KindPV             = "pv"
	KindBattery        = "battery"
	KindInverter       = "inverter" // hybrid: PV, battery and/or grid meter in one
	KindGridMeter      = "grid-meter"
	KindEVCharger      = "ev-charger"
	KindVehicleAccount = "vehicle-account"
	KindHeatPump       = "heat-pump"
	KindHeatingRod     = "heating-rod"
	KindClimate        = "climate" // air conditioning, dehumidifier
	KindAppliance      = "appliance"
	KindForecast       = "forecast"
	KindTariff         = "tariff"
)

// Kinds lists all kinds with their German label.
var Kinds = []struct{ Kind, Label string }{
	{KindPV, "PV-Anlage"}, {KindBattery, "Hausbatterie"}, {KindInverter, "Wechselrichter"}, {KindGridMeter, "Netzzähler"},
	{KindEVCharger, "Wallbox"}, {KindVehicleAccount, "Fahrzeugkonto"}, {KindHeatPump, "Wärmepumpe"}, {KindHeatingRod, "Heizstab"},
	{KindClimate, "Klima / Entfeuchter"}, {KindAppliance, "Haushaltsgerät"}, {KindForecast, "Prognose"}, {KindTariff, "Strompreis"},
}

func knownKind(k string) bool {
	for _, x := range Kinds {
		if x.Kind == k {
			return true
		}
	}
	return false
}

// KindOf returns the asset kind of a device: explicit, else derived from the
// driver's capabilities and the icon.
func KindOf(d Device) string {
	if d.Kind != "" {
		return d.Kind
	}
	drv, _ := sdk.Lookup(d.Type)
	has := drv.HasCapability
	switch {
	case has(sdk.CapCharger):
		return KindEVCharger
	case has(sdk.CapVehicles):
		return KindVehicleAccount
	case has(sdk.CapBattery) || (has(sdk.CapPV) && has(sdk.CapGrid)):
		return KindInverter
	case has(sdk.CapPV):
		return KindPV
	case has(sdk.CapGrid):
		return KindGridMeter
	case has(sdk.CapPVForecast):
		return KindForecast
	case has(sdk.CapTariff):
		return KindTariff
	}
	switch d.Icon {
	case "heatpump":
		return KindHeatPump
	case "water":
		return KindHeatingRod
	case "car":
		return KindEVCharger
	case "solar", "balcony":
		return KindPV
	}
	return KindAppliance
}

// Run kinds.
const (
	RunCharging = "charging"
	RunHeating  = "heating"
	RunOther    = "run"
)

// RunKind returns the kind of runs an asset of this kind has.
func RunKind(assetKind string) string {
	switch assetKind {
	case KindEVCharger:
		return RunCharging
	case KindHeatPump, KindHeatingRod, KindClimate:
		return RunHeating
	}
	return RunOther
}

const dateLayout = "2006-01-02"

// Retired reports whether the device is decommissioned at time t.
func (d Device) Retired(t time.Time) bool {
	if d.Decommissioned == "" {
		return false
	}
	end, err := time.ParseInLocation(dateLayout, d.Decommissioned, t.Location())
	return err == nil && !t.Before(end)
}

// validateLifecycle checks kinds and dates; retired devices must not have
// a site function or be part of a circuit.
func (c *Config) validateLifecycle() []error {
	var errs []error
	inCircuit := map[string]bool{}
	for _, ci := range c.Circuits {
		for _, d := range ci.Devices {
			inCircuit[d] = true
		}
		if ci.Meter != "" {
			inCircuit[ci.Meter] = true
		}
	}
	now := time.Now()
	for _, d := range c.Devices {
		if d.Kind != "" && !knownKind(d.Kind) {
			errs = append(errs, fmt.Errorf("device %q: unknown kind %q", d.ID, d.Kind))
		}
		switch d.Flexibility {
		case "", FlexNone, FlexShiftable, FlexInterruptible, FlexSurplus:
		default:
			errs = append(errs, fmt.Errorf("device %q: flexibility must be none, shiftable, interruptible or surplus", d.ID))
		}
		if d.RunThresholdW < 0 || d.RunThresholdW > 100000 {
			errs = append(errs, fmt.Errorf("device %q: runThresholdW out of range", d.ID))
		}
		var from, to time.Time
		var err error
		if d.Commissioned != "" {
			if from, err = time.Parse(dateLayout, d.Commissioned); err != nil {
				errs = append(errs, fmt.Errorf("device %q: commissioned must be a date YYYY-MM-DD", d.ID))
			}
		}
		if d.Decommissioned != "" {
			if to, err = time.Parse(dateLayout, d.Decommissioned); err != nil {
				errs = append(errs, fmt.Errorf("device %q: decommissioned must be a date YYYY-MM-DD", d.ID))
			}
		}
		if !from.IsZero() && !to.IsZero() && to.Before(from) {
			errs = append(errs, fmt.Errorf("device %q: decommissioned before commissioned", d.ID))
		}
		if d.Retired(now) {
			if roles := c.RolesOf(d.ID); len(roles) > 0 {
				errs = append(errs, fmt.Errorf("device %q is decommissioned but still used as %v", d.ID, roles))
			}
			if inCircuit[d.ID] {
				errs = append(errs, fmt.Errorf("device %q is decommissioned but still part of a circuit", d.ID))
			}
		}
	}
	return errs
}

// Flexibility of a device (what voltibri may do with it).
const (
	FlexNone          = "none"          // only measured
	FlexShiftable     = "shiftable"     // start can be delayed; the program then runs to its end
	FlexInterruptible = "interruptible" // may be switched on and off as long as it is done by its deadline
	FlexSurplus       = "surplus"       // no target, runs only on PV surplus (heating rod, dehumidifier)
)
