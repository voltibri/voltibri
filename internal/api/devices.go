// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/store"
	"github.com/voltibri/voltibri/sdk"
)

// deviceRoutes lets the web page add or change a single device without
// sending the whole configuration (setup dialogs, e.g. "Tibber verbinden").
func (h *handler) deviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/assets", h.assets)
	mux.HandleFunc("GET /api/devices/{id}", h.getDevice)
	mux.HandleFunc("PUT /api/devices/{id}", h.write(h.putDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", h.write(h.deleteDevice))
	mux.HandleFunc("PUT /api/config/section/{name}", h.write(h.putSection))
}

func (h *handler) current(r *http.Request) (*config.Config, error) {
	cfg, _, err := h.st.Current(r.Context())
	if errors.Is(err, store.ErrNoConfig) {
		return config.Default(), nil
	}
	return cfg, err
}

// getDevice returns one device with secrets redacted.
func (h *handler) getDevice(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	red := store.Redact(cfg)
	for _, d := range red.Devices {
		if d.ID == r.PathValue("id") {
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "Gerät nicht gefunden", nil)
}

type deviceRequest struct {
	Device  config.Device `json:"device"`
	Comment string        `json:"comment"`
	// Roles, if set, is the complete list of site functions of this device
	// (grid, pv, battery, pvForecast, tariff); others are taken away.
	Roles *[]string `json:"roles,omitempty"`
}

// putDevice adds the device or replaces the one with the same ID and saves
// a new configuration version. Secrets sent as store.Redacted (or left out)
// keep their stored value.
func (h *handler) putDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return
	}
	id := r.PathValue("id")
	if req.Device.ID == "" {
		req.Device.ID = id
	}
	if req.Device.ID != id {
		writeErr(w, http.StatusBadRequest, "Geräte-ID passt nicht zur Adresse", nil)
		return
	}
	drv, ok := sdk.Lookup(req.Device.Type)
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "Unbekannter Gerätetyp: "+req.Device.Type, nil)
		return
	}
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	if req.Device.Params == nil {
		req.Device.Params = map[string]any{}
	}
	idx := -1
	for i, d := range cfg.Devices {
		if d.ID == id {
			idx = i
		}
	}
	// secrets that were not sent again keep their stored value
	for _, name := range drv.SecretParams() {
		if v, ok := req.Device.Params[name]; !ok || v == "" {
			if idx >= 0 && cfg.Devices[idx].Type == req.Device.Type {
				if old, ok := cfg.Devices[idx].Params[name]; ok {
					req.Device.Params[name] = old
				}
			}
		}
	}
	if req.Roles != nil {
		if err := assignRoles(cfg, id, *req.Roles); err != nil {
			writeErr(w, http.StatusBadRequest, "Ungültige Rolle", err)
			return
		}
	}
	verb := "hinzugefügt"
	if idx >= 0 {
		cfg.Devices[idx] = req.Device
		verb = "geändert"
	} else {
		cfg.Devices = append(cfg.Devices, req.Device)
	}
	comment := req.Comment
	if comment == "" {
		comment = "Gerät " + id + " " + verb
	}
	h.save(w, r, cfg, comment)
}

// deleteDevice removes a device. Devices that still have a role (grid, PV,
// battery, forecast, tariff) or are a tariff source must be replaced first;
// loadpoint tuning of a removed charger is dropped with it.
func (h *handler) deleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	idx := -1
	for i, d := range cfg.Devices {
		if d.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "Gerät nicht gefunden", nil)
		return
	}
	if roles := cfg.RolesOf(id); len(roles) > 0 {
		writeErr(w, http.StatusConflict, "Das Gerät wird noch verwendet als: "+strings.Join(roles, ", ")+". Bitte zuerst ein anderes Gerät dafür eintragen.", nil)
		return
	}
	name := cfg.Devices[idx].Name
	if name == "" {
		name = id
	}
	cfg.Devices = append(cfg.Devices[:idx], cfg.Devices[idx+1:]...)
	lps := cfg.Charging.Loadpoints[:0]
	for _, lp := range cfg.Charging.Loadpoints {
		if lp.Charger != id {
			lps = append(lps, lp)
		}
	}
	cfg.Charging.Loadpoints = lps
	h.save(w, r, cfg, "Gerät "+name+" entfernt")
}

// assignRoles makes roles the complete set of site functions of device id.
func assignRoles(cfg *config.Config, id string, roles []string) error {
	want := map[string]bool{}
	for _, r := range roles {
		switch r {
		case "grid", "pv", "battery", "pvForecast", "tariff":
			want[r] = true
		default:
			return errors.New("unbekannte Rolle: " + r)
		}
	}
	single := func(cur *string, role string) {
		if want[role] {
			*cur = id
		} else if *cur == id {
			*cur = ""
		}
	}
	single(&cfg.Roles.Grid, "grid")
	single(&cfg.Roles.Battery, "battery")
	single(&cfg.Roles.PVForecast, "pvForecast")
	single(&cfg.Roles.Tariff, "tariff")
	pv := []string{}
	for _, p := range cfg.Roles.PV {
		if p != id {
			pv = append(pv, p)
		}
	}
	if want["pv"] {
		pv = append(pv, id)
	}
	cfg.Roles.PV = pv
	return nil
}

// putSection replaces one settings block of the configuration
// (PUT /api/config/section/{name}); unknown fields are rejected.
func (h *handler) putSection(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	name := r.PathValue("name")
	targets := map[string]struct {
		ptr   any
		label string
	}{
		"planner":  {&cfg.Planner, "Batteriestrategie"},
		"battery":  {&cfg.Battery, "Hausbatterie"},
		"grid":     {&cfg.Grid, "Netzanschluss"},
		"circuits": {&cfg.Circuits, "Unterverteilungen"},
	}
	t, ok := targets[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "Unbekannter Abschnitt: "+name, nil)
		return
	}
	if name == "circuits" {
		cfg.Circuits = nil // lists are replaced, not merged
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(t.ptr); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return
	}
	h.save(w, r, cfg, t.label+" geändert")
}

// assetInfo is a device as asset: derived kind and life cycle.
type assetInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	RunKind        string `json:"runKind"`
	Retired        bool   `json:"retired"`
	Commissioned   string `json:"commissioned,omitempty"`
	Decommissioned string `json:"decommissioned,omitempty"`
}

// assets lists all devices with their derived asset kind.
func (h *handler) assets(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	now := time.Now()
	out := []assetInfo{}
	for _, d := range cfg.Devices {
		name := d.Name
		if name == "" {
			name = d.ID
		}
		k := config.KindOf(d)
		out = append(out, assetInfo{ID: d.ID, Name: name, Kind: k, RunKind: config.RunKind(k), Retired: d.Retired(now),
			Commissioned: d.Commissioned, Decommissioned: d.Decommissioned})
	}
	writeJSON(w, http.StatusOK, out)
}
