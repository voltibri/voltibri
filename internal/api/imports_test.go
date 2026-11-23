package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/store"
)

func TestEvccImportAPI(t *testing.T) {
	evcc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":7,"created":"2025-05-01T10:00:00Z","finished":"2025-05-01T12:00:00Z","loadpoint":"Carport","vehicle":"ID.4","chargedEnergy":12,"price":3.6},
			{"id":8,"created":"2025-05-02T10:00:00Z","finished":"2025-05-02T12:00:00Z","loadpoint":"Wer","vehicle":"","chargedEnergy":4}]`))
	}))
	defer evcc.Close()
	h, st := setup(t, false)
	body := `{"url":"` + evcc.URL + `","mapping":{"Carport":"="},"dryRun":true}`
	rec := do(h, "POST", "/api/imports/evcc", body, local)
	var res evccResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.Preview.New != 1 || res.Preview.Excluded != 1 || res.Import != nil {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	if ss, _ := st.Runs(context.Background(), run.Filter{Limit: 10}); len(ss) != 0 {
		t.Fatal("dry run wrote sessions")
	}
	rec = do(h, "POST", "/api/imports/evcc", `{"url":"`+evcc.URL+`","mapping":{"Carport":"=","Wer":"wb-x"}}`, local)
	res = evccResponse{}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.Import == nil || res.Import.Count != 2 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	ss, _ := st.Runs(context.Background(), run.Filter{Limit: 10})
	if len(ss) != 2 || ss[0].Asset != "wb-x" || ss[0].Source != "evcc" {
		t.Fatalf("stored %+v", ss)
	}
	// repeat: nothing new
	rec = do(h, "POST", "/api/imports/evcc", `{"url":"`+evcc.URL+`","mapping":{"Carport":"=","Wer":"wb-x"}}`, local)
	if !strings.Contains(rec.Body.String(), `"duplicate": 2`) || strings.Contains(rec.Body.String(), `"import"`) {
		t.Fatalf("repeat: %s", rec.Body.String())
	}
	if rec := do(h, "GET", "/api/imports", "", local); !strings.Contains(rec.Body.String(), `"source": "evcc"`) {
		t.Fatalf("list: %s", rec.Body.String())
	}
	path := "/api/imports/" + itoa(res.Import.ID) + "/undo"
	if rec := do(h, "POST", path, "", "10.0.0.9:1"); rec.Code != http.StatusForbidden {
		t.Errorf("remote undo: %d", rec.Code)
	}
	if rec := do(h, "POST", path, "", local); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"removed": 2`) {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", path, "", local); rec.Code != http.StatusConflict {
		t.Errorf("undo twice: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/imports/evcc", `{"url":"http://127.0.0.1:1"}`, local); rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable: %d", rec.Code)
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }

func TestAssetsAndRuns(t *testing.T) {
	h, st := setup(t, false)
	do(h, "POST", "/api/config/import", `roles: { grid: demo, pv: [demo] }
devices:
  - { id: demo, type: demo }
  - { id: openwb, name: OpenWB, type: none, kind: ev-charger, decommissioned: "2025-02-15" }
`, local)
	rec := do(h, "GET", "/api/assets", "", local)
	if !strings.Contains(rec.Body.String(), `"id": "openwb"`) || !strings.Contains(rec.Body.String(), `"retired": true`) || !strings.Contains(rec.Body.String(), `"runKind": "charging"`) {
		t.Fatalf("assets: %s", rec.Body.String())
	}
	st.SaveRun(context.Background(), run.Run{Asset: "openwb", Kind: "charging", EnergyWh: 1000})
	if rec := do(h, "GET", "/api/runs?asset=openwb", "", local); !strings.Contains(rec.Body.String(), `"asset": "openwb"`) {
		t.Fatalf("runs: %s", rec.Body.String())
	}
	if rec := do(h, "GET", "/api/runs/summary", "", local); !strings.Contains(rec.Body.String(), `"count": 1`) {
		t.Fatalf("summary: %s", rec.Body.String())
	}
	st.SaveRun(context.Background(), run.Run{Asset: "openwb", Kind: "charging", Start: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), EnergyWh: 2000, PVWh: 500})
	st.SaveRun(context.Background(), run.Run{Asset: "openwb", Kind: "charging", Start: time.Date(2025, 1, 20, 12, 0, 0, 0, time.UTC), EnergyWh: 1000})
	st.SaveRun(context.Background(), run.Run{Asset: "openwb", Kind: "heating", Start: time.Date(2025, 2, 1, 12, 0, 0, 0, time.UTC), EnergyWh: 9})
	rec = do(h, "GET", "/api/runs/series?kind=charging&year=2025", "", local)
	if !strings.Contains(rec.Body.String(), `"key": "2025-01"`) || !strings.Contains(rec.Body.String(), `"energyWh": 3000`) || strings.Contains(rec.Body.String(), "2025-02") {
		t.Fatalf("monthly: %s", rec.Body.String())
	}
	if rec := do(h, "GET", "/api/runs?year=2025&kind=charging&limit=1&offset=1", "", local); !strings.Contains(rec.Body.String(), `"energyWh": 2000`) {
		t.Fatalf("paging: %s", rec.Body.String())
	}
}

