// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
)

const sessionCookie = "voltibri_session"

// Access levels of the routes.
type level int

const (
	levelPublic   level = iota
	levelUser           // any signed-in user
	levelOverview       // any of energy flow, forecast, heat pump (the answer is filtered)
	levelFlow           // energy flow and day totals
	levelForecast       // battery plan and forecast
	levelReports        // reports (filtered to the user's vehicles)
	levelAdmin          // administrator
	levelElevated       // administrator with a recent login (configuration)
)

// levelFor is the access table. Unknown routes need the highest level.
func levelFor(method, path string) level {
	switch path {
	case "/login", "/api/health", "/common.js", "/common.css", "/manifest.webmanifest", "/sw.js", "/favicon.svg",
		"/api/auth/state", "/api/auth/login", "/api/auth/setup", "/api/auth/invite":
		return levelPublic
	case "/", "/konto", "/api/status", "/api/drivers", "/api/auth/logout", "/api/auth/reauth", "/api/charging":
		return levelUser
	case "/api/state":
		return levelOverview
	case "/api/today":
		return levelFlow
	case "/api/plan", "/api/history":
		return levelForecast
	case "/berichte", "/api/assets", "/api/sessions":
		return levelReports
	case "/api/battery", "/api/battery/force", "/api/battery/boost":
		return levelAdmin // runtime settings: no re-authentication like the configuration
	case "/konfiguration", "/api/oauth/callback":
		return levelAdmin
	}
	switch {
	case strings.HasPrefix(path, "/icons/"), strings.HasPrefix(path, "/api/auth/oidc/"):
		return levelPublic // the OIDC handlers check the session themselves
	case strings.HasPrefix(path, "/api/me"):
		return levelUser
	case method == http.MethodPut && strings.HasPrefix(path, "/api/loads/") && strings.HasSuffix(path, "/priority"):
		return levelAdmin // the order of all surplus loads
	case strings.HasPrefix(path, "/api/loads"):
		return levelUser // list, mode of a heating rod
	case method == http.MethodPut && (strings.HasPrefix(path, "/api/vehicles/") || strings.HasPrefix(path, "/api/loadpoints/")):
		return levelUser // checked per wallbox and vehicle in the handlers
	case method == http.MethodGet && strings.HasPrefix(path, "/api/runs"):
		return levelReports
	}
	return levelElevated
}

// authenticate resolves the session and enforces the access table.
func (h *handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// cross-site requests must not change anything (in addition to SameSite)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "Anfrage von fremder Seite abgelehnt.", nil)
			return
		}
		var p *auth.Principal
		if c, err := r.Cookie(sessionCookie); err == nil {
			p, _ = h.auth.Resolve(r.Context(), c.Value)
		}
		r = r.WithContext(auth.With(r.Context(), p))
		lv := levelFor(r.Method, r.URL.Path)
		api := strings.HasPrefix(r.URL.Path, "/api/")
		if lv == levelPublic {
			next.ServeHTTP(w, r)
			return
		}
		if p == nil {
			if api {
				writeJSON(w, http.StatusUnauthorized, apiError{Error: "Bitte anmelden.", Code: "login"})
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		ok := true
		switch lv {
		case levelOverview:
			ok = p.Overview()
		case levelFlow:
			ok = p.Flow()
		case levelForecast:
			ok = p.Forecast()
		case levelReports:
			ok = p.Reports()
		case levelAdmin, levelElevated:
			ok = p.Admin()
		}
		if !ok {
			if api {
				writeErr(w, http.StatusForbidden, "Dafür fehlt dir die Berechtigung.", nil)
				return
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if lv == levelElevated {
			if !p.Elevated(time.Now()) {
				writeJSON(w, http.StatusUnauthorized, apiError{Error: "Für die Konfiguration bitte die Anmeldung bestätigen.", Code: "reauth"})
				return
			}
			h.auth.Extend(r.Context(), p)
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether a changing request comes from this site.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (h *handler) setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: secureRequest(r),
		SameSite: http.SameSiteLaxMode, MaxAge: int(auth.SessionIdle / time.Second)})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: secureRequest(r),
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage", err)
		return false
	}
	return true
}

func (h *handler) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", static("text/html; charset=utf-8", loginHTML))
	mux.HandleFunc("GET /konto", static("text/html; charset=utf-8", accountHTML))
	mux.HandleFunc("GET /api/auth/state", h.authState)
	mux.HandleFunc("POST /api/auth/setup", h.authSetup)
	mux.HandleFunc("POST /api/auth/login", h.authLogin)
	mux.HandleFunc("POST /api/auth/logout", h.authLogout)
	mux.HandleFunc("POST /api/auth/reauth", h.authReauth)
	mux.HandleFunc("GET /api/auth/invite", h.inviteInfo)
	mux.HandleFunc("POST /api/auth/invite", h.inviteAccept)

	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("PUT /api/me", h.putMe)
	mux.HandleFunc("PUT /api/me/password", h.putMyPassword)
	mux.HandleFunc("DELETE /api/me/sessions/{id}", h.deleteMySession)

	mux.HandleFunc("GET /api/users", h.listUsers)
	mux.HandleFunc("POST /api/users", h.saveUser)
	mux.HandleFunc("PUT /api/users/{id}", h.saveUser)
	mux.HandleFunc("DELETE /api/users/{id}", h.deleteUser)
	mux.HandleFunc("POST /api/users/{id}/invite", h.inviteUser)
	mux.HandleFunc("POST /api/users/{id}/logout", h.logoutUser)
	mux.HandleFunc("GET /api/groups", h.listGroups)
	mux.HandleFunc("POST /api/groups", h.saveGroup)
	mux.HandleFunc("PUT /api/groups/{id}", h.saveGroup)
	mux.HandleFunc("DELETE /api/groups/{id}", h.deleteGroup)
}

