// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/store"
	"github.com/voltibri/voltibri/sdk"
)

// testVehicle is a vehicle asset without live data.
type testVehicle struct{ id, name string }

func (v testVehicle) ID() string { return v.id }
func (v testVehicle) Vehicles(context.Context) ([]sdk.VehicleInfo, error) {
	return []sdk.VehicleInfo{{ID: v.id, Name: v.name}}, nil
}
func (v testVehicle) VehicleStatus(context.Context, string) (sdk.VehicleStatus, error) {
	return sdk.VehicleStatus{SoC: math.NaN(), LimitSoC: math.NaN(), RangeKm: math.NaN()}, nil
}

type namedCharger struct{ id string }

func (c namedCharger) ID() string { return c.id }
func (c namedCharger) ChargerStatus(context.Context) (sdk.ChargerStatus, error) {
	return sdk.ChargerStatus{Connected: true}, nil
}

func TestGroupPermissions(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: "carport", Type: "none", Kind: "ev-charger"}, {ID: "zaun", Type: "none", Kind: "ev-charger"}, {ID: "garage", Type: "none", Kind: "ev-charger"},
		{ID: "eup", Type: "vehicle", Name: "Dräger e-Up"}, {ID: "tavascan", Type: "vehicle", Name: "Tavascan", ChargeModes: []string{"pv", "fast"}}}
	if _, err := st.Save(ctx, cfg, "test", ""); err != nil {
		t.Fatal(err)
	}
	devs := map[string]sdk.Device{"carport": namedCharger{"carport"}, "zaun": namedCharger{"zaun"}, "garage": namedCharger{"garage"},
		"eup": testVehicle{"eup", "Dräger e-Up"}, "tavascan": testVehicle{"tavascan", "Tavascan"}}
	m, err := charging.New(cfg, time.UTC, devs, st.Cache("charging"), st)
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(ctx)
	// the Tavascan is at the carport, a guest at the fence
	m.SetLoadpointVehicle("carport", "tavascan")
	m.SetLoadpointVehicle("zaun", charging.Guest)
	m.Update(ctx, charging.SiteInput{Now: time.Now(), Chargers: map[string]sdk.ChargerStatus{"carport": {Connected: true}, "zaun": {Connected: true}, "garage": {}}})

	svc := auth.NewService(st)
	g, err := svc.SaveGroup(ctx, auth.Group{Name: "Dräger", Reports: true, Loadpoints: []string{"carport", "zaun"}, Vehicles: []string{"eup"}})
	if err != nil || g.ID != "draeger" {
		t.Fatalf("group: %+v %v", g, err)
	}
	base := Handler(fakeRuntime{charge: m}, st)
	admin := signedIn(t, base, st, auth.RoleAdmin)
	u, err := svc.SaveUser(ctx, auth.User{Login: "josi", Name: "Josi", Groups: []string{"draeger"}})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := svc.StartSession(ctx, u, "password", "test")
	josi := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		base.ServeHTTP(w, r)
	})

	// josi sees the two wallboxes, the Tavascan only as "belegt", only her vehicle
	rec := do(josi, "GET", "/api/charging", "", local)
	var cr chargingResponse
	json.Unmarshal(rec.Body.Bytes(), &cr)
	if len(cr.Loadpoints) != 2 || len(cr.Vehicles) != 1 || cr.Vehicles[0].Key != "eup" || cr.Admin {
		t.Fatalf("josi charging: %s", rec.Body.String())
	}
	for _, lp := range cr.Loadpoints {
		switch lp.ID {
		case "carport":
			if !lp.Foreign || lp.Vehicle != "" || lp.VehicleName != "belegt" || strings.Contains(rec.Body.String(), "Tavascan") {
				t.Fatalf("foreign car visible: %+v", lp)
			}
		case "zaun":
			if lp.Foreign || lp.Vehicle != charging.Guest {
				t.Fatalf("guest: %+v", lp)
			}
		}
	}
	try := func(h http.Handler, method, path, body string, want int) {
		t.Helper()
		if rec := do(h, method, path, body, local); rec.Code != want {
			t.Errorf("%s %s: want %d, got %d %s", method, path, want, rec.Code, rec.Body.String())
		}
	}
	try(josi, "PUT", "/api/loadpoints/carport/mode", `{"mode":"fast"}`, 403) // foreign car
	try(josi, "PUT", "/api/loadpoints/zaun/mode", `{"mode":"fast"}`, 204)    // guest: allowed
	try(josi, "PUT", "/api/loadpoints/garage/off", `{"off":true}`, 403)      // not her wallbox
	try(josi, "PUT", "/api/loadpoints/zaun/vehicle", `{"vehicle":"tavascan"}`, 403)
	try(josi, "PUT", "/api/loadpoints/zaun/vehicle", `{"vehicle":"eup"}`, 204)
	try(josi, "PUT", "/api/vehicles/eup", `{"mode":"fast"}`, 204)
	try(josi, "PUT", "/api/vehicles/tavascan", `{"mode":"fast"}`, 403)
	try(josi, "GET", "/api/state", "", 403)  // no overview
	try(josi, "GET", "/api/config", "", 403) // no configuration
	try(josi, "GET", "/api/users", "", 403)
	try(josi, "GET", "/api/runs/summary", "", 200) // reports of her vehicles
	try(josi, "GET", "/api/me", "", 200)
	try(admin, "GET", "/api/users", "", 200)
	try(admin, "PUT", "/api/loadpoints/carport/mode", `{"mode":"pv"}`, 204)
	// the Tavascan offers only pv and fast (and off)
	try(admin, "PUT", "/api/loadpoints/carport/mode", `{"mode":"boost"}`, 400)
	try(admin, "PUT", "/api/loadpoints/carport/mode", `{"mode":"off"}`, 204)
	try(admin, "PUT", "/api/vehicles/tavascan", `{"mode":"minpv"}`, 400)
	try(admin, "PUT", "/api/vehicles/tavascan", `{"mode":"fast"}`, 204)

	// reports: only the e-Up
	st.SaveRun(ctx, runOf("carport", "tavascan", "Tavascan"))
	st.SaveRun(ctx, runOf("zaun", "eup", "Dräger e-Up"))
	rec = do(josi, "GET", "/api/runs", "", local)
	if !strings.Contains(rec.Body.String(), "Dräger e-Up") || strings.Contains(rec.Body.String(), "Tavascan") {
		t.Fatalf("josi runs: %s", rec.Body.String())
	}
	if rec := do(admin, "GET", "/api/runs", "", local); !strings.Contains(rec.Body.String(), "Tavascan") {
		t.Fatalf("admin runs: %s", rec.Body.String())
	}

	// the configuration needs a recent login: an old admin session must confirm
	a, _, _ := st.UserByLogin(ctx, "test-admin")
	oldTok, _ := svc.StartSession(ctx, a, "password", "old")
	ss, _ := st.Session(ctx, auth.HashToken(oldTok))
	ss.ElevatedUntil = time.Now().Add(-time.Minute)
	st.UpdateSession(ctx, ss)
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: oldTok})
	rr := httptest.NewRecorder()
	base.ServeHTTP(rr, req)
	if rr.Code != 401 || !strings.Contains(rr.Body.String(), `"reauth"`) {
		t.Fatalf("stale admin: %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("GET", "/api/charging", nil) // everyday use still works
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: oldTok})
	rr = httptest.NewRecorder()
	base.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("stale admin everyday: %d", rr.Code)
	}

	// cross-site writes are refused
	req = httptest.NewRequest("PUT", "/api/loadpoints/zaun/mode", strings.NewReader(`{"mode":"pv"}`))
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rr = httptest.NewRecorder()
	base.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("cross-site: %d", rr.Code)
	}
}

