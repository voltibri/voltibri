// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package api serves the JSON API and the status page.
//
// NOTE: there is no authentication yet. The server binds to 127.0.0.1 by
// default; users, roles and OIDC are planned for the next milestone.
package api

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/site"
)

//go:embed index.html
var indexHTML []byte

// Source provides the data served by the API.
type Source interface {
	State() site.State
	Plan() planner.Plan
}

// Handler returns the HTTP handler.
func Handler(src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, src.State()) })
	mux.HandleFunc("GET /api/plan", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, src.Plan()) })
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
