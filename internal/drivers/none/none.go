// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package none is the "driver" of assets without a connection: retired
// devices (an old wallbox, removed heating rods) or devices voltibri does
// not measure. They only carry history (runs, imported data).
package none

import "github.com/voltibri/voltibri/sdk"

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:        "none",
		Name:        "Ohne Verbindung",
		Description: "Für stillgelegte Geräte oder Geräte ohne Messung – nur für Historie und Auswertungen, voltibri fragt nichts ab.",
		Category:    sdk.CatNone,
		Params:      []sdk.Param{},
		Factory:     func(c sdk.DeviceConfig) (sdk.Device, error) { return device(c.ID), nil },
	})
}

type device string

func (d device) ID() string { return string(d) }
