// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/site"
)

// state returns the live state, reduced to what the user may see.
func (h *handler) state(w http.ResponseWriter, r *http.Request) {
	st := h.rt.State()
	if p := auth.From(r.Context()); !p.Admin() {
		cfg, _ := h.current(r)
		st = visibleState(p, st, cfg)
	}
	writeJSON(w, http.StatusOK, st)
}

// heatGroups names the overview group of heating devices shown as one
// bubble to users without heat pump details.
var heatGroups = map[string]string{config.KindHeatPump: "Wärmepumpe", config.KindHeatingRod: "Warmwasser", config.KindClimate: "Klima"}

// visibleState filters the live state for a user:
//   - without the energy flow: no producers and consumers
//   - without heat pump details: no heat readings (COP, temperatures), and the
//     heating devices only as one total per kind (cannot be opened)
//   - never: loadpoints (vehicle names of others), errors (device addresses)
func visibleState(p *auth.Principal, st site.State, cfg *config.Config) site.State {
	st.Loadpoints, st.Errors = nil, nil
	if !p.Heat() {
		st.Heat = nil
	}
	if !p.Flow() {
		st.Producers, st.Consumers, st.Circuits, st.Excluded = []site.Reading{}, []site.Reading{}, nil, nil
		return st
	}
	if p.Heat() || cfg == nil {
		return st
	}
	kinds := map[string]string{}
	for _, d := range cfg.Devices {
		kinds[d.ID] = config.KindOf(d)
	}
	out := []site.Reading{}
	sums := map[string]int{} // kind -> index in out
	for _, c := range st.Consumers {
		name, heat := heatGroups[kinds[c.ID]]
		if !heat {
			out = append(out, c)
			continue
		}
		k := kinds[c.ID]
		if i, ok := sums[k]; ok {
			out[i].PowerW += c.PowerW
			continue
		}
		sums[k] = len(out)
		icon := "heatpump"
		if k == config.KindHeatingRod {
			icon = "water"
		}
		out = append(out, site.Reading{ID: "heat:" + k, Name: name, Icon: icon, PowerW: c.PowerW, Mode: c.Mode})
	}
	st.Consumers = out
	return st
}
