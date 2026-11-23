package importer

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

const evccJSON = `[
 {"id":1,"created":"2025-03-01T18:00:00+01:00","finished":"2025-03-01T21:00:00+01:00","loadpoint":"Carport","vehicle":"ID.4",
  "chargedEnergy":20.5,"solarPercentage":40,"price":4.1,"pricePerKWh":0.2},
 {"id":2,"created":"2025-03-02T10:00:00+01:00","finished":"2025-03-02T12:00:00+01:00","loadpoint":"Zaun","vehicle":"",
  "chargedEnergy":10,"solarPercentage":null,"price":null,"pricePerKWh":0.3},
 {"id":3,"created":"2025-03-03T10:00:00+01:00","finished":"2025-03-03T11:00:00+01:00","loadpoint":"Garage","vehicle":"ID.4","chargedEnergy":5},
 {"id":4,"created":"2025-03-04T10:00:00+01:00","finished":"0001-01-01T00:00:00Z","loadpoint":"Carport","vehicle":"ID.4","chargedEnergy":1}
]`

func TestPlanEvcc(t *testing.T) {
	list, err := ParseEvccSessions([]byte(evccJSON))
	if err != nil || len(list) != 4 {
		t.Fatalf("parse: %v %d", err, len(list))
	}
	if w, err := ParseEvccSessions([]byte(`{"result":` + evccJSON + `}`)); err != nil || len(w) != 4 {
		t.Fatalf("wrapped: %v %d", err, len(w))
	}
	lps := []Asset{{ID: "wallbox-carport", Name: "Wallbox Carport", Kind: "ev-charger"}, {ID: "wallbox-zaun", Name: "Wallbox Zaun", Kind: "ev-charger"}}
	p := PlanEvcc(list, map[string]bool{"3": true}, lps, map[string]string{"Zaun": "wallbox-zaun"})
	if p.Total != 4 || p.New != 2 || p.Duplicate != 1 || p.Unfinished != 1 || p.WithPrice != 2 || p.EnergyWh != 30500 {
		t.Fatalf("preview %+v", p)
	}
	if len(p.Loadpoints) != 2 || p.Loadpoints[0] != (LoadpointMatch{Title: "Carport", Sessions: 1, Target: "wallbox-carport", Auto: true, RunKind: "charging"}) ||
		p.Loadpoints[1].Auto || p.Loadpoints[1].Target != "wallbox-zaun" {
		t.Fatalf("matches %+v", p.Loadpoints)
	}
	a, b := p.Runs[0], p.Runs[1]
	if a.Asset != "wallbox-carport" || a.Kind != "charging" || a.VehicleName != "ID.4" || a.Vehicle != "evcc:ID.4" || a.PVWh != 8200 || a.GridWh != 12300 || math.Abs(a.CostCt-410) > 1e-9 || a.ExternalID != "1" || a.CostUnknown {
		t.Errorf("session 1 %+v", a)
	}
	if !b.Guest || b.PVWh != 0 || math.Abs(b.CostCt-300) > 1e-9 {
		t.Errorf("session 2 %+v", b)
	}
	// unknown title (e.g. a heat pump set up as loadpoint) is skipped by default
	p = PlanEvcc(list[2:3], nil, lps, nil)
	if p.New != 0 || p.Excluded != 1 || p.Loadpoints[0].Target != Skip {
		t.Errorf("unknown loadpoint not skipped %+v", p)
	}
	// ... unless the user keeps it under its evcc name
	p = PlanEvcc(list[2:3], nil, lps, map[string]string{"Garage": Keep})
	if p.New != 1 || p.Runs[0].Asset != "Garage" || !p.Runs[0].CostUnknown || p.Runs[0].Kind != "charging" {
		t.Errorf("kept loadpoint %+v", p)
	}
}

func TestFetchEvcc(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(evccJSON))
	}))
	defer srv.Close()
	list, err := FetchEvcc(context.Background(), srv.URL+"/")
	if err != nil || len(list) != 4 {
		t.Fatalf("fetch: %v %d", err, len(list))
	}
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer deny.Close()
	if _, err := FetchEvcc(context.Background(), deny.URL); err == nil {
		t.Error("401 accepted")
	}
}

func TestEvccHeatersBecomeHeatingRuns(t *testing.T) {
	list, _ := ParseEvccSessions([]byte(`[{"id":9,"created":"2025-10-01T10:00:00Z","finished":"2025-10-01T11:00:00Z","loadpoint":"Warmwasserbereitung",
		"vehicle":"Warmwasserbereitung","chargedEnergy":3,"solarPercentage":100,"price":0.24}]`))
	assets := []Asset{{ID: "heizstab-oben", Name: "Heizstab Puffer oben", Kind: "heating-rod"}}
	p := PlanEvcc(list, nil, assets, map[string]string{"Warmwasserbereitung": "heizstab-oben"})
	r := p.Runs[0]
	if r.Kind != "heating" || r.Asset != "heizstab-oben" || r.VehicleName != "" || r.Guest || r.PVWh != 3000 || r.Attrs["evccLoadpoint"] != "Warmwasserbereitung" {
		t.Fatalf("heating run %+v", r)
	}
	// kept under its evcc name: still not a vehicle
	p = PlanEvcc(list, nil, nil, map[string]string{"Warmwasserbereitung": Keep})
	if p.Runs[0].Kind != "run" || p.Runs[0].VehicleName != "" {
		t.Fatalf("kept heater %+v", p.Runs[0])
	}
}
