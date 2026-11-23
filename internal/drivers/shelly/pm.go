// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package shelly

import (
	"context"
	"fmt"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:         "shelly-pm",
		Name:         "Shelly PM (Zähler ohne Relais)",
		Description:  "Einphasiger Shelly-Leistungsmesser, z. B. Shelly PM Mini Gen3 oder Plus PM Mini (RPC-Komponente PM1).",
		Category:     sdk.CatMeter,
		Capabilities: []sdk.Capability{sdk.CapConsumer, sdk.CapPhases, sdk.CapEnergy},
		Params: []sdk.Param{
			{Name: "host", Label: "IP-Adresse / Hostname", Type: sdk.TypeHost},
			{Name: "channel", Label: "Kanal", Type: sdk.TypeInt, Default: 0.0, Min: sdk.Ptr(0), Max: sdk.Ptr(3), Advanced: true},
			{Name: "url", Label: "URL", Type: sdk.TypeURL, Advanced: true},
		},
		Factory: NewPM,
	})
}

// PM is a Shelly power meter channel (PM1 component).
type PM struct {
	id      string
	url     string
	channel int
}

// NewPM creates the meter. Parameters: host or url, channel (default 0).
func NewPM(cfg sdk.DeviceConfig) (sdk.Device, error) {
	u := cfg.StringOr("url", "")
	if u == "" {
		host, err := cfg.String("host")
		if err != nil {
			return nil, err
		}
		u = "http://" + host
	}
	return &PM{id: cfg.ID, url: u, channel: int(cfg.FloatOr("channel", 0))}, nil
}

func (p *PM) ID() string { return p.id }

type pmStatus struct {
	APower  float64 `json:"apower"`
	Current float64 `json:"current"`
	AEnergy struct {
		Total float64 `json:"total"` // Wh
	} `json:"aenergy"`
}

func (p *PM) status(ctx context.Context) (pmStatus, error) {
	var s pmStatus
	err := httpx.GetJSON(ctx, fmt.Sprintf("%s/rpc/PM1.GetStatus?id=%d", p.url, p.channel), &s)
	return s, err
}

// ConsumerPower implements sdk.PowerConsumer.
func (p *PM) ConsumerPower(ctx context.Context) (float64, error) {
	s, err := p.status(ctx)
	return s.APower, err
}

// Currents implements sdk.PhaseCurrents (single phase: L1).
func (p *PM) Currents(ctx context.Context) (sdk.Phases, error) {
	s, err := p.status(ctx)
	return sdk.Phases{s.Current, 0, 0}, err
}

// EnergyWh implements sdk.EnergyMeter.
func (p *PM) EnergyWh(ctx context.Context) (float64, error) {
	s, err := p.status(ctx)
	return s.AEnergy.Total, err
}
