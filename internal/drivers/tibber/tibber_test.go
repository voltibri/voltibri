// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package tibber

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/voltibri/voltibri/sdk"
)

func TestVehiclesWithOAuth(t *testing.T) {
	var gotVerifier string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "abc" || r.Form.Get("client_secret") != "sec" {
			http.Error(w, "bad", 400)
			return
		}
		gotVerifier = r.Form.Get("code_verifier")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "AT", "refresh_token": "RT", "expires_in": 3600})
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer AT" {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/homes", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"homes":[{"id":"h1"},{"id":"h2"}]}`))
	}))
	mux.HandleFunc("GET /v1/homes/{home}/devices", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"devices":[{"id":"car1","externalId":"tesla:5YJ123","info":{"name":"Blitz","brand":"Tesla","model":"Model Y"}}]}`))
	}))
	mux.HandleFunc("GET /v1/homes/h1/devices/car1", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"car1","capabilities":[
			{"id":"storage.stateOfCharge","value":57,"unit":"%"},
			{"id":"storage.targetStateOfCharge","value":"80","unit":"%"},
			{"id":"range.remaining","value":250000,"unit":"m"},
			{"id":"connector.status","value":"connected"},
			{"id":"charging.status","value":"idle"}]}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dev, err := sdk.New(sdk.DeviceConfig{ID: "tibber", Type: "tibber", Params: map[string]any{
		"clientId": "cid", "clientSecret": "sec", "authUrl": srv.URL + "/auth", "tokenUrl": srv.URL + "/token", "apiUrl": srv.URL + "/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	d := dev.(*Device)
	if d.Authorized() {
		t.Fatal("authorized without login")
	}
	u, state, err := d.AuthURL("http://localhost:7070/api/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	if pu.Query().Get("code_challenge_method") != "S256" || !strings.Contains(pu.Query().Get("scope"), "data-api-vehicles-read") {
		t.Fatalf("auth url: %s", u)
	}
	if err := d.Exchange(context.Background(), "abc", state); err != nil {
		t.Fatal(err)
	}
	if gotVerifier == "" || !d.Authorized() {
		t.Fatal("PKCE verifier not sent or not authorized")
	}
	vs, err := d.Vehicles(context.Background())
	if err != nil || len(vs) != 1 || vs[0].VIN != "5YJ123" || vs[0].Name != "Blitz" {
		t.Fatalf("vehicles: %v %+v", err, vs)
	}
	st, err := d.VehicleStatus(context.Background(), "car1")
	if err != nil || st.SoC != 0.57 || st.LimitSoC != 0.8 || st.RangeKm != 250 || !st.Connected || st.Charging {
		t.Fatalf("status: %v %+v", err, st)
	}
	// tokens survive a restart via the cache
	d2, _ := New(sdk.DeviceConfig{ID: "tibber", Params: map[string]any{"clientId": "cid", "clientSecret": "sec"}, Cache: d.cache})
	if !d2.(*Device).Authorized() {
		t.Fatal("token not restored from cache")
	}
}

func TestPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(`{"data":{"viewer":{"homes":[{"id":"h1","currentSubscription":{"priceInfo":{
			"today":[{"total":0.3012,"startsAt":"2026-11-20T00:00:00.000+01:00"},{"total":0.2911,"startsAt":"2026-11-20T00:15:00.000+01:00"}],
			"tomorrow":[]}}}]}}}`))
	}))
	defer srv.Close()
	dev, err := sdk.New(sdk.DeviceConfig{ID: "p", Type: "tibber-price", Params: map[string]any{"token": "tok", "url": srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	from, _ := http.ParseTime("Thu, 19 Nov 2026 22:00:00 GMT")
	ps, err := dev.(sdk.TariffSource).Prices(context.Background(), from, from.Add(48*3600e9))
	if err != nil || len(ps) != 2 || ps[0].Value < 30.11 || ps[0].Value > 30.13 || ps[0].Duration.Minutes() != 15 {
		t.Fatalf("prices: %v %+v", err, ps)
	}
}
