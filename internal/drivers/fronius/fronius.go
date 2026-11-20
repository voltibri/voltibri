// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package fronius implements a Fronius GEN24 / Symo driver.
//
// Measurements are read via the Fronius Solar API (HTTP, read only).
// Battery control uses Modbus TCP with the SunSpec storage model 124
// (StorCtl_Mod / InWRte with revert timeout). Requirements on the inverter:
// Modbus TCP enabled, "Inverter control via Modbus" allowed, SunSpec model
// type "int + SF".
//
// UNTESTED AGAINST REAL HARDWARE. Always start with dryRun: true and check
// the log output before letting voltibri write to the inverter.
package fronius

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() { sdk.Register("fronius", New) }

// Device is a Fronius inverter with optional battery and smart meter.
type Device struct {
	cfg        sdk.DeviceConfig
	baseURL    string
	capacityWh float64
	ss         *sunspec // nil if battery control is not configured
	revert     time.Duration

	mu     sync.Mutex
	cache  powerFlow
	cached time.Time
}

// New creates the device. Parameters:
//
//	host:        inverter IP or hostname (required)
//	capacityWh:  usable battery capacity (required for battery features)
//	modbusPort:  Modbus TCP port (default 502); set 0 to disable control
//	unitID:      Modbus unit id (default 1)
//	revert:      revert timeout for limits (default "5m")
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	host, err := cfg.String("host")
	if err != nil {
		return nil, err
	}
	d := &Device{
		cfg:        cfg,
		baseURL:    cfg.StringOr("url", "http://"+host),
		capacityWh: cfg.FloatOr("capacityWh", 0),
		revert:     cfg.DurationOr("revert", 5*time.Minute),
	}
	if port := int(cfg.FloatOr("modbusPort", 502)); port > 0 && cfg.Mode == sdk.ModeOwned {
		d.ss = newSunspec(fmt.Sprintf("tcp://%s:%d", host, port), uint8(cfg.FloatOr("unitID", 1)))
	}
	return d, nil
}

func (d *Device) ID() string { return d.cfg.ID }

type powerFlow struct {
	Body struct {
		Data struct {
			Site struct {
				PAkku *float64 `json:"P_Akku"` // + discharge, - charge
				PGrid *float64 `json:"P_Grid"` // + import, - export
				PLoad *float64 `json:"P_Load"` // negative = consumption
				PPV   *float64 `json:"P_PV"`
			} `json:"Site"`
			Inverters map[string]struct {
				SOC *float64 `json:"SOC"`
			} `json:"Inverters"`
		} `json:"Data"`
	} `json:"Body"`
}

// flow returns the realtime power flow, cached for 2 s so that the
// capability getters share one HTTP request per control cycle.
func (d *Device) flow(ctx context.Context) (powerFlow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.cached) < 2*time.Second {
		return d.cache, nil
	}
	var pf powerFlow
	if err := httpx.GetJSON(ctx, d.baseURL+"/solar_api/v1/GetPowerFlowRealtimeData.fcgi", &pf); err != nil {
		return pf, err
	}
	d.cache, d.cached = pf, time.Now()
	return pf, nil
}

func val(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// GridPower implements sdk.GridMeter.
func (d *Device) GridPower(ctx context.Context) (float64, error) {
	pf, err := d.flow(ctx)
	return val(pf.Body.Data.Site.PGrid), err
}

// PVPower implements sdk.PVProducer.
func (d *Device) PVPower(ctx context.Context) (float64, error) {
	pf, err := d.flow(ctx)
	return math.Max(0, val(pf.Body.Data.Site.PPV)), err
}

// SoC implements sdk.Battery.
func (d *Device) SoC(ctx context.Context) (float64, error) {
	pf, err := d.flow(ctx)
	if err != nil {
		return 0, err
	}
	for _, inv := range pf.Body.Data.Inverters {
		if inv.SOC != nil {
			return *inv.SOC / 100, nil
		}
	}
	return 0, fmt.Errorf("%s: no battery SoC reported", d.cfg.ID)
}

// BatteryPower implements sdk.Battery.
func (d *Device) BatteryPower(ctx context.Context) (float64, error) {
	pf, err := d.flow(ctx)
	return val(pf.Body.Data.Site.PAkku), err
}

// CapacityWh implements sdk.Battery.
func (d *Device) CapacityWh() float64 { return d.capacityWh }

// SetChargeLimit implements sdk.BatteryController.
func (d *Device) SetChargeLimit(ctx context.Context, w float64) error {
	if d.ss == nil {
		return fmt.Errorf("%s: battery control disabled (mode %s or modbusPort 0)", d.cfg.ID, d.cfg.Mode)
	}
	if d.cfg.DryRun {
		slog.Info("dry-run: would limit battery charging", "device", d.cfg.ID, "watts", w, "revert", d.revert)
		return nil
	}
	return d.ss.setChargeLimit(w, d.revert)
}

// ResetLimits implements sdk.BatteryController.
func (d *Device) ResetLimits(ctx context.Context) error {
	if d.ss == nil {
		return nil
	}
	if d.cfg.DryRun {
		slog.Info("dry-run: would reset battery limits", "device", d.cfg.ID)
		return nil
	}
	return d.ss.reset()
}
