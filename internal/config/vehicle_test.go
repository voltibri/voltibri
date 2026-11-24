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
