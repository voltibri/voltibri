package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/run"
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

func TestCachePersists(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := s.Cache("dev-a"), s.Cache("dev-b")
	if _, _, ok := a.Get("k"); ok {
		t.Fatal("empty cache returned a value")
	}
	a.Put("k", []byte("eins"))
	a.Put("k", []byte("zwei"))
	b.Put("k", []byte("andere"))
	s.Close()

	s, _ = Open(dir)
	defer s.Close()
	v, ts, ok := s.Cache("dev-a").Get("k")
	if !ok || string(v) != "zwei" || time.Since(ts) > time.Minute {
		t.Errorf("after reopen: %q %v %v", v, ts, ok)
	}
	if v, _, _ := s.Cache("dev-b").Get("k"); string(v) != "andere" {
		t.Errorf("namespaces mixed: %q", v)
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

	// import keeps existing measurements unless overwrite is requested
	imp := series.Series{
		{Time: t0, PVW: 1, LoadW: 1, SpotCt: math.NaN(), TempC: math.NaN()},
		{Time: t0.Add(2 * series.Slot), PVW: 2, LoadW: 2, SpotCt: math.NaN(), TempC: math.NaN()},
	}
	n, err := s.ImportHistory(ctx, imp, false)
	if err != nil || n != 1 {
		t.Fatalf("import: %d %v", n, err)
	}
	got, _ = s.LoadHistory(ctx, t0.Add(-time.Hour), loc)
	if len(got) != 3 || got[0].PVW != 5000 {
		t.Errorf("existing slot overwritten: %+v", got[0])
	}
	if n, _ := s.ImportHistory(ctx, imp, true); n != 2 {
		t.Errorf("overwrite import wrote %d", n)
	}
	hi, _ := s.HistoryInfo(ctx)
	if hi.Slots != 3 || !hi.First.Equal(t0) {
		t.Errorf("info %+v", hi)
	}
}

func TestRuns(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	start := time.Unix(1795000000, 0)
	for i, wh := range []float64{1000, 2000} {
		if _, err := st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Vehicle: "guest", VehicleName: "Gast", Guest: true, Mode: "fast",
			Start: start.Add(time.Duration(i) * time.Hour), End: start.Add(time.Duration(i)*time.Hour + 30*time.Minute),
			EnergyWh: wh, PVWh: wh / 2, BatteryWh: wh / 4, GridWh: wh / 4, CostCt: wh * 0.03, PriceNote: "Gast", Attrs: map[string]any{"socEnd": 0.8}}); err != nil {
			t.Fatal(err)
		}
	}
	st.SaveRun(ctx, run.Run{Asset: "rod", Kind: "heating", Start: start, End: start.Add(time.Hour), EnergyWh: 3000, PVWh: 3000})
	got, err := st.Runs(ctx, run.Filter{Kind: "charging", Limit: 10})
	if err != nil || len(got) != 2 || got[0].EnergyWh != 2000 || !got[0].Guest || got[0].Mode != "fast" || got[0].BatteryWh != 500 ||
		got[0].SolarWh() != 1500 || got[0].Attrs["socEnd"] != 0.8 || got[0].PriceNote != "Gast" || got[0].Source != "" {
		t.Fatalf("runs: %v %+v", err, got)
	}
	if got, _ := st.Runs(ctx, run.Filter{Asset: "rod"}); len(got) != 1 || got[0].Kind != "heating" {
		t.Fatalf("by asset: %+v", got)
	}
	sum, _ := st.RunSummaries(ctx, run.Filter{})
	if len(sum) != 2 || sum[1].Asset != "wb" || sum[1].Count != 2 || sum[1].EnergyWh != 3000 || sum[1].SolarWh != 2250 {
		t.Fatalf("summaries %+v", sum)
	}
}

func TestImportAndUndo(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	start := time.Unix(1790000000, 0)
	own, _ := st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Start: start, End: start.Add(time.Hour), EnergyWh: 500})
	evcc := func(id string, wh float64) run.Run {
		return run.Run{Asset: "wb", Kind: "charging", VehicleName: "Auto", Start: start.Add(-48 * time.Hour), End: start.Add(-47 * time.Hour),
			EnergyWh: wh, CostUnknown: true, ExternalID: id}
	}
	imp, skipped, err := st.ImportRuns(ctx, Import{Source: "evcc", Author: "nils", Params: map[string]any{"url": "http://evcc"}},
		[]run.Run{evcc("1", 1000), evcc("2", 2000)})
	if err != nil || imp.ID == 0 || imp.Count != 2 || skipped != 0 {
		t.Fatalf("import: %+v %d %v", imp, skipped, err)
	}
	imp2, skipped, _ := st.ImportRuns(ctx, Import{Source: "evcc", Author: "nils"}, []run.Run{evcc("2", 2000), evcc("3", 3000)})
	if imp2.Count != 1 || skipped != 1 {
		t.Fatalf("second import: %+v %d", imp2, skipped)
	}
	if imp3, skipped, _ := st.ImportRuns(ctx, Import{Source: "evcc"}, []run.Run{evcc("3", 3000)}); imp3.ID != 0 || skipped != 1 {
		t.Fatalf("empty import recorded: %+v", imp3)
	}
	ss, _ := st.Runs(ctx, run.Filter{Limit: 10})
	if len(ss) != 4 || ss[0].ID != own || ss[0].Source != "" || ss[1].Source != "evcc" || !ss[1].CostUnknown || ss[1].ImportID == 0 {
		t.Fatalf("runs: %+v", ss)
	}
	if sum, _ := st.RunSummaries(ctx, run.Filter{}); sum[0].Imported != 3 {
		t.Errorf("imported count %+v", sum)
	}
	n, err := st.UndoImport(ctx, imp.ID)
	if err != nil || n != 2 {
		t.Fatalf("undo: %d %v", n, err)
	}
	if _, err := st.UndoImport(ctx, imp.ID); err != ErrUndone {
		t.Errorf("undo twice: %v", err)
	}
	ss, _ = st.Runs(ctx, run.Filter{Limit: 10})
	if len(ss) != 2 || ss[1].ExternalID != "3" {
		t.Fatalf("after undo: %+v", ss)
	}
	ims, _ := st.Imports(ctx)
	if len(ims) != 2 || ims[1].UndoneAt == nil || ims[1].Params["url"] != "http://evcc" || ims[0].UndoneAt != nil {
		t.Fatalf("imports: %+v", ims)
	}
	if imp4, _, _ := st.ImportRuns(ctx, Import{Source: "evcc"}, []run.Run{evcc("1", 1000)}); imp4.Count != 1 {
		t.Fatalf("reimport: %+v", imp4)
	}
}

