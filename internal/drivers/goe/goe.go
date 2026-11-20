// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package goe implements the go-e Charger (HTTP API v2, local).
// The local HTTP API v2 must be enabled in the go-e app.
package goe

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() { sdk.Register("go-e", New) }

// Device is a go-e Charger.
type Device struct {
	cfg sdk.DeviceConfig
	url string
}

// New creates the device. Parameters: host (required) or url.
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	u := cfg.StringOr("url", "")
	if u == "" {
		host, err := cfg.String("host")
		if err != nil {
			return nil, err
		}
		u = "http://" + host
	}
	return &Device{cfg: cfg, url: u}, nil
}

func (d *Device) ID() string { return d.cfg.ID }

type status struct {
	Car int       `json:"car"` // 1 idle, 2 charging, 3 wait for car, 4 complete, 5 error
	Amp float64   `json:"amp"`
	Pnp int       `json:"pnp"` // active phases
	Nrg []float64 `json:"nrg"` // index 11 = total power W
}

// ChargerStatus implements sdk.Charger.
func (d *Device) ChargerStatus(ctx context.Context) (sdk.ChargerStatus, error) {
	var s status
	if err := httpx.GetJSON(ctx, d.url+"/api/status?filter=car,amp,pnp,nrg", &s); err != nil {
		return sdk.ChargerStatus{}, err
	}
	st := sdk.ChargerStatus{
		Connected: s.Car >= 2 && s.Car <= 4,
		Charging:  s.Car == 2,
		CurrentA:  s.Amp,
		Phases:    s.Pnp,
	}
	if len(s.Nrg) > 11 {
		st.PowerW = s.Nrg[11]
	}
	return st, nil
}

func (d *Device) set(ctx context.Context, key, value string) error {
	if d.cfg.Mode != sdk.ModeOwned {
		return fmt.Errorf("%s: control not allowed in mode %s", d.cfg.ID, d.cfg.Mode)
	}
	if d.cfg.DryRun {
		slog.Info("dry-run: would set go-e", "device", d.cfg.ID, key, value)
		return nil
	}
	var res map[string]any
	return httpx.GetJSON(ctx, d.url+"/api/set?"+url.Values{key: {value}}.Encode(), &res)
}

// EnableCharging implements sdk.ChargerController (frc: 1 = off, 0 = neutral).
func (d *Device) EnableCharging(ctx context.Context, enable bool) error {
	if enable {
		return d.set(ctx, "frc", "0")
	}
	return d.set(ctx, "frc", "1")
}

// SetCurrent implements sdk.ChargerController.
func (d *Device) SetCurrent(ctx context.Context, a float64) error {
	if a < 6 || a > 32 {
		return fmt.Errorf("%s: current %.1f A out of range 6..32", d.cfg.ID, a)
	}
	return d.set(ctx, "amp", fmt.Sprintf("%.0f", a))
}
