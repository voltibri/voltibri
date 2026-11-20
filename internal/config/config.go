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

	"gopkg.in/yaml.v3"

	"github.com/voltibri/voltibri/internal/meter"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/sdk"
)

// Config is the root configuration.
type Config struct {
	Site    Site           `yaml:"site" json:"site"`
	Battery Battery        `yaml:"battery" json:"battery"`
	Roles   Roles          `yaml:"roles" json:"roles"`
	Planner planner.Config `yaml:"planner" json:"planner"`
	Devices []Device       `yaml:"devices" json:"devices"`
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
	Name       string           `yaml:"name,omitempty" json:"name,omitempty"` // display name, e.g. "Dach Süd"
	Icon       string           `yaml:"icon,omitempty" json:"icon,omitempty"` // solar, balcony, car, heatpump, plug, water, house
	Type       string           `yaml:"type" json:"type"`
	Mode       string           `yaml:"mode,omitempty" json:"mode,omitempty"`
	Correction meter.Correction `yaml:"correction,omitempty" json:"correction,omitempty"`
	Params     map[string]any   `yaml:"params,omitempty" json:"params,omitempty"`
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
	if c.Roles.Battery != "" && c.Battery.CapacityWh <= 0 {
		errs = append(errs, errors.New("battery.capacityWh is required when roles.battery is set"))
	}
	return errors.Join(errs...)
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
