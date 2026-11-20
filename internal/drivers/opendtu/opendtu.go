// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package opendtu reads the total AC power of Hoymiles micro inverters via
// the OpenDTU REST API (GET /api/livedata/status). Read only for now;
// power limiting (curtailment at negative prices) will follow.
package opendtu

import (
	"context"
	"math"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:         "opendtu",
		Name:         "OpenDTU (Hoymiles)",
		Description:  "Summenleistung aller Mikrowechselrichter über die OpenDTU-REST-API.",
		Category:     sdk.CatInverter,
		Capabilities: []sdk.Capability{sdk.CapPV},
		Params: []sdk.Param{
			{Name: "host", Label: "IP-Adresse / Hostname", Type: sdk.TypeHost},
			{Name: "url", Label: "URL", Type: sdk.TypeURL, Advanced: true, Description: "Alternativ zur IP-Adresse, z. B. hinter einem Reverse Proxy."},
		},
		Factory: New,
	})
}

// Device is an OpenDTU instance.
type Device struct {
	id  string
	url string
}

// New creates the device. Parameters: host (required) or url.
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	url := cfg.StringOr("url", "")
	if url == "" {
		host, err := cfg.String("host")
		if err != nil {
			return nil, err
		}
		url = "http://" + host
	}
	return &Device{id: cfg.ID, url: url}, nil
}

func (d *Device) ID() string { return d.id }

type status struct {
	Total struct {
		Power struct {
			V float64 `json:"v"`
		} `json:"Power"`
	} `json:"total"`
}

// PVPower implements sdk.PVProducer.
func (d *Device) PVPower(ctx context.Context) (float64, error) {
	var s status
	if err := httpx.GetJSON(ctx, d.url+"/api/livedata/status", &s); err != nil {
		return 0, err
	}
	return math.Max(0, s.Total.Power.V), nil
}
