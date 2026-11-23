// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/voltibri/voltibri/internal/importer"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/store"
)

func (h *handler) importRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/imports", h.listImports)
	mux.HandleFunc("GET /api/runs", h.runs)
	mux.HandleFunc("GET /api/runs/summary", h.runSummaries)
	mux.HandleFunc("GET /api/runs/vehicles", h.runVehicles)
	mux.HandleFunc("GET /api/runs/series", h.runSeries)
	mux.HandleFunc("POST /api/imports/evcc", h.local(h.importEvcc))
	mux.HandleFunc("POST /api/imports/{id}/undo", h.local(h.undoImport))
}

func (h *handler) listImports(w http.ResponseWriter, r *http.Request) {
	list, err := h.st.Imports(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Importe konnten nicht gelesen werden", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type evccRequest struct {
	URL     string            `json:"url"`
	Mapping map[string]string `json:"mapping"` // evcc loadpoint title -> charger id ("" = keep title)
	DryRun  bool              `json:"dryRun"`
}

type evccResponse struct {
	Preview importer.EvccPreview `json:"preview"`
	Import  *store.Import        `json:"import,omitempty"` // nil on dry run or when nothing was new
	Skipped int                  `json:"skipped"`
}

// importEvcc previews (dryRun) or runs an import of evcc charging sessions.
func (h *handler) importEvcc(w http.ResponseWriter, r *http.Request) {
	var req evccRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.URL == "" {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage: Adresse von evcc fehlt", err)
		return
	}
	list, err := importer.FetchEvcc(r.Context(), req.URL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), nil)
		return
	}
	known, err := h.st.ImportedIDs(r.Context(), "evcc")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Bestehende Importe konnten nicht gelesen werden", err)
		return
	}
	cfg, err := h.current(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konfiguration konnte nicht gelesen werden", err)
		return
	}
	p := importer.PlanEvcc(list, known, importer.Assets(cfg), req.Mapping)
	res := evccResponse{Preview: p}
	if !req.DryRun && p.New > 0 {
		mapping := map[string]any{}
		for _, m := range p.Loadpoints {
			mapping[m.Title] = m.Target
		}
		imp, skipped, err := h.st.ImportRuns(r.Context(), store.Import{Source: "evcc", Author: "lokal",
			Description: strconv.Itoa(p.New) + " Läufe aus evcc", Params: map[string]any{"url": req.URL, "mapping": mapping}}, p.Runs)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Import fehlgeschlagen", err)
			return
		}
		res.Skipped = skipped
		if imp.ID != 0 {
			res.Import = &imp
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) undoImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Importnummer", nil)
		return
	}
	n, err := h.st.UndoImport(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrUndone):
		writeErr(w, http.StatusConflict, "Dieser Import wurde bereits rückgängig gemacht.", nil)
	case err != nil:
		writeErr(w, http.StatusNotFound, "Rückgängig machen fehlgeschlagen", err)
	default:
		writeJSON(w, http.StatusOK, map[string]int{"removed": n})
	}
}

// runFilter reads ?asset=, ?kind=, the period ?year= or ?from= / ?to=
// (dates YYYY-MM-DD, to inclusive, in the site time zone), ?limit= (up to
// 500) and ?offset=.
func (h *handler) runFilter(r *http.Request) (run.Filter, *time.Location) {
	q := r.URL.Query()
	loc := time.Local
	if cfg, err := h.current(r); err == nil {
		if l, err := time.LoadLocation(cfg.Site.Timezone); err == nil {
			loc = l
		}
	}
	f := run.Filter{Asset: q.Get("asset"), Kind: q.Get("kind"), Vehicle: q.Get("vehicle")}
	if y, err := strconv.Atoi(q.Get("year")); err == nil && y > 2000 && y < 2200 {
		f.From, f.To = time.Date(y, 1, 1, 0, 0, 0, 0, loc), time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)
	}
	if d, err := time.ParseInLocation("2006-01-02", q.Get("from"), loc); err == nil {
		f.From = d
	}
	if d, err := time.ParseInLocation("2006-01-02", q.Get("to"), loc); err == nil {
		f.To = d.AddDate(0, 0, 1)
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	return f, loc
}

// runs lists finished runs, newest first.
func (h *handler) runs(w http.ResponseWriter, r *http.Request) {
	f, _ := h.runFilter(r)
	list, err := h.st.Runs(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Läufe konnten nicht gelesen werden", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