func TestOverlappingImportHidesMeasuredRun(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	t0 := time.Unix(1795000000, 0)
	st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour), EnergyWh: 1800})
	st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Start: t0.Add(20 * time.Hour), End: t0.Add(21 * time.Hour), EnergyWh: 900})  // after
	st.SaveRun(ctx, run.Run{Asset: "other", Kind: "charging", Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour), EnergyWh: 700}) // other asset
	imp, _, _ := st.ImportRuns(ctx, Import{Source: "evcc"}, []run.Run{{Asset: "wb", Kind: "charging", Start: t0, End: t0.Add(12 * time.Hour), EnergyWh: 18400, ExternalID: "1"}})
	got, _ := st.Runs(ctx, run.Filter{})
	if len(got) != 3 {
		t.Fatalf("visible runs: %+v", got)
	}
	for _, r := range got {
		if r.EnergyWh == 1800 {
			t.Fatal("overlapping measured run still visible")
		}
	}
	if all, _ := st.Runs(ctx, run.Filter{All: true}); len(all) != 4 {
		t.Errorf("all: %d", len(all))
	}
	if sum, _ := st.RunSummaries(ctx, run.Filter{}); sum[1].Asset != "wb" || sum[1].Count != 2 || sum[1].EnergyWh != 19300 {
		t.Errorf("summary %+v", sum)
	}
	st.UndoImport(ctx, imp.ID)
	if got, _ := st.Runs(ctx, run.Filter{Asset: "wb"}); len(got) != 2 {
		t.Errorf("after undo: %+v", got)
	}
}

func TestLoadProfile(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Berlin")
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, loc)
	for i := 0; i < 8; i++ { // two hours
		st.AppendProfile(ctx, t0.Add(time.Duration(i)*15*time.Minute), []ProfileValue{
			{Asset: "wb", ImportWh: 2750, PVWh: 2000, BatteryWh: 250, GridWh: 500, CostCt: 15.123},
			{Asset: "fridge", ImportWh: 12.34},
			{Asset: "idle"}, // not stored
			{Asset: "pv", ExportWh: 1500},
		})
	}
	var rows int
	st.db.QueryRow(`SELECT COUNT(*) FROM load_profile`).Scan(&rows)
	if rows != 24 {
		t.Fatalf("rows %d (zero slots must not be stored)", rows)
	}
	sums, err := st.ProfileSums(ctx, []string{"wb", "fridge"}, t0, t0.Add(2*time.Hour), loc, BucketHour)
	if err != nil || len(sums) != 4 || sums[0].Key != "2026-06-01T10" || sums[0].Asset != "fridge" || math.Abs(sums[0].ImportWh-49.2) > 1e-9 ||
		sums[1].ImportWh != 11000 || sums[1].PVWh != 8000 || math.Abs(sums[1].CostCt-60.492) > 1e-9 {
		t.Fatalf("sums %v %+v", err, sums)
	}
	if tot, _ := st.ProfileSums(ctx, nil, time.Time{}, time.Time{}, loc, ""); len(tot) != 3 {
		t.Fatalf("totals %+v", tot)
	}
	starts, _ := st.ProfileStarts(ctx)
	if !starts["wb"].Equal(t0) || len(starts) != 3 {
		t.Fatalf("starts %v", starts)
	}
	// runs covered by the profile can be left out
	st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Start: t0.Add(-time.Hour), EnergyWh: 1})
	st.SaveRun(ctx, run.Run{Asset: "wb", Kind: "charging", Start: t0.Add(time.Hour), EnergyWh: 2})
	if rs, _ := st.Runs(ctx, run.Filter{ProfiledFrom: starts}); len(rs) != 1 || rs[0].EnergyWh != 1 {
		t.Fatalf("profiled runs %+v", rs)
	}
}
