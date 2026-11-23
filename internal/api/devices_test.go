package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/voltibri/voltibri/internal/store"
)

func TestPutDevice(t *testing.T) {
	h, st := setup(t, false)
	if rec := do(h, "POST", "/api/config/import", "roles: { grid: demo, pv: [demo] }\ndevices: [{ id: demo, type: demo }]\n", local); rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}

	// missing required secret is rejected with the field name
	rec := do(h, "PUT", "/api/devices/tibber", `{"device":{"type":"tibber","params":{"clientId":"abc"}}}`, local)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "clientSecret") {
		t.Fatalf("missing secret: %d %s", rec.Code, rec.Body.String())
	}
	// add
	rec = do(h, "PUT", "/api/devices/tibber", `{"device":{"type":"tibber","name":"Tibber","params":{"clientId":"abc","clientSecret":"s3cret"}}}`, local)
	if rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	// read back: secret redacted, other devices kept
	rec = do(h, "GET", "/api/devices/tibber", "", local)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "s3cret") || !strings.Contains(rec.Body.String(), store.Redacted) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	// change the client id without sending the secret again
	rec = do(h, "PUT", "/api/devices/tibber", `{"device":{"type":"tibber","name":"Tibber","params":{"clientId":"xyz"}}}`, local)
	if rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	cfg, vi, _ := st.Current(context.Background())
	if len(cfg.Devices) != 2 || cfg.Devices[1].Params["clientSecret"] != "s3cret" || cfg.Devices[1].Params["clientId"] != "xyz" || !strings.Contains(vi.Comment, "geändert") {
		t.Fatalf("stored: %+v %+v", cfg.Devices, vi)
	}

	// guards
	if rec := do(h, "PUT", "/api/devices/x", `{"device":{"id":"y","type":"demo"}}`, local); rec.Code != http.StatusBadRequest {
		t.Errorf("id mismatch: %d", rec.Code)
	}
	if rec := do(h, "PUT", "/api/devices/tibber", `{"device":{"type":"tibber"}}`, "192.168.1.5:4000"); rec.Code != http.StatusForbidden {
		t.Errorf("remote write: %d", rec.Code)
	}
	ro, _ := setup(t, true)
	if rec := do(ro, "PUT", "/api/devices/tibber", `{"device":{"type":"tibber"}}`, local); rec.Code != http.StatusConflict {
		t.Errorf("read-only: %d", rec.Code)
	}
}

func TestDeleteDeviceAndRoles(t *testing.T) {
	h, st := setup(t, false)
	if rec := do(h, "POST", "/api/config/import", "roles: { grid: demo, pv: [demo] }\ndevices: [{ id: demo, type: demo }, { id: p2, type: demo-producer }]\n", local); rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	// assign PV role to p2 while saving it
	rec := do(h, "PUT", "/api/devices/p2", `{"device":{"type":"demo-producer"},"roles":["pv"]}`, local)
	if rec.Code != 200 {
		t.Fatalf("roles: %d %s", rec.Code, rec.Body.String())
	}
	cfg, _, _ := st.Current(context.Background())
	if len(cfg.Roles.PV) != 2 || cfg.Roles.PV[1] != "p2" {
		t.Fatalf("pv roles %v", cfg.Roles.PV)
	}
	if rec := do(h, "PUT", "/api/devices/p2", `{"device":{"type":"demo-producer"},"roles":["boss"]}`, local); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown role: %d", rec.Code)
	}
	// a device with a role cannot be removed
	rec = do(h, "DELETE", "/api/devices/p2", "", local)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "PV-Erzeugung") {
		t.Fatalf("delete in use: %d %s", rec.Code, rec.Body.String())
	}
	do(h, "PUT", "/api/devices/p2", `{"device":{"type":"demo-producer"},"roles":[]}`, local)
	if rec := do(h, "DELETE", "/api/devices/p2", "", local); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	cfg, vi, _ := st.Current(context.Background())
	if len(cfg.Devices) != 1 || len(cfg.Roles.PV) != 1 || !strings.Contains(vi.Comment, "entfernt") {
		t.Fatalf("after delete: %+v %v %+v", cfg.Devices, cfg.Roles.PV, vi)
	}
	if rec := do(h, "DELETE", "/api/devices/nope", "", local); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
}

