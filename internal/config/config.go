// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package config defines the voltibri configuration, its validation and
// YAML import/export. The configuration is stored in the database (see
// package store); YAML is used for import, export and the optional
// read-only mode.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/voltibri/voltibri/internal/meter"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/pricing"
	"github.com/voltibri/voltibri/internal/tariff"
	"github.com/voltibri/voltibri/sdk"
)

// Config is the root configuration.
type Config struct {
	Site     Site           `yaml:"site" json:"site"`
	Battery  Battery        `yaml:"battery" json:"battery"`
	Roles    Roles          `yaml:"roles" json:"roles"`
	Planner  planner.Config `yaml:"planner" json:"planner"`
	Devices  []Device       `yaml:"devices" json:"devices"`
	Tariffs  tariff.Set     `yaml:"tariffs,omitempty" json:"tariffs,omitempty"`
	Charging Charging       `yaml:"charging,omitempty" json:"charging,omitempty"`
	Grid     GridConnection `yaml:"grid,omitempty" json:"grid,omitempty"`
	Circuits []Circuit      `yaml:"circuits,omitempty" json:"circuits,omitempty"`
}

// Guest policies: how a guest vehicle charging fast interacts with the
// home battery.
const (
	// GuestShare: the guest may use PV power; its energy is booked at the
	// grid price and kept out of the house load forecast.
	GuestShare = "share"
	// GuestBatteryFirst: guests only get what the battery does not need
	// (plus the minimum power from the grid).
	GuestBatteryFirst = "batteryFirst"
	// GuestForceBattery: the battery keeps charging with the surplus the
	// house would have without the guest, even if that means grid import.
	// Meant for chargers behind a separate meter (e.g. metering concept 40).
	GuestForceBattery = "forceBattery"
)

// Charging configures charge points (loadpoints). Every charger device is a
// loadpoint; entries here only tune it.
type Charging struct {
	Loadpoints []Loadpoint `yaml:"loadpoints,omitempty" json:"loadpoints,omitempty"`
	// Cards: what a charging card (RFID) starts – a vehicle or "guest" and a
	// charge mode for the session.
	Cards []Card `yaml:"cards,omitempty" json:"cards,omitempty"`
	// BatteryPrioritySoC: below this SoC PV surplus goes to the home battery
	// first (unless the battery plan says there is enough surplus later).
	// Default 0.5. Only the starting value: administrators change it at
	// runtime in the battery settings, that value wins.
	BatteryPrioritySoC float64 `yaml:"batteryPrioritySoC,omitempty" json:"batteryPrioritySoC,omitempty"`
}

// Loadpoint tunes one charger.
type Loadpoint struct {
	Charger        string  `yaml:"charger" json:"charger"`                                   // device id
	Phases         int     `yaml:"phases,omitempty" json:"phases,omitempty"`                 // default 3
	MinCurrentA    float64 `yaml:"minCurrent,omitempty" json:"minCurrent,omitempty"`         // default 6
	MaxCurrentA    float64 `yaml:"maxCurrent,omitempty" json:"maxCurrent,omitempty"`         // default 16
	SeparateMeter  bool    `yaml:"separateMeter,omitempty" json:"separateMeter,omitempty"`   // behind its own meter: uses tariffs.charging
	Guest          string  `yaml:"guest,omitempty" json:"guest,omitempty"`                   // share | batteryFirst | forceBattery
	GuestMode      string  `yaml:"guestMode,omitempty" json:"guestMode,omitempty"`           // charge mode for guests (default fast)
	GuestNoBattery bool    `yaml:"guestNoBattery,omitempty" json:"guestNoBattery,omitempty"` // guests never from the home battery
}

// LoadpointFor returns the tuning for a charger with defaults applied.
func (c *Config) LoadpointFor(charger string) Loadpoint {
	lp := Loadpoint{Charger: charger}
	for _, l := range c.Charging.Loadpoints {
		if l.Charger == charger {
			lp = l
		}
	}
	if lp.Phases == 0 {
		lp.Phases = 3
	}
	if lp.MinCurrentA == 0 {
		lp.MinCurrentA = 6
	}
	if lp.MaxCurrentA == 0 {
		lp.MaxCurrentA = 16
	}
	if lp.Guest == "" {
		lp.Guest = GuestShare
	}
	if lp.GuestMode == "" {
		lp.GuestMode = "off" // guests do not charge until someone starts it (card, button)
	}
	return lp
}

