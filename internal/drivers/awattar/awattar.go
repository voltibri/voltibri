// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package awattar provides day-ahead exchange prices (EPEX Spot DE-LU) via
// the public aWATTar API. Values are spot prices in ct/kWh without taxes or
// fees – used to detect negative prices (grid-friendly charging).
package awattar

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/drivers/httpx"
	"github.com/voltibri/voltibri/sdk"
)

func init() { sdk.Register("awattar", New) }

// Device is the aWATTar price source.
type Device struct {
	id  string
	url string

	mu      sync.Mutex
	slots   []sdk.Slot
	fetched time.Time
}

// New creates the price source. Parameter url (default https://api.awattar.de).
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	return &Device{id: cfg.ID, url: cfg.StringOr("url", "https://api.awattar.de")}, nil
}

func (d *Device) ID() string { return d.id }

type response struct {
	Data []struct {
		Start  int64   `json:"start_timestamp"` // ms
		End    int64   `json:"end_timestamp"`
		Market float64 `json:"marketprice"` // EUR/MWh
	} `json:"data"`
}

// Prices implements sdk.TariffSource.
func (d *Device) Prices(ctx context.Context, from, to time.Time) ([]sdk.Slot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.slots == nil || time.Since(d.fetched) > time.Hour {
		var r response
		u := fmt.Sprintf("%s/v1/marketdata?start=%d&end=%d", d.url, from.Add(-time.Hour).UnixMilli(), from.Add(48*time.Hour).UnixMilli())
		if err := httpx.GetJSON(ctx, u, &r); err != nil {
			if d.slots == nil {
				return nil, err
			}
		} else {
			var slots []sdk.Slot
			for _, p := range r.Data {
				s := time.UnixMilli(p.Start)
				slots = append(slots, sdk.Slot{Start: s, Duration: time.UnixMilli(p.End).Sub(s), Value: p.Market / 10})
			}
			d.slots, d.fetched = slots, time.Now()
		}
	}
	var out []sdk.Slot
	for _, s := range d.slots {
		if s.Start.Add(s.Duration).After(from) && s.Start.Before(to) {
			out = append(out, s)
		}
	}
	return out, nil
}