func TestRunSeriesBuckets(t *testing.T) {
	h, st := setup(t, false)
	do(h, "POST", "/api/config/import", "site: { timezone: Europe/Berlin }\nroles: { grid: demo, pv: [demo] }\ndevices: [{ id: demo, type: demo }]\n", local)
	loc, _ := time.LoadLocation("Europe/Berlin")
	for _, at := range []time.Time{time.Date(2025, 3, 1, 8, 10, 0, 0, loc), time.Date(2025, 3, 1, 8, 50, 0, 0, loc), time.Date(2025, 3, 1, 23, 30, 0, 0, loc), time.Date(2025, 3, 2, 0, 30, 0, 0, loc)} {
		st.SaveRun(context.Background(), run.Run{Asset: "wb", Kind: "charging", Start: at, End: at.Add(10 * time.Minute), EnergyWh: 1000})
	}
	// one day (to inclusive), per hour: 08 has two runs, 23 one; the run after midnight is outside
	rec := do(h, "GET", "/api/runs/series?bucket=hour&from=2025-03-01&to=2025-03-01", "", local)
	b := rec.Body.String()
	if !strings.Contains(b, `"key": "2025-03-01T08"`) || !strings.Contains(b, `"count": 2`) || !strings.Contains(b, `"key": "2025-03-01T23"`) || strings.Contains(b, "2025-03-02") {
		t.Fatalf("hours: %s", b)
	}
	rec = do(h, "GET", "/api/runs/series?bucket=day&from=2025-03-01&to=2025-03-31", "", local)
	if b := rec.Body.String(); !strings.Contains(b, `"key": "2025-03-01"`) || !strings.Contains(b, `"count": 3`) || !strings.Contains(b, `"key": "2025-03-02"`) {
		t.Fatalf("days: %s", b)
	}
	if rec := do(h, "GET", "/api/runs?from=2025-03-02&to=2025-03-02", "", local); strings.Count(rec.Body.String(), `"asset"`) != 1 {
		t.Fatalf("runs in range: %s", rec.Body.String())
	}
}

