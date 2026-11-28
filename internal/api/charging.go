// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
)

// Charging settings (modes, plans, vehicle choice) are runtime settings:
// they are stored directly and do not create configuration versions, so
// they also work when the configuration comes from a YAML file. They are
// only accepted from the local machine until authentication exists.

type chargingResponse struct {
	Loadpoints []charging.Loadpoint `json:"loadpoints"`
	Vehicles   []charging.Vehicle   `json:"vehicles"`
	Providers  []charging.Provider  `json:"providers"`
	Currency   string               `json:"currency"`
	GridCt     *float64             `json:"gridCt,omitempty"`
	FeedInCt   *float64             `json:"feedinCt,omitempty"`
	Local      bool                 `json:"local"` // the client may change settings
	Admin      bool                 `json:"admin"` // accounts and all vehicles
}

func opt(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// oauthStates maps OAuth state -> provider id for the redirect.
var oauthStates = struct {
	sync.Mutex
	m map[string]oauthState
}{m: map[string]oauthState{}}

type oauthState struct {
	provider string
	back     string // local page to return to
	created  time.Time
}

func (h *handler) chargingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/charging", h.getCharging)
	mux.HandleFunc("GET /api/sessions", h.getSessions)
	mux.HandleFunc("PUT /api/vehicles/{key...}", h.local(h.putVehicle))
	mux.HandleFunc("PUT /api/loadpoints/{id}/vehicle", h.local(h.putLoadpointVehicle))
	mux.HandleFunc("PUT /api/loadpoints/{id}/guestmode", h.local(h.putGuestMode))
	mux.HandleFunc("PUT /api/loadpoints/{id}/mode", h.local(h.putGuestMode)) // mode of the current session
	mux.HandleFunc("GET /api/charging/cards", h.getCards)
	mux.HandleFunc("PUT /api/loadpoints/{id}/off", h.local(h.putOff))
	mux.HandleFunc("GET /api/oauth/{id}/login", h.local(h.oauthLogin))
	mux.HandleFunc("GET /api/oauth/callback", h.oauthCallback)
}

// local marks runtime settings (independent of a read-only configuration);
// access is checked by authenticate and, per wallbox and vehicle, by the
// handlers.
func (h *handler) local(next http.HandlerFunc) http.HandlerFunc { return next }

func (h *handler) manager(w http.ResponseWriter) *charging.Manager {
	m := h.rt.Charging()
	if m == nil {
		writeErr(w, http.StatusServiceUnavailable, "voltibri läuft gerade nicht (Konfiguration prüfen).", nil)
	}
	return m
}

