// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"math"
	"net/http"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
)

// todayInfo are the day totals for the overview (from the load profile).
type todayInfo struct {
	Since          time.Time          `json:"since"` // start of the data (midnight, or later on the first day)
	PVWh           float64            `json:"pvWh"`
	LoadWh         float64            `json:"loadWh"`
	GridImportWh   float64            `json:"gridImportWh"`
	GridExportWh   float64            `json:"gridExportWh"`
	GridCostCt     float64            `json:"gridCostCt,omitempty"` // cost of the grid import (where prices are known)
	BatteryInWh    float64            `json:"batteryInWh"`          // charged into the home battery
	BatteryOutWh   float64            `json:"batteryOutWh"`         // discharged from the home battery
	PVByDevice     map[string]float64 `json:"pvByDevice,omitempty"` // generation per PV device
	Autarky        *float64           `json:"autarky,omitempty"`    // share of the load not from the grid
	SelfUse        *float64           `json:"selfUse,omitempty"`    // share of the PV used in the house
	ForecastRestWh float64            `json:"forecastRestWh"`       // PV still expected until midnight
	Covered        bool               `json:"covered"`              // data since midnight
}

// today returns the totals since midnight: PV, load, grid, autarky and the
// PV forecast for the rest of the day.
func (h *handler) today(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	loc, err := time.LoadLocation(cfg.Site.Timezone)
	if err != nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	sums, err := h.st.ProfileSums(r.Context(), nil, midnight, midnight.AddDate(0, 0, 1), loc, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Lastgang konnte nicht gelesen werden", err)
		return
	}
	pv := map[string]bool{}
	for _, id := range cfg.Roles.PV {
		pv[id] = true
	}
	var t todayInfo
	var batIn, batOut float64
	for _, s := range sums {
		switch {
		case pv[s.Asset]:
			t.PVWh += s.ExportWh
			if t.PVByDevice == nil {
				t.PVByDevice = map[string]float64{}
			}
			t.PVByDevice[s.Asset] += s.ExportWh
		case s.Asset == cfg.Roles.Grid+".grid":
			t.GridImportWh, t.GridExportWh, t.GridCostCt = s.ImportWh, s.ExportWh, s.CostCt
		case s.Asset == cfg.Roles.Battery+".battery":
			batIn, batOut = s.ImportWh, s.ExportWh
		}
	}
	t.BatteryInWh, t.BatteryOutWh = batIn, batOut
	t.LoadWh = math.Max(0, t.PVWh+t.GridImportWh-t.GridExportWh+batOut-batIn)
	if t.LoadWh > 0 {
		a := math.Max(0, math.Min(1, 1-t.GridImportWh/t.LoadWh))
		t.Autarky = &a
	}
	if t.PVWh > 0 {
		u := math.Max(0, math.Min(1, 1-t.GridExportWh/t.PVWh))
		t.SelfUse = &u
	}
	starts, _ := h.st.ProfileStarts(r.Context())
	t.Since = midnight
	if s, ok := starts[cfg.Roles.Grid+".grid"]; ok && s.After(midnight) {
		t.Since = s
	}
	t.Covered = !t.Since.After(midnight)
	// PV forecast for the rest of the day from the current plan
	for _, s := range h.rt.Plan().Slots {
		end := s.Start.Add(15 * time.Minute)
		if end.After(now) && s.Start.Before(midnight.AddDate(0, 0, 1)) {
			frac := 1.0
			if s.Start.Before(now) {
				frac = end.Sub(now).Hours() / 0.25
			}
			t.ForecastRestWh += s.PVW * 0.25 * frac
		}
	}
	if !auth.From(r.Context()).Forecast() {
		t.ForecastRestWh = 0 // the forecast is a permission of its own
	}
	writeJSON(w, http.StatusOK, t)
}
