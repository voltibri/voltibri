// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/pricing"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/tariff"
)

type recalcRequest struct {
	Vehicle string `json:"vehicle"`        // vehicle asset id
	From    string `json:"from,omitempty"` // YYYY-MM-DD (site time zone), empty: from the start
	To      string `json:"to,omitempty"`   // YYYY-MM-DD inclusive, empty: until now
	DryRun  bool   `json:"dryRun"`
}

type recalcResponse struct {
	Runs     int     `json:"runs"`     // runs recalculated
	Skipped  int     `json:"skipped"`  // without known prices for the period
	BeforeCt float64 `json:"beforeCt"` // total cost before
	AfterCt  float64 `json:"afterCt"`  // total cost with the current rule
	DryRun   bool    `json:"dryRun"`
}

// Original cost of a run before its first recalculation (kept in attrs, so
// imported costs are not lost).
const (
	attrOrigCost = "origCostCt"
	attrOrigNote = "origPriceNote"
)

// recalcRuns applies the current price rule of a vehicle to its charging
// runs in a period (POST /api/runs/recalc). Runs measured by voltibri keep
// their exact shares; others (imports) are priced with the average tariff
// of their time.
func (h *handler) recalcRuns(w http.ResponseWriter, r *http.Request) {
	var req recalcRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<12))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Vehicle == "" {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage: Fahrzeug fehlt", err)
		return
	}
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	d, ok := cfg.Device(req.Vehicle)
	if !ok || !d.IsVehicle() {
		writeErr(w, http.StatusNotFound, "Unbekanntes Fahrzeug", nil)
		return
	}
	loc, err := time.LoadLocation(cfg.Site.Timezone)
	if err != nil {
		loc = time.Local
	}
	// average prices of a period: from the running site (dynamic tariffs
	// with their price source), else fixed tariffs from the configuration
	prices := func(ctx context.Context, from, to time.Time) (float64, float64) {
		if m := h.rt.Charging(); m != nil {
			return m.AveragePrices(ctx, from, to)
		}
		feed, _ := tariff.New(cfg.Tariffs.FeedIn, loc, nil)
		grid, _ := tariff.New(cfg.Tariffs.Grid, loc, nil)
		return tariff.Average(ctx, feed, from, to), tariff.Average(ctx, grid, from, to)
	}
	f := run.Filter{Kind: config.RunCharging, Limit: 1 << 30, All: true}
	f.Vehicle = d.Name
	if f.Vehicle == "" {
		f.Vehicle = d.ID
	}
	for n, v := range cfg.VehicleNames() {
		if v == f.Vehicle && n != v {
			f.Vehicles = append(f.Vehicles, n)
		}
	}
	if req.From != "" {
		if f.From, err = time.ParseInLocation(time.DateOnly, req.From, loc); err != nil {
			writeErr(w, http.StatusBadRequest, "Ungültiges Datum (from)", err)
			return
		}
	}
	if req.To != "" {
		t, err := time.ParseInLocation(time.DateOnly, req.To, loc)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Ungültiges Datum (to)", err)
			return
		}
		f.To = t.AddDate(0, 0, 1)
	}
	runs, err := h.st.Runs(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Läufe konnten nicht gelesen werden", err)
		return
	}
	res := recalcResponse{DryRun: req.DryRun}
	var changed []run.Run
	for _, rn := range runs {
		if rn.Guest {
			continue
		}
		sh, exact := pricing.FromAttrs(rn.Attrs)
		if !exact {
			feedin, grid := prices(r.Context(), rn.Start, rn.End)
			if math.IsNaN(feedin) || math.IsNaN(grid) {
				res.Skipped++
				continue
			}
			sh = pricing.Uniform(rn.PVWh, rn.BatteryWh, rn.GridWh, feedin, grid)
		}
		cost, note := d.Price.Cost(sh, f.Vehicle)
		if d.Price.Empty() {
			note = "PV und Akku zur Einspeisevergütung, Netz zum Tarif"
		}
		if !exact {
			note += " (Durchschnittspreis des Zeitraums)"
		}
		res.Runs++
		res.BeforeCt += rn.CostCt
		res.AfterCt += cost
		if rn.Attrs == nil {
			rn.Attrs = map[string]any{}
		}
		if _, kept := rn.Attrs[attrOrigCost]; !kept {
			rn.Attrs[attrOrigNote] = rn.PriceNote
			if rn.CostUnknown {
				rn.Attrs[attrOrigCost] = nil
			} else {
				rn.Attrs[attrOrigCost] = rn.CostCt
			}
		}
		rn.CostCt, rn.CostUnknown, rn.PriceNote = cost, false, note
		changed = append(changed, rn)
	}
	if !req.DryRun && len(changed) > 0 {
		if err := h.st.UpdateRunCosts(r.Context(), changed); err != nil {
			writeErr(w, http.StatusInternalServerError, "Speichern fehlgeschlagen", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, res)
}