// Site holds global settings.
type Site struct {
	Timezone string   `yaml:"timezone" json:"timezone"`
	Listen   string   `yaml:"listen" json:"listen"`
	History  string   `yaml:"history,omitempty" json:"history,omitempty"` // legacy CSV history, imported once
	DryRun   bool     `yaml:"dryRun" json:"dryRun"`                       // never write to devices
	Holidays []string `yaml:"holidays,omitempty" json:"holidays,omitempty"`
}

// Battery describes the stationary battery.
type Battery struct {
	CapacityWh    float64 `yaml:"capacityWh" json:"capacityWh"`
	MaxChargeW    float64 `yaml:"maxChargeW" json:"maxChargeW"`
	MaxDischargeW float64 `yaml:"maxDischargeW" json:"maxDischargeW"`
	MinSoC        float64 `yaml:"minSoC" json:"minSoC"`
	ChargeEff     float64 `yaml:"chargeEfficiency" json:"chargeEfficiency"`
	DischargeEff  float64 `yaml:"dischargeEfficiency" json:"dischargeEfficiency"`
}

// Roles map site functions to device IDs.
type Roles struct {
	Grid       string   `yaml:"grid" json:"grid"`
	PV         []string `yaml:"pv" json:"pv"`
	Battery    string   `yaml:"battery" json:"battery"`
	PVForecast string   `yaml:"pvForecast" json:"pvForecast"`
	Tariff     string   `yaml:"tariff" json:"tariff"`
}

// Device is one configured device.
type Device struct {
	ID         string           `yaml:"id" json:"id"`
	Name       string           `yaml:"name,omitempty" json:"name,omitempty"`   // display name, e.g. "Dach Süd"
	Icon       string           `yaml:"icon,omitempty" json:"icon,omitempty"`   // solar, balcony, car, heatpump, plug, water, house
	Group      string           `yaml:"group,omitempty" json:"group,omitempty"` // overview group (default: pv / by icon)
	Type       string           `yaml:"type" json:"type"`
	Mode       string           `yaml:"mode,omitempty" json:"mode,omitempty"`
	Correction meter.Correction `yaml:"correction,omitempty" json:"correction,omitempty"`
	Params     map[string]any   `yaml:"params,omitempty" json:"params,omitempty"`
	// Runs: detect runs from the power (nil = yes for consumers). Off for
	// devices that cycle all day, e.g. a fridge.
	Runs *bool `yaml:"runs,omitempty" json:"runs,omitempty"`
	// RunThresholdW: a run starts above this power (default 25 W).
	RunThresholdW float64 `yaml:"runThresholdW,omitempty" json:"runThresholdW,omitempty"`
	// BatteryUse: "" (default, the home battery may supply this device) or
	// "never" (the battery is held back while it runs: PV and grid only).
	BatteryUse string `yaml:"batteryUse,omitempty" json:"batteryUse,omitempty"`
	// NoBatteryModes (vehicles): in these charge modes the home battery does
	// not supply the car, e.g. [fast] or [fast, minpv].
	NoBatteryModes []string `yaml:"noBatteryModes,omitempty" json:"noBatteryModes,omitempty"`
	// ChargeModes (vehicles): the charge modes offered for this vehicle, e.g.
	// [pv, fast]; empty = all. "off" is always possible.
	ChargeModes []string `yaml:"chargeModes,omitempty" json:"chargeModes,omitempty"`
	// Price (vehicles): price rule for the runs of this vehicle, e.g. PV at a
	// fixed price and grid +10 %; nil = default (PV and battery at the lost
	// feed-in tariff, grid at the tariff). See package pricing.
	Price *pricing.Rule `yaml:"price,omitempty" json:"price,omitempty"`
	// HeatUse (heating rods): what the rod heats – "hotwater" (top of the
	// buffer, showers) or "heating" (supports the heating circuits).
	HeatUse string `yaml:"heatUse,omitempty" json:"heatUse,omitempty"`
	// Flexibility: none (default), shiftable (start can be delayed, the
	// program then runs to its end), interruptible (may be switched on and
	// off as long as it is done by its deadline) or surplus (no target, runs
	// only on PV surplus).
	Flexibility string `yaml:"flexibility,omitempty" json:"flexibility,omitempty"`
	// Kind of asset (pv, battery, ev-charger, heat-pump, heating-rod, ...);
	// empty = derived from the driver and icon, see KindOf.
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Life cycle (YYYY-MM-DD). Decommissioned devices are not polled but keep
	// their history.
	Commissioned   string `yaml:"commissioned,omitempty" json:"commissioned,omitempty"`
	Decommissioned string `yaml:"decommissioned,omitempty" json:"decommissioned,omitempty"`
}

