// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/run"
)

// EvccSession is one charging session as returned by evcc's
// GET /api/sessions. Fields evcc does not document are optional.
type EvccSession struct {
	ID              int64    `json:"id"`
	Created         evccTime `json:"created"`
	Finished        evccTime `json:"finished"`
	Loadpoint       string   `json:"loadpoint"` // loadpoint title
	Vehicle         string   `json:"vehicle"`   // vehicle title, "" = unknown
	Odometer        *float64 `json:"odometer"`
	ChargedEnergy   float64  `json:"chargedEnergy"` // kWh
	SolarPercentage *float64 `json:"solarPercentage"`
	Price           *float64 `json:"price"` // total, currency units
	PricePerKWh     *float64 `json:"pricePerKWh"`
	ChargeDuration  *float64 `json:"chargeDuration"` // nanoseconds of actual charging
}

// evccTime accepts RFC 3339, null and Go's zero time (unfinished sessions).
type evccTime struct{ time.Time }

func (t *evccTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		t.Time = time.Time{}
		return nil
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	if v.Year() < 2000 {
		v = time.Time{}
	}
	t.Time = v
	return nil
}

var evccClient = &http.Client{Timeout: 30 * time.Second}

// FetchEvcc reads all charging sessions from an evcc instance.
func FetchEvcc(ctx context.Context, baseURL string) ([]EvccSession, error) {
	u := strings.TrimRight(baseURL, "/") + "/api/sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := evccClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("evcc nicht erreichbar: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("evcc verlangt eine Anmeldung (HTTP %d) – das unterstützt der Import noch nicht", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("evcc antwortet mit HTTP %d auf %s", resp.StatusCode, u)
	}
	return ParseEvccSessions(body)
}

// ParseEvccSessions decodes the response of /api/sessions (plain array or,
// in older versions, wrapped in {"result": [...]}).
func ParseEvccSessions(b []byte) ([]EvccSession, error) {
	var list []EvccSession
	if err := json.Unmarshal(b, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Result []EvccSession `json:"result"`
	}
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return nil, fmt.Errorf("unerwartete Antwort von evcc: %w", err)
	}
	return wrapped.Result, nil
}

// Asset is a voltibri device runs can be assigned to.
type Asset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Mapping targets besides a charger id.
const (
	Skip = "-" // do not import sessions of this evcc loadpoint
	Keep = "=" // import with the evcc title as loadpoint name
)

// LoadpointMatch tells which voltibri charger an evcc loadpoint maps to.
type LoadpointMatch struct {
	Title    string `json:"title"`    // evcc loadpoint title
	Sessions int    `json:"sessions"` // new sessions with this title
	Target   string `json:"target"`   // voltibri charger id, Keep (evcc title) or Skip
	Auto     bool   `json:"auto"`     // found by name, not given by the user
	RunKind  string `json:"runKind"`  // kind of the imported runs (charging, heating, run)
}

// EvccPreview is what an import would do.
type EvccPreview struct {
	Total      int              `json:"total"`      // sessions evcc returned
	New        int              `json:"new"`        // would be imported
	Duplicate  int              `json:"duplicate"`  // imported before
	Unfinished int              `json:"unfinished"` // still running in evcc, skipped
	Excluded   int              `json:"excluded"`   // loadpoints set to Skip
	WithPrice  int              `json:"withPrice"`  // new sessions with a price
	EnergyWh   float64          `json:"energyWh"`   // of the new sessions
	From       time.Time        `json:"from,omitzero"`
	To         time.Time        `json:"to,omitzero"`
	Loadpoints []LoadpointMatch `json:"loadpoints"`
	Runs       []run.Run        `json:"-"`
}

// matchLoadpoint finds the voltibri charger for an evcc title: exact name or
// id, then a unique partial match ("Carport" -> "Wallbox Carport").
func matchLoadpoint(title string, assets []Asset) string {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return ""
	}
	for _, lp := range assets {
		if strings.ToLower(lp.Name) == t || strings.ToLower(lp.ID) == t {
			return lp.ID
		}
	}
	found := ""
	for _, lp := range assets {
		n, id := strings.ToLower(lp.Name), strings.ToLower(lp.ID)
		if (n != "" && (strings.Contains(n, t) || strings.Contains(t, n))) || strings.Contains(id, t) {
			if found != "" && found != lp.ID {
				return "" // ambiguous
			}
			found = lp.ID
		}
	}
	return found
}