func (h *handler) getCharging(w http.ResponseWriter, r *http.Request) {
	m := h.rt.Charging()
	res := chargingResponse{Loadpoints: []charging.Loadpoint{}, Vehicles: []charging.Vehicle{}, Providers: []charging.Provider{},
		Currency: "EUR", Local: true, Admin: auth.From(r.Context()).Admin()}
	if m != nil {
		res.Loadpoints, res.Vehicles, res.Providers = m.Loadpoints(), m.Vehicles(), m.Providers()
		g, f := m.Prices(r.Context())
		res.GridCt, res.FeedInCt = opt(g), opt(f)
		visibleCharging(auth.From(r.Context()), &res)
	}
	if cfg, _, err := h.st.Current(r.Context()); err == nil && cfg.Tariffs.Currency != "" {
		res.Currency = cfg.Tariffs.Currency
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) getSessions(w http.ResponseWriter, r *http.Request) {
	m := h.manager(w)
	if m == nil {
		return
	}
	ss, err := m.Sessions(r.Context(), 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Ladevorgänge konnten nicht gelesen werden", err)
		return
	}
	if p := auth.From(r.Context()); !p.Admin() {
		mine := ss[:0]
		for _, s := range ss {
			if p.Vehicle(s.Vehicle) {
				mine = append(mine, s)
			}
		}
		ss = mine
	}
	writeJSON(w, http.StatusOK, ss)
}

func (h *handler) putVehicle(w http.ResponseWriter, r *http.Request) {
	m := h.manager(w)
	if m == nil {
		return
	}
	var s charging.VehicleSettings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&s); err != nil {
		writeErr(w, http.StatusBadRequest, "ungültige Daten", err)
		return
	}
	if !auth.From(r.Context()).Vehicle(r.PathValue("key")) {
		writeErr(w, http.StatusForbidden, "Dieses Fahrzeug ist für dich nicht freigegeben.", nil)
		return
	}
	if err := m.SetVehicleSettings(r.PathValue("key"), s); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) putLoadpointVehicle(w http.ResponseWriter, r *http.Request) {
	m := h.manager(w)
	if m == nil {
		return
	}
	var body struct {
		Vehicle string `json:"vehicle"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "ungültige Daten", err)
		return
	}
	if !h.mayControl(w, r, m, r.PathValue("id")) {
		return
	}
	if v := body.Vehicle; v != "" && v != charging.Guest && !auth.From(r.Context()).Vehicle(v) {
		writeErr(w, http.StatusForbidden, "Dieses Fahrzeug ist für dich nicht freigegeben.", nil)
		return
	}
	if err := m.SetLoadpointVehicle(r.PathValue("id"), body.Vehicle); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) putGuestMode(w http.ResponseWriter, r *http.Request) {
	m := h.manager(w)
	if m == nil {
		return
	}
	var body struct {
		Mode charging.Mode `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "ungültige Daten", err)
		return
	}
	if !h.mayControl(w, r, m, r.PathValue("id")) {
		return
	}
	if err := m.SetGuestMode(r.PathValue("id"), body.Mode); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// oauthLogin sends the browser to the provider's login page.
func (h *handler) oauthLogin(w http.ResponseWriter, r *http.Request) {
	m := h.manager(w)
	if m == nil {
		return
	}
	id := r.PathValue("id")
	a, ok := m.Authorizer(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "kein Konto mit Anmeldung: "+id, nil)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	u, state, err := a.AuthURL(scheme + "://" + r.Host + "/api/oauth/callback")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Anmeldung kann nicht gestartet werden", err)
		return
	}
	oauthStates.Lock()
	for k, v := range oauthStates.m {
		if time.Since(v.created) > 15*time.Minute {
			delete(oauthStates.m, k)
		}
	}
	oauthStates.m[state] = oauthState{provider: id, back: localPath(r.URL.Query().Get("back")), created: time.Now()}
	oauthStates.Unlock()
	http.Redirect(w, r, u, http.StatusFound)
}

// oauthCallback completes the login and returns to the charging panel.
func (h *handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	oauthStates.Lock()
	st, ok := oauthStates.m[q.Get("state")]
	delete(oauthStates.m, q.Get("state"))
	oauthStates.Unlock()
	back := func(msg string) {
		to := "/"
		if st.back != "" {
			to = st.back
		}
		http.Redirect(w, r, to+"?oauth="+url.QueryEscape(msg)+"#laden", http.StatusFound)
	}
	if !ok {
		back("Anmeldung abgelaufen oder unbekannt – bitte neu starten.")
		return
	}
	if e := q.Get("error"); e != "" {
		back("Anmeldung abgebrochen: " + e)
		return
	}
	m := h.rt.Charging()
	if m == nil {
		back("voltibri läuft gerade nicht.")
		return
	}
	a, ok := m.Authorizer(st.provider)
	if !ok {
		back("Konto nicht mehr vorhanden.")
		return
	}
	if err := a.Exchange(r.Context(), q.Get("code"), q.Get("state")); err != nil {
		back("Anmeldung fehlgeschlagen: " + err.Error())
		return
	}
	go m.Refresh(r.Context())
	back("ok")
}

