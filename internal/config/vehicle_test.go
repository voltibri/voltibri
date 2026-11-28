// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"

	"github.com/voltibri/voltibri/sdk"
)

func TestVehicleBindingAndNames(t *testing.T) {
	sdk.RegisterDriver(sdk.Driver{Type: "test-account", Capabilities: []sdk.Capability{sdk.CapVehicles},
		Factory: func(sdk.DeviceConfig) (sdk.Device, error) { return nil, nil }})
	c := &Config{Devices: []Device{
		{ID: "acct", Type: "test-account"},
		{ID: "tav", Type: "vehicle", Name: "Tavascan", Params: map[string]any{"source": "acct", "sourceVehicle": "v1", "aliases": "Cupra Tavascan, Tavi"}},
		{ID: "id3", Type: "vehicle", Name: "ID.3", Params: map[string]any{"source": "acct", "sourceVehicle": "v1"}},
		{ID: "bad", Type: "vehicle", Params: map[string]any{"source": "tav", "sourceVehicle": "x"}},
	}}
	if b := c.Devices[1].Vehicle(); b.Source != "acct" || len(b.Aliases) != 2 || b.Aliases[1] != "Tavi" {
		t.Fatalf("binding: %+v", b)
	}
	n := c.VehicleNames()
	if n["Cupra Tavascan"] != "Tavascan" || n["Tavascan"] != "Tavascan" || n["ID.3"] != "ID.3" {
		t.Fatalf("names: %v", n)
	}
	errs := c.validateVehicles()
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "already") || !strings.Contains(errs[1].Error(), "not a vehicle account") {
		t.Fatalf("errors: %v", errs)
	}
}

func TestVehicleChargeModes(t *testing.T) {
	c := &Config{Devices: []Device{
		{ID: "car", Type: "vehicle", ChargeModes: []string{"pv", "fast"}},
		{ID: "bad", Type: "vehicle", ChargeModes: []string{"pv", "turbo", "pv"}},
	}}
	if d := c.Devices[0]; !d.AllowsChargeMode("fast") || !d.AllowsChargeMode("off") || !d.AllowsChargeMode("") || d.AllowsChargeMode("boost") {
		t.Fatal("car: pv, fast and off allowed, boost not")
	}
	if !(Device{Type: "vehicle"}).AllowsChargeMode("boost") {
		t.Fatal("no list: every mode allowed")
	}
	errs := c.validateVehicles()
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), `unknown mode "turbo"`) || !strings.Contains(errs[1].Error(), `"pv" twice`) {
		t.Fatalf("errors: %v", errs)
	}
	c.Charging.Cards = []Card{{Card: "A", Vehicle: "car", Mode: "boost"}, {Card: "B", Vehicle: "car", Mode: "off"}, {Card: "C", Vehicle: "guest", Mode: "boost"}}
	errs = c.validateCards()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `card "A": mode "boost" is not allowed for vehicle car`) {
		t.Fatalf("card errors: %v", errs)
	}
}
