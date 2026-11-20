package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/sdk"
)

type nopDevice struct{ id string }

func (d nopDevice) ID() string { return d.id }

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type: "store-test", Name: "Test", Category: sdk.CatMeter,
		Capabilities: []sdk.Capability{sdk.CapGrid},
		Params: []sdk.Param{
			{Name: "host", Label: "Host", Type: sdk.TypeHost, Required: true},
			{Name: "password", Label: "Passwort", Type: sdk.TypeSecret},
		},
		Factory: func(c sdk.DeviceConfig) (sdk.Device, error) { return nopDevice{c.ID}, nil },
	})
}

func testConfig(password string) *config.Config {
	c := config.Default()
	c.Roles.Grid = "meter"
	c.Devices = []config.Device{{ID: "meter", Type: "store-test", Params: map[string]any{"host": "10.0.0.1", "password": password}}}
	return c
}

func TestConfigVersionsAndSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, _, err := s.Current(ctx); err != ErrNoConfig {
		t.Fatalf("expected ErrNoConfig, got %v", err)
	}
	var notified []int64
	s.OnChange(func(v int64) { notified = append(notified, v) })

	v1, err := s.Save(ctx, testConfig("hunter2"), "nils", "erste Einrichtung")
	if err != nil {
		t.Fatal(err)
	}
	// the secret must not be stored in plain text
	var raw string
	s.db.QueryRow(`SELECT config FROM config_versions WHERE id = ?`, v1).Scan(&raw)
	if strings.Contains(raw, "hunter2") || !strings.Contains(raw, encPrefix) {
		t.Fatalf("secret stored in plain text: %s", raw)
	}
	cur, vi, err := s.Current(ctx)
	if err != nil || cur.Devices[0].Params["password"] != "hunter2" || vi.Author != "nils" {
		t.Fatalf("current: %v %+v %v", cur, vi, err)
	}

	// saving a redacted export keeps the stored secret
	red := Redact(cur)
	if red.Devices[0].Params["password"] != Redacted {
		t.Fatal("not redacted")
	}
	red.Devices[0].Params["host"] = "10.0.0.2"
	v2, err := s.Save(ctx, red, "nils", "neue IP")
	if err != nil {
		t.Fatal(err)
	}
	cur, _, _ = s.Current(ctx)
	if cur.Devices[0].Params["password"] != "hunter2" || cur.Devices[0].Params["host"] != "10.0.0.2" {
		t.Fatalf("unexpected after redacted save: %v", cur.Devices[0].Params)
	}

	// rollback creates a new version with the old content
	v3, err := s.Rollback(ctx, v1, "nils")
	if err != nil {
		t.Fatal(err)
	}
	cur, vi, _ = s.Current(ctx)
	if vi.ID != v3 || cur.Devices[0].Params["host"] != "10.0.0.1" || !strings.Contains(vi.Comment, "Version 1") {
		t.Fatalf("rollback: %+v %v", vi, cur.Devices[0].Params)
	}
	vs, _ := s.Versions(ctx, 10)
	if len(vs) != 3 || vs[0].ID != v3 || vs[2].ID != v1 {
		t.Fatalf("versions %+v", vs)
	}
	if len(notified) != 3 || notified[1] != v2 {
		t.Errorf("notifications %v", notified)
	}

	// invalid configs are rejected
	bad := testConfig("x")
	bad.Devices[0].Params = map[string]any{}
	if _, err := s.Save(ctx, bad, "nils", ""); err == nil || !strings.Contains(err.Error(), "host: required") {
		t.Errorf("expected validation error, got %v", err)
	}

	// reopening with the same key file decrypts again
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	cur, _, err = s2.Current(ctx)
	if err != nil || cur.Devices[0].Params["password"] != "hunter2" {
		t.Fatalf("after reopen: %v %v", cur, err)
	}
}

func TestHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loc, _ := time.LoadLocation("Europe/Berlin")
	t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	recs := []series.Record{
		{Time: t0, PVW: 5000, LoadW: 600, SpotCt: -1.5, TempC: math.NaN()},
		{Time: t0.Add(series.Slot), PVW: 5100, LoadW: 650, SpotCt: math.NaN(), TempC: 24},
	}
	for _, r := range recs {
		if err := s.AppendHistory(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	recs[1].LoadW = 700 // replace
	s.AppendHistory(ctx, recs[1])
	got, err := s.LoadHistory(ctx, t0.Add(-time.Hour), loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SpotCt != -1.5 || got[0].HasTemp() || got[1].HasSpot() || got[1].LoadW != 700 || !got[1].Time.Equal(recs[1].Time) {
		t.Errorf("unexpected history %+v", got)
	}
}