func TestReportsCombineProfileAndRuns(t *testing.T) {
	h, st := setup(t, false)
	do(h, "POST", "/api/config/import", `site: { timezone: Europe/Berlin }
roles: { grid: demo, pv: [demo] }
devices:
  - { id: demo, type: demo }
  - { id: wb, type: none, kind: ev-charger }
  - { id: fridge, type: none, kind: appliance }
`, local)
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Berlin")
	day := func(d, hh int) time.Time { return time.Date(2026, 3, d, hh, 0, 0, 0, loc) }
	// imported run before the profile: 10 kWh on the 1st
	st.ImportRuns(ctx, store.Import{Source: "evcc"}, []run.Run{{Asset: "wb", Kind: "charging", Start: day(1, 20), End: day(1, 23), EnergyWh: 10000, PVWh: 0, GridWh: 10000, CostCt: 300, ExternalID: "1"}})
	// measured run on the 2nd overnight, profile 23:00 and 00:00 (4 + 6 kWh)
	st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Start: day(2, 23), End: day(3, 1), EnergyWh: 999})
	st.AppendProfile(ctx, day(2, 23), []store.ProfileValue{{Asset: "wb", ImportWh: 4000, GridWh: 4000, CostCt: 120}, {Asset: "fridge", ImportWh: 50}})
	st.AppendProfile(ctx, day(3, 0), []store.ProfileValue{{Asset: "wb", ImportWh: 6000, PVWh: 6000, CostCt: 48}})

	rec := do(h, "GET", "/api/runs/series?kind=charging&bucket=day&from=2026-03-01&to=2026-03-31", "", local)
	var b []store.RunBucket
	json.Unmarshal(rec.Body.Bytes(), &b)
	got := map[string]store.RunBucket{}
	for _, x := range b {
		got[x.Key] = x
	}
	// the overnight run is split by the profile, its own energy (999) is not counted twice
	if got["2026-03-01"].EnergyWh != 10000 || got["2026-03-02"].EnergyWh != 4000 || got["2026-03-02"].Count != 1 ||
		got["2026-03-03"].EnergyWh != 6000 || got["2026-03-03"].PVWh != 6000 || got["2026-03-03"].Count != 0 {
		t.Fatalf("series %s", rec.Body.String())
	}
	rec = do(h, "GET", "/api/runs/summary?from=2026-03-01&to=2026-03-31", "", local)
	var s []store.RunSummary
	json.Unmarshal(rec.Body.Bytes(), &s)
	sum := map[string]store.RunSummary{}
	for _, x := range s {
		sum[x.Asset] = x
	}
	if w := sum["wb"]; w.Count != 2 || w.EnergyWh != 20000 || w.CostCt != 468 || w.SolarWh != 6000 {
		t.Fatalf("wallbox summary %+v", w)
	}
	if f := sum["fridge"]; f.Count != 0 || f.EnergyWh != 50 || f.Kind != "run" {
		t.Fatalf("fridge summary %+v (%s)", f, rec.Body.String())
	}
}

func TestVehicleFilter(t *testing.T) {
	h, st := setup(t, false)
	do(h, "POST", "/api/config/import", "roles: { grid: demo, pv: [demo] }\ndevices: [{ id: demo, type: demo }, { id: wb, type: none, kind: ev-charger }]\n", local)
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	st.ImportRuns(ctx, store.Import{Source: "evcc"}, []run.Run{
		{Asset: "wb", Kind: "charging", Vehicle: "evcc:ID.3", VehicleName: "ID.3", Start: t0, EnergyWh: 1000, ExternalID: "1"},
		{Asset: "wb", Kind: "charging", Vehicle: "evcc:ID.3", VehicleName: "ID.3", Start: t0.Add(time.Hour), EnergyWh: 2000, ExternalID: "2"},
		{Asset: "wb", Kind: "charging", Vehicle: "guest", VehicleName: "Gastfahrzeug", Guest: true, Start: t0.Add(2 * time.Hour), EnergyWh: 500, ExternalID: "3"}})
	// the same car from voltibri (other key, same name) counts too
	st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Vehicle: "tibber/xyz", VehicleName: "ID.3", Start: t0.Add(3 * time.Hour), EnergyWh: 4000})
	rec := do(h, "GET", "/api/runs/vehicles?kind=charging", "", local)
	var vs []store.RunVehicle
	json.Unmarshal(rec.Body.Bytes(), &vs)
	if len(vs) != 2 || vs[0].Name != "ID.3" || vs[0].Count != 3 || !vs[1].Guest {
		t.Fatalf("vehicles %s", rec.Body.String())
	}
	rec = do(h, "GET", "/api/runs/summary?vehicle=ID.3", "", local)
	var s []store.RunSummary
	json.Unmarshal(rec.Body.Bytes(), &s)
	if len(s) != 1 || s[0].EnergyWh != 7000 || s[0].Count != 3 {
		t.Fatalf("summary %s", rec.Body.String())
	}
	if rec := do(h, "GET", "/api/runs?vehicle=Gastfahrzeug", "", local); strings.Count(rec.Body.String(), `"asset"`) != 1 {
		t.Fatalf("runs %s", rec.Body.String())
	}
}