// Default returns a configuration with defaults and no devices.
func Default() *Config {
	return &Config{
		Site:    Site{Timezone: "Europe/Berlin", Listen: "127.0.0.1:7070", DryRun: true},
		Battery: Battery{MaxChargeW: 5000, MaxDischargeW: 5000, MinSoC: 0.05, ChargeEff: 0.95, DischargeEff: 0.95},
		Planner: planner.DefaultConfig(),
	}
}

// Load reads and validates a YAML file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads YAML (unknown keys are rejected) and validates it.
func Parse(b []byte) (*Config, error) {
	c := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, err
	}
	return c, c.Validate()
}

// YAML renders the configuration.
func (c *Config) YAML() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// Validate checks structure, references and device parameters. A config
// without devices is valid (voltibri then waits to be set up).
func (c *Config) Validate() error {
	var errs []error
	ids := map[string]sdk.Driver{}
	for i, d := range c.Devices {
		if d.ID == "" || d.Type == "" {
			errs = append(errs, fmt.Errorf("device #%d: id and type are required", i+1))
			continue
		}
		if _, dup := ids[d.ID]; dup {
			errs = append(errs, fmt.Errorf("duplicate device id %q", d.ID))
		}
		switch sdk.ControlMode(d.Mode) {
		case "":
			c.Devices[i].Mode = string(sdk.ModeObserved)
		case sdk.ModeOwned, sdk.ModeDelegated, sdk.ModeObserved:
		default:
			errs = append(errs, fmt.Errorf("device %q: unknown mode %q (owned, delegated, observed)", d.ID, d.Mode))
		}
		drv, ok := sdk.Lookup(d.Type)
		if !ok {
			errs = append(errs, fmt.Errorf("device %q: unknown type %q", d.ID, d.Type))
		} else if _, err := drv.Validate(d.Params); err != nil {
			errs = append(errs, fmt.Errorf("device %q: %w", d.ID, err))
		}
		if d.Correction.Factor < 0 {
			errs = append(errs, fmt.Errorf("device %q: correction factor must be positive", d.ID))
		}
		ids[d.ID] = drv
	}
	check := func(role, id string, capab sdk.Capability) {
		if id == "" {
			return
		}
		drv, ok := ids[id]
		if !ok {
			errs = append(errs, fmt.Errorf("roles.%s: unknown device %q", role, id))
			return
		}
		if drv.Capabilities != nil && !drv.HasCapability(capab) {
			errs = append(errs, fmt.Errorf("roles.%s: device %q (%s) cannot act as %s", role, id, drv.Type, capab))
		}
	}
	for _, id := range c.Roles.PV {
		check("pv", id, sdk.CapPV)
	}
	check("grid", c.Roles.Grid, sdk.CapGrid)
	check("battery", c.Roles.Battery, sdk.CapBattery)
	check("pvForecast", c.Roles.PVForecast, sdk.CapPVForecast)
	check("tariff", c.Roles.Tariff, sdk.CapTariff)
	sources := map[string]bool{}
	for id, drv := range ids {
		if drv.Capabilities == nil || drv.HasCapability(sdk.CapTariff) {
			sources[id] = true
		}
	}
	for name, t := range map[string]*tariff.Config{"grid": c.Tariffs.Grid, "feedin": c.Tariffs.FeedIn, "charging": c.Tariffs.Charging} {
		if err := t.Validate(sources); err != nil {
			errs = append(errs, fmt.Errorf("tariffs.%s: %w", name, err))
		}
	}
	for i, lp := range c.Charging.Loadpoints {
		drv, ok := ids[lp.Charger]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: unknown charger %q", i, lp.Charger))
		case drv.Capabilities != nil && !drv.HasCapability(sdk.CapCharger):
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: device %q is no charger", i, lp.Charger))
		}
		switch lp.Guest {
		case "", GuestShare, GuestBatteryFirst, GuestForceBattery:
		default:
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: unknown guest policy %q (share, batteryFirst, forceBattery)", i, lp.Guest))
		}
		switch lp.GuestMode {
		case "", "external", "pv", "minpv", "fast", "boost":
		default:
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: unknown guest mode %q (external, pv, minpv, fast, boost)", i, lp.GuestMode))
		}
		if lp.Phases != 0 && lp.Phases != 1 && lp.Phases != 3 {
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: phases must be 1 or 3", i))
		}
		if lp.MinCurrentA != 0 && lp.MaxCurrentA != 0 && lp.MinCurrentA > lp.MaxCurrentA {
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: minCurrent > maxCurrent", i))
		}
		if lp.SeparateMeter && c.Tariffs.Charging == nil {
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: separateMeter needs tariffs.charging", i))
		}
	}
	if p := c.Charging.BatteryPrioritySoC; p < 0 || p > 1 {
		errs = append(errs, errors.New("charging.batteryPrioritySoC must be between 0 and 1"))
	}
	if c.Roles.Battery != "" && c.Battery.CapacityWh <= 0 {
		errs = append(errs, errors.New("battery.capacityWh is required when roles.battery is set"))
	}
	errs = append(errs, c.validateRanges()...)
	errs = append(errs, c.validateCircuits()...)
	errs = append(errs, c.validateLifecycle()...)
	errs = append(errs, c.validateCards()...)
	errs = append(errs, c.validateVehicles()...)
	return errors.Join(errs...)
}