// localPath accepts only paths on this server (no open redirect).
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\?#") {
		return ""
	}
	return p
}

// putOff switches a wallbox off or on ({"off": true}).
func (h *handler) putOff(w http.ResponseWriter, r *http.Request) {
	m := h.manager(w)
	if m == nil {
		return
	}
	var req struct {
		Off bool `json:"off"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return
	}
	if !h.mayControl(w, r, m, r.PathValue("id")) {
		return
	}
	if err := m.SetOff(r.PathValue("id"), req.Off); err != nil {
		writeErr(w, http.StatusNotFound, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getCards lists the charging cards the wallboxes know and their settings.
func (h *handler) getCards(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Known    []string      `json:"known"`
		Settings []config.Card `json:"settings"`
	}{Known: []string{}, Settings: []config.Card{}}
	if m := h.rt.Charging(); m != nil {
		out.Known = m.Cards(r.Context())
	}
	if cfg, err := h.current(r); err == nil && cfg.Charging.Cards != nil {
		out.Settings = cfg.Charging.Cards
	}
	writeJSON(w, http.StatusOK, out)
}

// foreign reports whether another user's vehicle is connected (or chosen
// before plugging in): the user only sees that the wallbox is in use and may
// not change anything.
func foreign(p *auth.Principal, lp charging.Loadpoint) bool {
	return !p.Admin() && (lp.Connected || !lp.ManualUntil.IsZero()) && lp.Vehicle != "" && lp.Vehicle != charging.Guest && !p.Vehicle(lp.Vehicle)
}

// visibleCharging filters the charging view to what the user may see.
// Prices: a user sees only what a kWh costs one of their vehicles chosen
// at a wallbox (its price rule), never the tariffs as such, nothing for a
// guest vehicle and nothing of another user's vehicle.
func visibleCharging(p *auth.Principal, res *chargingResponse) {
	if p.Admin() {
		return
	}
	res.GridCt, res.FeedInCt = nil, nil
	lps := []charging.Loadpoint{}
	for _, lp := range res.Loadpoints {
		if !p.Loadpoint(lp.ID) {
			continue
		}
		lp.PriceCt = nil
		if lp.Vehicle == charging.Guest {
			lp.Prices = nil
		}
		if foreign(p, lp) {
			lp.Foreign = true
			lp.Vehicle, lp.VehicleName, lp.Manual, lp.Card, lp.Reason, lp.Plan, lp.Finish, lp.Prices = "", "belegt", false, "", "", nil, nil, nil
			if lp.Session != nil {
				lp.Session = &charging.Session{Start: lp.Session.Start, EnergyWh: lp.Session.EnergyWh, CostUnknown: true}
			}
		}
		lps = append(lps, lp)
	}
	res.Loadpoints = lps
	vs := []charging.Vehicle{}
	for _, v := range res.Vehicles {
		if v.Asset && p.Vehicle(v.Key) {
			vs = append(vs, v)
		}
	}
	res.Vehicles = vs
	res.Providers = []charging.Provider{}
}

// mayControl checks that the user may change the loadpoint now.
func (h *handler) mayControl(w http.ResponseWriter, r *http.Request, m *charging.Manager, id string) bool {
	p := auth.From(r.Context())
	for _, lp := range m.Loadpoints() {
		if lp.ID != id {
			continue
		}
		switch {
		case !p.Loadpoint(id):
			writeErr(w, http.StatusForbidden, "Diese Wallbox ist für dich nicht freigegeben.", nil)
			return false
		case foreign(p, lp):
			msg := "An der Wallbox lädt gerade ein anderes Fahrzeug."
			if !lp.Connected {
				msg = "Die Wallbox ist gerade für ein anderes Fahrzeug reserviert."
			}
			writeErr(w, http.StatusForbidden, msg, nil)
			return false
		}
		return true
	}
	writeErr(w, http.StatusNotFound, "unbekannte Wallbox "+id, nil)
	return false
}
