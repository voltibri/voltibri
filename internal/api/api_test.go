package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voltibri/voltibri/internal/config"
	_ "github.com/voltibri/voltibri/internal/drivers/all"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/server"
	"github.com/voltibri/voltibri/internal/site"
	"github.com/voltibri/voltibri/internal/store"
)

type fakeRuntime struct{ readOnly bool }

func (f fakeRuntime) State() site.State  { return site.State{} }
func (f fakeRuntime) Plan() planner.Plan { return planner.Plan{} }
func (f fakeRuntime) Status() server.Status {
	return server.Status{ReadOnly: f.readOnly, ConfigSrc: "x.yaml"}
}
func (f fakeRuntime) ReadOnly() bool { return f.readOnly }

func setup(t *testing.T, readOnly bool) (http.Handler, *store.Store) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return Handler(fakeRuntime{readOnly}, st), st
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