func TestPagesServed(t *testing.T) {
	h, _ := setup(t, false)
	for path, want := range map[string]string{"/berichte": "Pro Monat", "/konfiguration": "Infrastruktur", "/common.js": "voltibri", "/common.css": ".setup", "/": "common.js"} {
		if rec := do(h, "GET", path, "", local); rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{"/konfiguration": "/konfiguration", "//evil.com": "", "https://x": "", "/a?b": "", "": ""} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q", in, got)
		}
	}
}

func TestPutSection(t *testing.T) {
	h, st := setup(t, false)
	if rec := do(h, "POST", "/api/config/import", "roles: { grid: demo, pv: [demo] }\ndevices: [{ id: demo, type: demo }]\n", local); rec.Code != 200 {
		t.Fatalf("import: %d", rec.Code)
	}
	rec := do(h, "PUT", "/api/config/section/planner", `{"morningBufferSoC":0.3,"fillOnNegativePrice":true}`, local)
	if rec.Code != 200 {
		t.Fatalf("planner: %d %s", rec.Code, rec.Body.String())
	}
	cfg, vi, _ := st.Current(context.Background())
	if cfg.Planner.MorningBufferSoC != 0.3 || !cfg.Planner.FillOnNegativePrice || cfg.Planner.PVPessimism != 0.8 || !strings.Contains(vi.Comment, "Batteriestrategie") {
		t.Fatalf("planner stored: %+v %q", cfg.Planner, vi.Comment)
	}
	// 25 instead of 0.25 is rejected
	if rec := do(h, "PUT", "/api/config/section/planner", `{"morningBufferSoC":25}`, local); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "morningBufferSoC") {
		t.Errorf("range: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/api/config/section/battery", `{"capacityWh":13800,"maxChargeW":6000}`, local); rec.Code != 200 {
		t.Errorf("battery: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/api/config/section/planner", `{"typo":1}`, local); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := do(h, "PUT", "/api/config/section/devices", `{}`, local); rec.Code != http.StatusNotFound {
		t.Errorf("unknown section: %d", rec.Code)
	}
}

func TestCircuitSections(t *testing.T) {
	h, st := setup(t, false)
	do(h, "POST", "/api/config/import", "roles: { grid: demo, pv: [demo] }\ndevices: [{ id: demo, type: demo }, { id: a, type: demo-consumer }, { id: b, type: demo-consumer }]\n", local)
	if rec := do(h, "PUT", "/api/config/section/grid", `{"fuseA":63,"phases":3}`, local); rec.Code != 200 {
		t.Fatalf("grid: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/api/config/section/circuits", `[{"id":"wb","name":"Wallboxen","maxCurrent":32,"devices":["a","b"]}]`, local); rec.Code != 200 {
		t.Fatalf("circuits: %d %s", rec.Code, rec.Body.String())
	}
	// replacing with a shorter list really removes entries
	if rec := do(h, "PUT", "/api/config/section/circuits", `[{"id":"x","maxCurrent":16,"devices":["a"]}]`, local); rec.Code != 200 {
		t.Fatalf("circuits 2: %d", rec.Code)
	}
	cfg, _, _ := st.Current(context.Background())
	if cfg.Grid.FuseA != 63 || len(cfg.Circuits) != 1 || cfg.Circuits[0].ID != "x" || len(cfg.Circuits[0].Devices) != 1 {
		t.Fatalf("stored %+v %+v", cfg.Grid, cfg.Circuits)
	}
	if rec := do(h, "PUT", "/api/config/section/circuits", `[{"id":"x","maxCurrent":16,"devices":["ghost"]}]`, local); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown device accepted: %d", rec.Code)
	}
}