// validateRanges checks battery and planner values for plausible ranges, so
// a typo in the UI (25 instead of 0.25) is rejected.
func (c *Config) validateRanges() []error {
	var errs []error
	check := func(name string, v, lo, hi float64) {
		if v < lo || v > hi {
			errs = append(errs, fmt.Errorf("%s must be between %g and %g (got %g)", name, lo, hi, v))
		}
	}
	b, p := c.Battery, c.Planner
	check("battery.capacityWh", b.CapacityWh, 0, 1e6)
	check("battery.maxChargeW", b.MaxChargeW, 0, 1e6)
	check("battery.maxDischargeW", b.MaxDischargeW, 0, 1e6)
	check("battery.minSoC", b.MinSoC, 0, 0.9)
	check("battery.chargeEfficiency", b.ChargeEff, 0, 1)
	check("battery.dischargeEfficiency", b.DischargeEff, 0, 1)
	check("planner.morningBufferSoC", p.MorningBufferSoC, 0, 0.9)
	check("planner.safetyMarginSoC", p.SafetyMarginSoC, 0, 0.5)
	check("planner.pvPessimism", p.PVPessimism, 0, 1.5)
	if p.Robustness != 0 {
		check("planner.robustness", p.Robustness, 1, 5)
	}
	check("planner.minNightHours", p.MinNightHours, 0, 12)
	return errs
}

// Ready reports whether the configuration is complete enough to run the
// control loop (at least a grid meter).
func (c *Config) Ready() error {
	if c.Roles.Grid == "" {
		return errors.New("no grid meter configured (roles.grid)")
	}
	return nil
}

// Clone returns a deep copy (via YAML round trip).
func (c *Config) Clone() *Config {
	b, err := c.YAML()
	if err != nil {
		panic(err)
	}
	out := Default()
	if err := yaml.Unmarshal(b, out); err != nil {
		panic(err)
	}
	return out
}

