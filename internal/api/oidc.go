// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
)

// Login services (OpenID Connect): administrators register them, users link
// them to their account under "Konto" and then sign in with them.

func (h *handler) oidcRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/oidc/{id}/start", h.oidcStart)
	mux.HandleFunc("GET /api/auth/oidc/callback", h.oidcCallback)
	mux.HandleFunc("GET /api/auth/providers", h.listProviders)
	mux.HandleFunc("POST /api/auth/providers", h.saveProvider)
	mux.HandleFunc("PUT /api/auth/providers/{id}", h.saveProvider)
	mux.HandleFunc("DELETE /api/auth/providers/{id}", h.deleteProvider)
	mux.HandleFunc("DELETE /api/me/identities/{provider}/{subject...}", h.unlinkIdentity)
}

func (h *handler) providerStore() auth.ProviderStore { return h.st }

func (h *handler) provider(r *http.Request, id string) (auth.Provider, bool) {
	ps, err := h.providerStore().Providers(r.Context())
	if err != nil {
		return auth.Provider{}, false
	}
	for _, p := range ps {
		if p.ID == id {
			return p, true
		}
	}
	return auth.Provider{}, false
}

// publicProviders are the login buttons (id and name only).
func (h *handler) publicProviders(r *http.Request) []map[string]string {
	ps, _ := h.providerStore().Providers(r.Context())
	out := []map[string]string{}
	for _, p := range ps {
		out = append(out, map[string]string{"id": p.ID, "name": p.Name})
	}
	return out
}

func redirectURI(r *http.Request) string {
	scheme := "http"
	if secureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/auth/oidc/callback"
}

func (h *handler) principalOf(r *http.Request) *auth.Principal {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if p, err := h.auth.Resolve(r.Context(), c.Value); err == nil {
			return p
		}
	}
	return nil
}