func TestLoginFlow(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := Handler(fakeRuntime{}, st)
	// first administrator: only from this machine
	body := `{"login":"nils","name":"Nils","password":"ein-langes-passwort"}`
	if rec := do(h, "POST", "/api/auth/setup", body, "192.168.1.9:1"); rec.Code != 403 {
		t.Fatalf("remote setup: %d", rec.Code)
	}
	rec := do(h, "POST", "/api/auth/setup", body, local)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Set-Cookie"), sessionCookie+"=") || !strings.Contains(rec.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("setup: %d %v", rec.Code, rec.Header())
	}
	if rec := do(h, "POST", "/api/auth/setup", body, local); rec.Code != 400 {
		t.Fatalf("second setup: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/auth/login", `{"login":"NILS","password":"falsch-falsch"}`, "10.0.0.1:1"); rec.Code != 401 {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec = do(h, "POST", "/api/auth/login", `{"login":"NILS","password":"ein-langes-passwort"}`, "10.0.0.1:1")
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	cookie := rec.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"admin": true`) || !strings.Contains(rr.Body.String(), `"elevated": true`) {
		t.Fatalf("me: %d %s", rr.Code, rr.Body.String())
	}
	// brute force: throttled after a few failures
	code := 0
	for i := 0; i < 10; i++ {
		code = do(h, "POST", "/api/auth/login", `{"login":"nils","password":"nope-nope-nope"}`, "10.0.0.2:1").Code
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("throttle: %d", code)
	}
}

func runOf(asset, vehicle, name string) run.Run {
	start := time.Now().Add(-2 * time.Hour)
	return run.Run{Asset: asset, Kind: config.RunCharging, Vehicle: vehicle, VehicleName: name, Mode: "pv", Start: start, End: start.Add(time.Hour), EnergyWh: 5000}
}
