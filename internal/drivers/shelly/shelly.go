// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package shelly implements Shelly Gen2+ switches (Plus/Pro/Gen3) via the
// local RPC API (Switch.GetStatus / Switch.Set).
package shelly

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() { sdk.Register("shelly", New) }

// Device is one switch channel of a Shelly device.
type Device struct {
	cfg     sdk.DeviceConfig
	url     string
	channel int
}

// New creates the device. Parameters: host (required) or url, channel (default 0).
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	u := cfg.StringOr("url", "")
	if u == "" {
		host, err := cfg.String("host")
		if err != nil {
			return nil, err
		}
		u = "http://" + host
	}
	return &Device{cfg: cfg, url: u, channel: int(cfg.FloatOr("channel", 0))}, nil
}

func (d *Device) ID() string { return d.cfg.ID }

type switchStatus struct {
	Output bool    `json:"output"`
	APower float64 `json:"apower"`
}

func (d *Device) status(ctx context.Context) (switchStatus, error) {
	var s switchStatus
	err := httpx.GetJSON(ctx, fmt.Sprintf("%s/rpc/Switch.GetStatus?id=%d", d.url, d.channel), &s)
	return s, err
}

// SwitchState implements sdk.Switch.
func (d *Device) SwitchState(ctx context.Context) (bool, error) {
	s, err := d.status(ctx)
	return s.Output, err
}

// ConsumerPower implements sdk.PowerConsumer.
func (d *Device) ConsumerPower(ctx context.Context) (float64, error) {
	s, err := d.status(ctx)
	return s.APower, err
}

// SetSwitch implements sdk.Switch.
func (d *Device) SetSwitch(ctx context.Context, on bool) error {
	if d.cfg.Mode != sdk.ModeOwned {
		return fmt.Errorf("%s: control not allowed in mode %s", d.cfg.ID, d.cfg.Mode)
	}
	if d.cfg.DryRun {
		slog.Info("dry-run: would switch shelly", "device", d.cfg.ID, "on", on)
		return nil
	}
	var res map[string]any
	return httpx.GetJSON(ctx, fmt.Sprintf("%s/rpc/Switch.Set?id=%d&on=%t", d.url, d.channel, on), &res)
}