// setupAllowed: the first administrator may be created from this machine or
// with the setup token from the log.
func (h *handler) setupAllowed(r *http.Request, token string) bool {
	return isLoopback(r) || (h.setupToken != "" && token == h.setupToken)
}

func (h *handler) authState(w http.ResponseWriter, r *http.Request) {
	has, err := h.auth.HasUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Benutzer konnten nicht gelesen werden", err)
		return
	}
	out := map[string]any{"setup": !has, "setupHere": !has && isLoopback(r), "providers": h.publicProviders(r)}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if p, err := h.auth.Resolve(r.Context(), c.Value); err == nil {
			out["user"] = p.User
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) authSetup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Login, Name, Password, Token string }
	if !decode(w, r, &req) {
		return
	}
	if !h.setupAllowed(r, req.Token) {
		writeErr(w, http.StatusForbidden, "Die Ersteinrichtung geht nur am Rechner, auf dem voltibri läuft, oder mit dem Einrichtungslink aus dem Protokoll.", nil)
		return
	}
	u, err := h.auth.CreateFirstAdmin(r.Context(), req.Login, req.Name, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.setupToken = ""
	slog.Info("first administrator created", "login", u.Login)
	h.signIn(w, r, u, "password")
}

func (h *handler) signIn(w http.ResponseWriter, r *http.Request, u auth.User, method string) {
	token, err := h.auth.StartSession(r.Context(), u, method, r.UserAgent())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Anmeldung fehlgeschlagen", err)
		return
	}
	h.setSession(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (h *handler) authLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Login, Password string }
	if !decode(w, r, &req) {
		return
	}
	u, err := h.auth.CheckPassword(r.Context(), req.Login, req.Password, clientIP(r))
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, auth.ErrThrottled) {
			code = http.StatusTooManyRequests
		}
		slog.Warn("login failed", "login", req.Login, "ip", clientIP(r))
		writeErr(w, code, err.Error(), nil)
		return
	}
	h.signIn(w, r, u, "password")
}

