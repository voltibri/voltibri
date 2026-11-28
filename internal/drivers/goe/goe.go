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
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:         "go-e",
		Name:         "go-e Charger",
		Description:  "Wallbox über die lokale HTTP API v2 (in der go-e App aktivieren).",
		Category:     sdk.CatCharger,
		Capabilities: []sdk.Capability{sdk.CapCharger, sdk.CapChargerControl, sdk.CapPhases, sdk.CapPhaseSwitch, sdk.CapEnergy},
		Params: []sdk.Param{
			{Name: "host", Label: "IP-Adresse / Hostname", Type: sdk.TypeHost},
			{Name: "url", Label: "URL", Type: sdk.TypeURL, Advanced: true},
		},
		Factory: New,
	})
}

// Device is a go-e Charger.
type Device struct {
	cfg sdk.DeviceConfig
	url string

	mu      sync.Mutex
	cards   []string // names by index, cached
	cardsAt time.Time
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
	Car   int       `json:"car"` // 1 idle, 2 charging, 3 wait for car, 4 complete, 5 error
	Amp   float64   `json:"amp"`
	Pnp   int       `json:"pnp"` // phases the charger has released (not what the car draws)
	Nrg   []float64 `json:"nrg"` // 4..6 = current L1..L3 A, 11 = total power W
	Eto   float64   `json:"eto"` // total energy Wh
	Trx   *int      `json:"trx"` // transaction: null none, 0 without card, n = card n (1-based)
	Cards []struct {
		Name   string `json:"name"`
		CardID bool   `json:"cardId"` // a card is taught in
	} `json:"cards"`
}

// drawA is the current from which a phase counts as drawn (the car's
// standby on a phase stays below).
const drawA = 1.0

// Currents implements sdk.PhaseCurrents.
func (d *Device) Currents(ctx context.Context) (sdk.Phases, error) {
	var s status
	if err := httpx.GetJSON(ctx, d.url+"/api/status?filter=nrg", &s); err != nil {
		return sdk.Phases{}, err
	}
	if len(s.Nrg) < 7 {
		return sdk.Phases{}, fmt.Errorf("%s: unexpected nrg array", d.cfg.ID)
	}
	return sdk.Phases{s.Nrg[4], s.Nrg[5], s.Nrg[6]}, nil
}

// EnergyWh implements sdk.EnergyMeter.
func (d *Device) EnergyWh(ctx context.Context) (float64, error) {
	var s status
	err := httpx.GetJSON(ctx, d.url+"/api/status?filter=eto", &s)
	return s.Eto, err
}

// SetPhases implements sdk.PhaseSwitcher (psm: 1 = single, 2 = three phase).
func (d *Device) SetPhases(ctx context.Context, phases int) error {
	switch phases {
	case 1:
		return d.set(ctx, "psm", "1")
	case 3:
		return d.set(ctx, "psm", "2")
	}
	return fmt.Errorf("%s: phases must be 1 or 3", d.cfg.ID)
}

// ChargerStatus implements sdk.Charger.
func (d *Device) ChargerStatus(ctx context.Context) (sdk.ChargerStatus, error) {
	var s status
	if err := httpx.GetJSON(ctx, d.url+"/api/status?filter=car,amp,pnp,nrg,trx", &s); err != nil {
		return sdk.ChargerStatus{}, err
	}
	st := sdk.ChargerStatus{
		Connected: s.Car >= 2 && s.Car <= 4,
		Charging:  s.Car == 2,
		CurrentA:  s.Amp,
	}
	if len(s.Nrg) > 11 {
		st.PowerW = s.Nrg[11]
	}
	// the phases the car draws on, from the current per phase: pnp counts
	// the released phases, a car with a 2-phase charger draws on fewer
	if st.Charging && len(s.Nrg) > 6 {
		for _, a := range s.Nrg[4:7] {
			if a >= drawA {
				st.Phases++
			}
		}
	}
	if s.Trx != nil && *s.Trx > 0 && st.Connected {
		if names, err := d.cardNames(ctx); err == nil && *s.Trx-1 < len(names) {
			st.Card = names[*s.Trx-1]
		}
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

// cardNames returns the names of all card slots (cached for 10 minutes);
// unnamed cards are called "Karte n".
func (d *Device) cardNames(ctx context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cards != nil && time.Since(d.cardsAt) < 10*time.Minute {
		return d.cards, nil
	}
	var s status
	if err := httpx.GetJSON(ctx, d.url+"/api/status?filter=cards", &s); err != nil {
		return nil, err
	}
	names := make([]string, len(s.Cards))
	for i, c := range s.Cards {
		names[i] = c.Name
		if c.Name == "" || c.Name == "n/a" {
			names[i] = fmt.Sprintf("Karte %d", i+1)
		}
	}
	d.cards, d.cardsAt = names, time.Now()
	return names, nil
}

// Cards implements sdk.CardLister: the taught-in cards.
func (d *Device) Cards(ctx context.Context) ([]string, error) {
	var s status
	if err := httpx.GetJSON(ctx, d.url+"/api/status?filter=cards", &s); err != nil {
		return nil, err
	}
	out := []string{}
	for i, c := range s.Cards {
		if !c.CardID {
			continue
		}
		n := c.Name
		if n == "" || n == "n/a" {
			n = fmt.Sprintf("Karte %d", i+1)
		}
		out = append(out, n)
	}
	return out, nil
}