func (h *handler) oidcStart(w http.ResponseWriter, r *http.Request) {
	p, ok := h.provider(r, r.PathValue("id"))
	if !ok {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Unbekannter Anmeldedienst."), http.StatusFound)
		return
	}
	f := auth.PendingFlow{Purpose: r.URL.Query().Get("purpose"), Next: nextPath(r.URL.Query().Get("next"))}
	switch f.Purpose {
	case auth.FlowLink, auth.FlowReauth:
		me := h.principalOf(r)
		if me == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		f.UserID = me.User.ID
	default:
		f.Purpose = auth.FlowLogin
	}
	u, err := h.oidc.Start(r.Context(), p, f, redirectURI(r))
	if err != nil {
		slog.Warn("login service unavailable", "provider", p.ID, "err", err)
		back := "/login?error="
		if f.Purpose != auth.FlowLogin {
			back = "/konto?msg="
		}
		http.Redirect(w, r, back+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (h *handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, ok := h.oidc.Flow(q.Get("state"))
	fail := func(msg string) {
		to := "/login?error="
		if ok && f.Purpose != auth.FlowLogin {
			to = "/konto?msg="
		}
		http.Redirect(w, r, to+url.QueryEscape(msg), http.StatusFound)
	}
	if !ok {
		fail("Die Anmeldung ist abgelaufen – bitte noch einmal versuchen.")
		return
	}
	if e := q.Get("error"); e != "" {
		fail("Anmeldung abgebrochen (" + e + ").")
		return
	}
	p, found := h.provider(r, f.Provider)
	if !found {
		fail("Der Anmeldedienst ist nicht mehr eingerichtet.")
		return
	}
	claims, err := h.oidc.Finish(r.Context(), p, f, q.Get("code"))
	if err != nil {
		slog.Warn("external login failed", "provider", p.ID, "err", err)
		fail(err.Error())
		return
	}
	ctx := r.Context()
	st := h.auth.Store()
	idn, idErr := st.IdentityBySubject(ctx, p.ID, claims.Subject)
	next := f.Next
	if next == "" {
		next = "/"
	}
	switch f.Purpose {
	case auth.FlowLogin:
		if idErr != nil {
			fail("Dieses " + p.Name + "-Konto ist mit keinem Benutzer verbunden. Melde dich einmal mit Benutzername und Passwort an und verbinde es unter „Konto“.")
			return
		}
		u, err := st.User(ctx, idn.UserID)
		if err != nil || u.Disabled {
			fail("Der Benutzer ist gesperrt.")
			return
		}
		token, err := h.auth.StartSession(ctx, u, p.ID, r.UserAgent())
		if err != nil {
			fail("Anmeldung fehlgeschlagen.")
			return
		}
		st.SaveIdentity(ctx, auth.Identity{Provider: p.ID, Subject: claims.Subject, UserID: u.ID, Email: claims.Email, Name: claims.Name})
		h.setSession(w, r, token)
		http.Redirect(w, r, next, http.StatusFound)
	case auth.FlowLink:
		me := h.principalOf(r)
		if me == nil || me.User.ID != f.UserID {
			fail("Bitte erst anmelden.")
			return
		}
		if idErr == nil && idn.UserID != me.User.ID {
			fail("Dieses " + p.Name + "-Konto ist schon mit einem anderen Benutzer verbunden.")
			return
		}
		if err := st.SaveIdentity(ctx, auth.Identity{Provider: p.ID, Subject: claims.Subject, UserID: me.User.ID, Email: claims.Email,
			Name: claims.Name, Created: time.Now()}); err != nil {
			fail("Verbinden fehlgeschlagen.")
			return
		}
		slog.Info("login linked", "user", me.User.Login, "provider", p.ID)
		http.Redirect(w, r, "/konto?msg="+url.QueryEscape(p.Name+" ist verbunden. Du kannst dich jetzt damit anmelden."), http.StatusFound)
	case auth.FlowReauth:
		me := h.principalOf(r)
		if me == nil || me.User.ID != f.UserID || idErr != nil || idn.UserID != me.User.ID {
			fail("Die Bestätigung passt nicht zu deinem Konto.")
			return
		}
		if err := h.auth.Elevate(ctx, me, p.ID); err != nil {
			fail("Bestätigung fehlgeschlagen.")
			return
		}
		http.Redirect(w, r, next, http.StatusFound)
	}
}

// unlinkIdentity removes a linked login; the user keeps at least one way
// to sign in.
func (h *handler) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	me := auth.From(r.Context())
	st := h.auth.Store()
	ids, err := st.Identities(r.Context(), me.User.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Konto konnte nicht gelesen werden", err)
		return
	}
	if !me.User.HasPassword && len(ids) <= 1 {
		writeErr(w, http.StatusBadRequest, "Das ist dein einziger Anmeldeweg. Lege erst ein Passwort fest oder verbinde einen anderen Dienst.", nil)
		return
	}
	if err := st.DeleteIdentity(r.Context(), me.User.ID, r.PathValue("provider"), r.PathValue("subject")); err != nil {
		writeErr(w, http.StatusInternalServerError, "Trennen fehlgeschlagen", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := h.providerStore().Providers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Anmeldedienste konnten nicht gelesen werden", err)
		return
	}
	for i := range ps {
		ps[i].ClientSecret = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": ps, "redirectUri": redirectURI(r)})
}

var providerIDRe = regexp.MustCompile(`[^a-z0-9]+`)

func (h *handler) saveProvider(w http.ResponseWriter, r *http.Request) {
	var p auth.Provider
	if !decode(w, r, &p) {
		return
	}
	p.ID, p.Name, p.Issuer, p.ClientID = r.PathValue("id"), strings.TrimSpace(p.Name), strings.TrimRight(strings.TrimSpace(p.Issuer), "/"), strings.TrimSpace(p.ClientID)
	if p.Name == "" || p.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "Name und Client-ID sind nötig.", nil)
		return
	}
	if u, err := url.Parse(p.Issuer); err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname()))) {
		writeErr(w, http.StatusBadRequest, "Aussteller-URL: https://… (http nur im lokalen Netz)", nil)
		return
	}
	if p.ID == "" {
		p.ID = strings.Trim(providerIDRe.ReplaceAllString(strings.ToLower(p.Name), "-"), "-")
		if p.ID == "" {
			p.ID = "login"
		}
		if _, exists := h.provider(r, p.ID); exists {
			p.ID += "-" + auth.NewID()[:4]
		}
		p.Created = time.Now()
	} else if old, ok := h.provider(r, p.ID); ok {
		p.Created = old.Created
	} else {
		writeErr(w, http.StatusNotFound, "Unbekannter Anmeldedienst", nil)
		return
	}
	// check the issuer right away (a typo would only show at the first login)
	if _, err := h.oidc.Start(r.Context(), p, auth.PendingFlow{Purpose: "check"}, redirectURI(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if err := h.providerStore().SaveProvider(r.Context(), p); err != nil {
		writeErr(w, http.StatusInternalServerError, "Speichern fehlgeschlagen", err)
		return
	}
	p.ClientSecret, p.HasSecret = "", true
	writeJSON(w, http.StatusOK, p)
}

func (h *handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := h.providerStore().DeleteProvider(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "Löschen fehlgeschlagen", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isLocalHost: plain http is accepted for a login service in the home network.
func isLocalHost(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") ||
		strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "10.") || host == "127.0.0.1"
}

// nextPath accepts a page on this server to return to (with query and
// fragment), never another site.
func nextPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") || strings.HasPrefix(p, "/api/") {
		return ""
	}
	return p
}
