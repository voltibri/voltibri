// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/sdk"
)

// MCP (Model Context Protocol) lets an AI assistant read voltibri and – with
// a write token – help with the setup: POST /mcp, JSON-RPC 2.0 over
// "Streamable HTTP" without server-sent events (every request gets its
// answer directly; there are no notifications from the server).
//
// The caller authenticates with an access token (Authorization: Bearer,
// created on the account page). Every tool runs as an internal request
// through the normal API with the caller's permissions: the same checks,
// the same configuration versions (with rollback), nothing an assistant
// could do that the user could not do in the web page.

const (
	mcpMaxText = 60000 // characters of one tool answer
)

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `voltibri ist ein Energiemanagement für Eigenheime: PV, Hausbatterie, Wallboxen, Wärmepumpe und Heizstäbe. Es misst alle 10 s, prognostiziert PV und Verbrauch, plant die Batterie (spätes Laden, automatische Ladegrenzen nach Prognose) und verteilt den PV-Überschuss nach Priorität auf Wallboxen und Heizstäbe (Boost = Überschuss + Hausbatterie).

So arbeitest du damit:
- Starte mit "overview" (Zustand, Plan, Batterie, Verbraucher). Details mit "read" (z. B. /api/config, /api/plan, /api/today, /api/history?days=7, /api/charging, /api/battery, /api/loads, /api/config/versions).
- Für die Einrichtung neuer Geräte: "drivers" (Treiber und ihre Parameter), dann "change" mit PUT /api/devices/{id}.
- Einheiten: Leistung in W, Energie in Wh, Ladestände als Anteil 0..1, Preise in ct/kWh. Netz: + Bezug, − Einspeisung; Batterie: + Entladen, − Laden.
- Konfigurationsänderungen (Geräte, Abschnitte) erzeugen eine neue Version; sie lässt sich über POST /api/config/versions/{id}/rollback zurückholen. Laufzeit-Einstellungen (Batterie, Prioritäten, Lademodi) wirken sofort.
- Erkläre vor jeder Änderung, was du ändern willst und warum, und ändere nur, worum der Benutzer gebeten hat. Geheimnisse (Passwörter, Tokens) erscheinen als "***" und bleiben beim Speichern erhalten, wenn du sie so zurückschickst oder weglässt.`

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var mcpTools = []mcpTool{
	{Name: "overview", Title: "Überblick",
		Description: "Aktueller Zustand der Anlage in einem Aufruf: Leistungen (PV, Netz, Batterie, Haus, Verbraucher), Ladestand, Status und Version der Konfiguration, Batterieplan (Modus, Begründung), Batterie-Einstellungen und Ladegrenzen, Verbraucher mit Priorität.",
		InputSchema: obj(map[string]any{}), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "read", Title: "Lesen",
		Description: "GET auf einen Endpunkt der voltibri-API (nur /api/…). Nützlich: /api/config (Konfiguration, Geheimnisse geschwärzt), /api/config/export (YAML), /api/config/versions, /api/state, /api/plan (48 h in 15-min-Slots), /api/today (Tageswerte, Kosten je Gerät), /api/history?days=7, /api/charging (Wallboxen, Fahrzeuge), /api/battery, /api/loads, /api/sessions (Ladevorgänge), /api/runs/summary, /api/devices/{id}, /api/status.",
		InputSchema: obj(map[string]any{
			"path": map[string]any{"type": "string", "description": "Pfad, z. B. /api/plan oder /api/history?days=7"},
		}, "path"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "drivers", Title: "Treiber",
		Description: "Verfügbare Gerätetreiber mit Fähigkeiten und Parametern (für die Einrichtung). Ohne Suche nur eine Liste; mit query die passenden Treiber mit allen Parametern.",
		InputSchema: obj(map[string]any{
			"query": map[string]any{"type": "string", "description": "Suche in Typ, Name, Beschreibung und Kategorie, z. B. fronius, shelly, wallbox"},
		}), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "change", Title: "Ändern",
		Description: `PUT, POST oder DELETE auf die voltibri-API (nur mit einem Zugang mit Schreibrechten). Die wichtigsten Endpunkte:
- PUT /api/devices/{id} {"device": {id, name, type, mode (owned|delegated|observed), params, icon, kind, flexibility, heatUse, batteryUse, ...}, "roles": ["grid","pv","battery","pvForecast","tariff"]?, "comment": "..."} – Gerät anlegen oder ersetzen (neue Konfigurationsversion)
- DELETE /api/devices/{id}
- PUT /api/config/section/{planner|battery|grid|circuits|cards|loadpoints} – einen Abschnitt ersetzen
- POST /api/config/validate {"config": {...}} – Konfiguration prüfen, ohne zu speichern
- POST /api/config/versions/{id}/rollback – frühere Version zurückholen
- PUT /api/battery {...alle Felder aus GET /api/battery settings...} – Ladegrenzen (limitMode auto|manual), Boost, Vorrang
- POST /api/battery/force {"targetSoc": 0.8} / DELETE /api/battery/force – Zwangsladung; DELETE /api/battery/boost – Auto-Boost beenden
- PUT /api/loads/{id}/priority {"priority": 0-10}, PUT /api/loads/{id}/mode {"mode": "off|surplus|boost"}, PUT /api/loads/{id}/top {"on": true}
- PUT /api/vehicles/{key} {Fahrzeug-Einstellungen: mode, plans, ...}, PUT /api/loadpoints/{id}/mode {"mode": "..."}, PUT /api/loadpoints/{id}/vehicle {"vehicle": "<key>|guest|"} – Fahrzeug an der Wallbox wählen ("" = automatisch); vor dem Einstecken gilt die Wahl 10 min, danach ist die Wallbox wieder gesperrt
- Preisregel eines Fahrzeugs: Feld "price" am Gerät (type vehicle), je Anteil pv|battery|grid (oder all): {"mode": "inherit|replace|adjust", "base": "fixed|feedin|grid|free" (replace), "ct": 15 (fixed), "percent": 10, "addCt": 1}
- POST /api/runs/recalc {"vehicle": "id", "from": "JJJJ-MM-TT"?, "to": "JJJJ-MM-TT"?, "dryRun": true} – Ladevorgänge eines Fahrzeugs mit der gespeicherten Preisregel neu bewerten (erst mit dryRun prüfen)`,
		InputSchema: obj(map[string]any{
			"method": map[string]any{"type": "string", "enum": []string{"PUT", "POST", "DELETE"}},
			"path":   map[string]any{"type": "string", "description": "Pfad unter /api/"},
			"body":   map[string]any{"description": "JSON-Inhalt der Anfrage (Objekt oder Liste)"},
		}, "method", "path"), Annotations: map[string]any{"destructiveHint": true}},
}

// mcp handles POST /mcp (a single request or a batch).
func (h *handler) mcp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var reqs []mcpRequest
		if json.Unmarshal(body, &reqs) != nil {
			writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{-32700, "parse error"}})
			return
		}
		var out []mcpResponse
		for _, q := range reqs {
			if res, ok := h.mcpOne(r, q); ok {
				out = append(out, res)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var q mcpRequest
	if json.Unmarshal(body, &q) != nil {
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{-32700, "parse error"}})
		return
	}
	res, ok := h.mcpOne(r, q)
	if !ok { // notification or response: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// mcpOne answers one message; ok is false for notifications.
func (h *handler) mcpOne(r *http.Request, q mcpRequest) (mcpResponse, bool) {
	if len(q.ID) == 0 || q.Method == "" {
		return mcpResponse{}, false
	}
	res := mcpResponse{JSONRPC: "2.0", ID: q.ID}
	switch q.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(q.Params, &p)
		v := mcpVersions[0]
		for _, s := range mcpVersions {
			if s == p.ProtocolVersion {
				v = s
			}
		}
		res.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "voltibri", "title": "voltibri", "version": h.appVersion},
			"instructions":    mcpInstructions,
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		tools := mcpTools
		if auth.From(r.Context()).ReadOnly() {
			tools = tools[:len(tools)-1] // no "change" for a read-only token
		}
		res.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(q.Params, &p) != nil {
			res.Error = &mcpError{-32602, "invalid params"}
			break
		}
		text, isErr := h.mcpCall(r, p.Name, p.Arguments)
		if len(text) > mcpMaxText {
			text = text[:mcpMaxText] + fmt.Sprintf("\n… (gekürzt, %d Zeichen insgesamt – enger abfragen)", len(text))
		}
		res.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	default:
		res.Error = &mcpError{-32601, "method not found: " + q.Method}
	}
	return res, true
}

