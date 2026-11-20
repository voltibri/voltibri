// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads the voltibri YAML configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/sdk"
)

// Config is the root configuration.
type Config struct {
	Site    Site           `yaml:"site"`
	Battery Battery        `yaml:"battery"`
	Roles   Roles          `yaml:"roles"`
	Planner planner.Config `yaml:"planner"`
	Devices []Device       `yaml:"devices"`
}

// Site holds global settings.
type Site struct {
	Timezone string   `yaml:"timezone"`
	Listen   string   `yaml:"listen"`
	History  string   `yaml:"history"` // CSV file with 15-minute history
	DryRun   bool     `yaml:"dryRun"`  // never write to devices
	Holidays []string `yaml:"holidays"`
}

// Battery describes the stationary battery.
type Battery struct {
	CapacityWh    float64 `yaml:"capacityWh"`
	MaxChargeW    float64 `yaml:"maxChargeW"`
	MaxDischargeW float64 `yaml:"maxDischargeW"`
	MinSoC        float64 `yaml:"minSoC"`
	ChargeEff     float64 `yaml:"chargeEfficiency"`
	DischargeEff  float64 `yaml:"dischargeEfficiency"`
}

// Roles map site functions to device IDs.
type Roles struct {
	Grid       string   `yaml:"grid"`
	PV         []string `yaml:"pv"`
	Battery    string   `yaml:"battery"`
	PVForecast string   `yaml:"pvForecast"`
	Tariff     string   `yaml:"tariff"`
}

// Device is one configured device.
type Device struct {
	ID     string         `yaml:"id"`
	Type   string         `yaml:"type"`
	Mode   string         `yaml:"mode"`
	Params map[string]any `yaml:"params"`
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Site:    Site{Timezone: "Europe/Berlin", Listen: "127.0.0.1:7070", History: "voltibri-history.csv", DryRun: true},
		Battery: Battery{MaxChargeW: 5000, MaxDischargeW: 5000, MinSoC: 0.05, ChargeEff: 0.95, DischargeEff: 0.95},
		Planner: planner.DefaultConfig(),
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	ids := map[string]bool{}
	for i, d := range c.Devices {
		if d.ID == "" || d.Type == "" {
			return fmt.Errorf("device #%d: id and type are required", i+1)
		}
		if ids[d.ID] {
			return fmt.Errorf("duplicate device id %q", d.ID)
		}
		ids[d.ID] = true
		switch sdk.ControlMode(d.Mode) {
		case "":
			c.Devices[i].Mode = string(sdk.ModeObserved)
		case sdk.ModeOwned, sdk.ModeDelegated, sdk.ModeObserved:
		default:
			return fmt.Errorf("device %q: unknown mode %q (owned, delegated, observed)", d.ID, d.Mode)
		}
	}
	check := func(role, id string) error {
		if id != "" && !ids[id] {
			return fmt.Errorf("roles.%s: unknown device %q", role, id)
		}
		return nil
	}
	for _, id := range c.Roles.PV {
		if err := check("pv", id); err != nil {
			return err
		}
	}
	for role, id := range map[string]string{"grid": c.Roles.Grid, "battery": c.Roles.Battery, "pvForecast": c.Roles.PVForecast, "tariff": c.Roles.Tariff} {
		if err := check(role, id); err != nil {
			return err
		}
	}
	if c.Roles.Grid == "" {
		return fmt.Errorf("roles.grid is required")
	}
	if c.Roles.Battery != "" && c.Battery.CapacityWh <= 0 {
		return fmt.Errorf("battery.capacityWh is required when roles.battery is set")
	}
	return nil
}