// RolesOf lists the site functions a device fulfils (German labels for the
// user interface): grid meter, PV, battery, forecast, price source.
func (c *Config) RolesOf(id string) []string {
	var out []string
	if c.Roles.Grid == id {
		out = append(out, "Netzzähler")
	}
	for _, p := range c.Roles.PV {
		if p == id {
			out = append(out, "PV-Erzeugung")
		}
	}
	if c.Roles.Battery == id {
		out = append(out, "Hausbatterie")
	}
	if c.Roles.PVForecast == id {
		out = append(out, "PV-Prognose")
	}
	if c.Roles.Tariff == id {
		out = append(out, "Strompreis")
	}
	for i, t := range []*tariff.Config{c.Tariffs.Grid, c.Tariffs.FeedIn, c.Tariffs.Charging} {
		if t != nil && t.Source == id {
			out = append(out, "Preisquelle "+[]string{"Netzbezug", "Einspeisung", "Ladestrom"}[i])
		}
	}
	return out
}

// GridConnection describes the house connection.
type GridConnection struct {
	FuseA  float64 `yaml:"fuseA,omitempty" json:"fuseA,omitempty"`   // main fuse per phase (e.g. 63); 0 = not monitored
	Phases int     `yaml:"phases,omitempty" json:"phases,omitempty"` // default 3
	// MaxImbalanceA is the highest difference between the phase currents
	// (Schieflast); 0 = DefaultImbalanceA. Load management keeps 1- and
	// 2-phase charging below it.
	MaxImbalanceA float64 `yaml:"maxImbalance,omitempty" json:"maxImbalance,omitempty"`
}

// DefaultImbalanceA is the permitted unbalanced load in Germany: 4.6 kVA
// between the phases (VDE-AR-N 4100), 20 A at 230 V.
const DefaultImbalanceA = 20.0

// ImbalanceA returns the permitted unbalanced load in ampere (0 for a
// single-phase connection).
func (g GridConnection) ImbalanceA() float64 {
	switch {
	case g.Phases == 1:
		return 0
	case g.MaxImbalanceA > 0:
		return g.MaxImbalanceA
	}
	return DefaultImbalanceA
}

// Circuit is a sub-distribution with its own fuse, e.g. two wallboxes that
// share 32 A. Load management keeps the sum below MaxCurrentA per phase.
type Circuit struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name,omitempty" json:"name,omitempty"`
	Parent      string   `yaml:"parent,omitempty" json:"parent,omitempty"` // other circuit; empty = house connection
	MaxCurrentA float64  `yaml:"maxCurrent" json:"maxCurrent"`             // fuse per phase
	Phases      int      `yaml:"phases,omitempty" json:"phases,omitempty"` // default 3
	Meter       string   `yaml:"meter,omitempty" json:"meter,omitempty"`   // device measuring the whole circuit (phase currents)
	Devices     []string `yaml:"devices,omitempty" json:"devices,omitempty"`
}

