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

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	_ "github.com/voltibri/voltibri/internal/drivers/all"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/internal/server"
	"github.com/voltibri/voltibri/internal/site"
	"github.com/voltibri/voltibri/internal/store"
	"github.com/voltibri/voltibri/sdk"
)

type fakeRuntime struct {
	readOnly bool
	charge   *charging.Manager
}

func (f fakeRuntime) State() site.State  { return site.State{} }
func (f fakeRuntime) Plan() planner.Plan { return planner.Plan{} }
func (f fakeRuntime) Status() server.Status {
	return server.Status{ReadOnly: f.readOnly, ConfigSrc: "x.yaml"}
}
func (f fakeRuntime) ReadOnly() bool              { return f.readOnly }
func (f fakeRuntime) Charging() *charging.Manager { return f.charge }

func setup(t *testing.T, readOnly bool) (http.Handler, *store.Store) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return Handler(fakeRuntime{readOnly: readOnly}, st), st
}

func do(h http.Handler, method, path, body, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const local = "127.0.0.1:50000"

func TestConfigLifecycle(t *testing.T) {
	h, _ := setup(t, false)

	// drivers with schemas
	rec := do(h, "GET", "/api/drivers", "", local)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"type": "fronius"`) || !strings.Contains(rec.Body.String(), `"params"`) {
		t.Fatalf("drivers: %d %s", rec.Code, rec.Body.String()[:200])
	}

	// empty config at start
	rec = do(h, "GET", "/api/config", "", local)
	var cr configResponse
	json.Unmarshal(rec.Body.Bytes(), &cr)
	if rec.Code != 200 || cr.Version.ID != 0 || len(cr.Config.Devices) != 0 {
		t.Fatalf("initial config: %d %s", rec.Code, rec.Body.String())
	}

	// invalid config is rejected with details
	rec = do(h, "PUT", "/api/config", `{"config":{"devices":[{"id":"x","type":"shelly"}],"roles":{"grid":"x"}}}`, local)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot act as grid") {
		t.Fatalf("invalid save: %d %s", rec.Code, rec.Body.String())
	}

	// valid config via YAML import
	yaml := `roles: { grid: demo, pv: [demo], battery: demo }
battery: { capacityWh: 10000 }
devices:
  - { id: demo, type: demo, mode: owned }
`
	rec = do(h, "POST", "/api/config/import", yaml, local)
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}

	// edit via JSON
	rec = do(h, "GET", "/api/config", "", local)
	json.Unmarshal(rec.Body.Bytes(), &cr)
	cr.Config.Devices[0].Name = "Testanlage"
	body, _ := json.Marshal(map[string]any{"config": cr.Config, "comment": "Name gesetzt"})
	rec = do(h, "PUT", "/api/config", string(body), local)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id": 2`) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(h, "GET", "/api/config/versions", "", local)
	var vs []store.VersionInfo
	json.Unmarshal(rec.Body.Bytes(), &vs)
	if len(vs) != 2 || vs[0].Comment != "Name gesetzt" || vs[1].Comment != "YAML-Import" {
		t.Fatalf("versions: %s", rec.Body.String())
	}

	rec = do(h, "POST", "/api/config/versions/1/rollback", "", local)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id": 3`) {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(h, "GET", "/api/config/export", "", local)
	c, err := config.Parse(rec.Body.Bytes())
	if rec.Code != 200 || err != nil || c.Devices[0].Name != "" {
		t.Fatalf("export after rollback: %d %v %s", rec.Code, err, rec.Body.String())
	}
}

func TestWritesRestricted(t *testing.T) {
	h, _ := setup(t, false)
	if rec := do(h, "PUT", "/api/config", `{"config":{}}`, "192.168.1.20:1234"); rec.Code != http.StatusForbidden {
		t.Errorf("remote write: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", "192.168.1.20:1234"); rec.Code != 200 {
		t.Errorf("remote read: %d", rec.Code)
	}
	ro, _ := setup(t, true)
	if rec := do(ro, "PUT", "/api/config", `{"config":{}}`, local); rec.Code != http.StatusConflict {
		t.Errorf("read-only write: %d", rec.Code)
	}
}

func TestFavicon(t *testing.T) {
	h, _ := setup(t, false)
	rec := do(h, "GET", "/favicon.svg", "", local)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("favicon: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = do(h, "GET", "/", "", local)
	if !strings.Contains(rec.Body.String(), `href="/favicon.svg"`) {
		t.Fatal("index.html does not link the favicon")
	}
}

func TestHistory(t *testing.T) {
	h, st := setup(t, false)
	now := series.SlotStart(time.Now())
	for i, ago := range []time.Duration{3 * time.Hour, time.Hour, 15 * time.Minute} {
		r := series.Record{Time: now.Add(-ago), PVW: float64(100 * (i + 1)), LoadW: 50, SpotCt: math.NaN(), TempC: math.NaN()}
		if err := st.AppendHistory(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	rec := do(h, "GET", "/api/history", "", local)
	var got []historySlot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
	if len(got) != 2 || got[0].PVW != 200 || got[1].PVW != 300 {
		t.Fatalf("want the 2 slots of the last 2 h, got %+v", got)
	}
	if rec := do(h, "GET", "/api/history?hours=99", "", local); rec.Code != 400 {
		t.Fatalf("hours=99: want 400, got %d", rec.Code)
	}
}

type testCharger struct{}

func (testCharger) ID() string { return "wb" }
func (testCharger) ChargerStatus(context.Context) (sdk.ChargerStatus, error) {
	return sdk.ChargerStatus{Connected: true}, nil
}

func TestChargingEndpoints(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.Devices = []config.Device{{ID: "wb", Type: "x", Name: "Carport"}}
	m, err := charging.New(cfg, time.UTC, map[string]sdk.Device{"wb": testCharger{}}, st.Cache("charging"), st)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(context.Background(), charging.SiteInput{Now: time.Now(), Chargers: map[string]sdk.ChargerStatus{"wb": {Connected: true}}})
	h := Handler(fakeRuntime{charge: m}, st)

	rec := do(h, "GET", "/api/charging", "", local)
	var cr chargingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil || len(cr.Loadpoints) != 1 || cr.Loadpoints[0].Vehicle != charging.Guest || !cr.Local {
		t.Fatalf("charging: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/api/loadpoints/wb/guestmode", `{"mode":"pv"}`, local); rec.Code != 204 {
		t.Fatalf("guest mode: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/api/loadpoints/wb/guestmode", `{"mode":"turbo"}`, local); rec.Code != 400 {
		t.Fatalf("bad mode: %d", rec.Code)
	}
	if rec := do(h, "PUT", "/api/loadpoints/wb/guestmode", `{"mode":"pv"}`, "192.168.1.5:4000"); rec.Code != 403 {
		t.Fatalf("remote write: %d", rec.Code)
	}
	if rec := do(h, "PUT", "/api/vehicles/tibber/nope", `{"mode":"fast"}`, local); rec.Code != 400 {
		t.Fatalf("unknown vehicle: %d", rec.Code)
	}
	rec = do(h, "GET", "/api/oauth/callback?state=x&code=y", "", local)
	if rec.Code != 302 || !strings.Contains(rec.Header().Get("Location"), "#laden") {
		t.Fatalf("callback: %d %v", rec.Code, rec.Header())
	}
	if rec := do(h, "GET", "/api/sessions", "", local); rec.Code != 200 {
		t.Fatalf("sessions: %d", rec.Code)
	}
}
