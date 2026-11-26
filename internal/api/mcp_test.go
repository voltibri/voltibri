// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/store"
)

// rpc sends one JSON-RPC message to /mcp with the bearer token.
func rpc(t *testing.T, h http.Handler, token, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://claude.example") // no cookies: the origin does not matter
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// toolText calls a tool and returns its text and isError.
func toolText(t *testing.T, h http.Handler, token, name string, args map[string]any) (string, bool) {
	t.Helper()
	code, out := rpc(t, h, token, "tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := out["result"].(map[string]any)
	if code != http.StatusOK || !ok {
		t.Fatalf("%s: %d %v", name, code, out)
	}
	c := res["content"].([]any)[0].(map[string]any)
	return c["text"].(string), res["isError"] == true
}

func TestMCP(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cfg := config.Default()
	if _, err := st.Save(ctx, cfg, "test", ""); err != nil {
		t.Fatal(err)
	}
	h := Handler(fakeRuntime{}, st)
	svc := auth.NewService(st)
	admin, _ := svc.SaveUser(ctx, auth.User{Login: "admin", Role: auth.RoleAdmin})
	read, _, err := svc.CreateToken(ctx, admin, "Claude lesen", false)
	if err != nil {
		t.Fatal(err)
	}
	write, _, _ := svc.CreateToken(ctx, admin, "Claude Setup", true)
	if !strings.HasPrefix(read, auth.TokenPrefix) {
		t.Fatalf("token %q", read)
	}

	// no or a wrong token
	if code, _ := rpc(t, h, "vtb_falsch", "ping", nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	// a browser session is no access token
	cookie, _ := svc.StartSession(ctx, admin, "password", "test")
	if code, _ := rpc(t, h, cookie, "ping", nil); code != http.StatusUnauthorized {
		t.Fatalf("session as bearer: %d", code)
	}

	// handshake
	code, out := rpc(t, h, read, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	res, _ := out["result"].(map[string]any)
	if code != http.StatusOK || res["protocolVersion"] != "2025-03-26" || !strings.Contains(res["instructions"].(string), "voltibri") {
		t.Fatalf("initialize: %d %v", code, out)
	}
	// notifications get no answer
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+read)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification: %d", rec.Code)
	}

	// a read-only token sees no "change"
	tools := func(token string) (names []string) {
		_, out := rpc(t, h, token, "tools/list", nil)
		for _, x := range out["result"].(map[string]any)["tools"].([]any) {
			names = append(names, x.(map[string]any)["name"].(string))
		}
		return
	}
	if got := strings.Join(tools(read), ","); got != "overview,read,drivers" {
		t.Fatalf("read tools: %s", got)
	}
	if got := strings.Join(tools(write), ","); got != "overview,read,drivers,change" {
		t.Fatalf("write tools: %s", got)
	}

	// reading through the normal API
	if text, isErr := toolText(t, h, read, "read", map[string]any{"path": "/api/config"}); isErr || !strings.Contains(text, `"timezone"`) {
		t.Fatalf("read config: %v %.200s", isErr, text)
	}
	if text, isErr := toolText(t, h, read, "overview", nil); isErr || !strings.Contains(text, `"status"`) {
		t.Fatalf("overview: %.300s", text)
	}
	if text, _ := toolText(t, h, read, "drivers", map[string]any{"query": "demo"}); !strings.Contains(text, `"demo"`) {
		t.Fatalf("drivers: %.200s", text)
	}
	for _, bad := range []string{"/api/me", "/api/auth/state", "/konfiguration", "http://evil/api/config", "/api/../konto"} {
		if _, isErr := toolText(t, h, read, "read", map[string]any{"path": bad}); !isErr {
			t.Errorf("path %s allowed", bad)
		}
	}

	// changing: not with the read token (neither the tool nor the API directly)
	grid := map[string]any{"method": "PUT", "path": "/api/config/section/grid", "body": map[string]any{"fuseA": 35, "phases": 3}}
	if text, isErr := toolText(t, h, read, "change", grid); !isErr {
		t.Fatalf("read token changed: %s", text)
	}
	req = httptest.NewRequest(http.MethodPut, "/api/config/section/grid", strings.NewReader(`{"fuseA":35}`))
	req.Header.Set("Authorization", "Bearer "+read)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read token PUT: %d", rec.Code)
	}
	// the write token changes the configuration (a new version) without re-authentication
	if text, isErr := toolText(t, h, write, "change", grid); isErr {
		t.Fatalf("write: %s", text)
	}
	if c, _, _ := st.Current(ctx); c.Grid.FuseA != 35 {
		t.Fatalf("not saved: %+v", c.Grid)
	}
	// validation errors come back as a tool error
	if _, isErr := toolText(t, h, write, "change", map[string]any{"method": "PUT", "path": "/api/config/section/nope", "body": map[string]any{}}); !isErr {
		t.Fatal("unknown section accepted")
	}

	// tokens cannot create tokens; a revoked token stops working
	req = httptest.NewRequest(http.MethodPost, "/api/me/tokens", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Authorization", "Bearer "+write)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token created a token: %d", rec.Code)
	}
	st.DeleteSession(ctx, auth.HashToken(read))
	if code, _ := rpc(t, h, read, "ping", nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", code)
	}
}

func TestCreateTokenNeedsConfirmedLogin(t *testing.T) {
	h, st := setup(t, false) // admin signed in just now: confirmed
	rec := do(h, http.MethodPost, "/api/me/tokens", `{"name":"Claude","write":true}`, local)
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !strings.HasPrefix(out.Token, auth.TokenPrefix) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	// shown as a token on the account page
	rec = do(h, http.MethodGet, "/api/me", "", local)
	if !strings.Contains(rec.Body.String(), `"token-write"`) {
		t.Fatalf("me: %s", rec.Body)
	}
	// an old login: a write token needs the confirmation first
	ctx := context.Background()
	u, _, _ := auth.NewService(st).Store().UserByLogin(ctx, "test-admin")
	ss, _ := st.Sessions(ctx, u.ID)
	for _, s := range ss {
		if !s.IsToken() {
			s.ElevatedUntil = s.Created
			st.UpdateSession(ctx, s)
		}
	}
	if rec := do(h, http.MethodPost, "/api/me/tokens", `{"name":"Claude","write":true}`, local); rec.Code != http.StatusUnauthorized {
		t.Fatalf("write token without confirmation: %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/me/tokens", `{"name":"Lesen"}`, local); rec.Code != http.StatusOK {
		t.Fatalf("read token: %d", rec.Code)
	}
}
