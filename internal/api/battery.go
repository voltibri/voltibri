// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"

	"github.com/voltibri/voltibri/internal/site"
)

// Battery settings (charge targets, max. discharge power, boost), the force
// charging and ending the automatic boost (the battery helps the surplus loads) are runtime
// settings for administrators, like the charging settings: they do not
// create configuration versions. What can be set depends on the
// capabilities of the battery driver (features in GET /api/battery).

func (h *handler) batteryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/battery", h.getBattery)
	mux.HandleFunc("PUT /api/battery", h.putBattery)
	mux.HandleFunc("POST /api/battery/force", h.postForce)
	mux.HandleFunc("DELETE /api/battery/force", h.deleteForce)
	mux.HandleFunc("DELETE /api/battery/boost", h.deleteBoost)
	mux.HandleFunc("PUT /api/battery/soc", h.putBatterySoC)
	mux.HandleFunc("DELETE /api/battery/soc", h.putBatterySoC)
}

// putBatterySoC sets the state of charge by hand ({"soc": 0.55}) while the
// battery does not report it; DELETE goes back to the reading.
func (h *handler) putBatterySoC(w http.ResponseWriter, r *http.Request) {
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
	if err := s.SetBatterySoC(v); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.BatteryInfo())
}

func (h *handler) battery(w http.ResponseWriter) *site.Site {
	s := h.rt.Battery()
	if s == nil {
		writeErr(w, http.StatusServiceUnavailable, "voltibri läuft gerade nicht (Konfiguration prüfen).", nil)
	}
	return s
}

func (h *handler) getBattery(w http.ResponseWriter, r *http.Request) {
	if s := h.battery(w); s != nil {
		writeJSON(w, http.StatusOK, s.BatteryInfo())
	}
}

func (h *handler) putBattery(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var bs site.BatterySettings
	if !decode(w, r, &bs) {
		return
	}
	if err := s.SetBatterySettings(bs); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.BatteryInfo())
}

func (h *handler) postForce(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	var req struct {
		TargetSoC float64 `json:"targetSoc"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.StartForceCharge(req.TargetSoC); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.BatteryInfo())
}

func (h *handler) deleteForce(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	if err := s.StopForceCharge(); err != nil {
		writeErr(w, http.StatusInternalServerError, "Zwangsladung konnte nicht beendet werden", err)
		return
	}
	writeJSON(w, http.StatusOK, s.BatteryInfo())
}

func (h *handler) deleteBoost(w http.ResponseWriter, r *http.Request) {
	s := h.battery(w)
	if s == nil {
		return
	}
	if err := s.StopBoost(); err != nil {
		writeErr(w, http.StatusInternalServerError, "Boost konnte nicht beendet werden", err)
		return
	}
	writeJSON(w, http.StatusOK, s.BatteryInfo())
}