// mcpCall runs a tool and returns its text and whether it failed.
func (h *handler) mcpCall(r *http.Request, name string, raw json.RawMessage) (string, bool) {
	var args struct {
		Path   string          `json:"path"`
		Method string          `json:"method"`
		Body   json.RawMessage `json:"body"`
		Query  string          `json:"query"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &args) != nil {
		return "Ungültige Argumente.", true
	}
	p := auth.From(r.Context())
	slog.Info("mcp tool", "user", p.User.Login, "tool", name, "method", args.Method, "path", args.Path)
	switch name {
	case "overview":
		return h.mcpOverview(r)
	case "read":
		return h.internal(r, http.MethodGet, args.Path, nil)
	case "drivers":
		return mcpDrivers(args.Query), false
	case "change":
		if p.ReadOnly() {
			return "Dieser Zugang darf nur lesen. Für Änderungen auf der Konto-Seite einen Zugang mit Schreibrechten anlegen.", true
		}
		m := strings.ToUpper(args.Method)
		if m != http.MethodPut && m != http.MethodPost && m != http.MethodDelete {
			return "method muss PUT, POST oder DELETE sein.", true
		}
		var body []byte
		if len(args.Body) > 0 && string(args.Body) != "null" {
			body = args.Body
		}
		return h.internal(r, m, args.Path, body)
	}
	return "Unbekanntes Tool: " + name, true
}

// internal sends a request through the normal API as the caller.
func (h *handler) internal(r *http.Request, method, path string, body []byte) (string, bool) {
	u, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(u.Path, "/api/") || strings.HasPrefix(u.Path, "/api/auth/") || strings.HasPrefix(u.Path, "/api/me") ||
		strings.Contains(u.Path, "..") || u.Host != "" {
		return "Nur Pfade unter /api/ sind erlaubt (ohne Anmeldung und Konto).", true
	}
	req := httptest.NewRequestWithContext(r.Context(), method, u.RequestURI(), bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.root.ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code == http.StatusNoContent || out == "" {
		out = "OK"
	}
	if rec.Code >= 300 {
		return fmt.Sprintf("HTTP %d: %s", rec.Code, out), true
	}
	return out, false
}

// mcpOverview combines the most important answers.
func (h *handler) mcpOverview(r *http.Request) (string, bool) {
	parts := []struct{ key, path string }{
		{"status", "/api/status"}, {"state", "/api/state"}, {"battery", "/api/battery"}, {"loads", "/api/loads"}, {"plan", "/api/plan"},
	}
	out := map[string]any{}
	for _, p := range parts {
		text, failed := h.internal(r, http.MethodGet, p.path, nil)
		var v any
		if failed || json.Unmarshal([]byte(text), &v) != nil {
			out[p.key] = text // e.g. no permission, not running
			continue
		}
		if p.key == "plan" { // the slots are long: summary only (details via read /api/plan)
			if m, ok := v.(map[string]any); ok {
				delete(m, "slots")
			}
		}
		out[p.key] = v
	}
	b, _ := json.MarshalIndent(out, "", " ")
	return string(b), false
}

// mcpDrivers lists the drivers; with a query the matching ones in full.
func mcpDrivers(query string) string {
	ds := sdk.Drivers()
	sort.Slice(ds, func(i, j int) bool { return ds[i].Type < ds[j].Type })
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		var b strings.Builder
		b.WriteString("Typ | Name | Kategorie | Fähigkeiten\n")
		for _, d := range ds {
			caps := make([]string, len(d.Capabilities))
			for i, c := range d.Capabilities {
				caps[i] = string(c)
			}
			fmt.Fprintf(&b, "%s | %s | %s | %s\n", d.Type, d.Name, d.Category, strings.Join(caps, ", "))
		}
		b.WriteString("\nMit query (z. B. dem Typ) kommen Beschreibung und Parameter.")
		return b.String()
	}
	var hits []sdk.Driver
	for _, d := range ds {
		if strings.Contains(strings.ToLower(d.Type+" "+d.Name+" "+d.Description+" "+string(d.Category)), q) {
			hits = append(hits, d)
		}
	}
	if len(hits) == 0 {
		return "Kein Treiber passt zu „" + query + "“. Ohne query gibt es die ganze Liste."
	}
	b, _ := json.MarshalIndent(hits, "", " ")
	return string(b)
}
