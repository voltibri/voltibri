// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/store"
)

// fakeIdP is a minimal OpenID Connect provider.
type fakeIdP struct {
	srv     *httptest.Server
	nonce   string
	subject string
}

func newIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{subject: "user-123"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_secret") != "s3cret" || r.Form.Get("code") != "good" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		claims, _ := json.Marshal(map[string]any{"iss": f.srv.URL, "sub": f.subject, "aud": "voltibri", "exp": time.Now().Add(time.Minute).Unix(),
			"nonce": f.nonce, "email": "josi@example.com", "name": "Josi"})
		enc := base64.RawURLEncoding.EncodeToString
		json.NewEncoder(w).Encode(map[string]string{"id_token": enc([]byte(`{"alg":"RS256"}`)) + "." + enc(claims) + ".sig", "token_type": "Bearer"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// start begins a flow and returns the state (and remembers the nonce).
func (f *fakeIdP) start(t *testing.T, h http.Handler, path string, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if rr.Code != 302 || !strings.HasPrefix(loc.String(), f.srv.URL+"/authorize") || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("start: %d %s", rr.Code, loc)
	}
	f.nonce = loc.Query().Get("nonce")
	return loc.Query().Get("state")
}

func callback(h http.Handler, state, code string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/auth/oidc/callback?state="+url.QueryEscape(state)+"&code="+code, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestOIDCLinkAndLogin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	idp := newIdP(t)
	base := Handler(fakeRuntime{}, st)
	admin := signedIn(t, base, st, auth.RoleAdmin)

	// the administrator registers the service (checked right away)
	if rec := do(admin, "POST", "/api/auth/providers", `{"name":"Pocket ID","issuer":"http://127.0.0.1:1","clientId":"voltibri"}`, local); rec.Code != 400 {
		t.Fatalf("unreachable issuer accepted: %d", rec.Code)
	}
	rec := do(admin, "POST", "/api/auth/providers", `{"name":"Pocket ID","issuer":"`+idp.srv.URL+`","clientId":"voltibri","clientSecret":"s3cret"}`, local)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id": "pocket-id"`) || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("provider: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(admin, "GET", "/api/auth/providers", "", local); strings.Contains(rec.Body.String(), "s3cret") || !strings.Contains(rec.Body.String(), "/api/auth/oidc/callback") {
		t.Fatalf("list leaks secret or misses redirect: %s", rec.Body.String())
	}
	if rec := doAnon(base, "GET", "/api/auth/state", ""); !strings.Contains(rec.Body.String(), `"name": "Pocket ID"`) {
		t.Fatalf("login buttons: %s", rec.Body.String())
	}

	// not linked yet: no login
	state := idp.start(t, base, "/api/auth/oidc/pocket-id/start", nil)
	if rr := callback(base, state, "good", nil); !strings.HasPrefix(rr.Header().Get("Location"), "/login?error=") {
		t.Fatalf("unlinked login: %v", rr.Header())
	}

	// josi links it from her account page
	svc := auth.NewService(st)
	u, _ := svc.SaveUser(ctx, auth.User{Login: "josi"})
	tok, _ := svc.StartSession(ctx, u, "password", "test")
	josi := &http.Cookie{Name: sessionCookie, Value: tok}
	state = idp.start(t, base, "/api/auth/oidc/pocket-id/start?purpose=link", josi)
	if rr := callback(base, state, "good", josi); !strings.HasPrefix(rr.Header().Get("Location"), "/konto?msg=Pocket") {
		t.Fatalf("link: %v", rr.Header())
	}
	if ids, _ := st.Identities(ctx, u.ID); len(ids) != 1 || ids[0].Email != "josi@example.com" {
		t.Fatalf("identities: %+v", ids)
	}

	// replayed state and wrong codes fail
	if rr := callback(base, state, "good", nil); !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Fatalf("replay: %v", rr.Header())
	}
	state = idp.start(t, base, "/api/auth/oidc/pocket-id/start?next=/berichte", nil)
	if rr := callback(base, state, "bad", nil); !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Fatalf("bad code: %v", rr.Header())
	}

	// now she signs in with it
	state = idp.start(t, base, "/api/auth/oidc/pocket-id/start?next=/berichte", nil)
	rr := callback(base, state, "good", nil)
	if rr.Header().Get("Location") != "/berichte" || !strings.Contains(rr.Header().Get("Set-Cookie"), sessionCookie) {
		t.Fatalf("login: %v", rr.Header())
	}
	// a wrong nonce (token not from this flow) is refused
	state = idp.start(t, base, "/api/auth/oidc/pocket-id/start", nil)
	idp.nonce = "other"
	if rr := callback(base, state, "good", nil); !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Fatalf("nonce: %v", rr.Header())
	}
}