// validateCircuits checks the installation tree.
func (c *Config) validateCircuits() []error {
	var errs []error
	if c.Grid.FuseA < 0 || c.Grid.FuseA > 1000 {
		errs = append(errs, fmt.Errorf("grid.fuseA must be between 0 and 1000"))
	}
	if c.Grid.MaxImbalanceA < 0 || c.Grid.MaxImbalanceA > 1000 {
		errs = append(errs, fmt.Errorf("grid.maxImbalance must be between 0 and 1000"))
	}
	if p := c.Grid.Phases; p != 0 && p != 1 && p != 3 {
		errs = append(errs, fmt.Errorf("grid.phases must be 1 or 3"))
	}
	devs := map[string]bool{}
	for _, d := range c.Devices {
		devs[d.ID] = true
	}
	ids := map[string]bool{}
	for _, ci := range c.Circuits {
		if ci.ID == "" || ci.ID == "grid" {
			errs = append(errs, fmt.Errorf("circuits: id is required and must not be \"grid\""))
			continue
		}
		if ids[ci.ID] {
			errs = append(errs, fmt.Errorf("circuits: duplicate id %q", ci.ID))
		}
		ids[ci.ID] = true
	}
	owner := map[string]string{}
	parent := map[string]string{}
	for _, ci := range c.Circuits {
		parent[ci.ID] = ci.Parent
		if ci.MaxCurrentA < 0 || ci.MaxCurrentA > 1000 {
			errs = append(errs, fmt.Errorf("circuit %q: maxCurrent must be between 0 and 1000", ci.ID))
		}
		if ci.Phases != 0 && ci.Phases != 1 && ci.Phases != 3 {
			errs = append(errs, fmt.Errorf("circuit %q: phases must be 1 or 3", ci.ID))
		}
		if ci.Parent != "" && !ids[ci.Parent] {
			errs = append(errs, fmt.Errorf("circuit %q: unknown parent %q", ci.ID, ci.Parent))
		}
		if ci.Meter != "" {
			if !devs[ci.Meter] {
				errs = append(errs, fmt.Errorf("circuit %q: unknown meter %q", ci.ID, ci.Meter))
			} else if drv, ok := sdk.Lookup(c.device(ci.Meter).Type); ok && !drv.HasCapability(sdk.CapPhases) {
				errs = append(errs, fmt.Errorf("circuit %q: meter %q cannot measure phase currents", ci.ID, ci.Meter))
			}
		}
		for _, d := range ci.Devices {
			if !devs[d] {
				errs = append(errs, fmt.Errorf("circuit %q: unknown device %q", ci.ID, d))
			}
			if o, dup := owner[d]; dup {
				errs = append(errs, fmt.Errorf("device %q is in circuits %q and %q", d, o, ci.ID))
			}
			owner[d] = ci.ID
		}
	}
	for _, ci := range c.Circuits { // cycles
		seen := map[string]bool{}
		for id := ci.ID; id != ""; id = parent[id] {
			if seen[id] {
				errs = append(errs, fmt.Errorf("circuit %q: parents form a loop", ci.ID))
				break
			}
			seen[id] = true
		}
	}
	return errs
}

func (c *Config) device(id string) Device {
	for _, d := range c.Devices {
		if d.ID == id {
			return d
		}
	}
	return Device{}
}

// Card is the setting of one charging card (RFID).
type Card struct {
	Card    string `yaml:"card" json:"card"`                           // name as in the wallbox app
	Vehicle string `yaml:"vehicle,omitempty" json:"vehicle,omitempty"` // vehicle key or "guest"; empty = detect
	Mode    string `yaml:"mode,omitempty" json:"mode,omitempty"`       // charge mode for the session; empty = vehicle default
}

// validateCards checks the charging card settings.
func (c *Config) validateCards() []error {
	var errs []error
	seen := map[string]bool{}
	for _, k := range c.Charging.Cards {
		if k.Card == "" {
			errs = append(errs, errors.New("charging.cards: card name is required"))
			continue
		}
		if seen[strings.ToLower(k.Card)] {
			errs = append(errs, fmt.Errorf("charging.cards: card %q twice", k.Card))
		}
		seen[strings.ToLower(k.Card)] = true
		switch k.Mode {
		case "", "off", "external", "pv", "pv1", "minpv", "fast", "boost":
			if d, ok := c.Device(k.Vehicle); ok && d.IsVehicle() && !d.AllowsChargeMode(k.Mode) {
				errs = append(errs, fmt.Errorf("charging.cards: card %q: mode %q is not allowed for vehicle %s", k.Card, k.Mode, k.Vehicle))
			}
		default:
			errs = append(errs, fmt.Errorf("charging.cards: card %q: unknown mode %q", k.Card, k.Mode))
		}
	}
	for i, lp := range c.Charging.Loadpoints {
		switch lp.GuestMode {
		case "", "off", "external", "pv", "pv1", "minpv", "fast", "boost":
		default:
			errs = append(errs, fmt.Errorf("charging.loadpoints[%d]: unknown guestMode %q", i, lp.GuestMode))
		}
	}
	return errs
}

// AllowsChargeMode reports whether the charge mode may be chosen for this
// vehicle: "off" and "" (the vehicle's default) always, the others when
// ChargeModes is empty or lists them.
func (d Device) AllowsChargeMode(mode string) bool {
	if mode == "" || mode == "off" || len(d.ChargeModes) == 0 {
		return true
	}
	return slices.Contains(d.ChargeModes, mode)
}
