package site

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/internal/store"
)

type memHist struct {
	runs    []run.Run
	profile map[time.Time][]store.ProfileValue
}

func (m *memHist) LoadHistory(context.Context, time.Time, *time.Location) (series.Series, error) {
	return nil, nil
}
func (m *memHist) AppendHistory(context.Context, series.Record) error { return nil }
func (m *memHist) SaveRun(_ context.Context, r run.Run) (int64, error) {
	m.runs = append(m.runs, r)
	return int64(len(m.runs)), nil
}
func (m *memHist) Runs(context.Context, run.Filter) ([]run.Run, error) { return m.runs, nil }
func (m *memHist) AppendProfile(_ context.Context, slot time.Time, v []store.ProfileValue) error {
	if m.profile == nil {
		m.profile = map[time.Time][]store.ProfileValue{}
	}
	m.profile[slot] = append(m.profile[slot], v...)
	return nil
}

func TestProfileAndRunDetection(t *testing.T) {
	off := false
	cfg := config.Default()
	cfg.Roles.Grid = "grid"
	cfg.Devices = []config.Device{{ID: "grid", Type: "demo"}, {ID: "washer", Type: "shelly", Icon: "plug"},
		{ID: "fridge", Type: "shelly", Runs: &off}}
	h := &memHist{}
	s := &Site{cfg: cfg, hist: h}
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	// washer: 2 kW for 30 min, 2 min pause (part of the program), 1 kW for 10 min, then off
	power := func(m float64) float64 {
		switch {
		case m < 30:
			return 2000
		case m < 32:
			return 2
		case m < 42:
			return 1000
		}
		return 0
	}
	// house: 3 kW load, half from the grid, half PV; grid price 30 ct, feed-in 8 ct
	for i := 0; i <= 60*6; i++ { // 60 minutes in 10 s steps
		now := t0.Add(time.Duration(i) * 10 * time.Second)
		st := State{Time: now, LoadW: 3000, GridW: 1500, PVW: 1500,
			Consumers: []Reading{{ID: "washer", PowerW: power(float64(i) / 6)}, {ID: "fridge", PowerW: 80}}}
		s.sampleProfile(st, sourcesOf(st, 30, 8))
	}
	if len(h.runs) != 1 {
		t.Fatalf("runs: %+v", h.runs)
	}
	r := h.runs[0]
	// 1000 Wh + 167 Wh, the short pause did not end the run
	if r.Asset != "washer" || r.Kind != "run" || math.Abs(r.EnergyWh-1166.7) > 15 || !r.End.Equal(t0.Add(42*time.Minute)) ||
		math.Abs(r.GridWh-r.EnergyWh/2) > 1 || math.Abs(r.CostCt-r.EnergyWh/1000*19) > 1 {
		t.Fatalf("run %+v", r)
	}
	s.flushProfile(context.Background(), t0)
	var wash, fridge, grid store.ProfileValue
	for _, v := range h.profile[t0] {
		switch v.Asset {
		case "washer":
			wash = v
		case "fridge":
			fridge = v
		case "grid.grid":
			grid = v
		}
	}
	if math.Abs(wash.ImportWh-1166.7) > 15 || math.Abs(fridge.ImportWh-80) > 1 || math.Abs(grid.ImportWh-1500) > 5 || math.Abs(wash.PVWh-wash.ImportWh/2) > 1 {
		t.Fatalf("profile wash %+v fridge %+v grid %+v", wash, fridge, grid)
	}
}
