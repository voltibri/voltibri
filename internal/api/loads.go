// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"

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
