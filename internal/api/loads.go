// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/site"
)

// Surplus loads (wallboxes and heating rods in steps): their priority in
// the surplus order (administrators) and the mode of a heating rod (off,
// surplus, boost) and "first for today" (until midnight). Runtime settings without configuration versions, meant
// for other systems too (e.g. Home Assistant). The charge mode of a
// wallbox is the vehicle's (/api/vehicles, /api/loadpoints).

func (h *handler) loadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/loads", h.getLoads)
	mux.HandleFunc("PUT /api/loads/{id}/priority", h.putLoadPriority)
	mux.HandleFunc("PUT /api/loads/{id}/mode", h.putLoadMode)
	mux.HandleFunc("PUT /api/loads/{id}/top", h.putLoadTop)
	mux.HandleFunc("PUT /api/loads/{id}/soc", h.putLoadSoC)
	mux.HandleFunc("DELETE /api/loads/{id}/soc", h.putLoadSoC)
	mux.HandleFunc("PUT /api/loads/{id}/limit", h.putLoadLimit)
	mux.HandleFunc("DELETE /api/loads/{id}/limit", h.putLoadLimit)
	mux.HandleFunc("PUT /api/loads/{id}/on", h.putLoadOn)
}

// putLoadSoC: state of charge of a device's battery by hand ({"soc": 0.3});
// DELETE forgets it.
func (h *handler) putLoadSoC(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var v *float64
	if r.Method != http.MethodDelete {
		var req struct {
			SoC *float64 `json:"soc"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.SoC == nil {
			writeErr(w, http.StatusBadRequest, "ungültige Daten: soc fehlt", nil)
			return
		}
		v = req.SoC
	}
	if err := s.SetLoadSoC(r.PathValue("id"), v); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putLoadLimit: limit of the current charge ({"soc": 0.8} or {"add": 0.2});
// DELETE goes back to the device's.
func (h *handler) putLoadLimit(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var l *charging.Limit
	if r.Method != http.MethodDelete {
		l = &charging.Limit{}
		if !decode(w, r, l) {
			return
		}
	}
	if err := s.SetLoadLimit(r.PathValue("id"), l); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putLoadOn switches a plug that is off at the limit on again (new charge).
func (h *handler) putLoadOn(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	if err := s.SwitchLoadOn(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) getLoads(w http.ResponseWriter, r *http.Request) {
	if s := h.battery(w); s != nil {
		writeJSON(w, http.StatusOK, s.Loads())
	}
}

func (h *handler) putLoadPriority(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var req struct {
		Priority int `json:"priority"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.SetLoadPriority(r.PathValue("id"), req.Priority); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.Loads())
}

// putLoadTop puts a load first for today ({"on": true}) or ends that.
func (h *handler) putLoadTop(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.SetLoadTop(r.PathValue("id"), req.On); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.Loads())
}

func (h *handler) putLoadMode(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var req struct {
		Mode site.LoadMode `json:"mode"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.SetLoadMode(r.PathValue("id"), req.Mode); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.Loads())
}
