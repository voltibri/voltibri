// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package api serves the JSON API and the status page.
//
// Every route needs a signed-in user except the login page and static
// files; see levelFor for the access table.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/server"
	"github.com/voltibri/voltibri/internal/site"
	"github.com/voltibri/voltibri/internal/store"
	"github.com/voltibri/voltibri/sdk"
)

//go:embed index.html
var indexHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

//go:embed login.html
var loginHTML []byte

//go:embed account.html
var accountHTML []byte

//go:embed config.html
var configHTML []byte

//go:embed reports.html
var reportsHTML []byte

//go:embed common.js
var commonJS []byte

//go:embed common.css
var commonCSS []byte

//go:embed manifest.webmanifest
var manifest []byte

//go:embed sw.js
var serviceWorker []byte

//go:embed icons
var icons embed.FS

// static serves an embedded file.
func static(ctype string, b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}
}

// Runtime provides live data and status.
type Runtime interface {
	State() site.State
	Plan() planner.Plan
	Status() server.Status
	ReadOnly() bool
	Charging() *charging.Manager // nil while no site runs
}

// Handler returns the HTTP handler.
func Handler(rt Runtime, st *store.Store) http.Handler {
	h := &handler{rt: rt, st: st, auth: auth.NewService(st), oidc: auth.NewOIDC()}
	if has, err := h.auth.HasUsers(context.Background()); err == nil && !has {
		h.setupToken, _ = auth.NewToken()
		slog.Warn("no user yet: create the first administrator on this machine or open /login?setup=" + h.setupToken)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", h.state)
	mux.HandleFunc("GET /api/plan", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, rt.Plan()) })
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, rt.Status()) })
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/history", h.history)
	mux.HandleFunc("GET /api/today", h.today)
	h.authRoutes(mux)
	h.oidcRoutes(mux)
	h.chargingRoutes(mux)
	h.deviceRoutes(mux)
	h.importRoutes(mux)
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
	mux.HandleFunc("GET /konfiguration", static("text/html; charset=utf-8", configHTML))
	mux.HandleFunc("GET /berichte", static("text/html; charset=utf-8", reportsHTML))
	mux.HandleFunc("GET /common.js", static("text/javascript; charset=utf-8", commonJS))
	mux.HandleFunc("GET /common.css", static("text/css; charset=utf-8", commonCSS))
	mux.HandleFunc("GET /manifest.webmanifest", static("application/manifest+json", manifest))
	mux.HandleFunc("GET /sw.js", static("text/javascript; charset=utf-8", serviceWorker))
	mux.Handle("GET /icons/", http.FileServerFS(icons))
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(faviconSVG)
	})
	return h.authenticate(mux)
}

type handler struct {
	rt         Runtime
	st         *store.Store
	auth       *auth.Service
	oidc       *auth.OIDC
	setupToken string // first administrator from another machine (logged once)
}

// apiError is the error format of all endpoints.
type apiError struct {
	Error   string   `json:"error"`
	Details []string `json:"details,omitempty"`
	Code    string   `json:"code,omitempty"` // "login": not signed in, "reauth": confirm the login
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
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

// write guards configuration changes against read-only mode (access is
// checked by authenticate: administrators with a recent login).
func (h *handler) write(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.rt.ReadOnly() {
			writeErr(w, http.StatusConflict, "Die Konfiguration wird über eine YAML-Datei verwaltet ("+h.rt.Status().ConfigSrc+") und kann hier nicht geändert werden.", nil)
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

// historySlot is one measured 15-minute slot for the plan chart.
type historySlot struct {
	Start time.Time `json:"start"`
	PVW   *float64  `json:"pvW,omitempty"`   // measured
	LoadW *float64  `json:"loadW,omitempty"` // measured
	SoC   *float64  `json:"soc,omitempty"`   // measured, end of slot
	// the plan of the slot's day (first plan made that day)
	PlanPVW   *float64 `json:"planPvW,omitempty"`
	PlanLoadW *float64 `json:"planLoadW,omitempty"`
	PlanSoC   *float64 `json:"planSoC,omitempty"`
}

func optNum(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// history returns measured and planned slots, oldest first: the last
// ?hours= hours (default 2, at most 48) or ?from=&to= (RFC 3339, at most
// 8 days).
func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to := time.Now()
	from := to.Add(-2 * time.Hour)
	if v := q.Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 48 {
			writeErr(w, http.StatusBadRequest, "hours muss zwischen 1 und 48 liegen", err)
			return
		}
		from = to.Add(-time.Duration(n) * time.Hour)
	}
	if q.Get("from") != "" {
		f, err1 := time.Parse(time.RFC3339, q.Get("from"))
		t, err2 := time.Parse(time.RFC3339, q.Get("to"))
		if err1 != nil || err2 != nil || !t.After(f) || t.Sub(f) > 8*24*time.Hour {
			writeErr(w, http.StatusBadRequest, "from und to: Zeitpunkte (RFC 3339), höchstens 8 Tage auseinander", nil)
			return
		}
		from, to = f, t
	}
	recs, err := h.st.LoadHistory(r.Context(), from, time.Local)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Verlauf konnte nicht gelesen werden", err)
		return
	}
	plans, err := h.st.PlanSlots(r.Context(), from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Plan konnte nicht gelesen werden", err)
		return
	}
	bySlot := map[int64]*historySlot{}
	get := func(t time.Time) *historySlot {
		k := t.Unix()
		if s, ok := bySlot[k]; ok {
			return s
		}
		s := &historySlot{Start: t}
		bySlot[k] = s
		return s
	}
	for _, rec := range recs {
		if !rec.Time.Before(to) {
			break
		}
		s := get(rec.Time)
		s.PVW, s.LoadW = optNum(rec.PVW), optNum(rec.LoadW)
	}
	for _, p := range plans {
		s := get(p.Start.In(time.Local))
		s.SoC, s.PlanPVW, s.PlanLoadW, s.PlanSoC = optNum(p.SoC), optNum(p.PlanPVW), optNum(p.PlanLoadW), optNum(p.PlanSoC)
	}
	out := make([]historySlot, 0, len(bySlot))
	for _, s := range bySlot {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	writeJSON(w, http.StatusOK, out)
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
