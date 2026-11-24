// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/store"
)

// Reports combine the two data sources: the load profile (energy per device
// and 15 minutes, exact also for runs over midnight and with dynamic
// prices) where it exists, and the runs for the time before (e.g. imported
// from evcc). The number of runs always comes from the runs.

// profileScope returns the devices whose load profile belongs to the
// report (consumers of the selected run kind, or the selected device) and
// the first profiled slot of each.
func (h *handler) profileScope(r *http.Request, f run.Filter) ([]string, map[string]time.Time, map[string]string, error) {
	cfg, err := h.current(r)
	if err != nil {
		return nil, nil, nil, err
	}
	starts, err := h.st.ProfileStarts(r.Context())
	if err != nil {
		return nil, nil, nil, err
	}
	kinds := map[string]string{} // device -> run kind
	var assets []string
	from := map[string]time.Time{}
	for _, d := range cfg.Devices {
		k := config.KindOf(d)
		switch k {
		case config.KindPV, config.KindBattery, config.KindInverter, config.KindGridMeter,
			config.KindForecast, config.KindTariff, config.KindVehicleAccount, config.KindVehicle:
			continue // producers and meters are not part of consumption reports
		}
		rk := config.RunKind(k)
		kinds[d.ID] = rk
		if (f.Asset != "" && f.Asset != d.ID) || (f.Kind != "" && f.Kind != rk) {
			continue
		}
		if s, ok := starts[d.ID]; ok {
			assets = append(assets, d.ID)
			from[d.ID] = s
		}
	}
	if f.Vehicle != "" || f.Only != nil {
		// the load profile does not know which car was charging: vehicle
		// reports come from the runs only
		return nil, map[string]time.Time{}, kinds, nil
	}
	return assets, from, kinds, nil
}

// runSeries returns totals per ?bucket= (hour, day, month).
func (h *handler) runSeries(w http.ResponseWriter, r *http.Request) {
	f, loc := h.runFilter(r)
	b := r.URL.Query().Get("bucket")
	switch b {
	case store.BucketHour, store.BucketDay, store.BucketMonth:
	default:
		b = store.BucketMonth
	}
	ctx := r.Context()
	assets, from, _, err := h.profileScope(r, f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	counts, err := h.st.RunBuckets(ctx, f, loc, b) // all runs: number per bucket
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Läufe konnten nicht gelesen werden", err)
		return
	}
	out := map[string]*store.RunBucket{}
	for i := range counts {
		c := counts[i]
		out[c.Key] = &store.RunBucket{Key: c.Key, Start: c.Start, Count: c.Count}
	}
	get := func(key string, start time.Time) *store.RunBucket {
		if o, ok := out[key]; ok {
			return o
		}
		o := &store.RunBucket{Key: key, Start: start}
		out[key] = o
		return o
	}
	// energy before the profile started: from runs
	fr := f
	fr.ProfiledFrom = from
	pre, err := h.st.RunBuckets(ctx, fr, loc, b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Läufe konnten nicht gelesen werden", err)
		return
	}
	for _, p := range pre {
		o := get(p.Key, p.Start)
		o.EnergyWh += p.EnergyWh
		o.PVWh += p.PVWh
		o.BatteryWh += p.BatteryWh
		o.GridWh += p.GridWh
		o.CostCt += p.CostCt
	}
	// since then: from the load profile
	if len(assets) > 0 {
		sums, err := h.st.ProfileSums(ctx, assets, f.From, f.To, loc, b)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Lastgang konnte nicht gelesen werden", err)
			return
		}
		for _, p := range sums {
			o := get(p.Key, p.Start)
			o.EnergyWh += p.ImportWh
			o.PVWh += p.PVWh
			o.BatteryWh += p.BatteryWh
			o.GridWh += p.GridWh
			o.CostCt += p.CostCt
		}
	}
	list := make([]store.RunBucket, 0, len(out))
	for _, o := range out {
		list = append(list, *o)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	writeJSON(w, http.StatusOK, list)
}

// runSummaries returns one summary per device for the period.
func (h *handler) runSummaries(w http.ResponseWriter, r *http.Request) {
	f, _ := h.runFilter(r)
	ctx := r.Context()
	assets, from, kinds, err := h.profileScope(r, f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	all, err := h.st.RunSummaries(ctx, f) // number, period, imported
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Läufe konnten nicht gelesen werden", err)
		return
	}
	fr := f
	fr.ProfiledFrom = from
	pre, err := h.st.RunSummaries(ctx, fr) // energy before the profile
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Läufe konnten nicht gelesen werden", err)
		return
	}
	type key struct{ asset, kind string }
	out := map[key]*store.RunSummary{}
	order := []key{}
	get := func(k key) *store.RunSummary {
		if o, ok := out[k]; ok {
			return o
		}
		o := &store.RunSummary{Asset: k.asset, Kind: k.kind}
		out[k] = o
		order = append(order, k)
		return o
	}
	for _, s := range all {
		o := get(key{s.Asset, s.Kind})
		o.Count, o.Imported, o.First, o.Last = s.Count, s.Imported, s.First, s.Last
	}
	for _, s := range pre {
		o := get(key{s.Asset, s.Kind})
		o.EnergyWh, o.SolarWh, o.CostCt = s.EnergyWh, s.SolarWh, s.CostCt
	}
	if len(assets) > 0 {
		sums, err := h.st.ProfileSums(ctx, assets, f.From, f.To, time.Local, "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Lastgang konnte nicht gelesen werden", err)
			return
		}
		for _, p := range sums {
			if p.ImportWh <= 0 {
				continue
			}
			o := get(key{p.Asset, kinds[p.Asset]})
			o.EnergyWh += p.ImportWh
			o.SolarWh += p.PVWh + p.BatteryWh
			o.CostCt += p.CostCt
			if s := from[p.Asset]; o.First.IsZero() || s.Before(o.First) {
				if o.Count == 0 {
					o.First = s
				}
			}
			if o.Last.IsZero() {
				o.Last = time.Now()
			}
		}
	}
	list := make([]store.RunSummary, 0, len(order))
	for _, k := range order {
		list = append(list, *out[k])
	}
	writeJSON(w, http.StatusOK, list)
}

// runVehicles lists the vehicles of the selected runs (for the filter).
func (h *handler) runVehicles(w http.ResponseWriter, r *http.Request) {
	f, _ := h.runFilter(r)
	list, err := h.st.RunVehicles(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Fahrzeuge konnten nicht gelesen werden", err)
		return
	}
	if cfg, err := h.current(r); err == nil {
		list = mergeVehicles(list, cfg.VehicleNames())
	}
	writeJSON(w, http.StatusOK, list)
}

// mergeVehicles counts runs under a former name (alias) towards the vehicle
// asset that has this name now.
func mergeVehicles(list []store.RunVehicle, names map[string]string) []store.RunVehicle {
	idx := map[string]int{}
	out := []store.RunVehicle{}
	for _, v := range list {
		if n, ok := names[v.Name]; ok {
			v.Name = n
		}
		i, ok := idx[v.Name]
		if !ok {
			idx[v.Name] = len(out)
			out = append(out, v)
			continue
		}
		o := &out[i]
		o.Count += v.Count
		o.EnergyWh += v.EnergyWh
		o.Guest = o.Guest || v.Guest
		if v.Last.After(o.Last) {
			o.Last = v.Last
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
