// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package api serves the JSON API and the status page.
//
// NOTE: there is no authentication yet. Until users and roles exist, write
// endpoints only accept requests from the local machine (loopback).
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/server"
	"github.com/voltibri/voltibri/internal/site"
	"github.com/voltibri/voltibri/internal/store"
	"github.com/voltibri/voltibri/sdk"
)

//go:embed index.html
var indexHTML []byte

// Runtime provides live data and status.
type Runtime interface {
	State() site.State
	Plan() planner.Plan
	Status() server.Status
	ReadOnly() bool
}

// Handler returns the HTTP handler.
func Handler(rt Runtime, st *store.Store) http.Handler {
	h := &handler{rt: rt, st: st}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, rt.State()) })
	mux.HandleFunc("GET /api/plan", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, rt.Plan()) })
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, rt.Status()) })
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/drivers", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, sdk.Drivers()) })

	mux.HandleFunc("GET /api/config", h.getConfig)
	mux.HandleFunc("PUT /api/config", h.write(h.putConfig))
	mux.HandleFunc("POST /api/config/validate", h.validate)
	mux.HandleFunc("GET /api/config/export", h.export)
	mux.HandleFunc("POST /api/config/import", h.write(h.importYAML))
	mux.HandleFunc("GET /api/config/versions", h.versions)
	mux.HandleFunc("GET /api/config/versions/{id}", h.version)
	mux.HandleFunc("POST /api/config/versions/{id}/rollback", h.write(h.rollback))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	return mux
}

type handler struct {
	rt Runtime
	st *store.Store
}

// apiError is the error format of all endpoints.
type apiError struct {
	Error   string   `json:"error"`
	Details []string `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string, err error) {
	e := apiError{Error: msg}
	if err != nil {
		e.Details = strings.Split(err.Error(), "\n")
	}
	writeJSON(w, code, e)
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// write guards configuration changes: read-only mode and (until
// authentication exists) loopback only.
func (h *handler) write(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.rt.ReadOnly() {
			writeErr(w, http.StatusConflict, "Die Konfiguration wird über eine YAML-Datei verwaltet ("+h.rt.Status().ConfigSrc+") und kann hier nicht geändert werden.", nil)
			return
		}
		if !isLoopback(r) {
			writeErr(w, http.StatusForbidden, "Änderungen sind bis zur Einführung der Anmeldung nur vom lokalen Rechner aus erlaubt.", nil)
			return
		}
		next(w, r)
	}
}

type configResponse struct {
	Version  store.VersionInfo `json:"version"`
	ReadOnly bool              `json:"readOnly"`
	Config   *config.Config    `json:"config"`
}

func (h *handler) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg, vi, err := h.st.Current(r.Context())
	if errors.Is(err, store.ErrNoConfig) {
		cfg, err = config.Default(), nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	writeJSON(w, http.StatusOK, configResponse{Version: vi, ReadOnly: h.rt.ReadOnly(), Config: store.Redact(cfg)})
}

type saveRequest struct {
	Config  *config.Config `json:"config"`
	Comment string         `json:"comment"`
}

func decodeSave(r *http.Request) (*saveRequest, error) {
	var req saveRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, errors.New("config missing")
	}
	return &req, nil
}

func (h *handler) putConfig(w http.ResponseWriter, r *http.Request) {
	req, err := decodeSave(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return
	}
	h.save(w, r, req.Config, req.Comment)
}

func (h *handler) save(w http.ResponseWriter, r *http.Request, cfg *config.Config, comment string) {
	id, err := h.st.Save(r.Context(), cfg, "lokal", comment)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "Konfiguration ist ungültig", err)
		return
	}
	_, vi, _ := h.st.Version(r.Context(), id)
	writeJSON(w, http.StatusOK, vi)
}

func (h *handler) validate(w http.ResponseWriter, r *http.Request) {
	req, err := decodeSave(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return
	}
	if err := req.Config.Validate(); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "Konfiguration ist ungültig", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

func (h *handler) export(w http.ResponseWriter, r *http.Request) {
	cfg, _, err := h.st.Current(r.Context())
	if err != nil {
		writeErr(w, http.StatusNotFound, "Keine Konfiguration vorhanden", err)
		return
	}
	b, err := store.Redact(cfg).YAML()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Export fehlgeschlagen", err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="voltibri.yaml"`)
	w.Write(b)
}

func (h *handler) importYAML(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return
	}
	cfg, err := config.Parse(b)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "YAML ist ungültig", err)
		return
	}
	h.save(w, r, cfg, "YAML-Import")
}

func (h *handler) versions(w http.ResponseWriter, r *http.Request) {
	vs, err := h.st.Versions(r.Context(), 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Versionen konnten nicht gelesen werden", err)
		return
	}
	writeJSON(w, http.StatusOK, vs)
}

func pathID(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }

func (h *handler) version(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Versionsnummer", nil)
		return
	}
	cfg, vi, err := h.st.Version(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Version nicht gefunden", err)
		return
	}
	writeJSON(w, http.StatusOK, configResponse{Version: vi, ReadOnly: h.rt.ReadOnly(), Config: store.Redact(cfg)})
}

func (h *handler) rollback(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Versionsnummer", nil)
		return
	}
	nid, err := h.st.Rollback(r.Context(), id, "lokal")
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "Zurücksetzen fehlgeschlagen", err)
		return
	}
	_, vi, _ := h.st.Version(r.Context(), nid)
	writeJSON(w, http.StatusOK, vi)
}