func (h *handler) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		h.auth.Logout(r.Context(), c.Value)
	}
	clearSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) authReauth(w http.ResponseWriter, r *http.Request) {
	p := auth.From(r.Context())
	var req struct{ Password string }
	if !decode(w, r, &req) {
		return
	}
	if _, err := h.auth.CheckPassword(r.Context(), p.User.Login, req.Password, clientIP(r)); err != nil {
		writeErr(w, http.StatusUnauthorized, "Das Passwort stimmt nicht.", nil)
		return
	}
	if err := h.auth.Elevate(r.Context(), p, "password"); err != nil {
		writeErr(w, http.StatusInternalServerError, "Bestätigung fehlgeschlagen", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) inviteInfo(w http.ResponseWriter, r *http.Request) {
	u, err := h.auth.InviteUser(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"login": u.Login, "name": u.DisplayName()})
}

func (h *handler) inviteAccept(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token, Password string }
	if !decode(w, r, &req) {
		return
	}
	token, u, err := h.auth.AcceptInvite(r.Context(), req.Token, req.Password, r.UserAgent())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.setSession(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// meResponse describes the signed-in user and what they may do.
type meResponse struct {
	User          auth.User       `json:"user"`
	Admin         bool            `json:"admin"`
	Elevated      bool            `json:"elevated"`
	ElevatedUntil time.Time       `json:"elevatedUntil,omitzero"`
	Overview      bool            `json:"overview"` // any of flow, forecast, heat
	Flow          bool            `json:"flow"`
	Forecast      bool            `json:"forecast"`
	Heat          bool            `json:"heat"`
	Reports       bool            `json:"reports"`
	Loadpoints    []string        `json:"loadpoints"` // nil: all
	Vehicles      []string        `json:"vehicles"`   // nil: all
	Identities    []auth.Identity `json:"identities"`
	Sessions      []sessionView   `json:"sessions"`
}

type sessionView struct {
	auth.Session
	Current bool `json:"current"`
}

func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	p := auth.From(r.Context())
	now := time.Now()
	out := meResponse{User: p.User, Admin: p.Admin(), Elevated: p.Elevated(now), Overview: p.Overview(), Flow: p.Flow(), Forecast: p.Forecast(), Heat: p.Heat(), Reports: p.Reports(),
		Loadpoints: p.Loadpoints(), Vehicles: p.Vehicles()}
	if out.Elevated {
		out.ElevatedUntil = p.Session.ElevatedUntil
	}
	ids, err := h.auth.Store().Identities(r.Context(), p.User.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konto konnte nicht gelesen werden", err)
		return
	}
	out.Identities = ids
	ss, _ := h.auth.Store().Sessions(r.Context(), p.User.ID)
	for _, s := range ss {
		out.Sessions = append(out.Sessions, sessionView{Session: s, Current: s.ID == p.Session.ID})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) putMe(w http.ResponseWriter, r *http.Request) {
	p := auth.From(r.Context())
	var req struct{ Name, Email string }
	if !decode(w, r, &req) {
		return
	}
	u := p.User
	u.Name, u.Email = req.Name, req.Email
	if _, err := h.auth.SaveUser(r.Context(), u); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) putMyPassword(w http.ResponseWriter, r *http.Request) {
	p := auth.From(r.Context())
	var req struct{ Current, Password string }
	if !decode(w, r, &req) {
		return
	}
	if p.User.HasPassword {
		if _, err := h.auth.CheckPassword(r.Context(), p.User.Login, req.Current, clientIP(r)); err != nil {
			writeErr(w, http.StatusUnauthorized, "Das bisherige Passwort stimmt nicht.", nil)
			return
		}
	}
	if err := h.auth.SetPassword(r.Context(), p.User.ID, req.Password, p.Session.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) deleteMySession(w http.ResponseWriter, r *http.Request) {
	p := auth.From(r.Context())
	ss, err := h.auth.Store().Sessions(r.Context(), p.User.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Sitzungen konnten nicht gelesen werden", err)
		return
	}
	for _, s := range ss {
		if s.ID == r.PathValue("id") {
			h.auth.Store().DeleteSession(r.Context(), s.ID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "Sitzung nicht gefunden", nil)
}

// --- administration ---

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := h.auth.Store().Users(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Benutzer konnten nicht gelesen werden", err)
		return
	}
	if us == nil {
		us = []auth.User{}
	}
	writeJSON(w, http.StatusOK, us)
}

func (h *handler) saveUser(w http.ResponseWriter, r *http.Request) {
	var u auth.User
	if !decode(w, r, &u) {
		return
	}
	u.ID = r.PathValue("id")
	saved, err := h.auth.SaveUser(r.Context(), u)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	slog.Info("user saved", "login", saved.Login, "role", saved.Role, "by", auth.From(r.Context()).User.Login)
	writeJSON(w, http.StatusOK, saved)
}

func (h *handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == auth.From(r.Context()).User.ID {
		writeErr(w, http.StatusBadRequest, "Du kannst dich nicht selbst löschen.", nil)
		return
	}
	if err := h.auth.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// inviteUser returns a link with which the user sets a (new) password.
func (h *handler) inviteUser(w http.ResponseWriter, r *http.Request) {
	token, exp, err := h.auth.Invite(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Einladung fehlgeschlagen", err)
		return
	}
	scheme := "http"
	if secureRequest(r) {
		scheme = "https"
	}
	writeJSON(w, http.StatusOK, map[string]any{"link": scheme + "://" + r.Host + "/login?invite=" + url.QueryEscape(token), "expires": exp})
}

func (h *handler) logoutUser(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.Store().DeleteSessions(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "Abmelden fehlgeschlagen", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listGroups(w http.ResponseWriter, r *http.Request) {
	gs, err := h.auth.Store().Groups(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gruppen konnten nicht gelesen werden", err)
		return
	}
	writeJSON(w, http.StatusOK, gs)
}

func (h *handler) saveGroup(w http.ResponseWriter, r *http.Request) {
	var g auth.Group
	if !decode(w, r, &g) {
		return
	}
	g.ID = r.PathValue("id")
	saved, err := h.auth.SaveGroup(r.Context(), g)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.Store().DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "Löschen fehlgeschlagen", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
