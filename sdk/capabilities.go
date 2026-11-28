// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: Apache-2.0

// Package sdk defines the contract between the voltibri core and device
// plugins. A device is any value implementing Device; its capabilities are
// discovered through Go interface assertions (e.g. dev.(sdk.Battery)).
//
// Sign conventions used throughout voltibri:
//   - power values are in watts (W), energy values in watt-hours (Wh)
//   - grid power is positive for import and negative for export
//   - battery power is positive for discharge and negative for charge
//   - state of charge (SoC) is a fraction between 0 and 1
package sdk

import (
	"context"
	"time"
)

// Device is the minimal interface every plugin device implements.
type Device interface {
	// ID returns the unique device identifier from the configuration.
	ID() string
}

// GridMeter measures the power at the grid connection point.
type GridMeter interface {
	// GridPower returns the current grid power in W (+import, -export).
	GridPower(ctx context.Context) (float64, error)
}

// PVProducer measures photovoltaic production.
type PVProducer interface {
	// PVPower returns the current PV production in W (always >= 0).
	PVPower(ctx context.Context) (float64, error)
}

// Battery reports the state of a stationary battery.
type Battery interface {
	// SoC returns the state of charge as fraction 0..1.
	SoC(ctx context.Context) (float64, error)
	// BatteryPower returns the battery power in W (+discharge, -charge).
	BatteryPower(ctx context.Context) (float64, error)
	// CapacityWh returns the usable capacity in Wh.
	CapacityWh() float64
}

// BatteryController can limit the charge power of a battery.
//
// Implementations MUST make limits revert automatically on the device side
// after a timeout, so that a crashed voltibri never leaves the battery in a
// restricted state.
type BatteryController interface {
	// SetChargeLimit restricts charging to at most w watts (0 = no charging).
	SetChargeLimit(ctx context.Context, w float64) error
	// ResetLimits returns the battery to its autonomous default behaviour.
	ResetLimits(ctx context.Context) error
}

// ChargerStatus describes the state of an EV charger.
type ChargerStatus struct {
	Connected bool    // a vehicle is plugged in
	Charging  bool    // energy is flowing
	PowerW    float64 // current charging power
	CurrentA  float64 // configured current per phase
	Phases    int     // phases the car draws on, measured (0 = unknown or not charging)
	Card      string  // RFID card used for this session ("" = none)
}

// CardLister is a charger with RFID cards; it lists their names.
type CardLister interface {
	Cards(ctx context.Context) ([]string, error)
}

// Charger is an EV charging station.
type Charger interface {
	ChargerStatus(ctx context.Context) (ChargerStatus, error)
}

// ChargerController can control an EV charging station.
type ChargerController interface {
	// EnableCharging allows or forbids charging.
	EnableCharging(ctx context.Context, enable bool) error
	// SetCurrent sets the charging current per phase in ampere.
	SetCurrent(ctx context.Context, ampere float64) error
}

// Switch is a relay that can be read and switched (e.g. a smart plug).
type Switch interface {
	SwitchState(ctx context.Context) (bool, error)
	SetSwitch(ctx context.Context, on bool) error
}

// PowerConsumer reports the power drawn by a single consumer.
type PowerConsumer interface {
	ConsumerPower(ctx context.Context) (float64, error)
}

// Phases holds a value per phase (L1, L2, L3).
type Phases [3]float64

// Stager is a load that runs in steps, e.g. a heating rod whose phases are
// switched one by one: step 0 is off, Steps() is full power. Writing needs
// mode "owned" and respects the dry run.
type Stager interface {
	Steps() int
	StepPowerW() float64 // nominal power of one step
	Step(ctx context.Context) (int, error)
	SetStep(ctx context.Context, n int) error
}

// PhaseCurrents reports the current per phase in ampere (as measured by
// the device on its own terminals; the site maps them to grid phases).
type PhaseCurrents interface {
	Currents(ctx context.Context) (Phases, error)
}

// PhaseSwitcher can switch between single and three phase operation.
type PhaseSwitcher interface {
	SetPhases(ctx context.Context, phases int) error
}

// EnergyMeter reports a cumulative energy counter in Wh.
type EnergyMeter interface {
	EnergyWh(ctx context.Context) (float64, error)
}