// PlanEvcc converts evcc sessions. known holds external ids imported
// before; mapping (evcc title -> charger id, Keep or Skip) overrides the
// automatic match. evcc loadpoints without a matching charger (heat pumps,
// dehumidifiers ... set up as loadpoints) are skipped unless mapped.
func PlanEvcc(list []EvccSession, known map[string]bool, assets []Asset, mapping map[string]string) EvccPreview {
	p := EvccPreview{Total: len(list)}
	match := map[string]*LoadpointMatch{}
	for _, e := range list {
		if e.Created.IsZero() || e.Finished.IsZero() {
			p.Unfinished++
			continue
		}
		ext := strconv.FormatInt(e.ID, 10)
		if e.ID == 0 {
			ext = fmt.Sprintf("%d/%s", e.Created.Unix(), e.Loadpoint)
		}
		if known[ext] {
			p.Duplicate++
			continue
		}
		m, ok := match[e.Loadpoint]
		if !ok {
			m = &LoadpointMatch{Title: e.Loadpoint}
			if t, given := mapping[e.Loadpoint]; given {
				m.Target = t
			} else if t := matchLoadpoint(e.Loadpoint, assets); t != "" {
				m.Target, m.Auto = t, true
			} else {
				m.Target, m.Auto = Skip, true
			}
			m.RunKind = runKind(m.Target, e, assets)
			match[e.Loadpoint] = m
		}
		m.Sessions++
		if m.Target == Skip {
			p.Excluded++
			continue
		}
		asset := m.Target
		if asset == Keep || asset == "" {
			asset = e.Loadpoint
		}
		c := run.Run{Asset: asset, Kind: m.RunKind, Start: e.Created.Time, End: e.Finished.Time, EnergyWh: e.ChargedEnergy * 1000,
			ExternalID: ext, Source: "evcc", Attrs: map[string]any{"evccLoadpoint": e.Loadpoint}}
		if e.Odometer != nil {
			c.Attrs["odometerKm"] = *e.Odometer
		}
		if e.ChargeDuration != nil {
			c.Attrs["activeSeconds"] = *e.ChargeDuration / 1e9
		}
		if c.Kind == config.RunCharging { // evcc names heaters as "vehicles" – only cars are vehicles here
			if e.Vehicle == "" {
				c.Vehicle, c.VehicleName, c.Guest = charging.Guest, "Gastfahrzeug", true
			} else {
				c.Vehicle, c.VehicleName = "evcc:"+e.Vehicle, e.Vehicle
			}
		}
		// evcc only knows the self-produced share (PV incl. battery)
		c.GridWh = c.EnergyWh
		if e.SolarPercentage != nil {
			c.PVWh = c.EnergyWh * *e.SolarPercentage / 100
			c.GridWh = c.EnergyWh - c.PVWh
		}
		c.PriceNote = "aus evcc übernommen"
		switch {
		case e.Price != nil:
			c.CostCt = *e.Price * 100
		case e.PricePerKWh != nil:
			c.CostCt = *e.PricePerKWh * 100 * e.ChargedEnergy
		default:
			c.CostUnknown = true
		}
		if !c.CostUnknown {
			p.WithPrice++
		}
		p.New++
		p.EnergyWh += c.EnergyWh
		if p.From.IsZero() || c.Start.Before(p.From) {
			p.From = c.Start
		}
		if c.Start.After(p.To) {
			p.To = c.Start
		}
		p.Runs = append(p.Runs, c)
	}
	p.Loadpoints = []LoadpointMatch{}
	for _, m := range match {
		p.Loadpoints = append(p.Loadpoints, *m)
	}
	sort.Slice(p.Loadpoints, func(i, j int) bool { return p.Loadpoints[i].Title < p.Loadpoints[j].Title })
	return p
}

// Assets lists the devices runs can be assigned to: all physical assets,
// retired ones included (not forecasts, tariffs or vehicle accounts).
func Assets(cfg *config.Config) []Asset {
	out := []Asset{}
	for _, d := range cfg.Devices {
		switch k := config.KindOf(d); k {
		case config.KindForecast, config.KindTariff, config.KindVehicleAccount:
		default:
			name := d.Name
			if name == "" {
				name = d.ID
			}
			out = append(out, Asset{ID: d.ID, Name: name, Kind: k})
		}
	}
	return out
}

// runKind decides the kind of runs: from the target asset, or – for runs
// kept under their evcc name – charging only if a real vehicle was there
// (evcc names heaters as their own "vehicle").
func runKind(target string, e EvccSession, assets []Asset) string {
	for _, a := range assets {
		if a.ID == target {
			return config.RunKind(a.Kind)
		}
	}
	if e.Vehicle != "" && e.Vehicle != e.Loadpoint {
		return config.RunCharging
	}
	return config.RunOther
}