// Slot is a forecast or price value for a time interval [Start, Start+Duration).
type Slot struct {
	Start    time.Time
	Duration time.Duration
	Value    float64
}

// PVForecaster delivers a PV production forecast (Value in W, mean power).
type PVForecaster interface {
	PVForecast(ctx context.Context, from, to time.Time) ([]Slot, error)
}

// TariffSource delivers energy prices (Value in currency per kWh).
type TariffSource interface {
	Prices(ctx context.Context, from, to time.Time) ([]Slot, error)
}

// TemperatureForecaster delivers outdoor temperature forecasts (Value in °C).
type TemperatureForecaster interface {
	TemperatureForecast(ctx context.Context, from, to time.Time) ([]Slot, error)
}

// VehicleInfo identifies an electric vehicle offered by a VehicleProvider.
type VehicleInfo struct {
	ID    string `json:"id"`    // stable id within the provider
	Name  string `json:"name"`  // display name as set by the user in the vendor app
	Brand string `json:"brand"` // e.g. "Tesla"
	Model string `json:"model"` // e.g. "Model Y"
	VIN   string `json:"vin,omitempty"`
	// Defaults known without the car (manual vehicles); user settings win.
	CapacityWh float64 `json:"capacityWh,omitempty"`
	MaxPowerW  float64 `json:"maxPowerW,omitempty"` // onboard charger limit
	Phases     int     `json:"phases,omitempty"`    // phases the onboard charger uses (0 = unknown)
}

// VehicleStatus is the state reported by the vehicle (via its cloud).
// Unknown numbers are NaN.
type VehicleStatus struct {
	SoC       float64   // state of charge 0..1
	LimitSoC  float64   // charge limit set in the car 0..1
	RangeKm   float64   // remaining range
	Connected bool      // plugged in
	Charging  bool      // energy is flowing
	Updated   time.Time // when the provider last refreshed the data
}

// VehicleProvider is an account (e.g. Tibber, a vendor cloud) that knows
// one or more vehicles. Providers should cache: vehicle clouds are slow and
// rate limited.
type VehicleProvider interface {
	Vehicles(ctx context.Context) ([]VehicleInfo, error)
	VehicleStatus(ctx context.Context, id string) (VehicleStatus, error)
}

// OAuthAuthorizer is implemented by devices that need an interactive
// OAuth login in the browser. The core serves the redirect endpoint.
type OAuthAuthorizer interface {
	// AuthURL returns the URL the user is sent to; state must be passed
	// back unchanged to Exchange.
	AuthURL(redirectURI string) (url, state string, err error)
	// Exchange completes the login with the code from the redirect.
	Exchange(ctx context.Context, code, state string) error
	// Authorized reports whether usable tokens are available.
	Authorized() bool
}

// BatteryLimiter sets charge and discharge limits at once (negative = no
// limit; both negative = autonomous operation). Used to keep the battery
// from feeding excluded loads (e.g. an EV charging from the grid), for the
// maximum discharge power and the lower charge target (discharge 0).
type BatteryLimiter interface {
	SetLimits(ctx context.Context, chargeW, dischargeW float64) error
}

// HeatStatus is what a heat pump reports about its heat output. Unknown
// numbers are NaN.
type HeatStatus struct {
	ThermalW   float64   // heat output now
	COP        float64   // coefficient of performance now
	ElectricW  float64   // electrical power of the heat pump itself (compressor)
	FlowC      float64   // flow temperature
	ReturnC    float64   // return temperature
	OutdoorC   float64   // outdoor temperature
	HotWaterC  float64   // domestic hot water temperature
	HeatWh     float64   // heat counter (total)
	ElectricWh float64   // electrical energy counter (total)
	State      string    // operating state as text
	Updated    time.Time // last value received
}

// HeatSource is a heat pump (or heat meter) that reports heat output.
type HeatSource interface {
	HeatStatus(ctx context.Context) (HeatStatus, error)
}

// BatteryForceCharger can make the battery charge with a given power even
// if that power has to come from the grid. Used for metering concepts where
// a charger sits behind a separate (cascaded) meter, and for the force charging
// (charge now up to a target). Like the limits it MUST revert on the device
// side after a timeout; voltibri refreshes it as long as it is wanted.
type BatteryForceCharger interface {
	ForceCharge(ctx context.Context, w float64) error
}
