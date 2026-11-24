package site

import (
	"context"
	"testing"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/planner"
)

type fakeBat struct{ charge, discharge []float64 }

func (f *fakeBat) SetChargeLimit(context.Context, float64) error { return nil }
func (f *fakeBat) ResetLimits(context.Context) error             { return nil }
func (f *fakeBat) SetLimits(_ context.Context, c, d float64) error {
	f.charge, f.discharge = append(f.charge, c), append(f.discharge, d)
	return nil
}

func TestDischargeLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Battery.MaxDischargeW = 5000
	cfg.Devices = []config.Device{{ID: "hp", Type: "shelly", Icon: "heatpump", BatteryUse: config.BatteryNever}, {ID: "tv", Type: "shelly"}}
	bat := &fakeBat{}
	s := &Site{cfg: cfg, batCtl: bat, discharge: -1, plan: planner.Plan{}}
	ctx := context.Background()
	// heat pump 2 kW (excluded), TV 300 W, house rest 500 W, no PV: battery may give 800 W (+margin)
	st := State{HasBattery: true, LoadW: 2800, PVW: 0, Consumers: []Reading{{ID: "hp", Name: "Wärmepumpe", PowerW: 2000}, {ID: "tv", PowerW: 300}}}
	s.updateDischargeLimit(ctx, &st, 0)
	if st.DischargeLimitW != 850 || st.ExcludedW != 2000 || len(st.Excluded) != 1 || bat.discharge[len(bat.discharge)-1] != 850 {
		t.Fatalf("limit %v excluded %v %v writes %v", st.DischargeLimitW, st.ExcludedW, st.Excluded, bat.discharge)
	}
	// plus an EV charging excluded (from the charging manager) and 1 kW PV
	st = State{HasBattery: true, LoadW: 13800, PVW: 1000, Consumers: st.Consumers}
	s.limits.at = s.limits.at.Add(-time10s) // allow the next write
	s.updateDischargeLimit(ctx, &st, 11000)
	if st.DischargeLimitW != 0+50 {
		t.Fatalf("with EV: %v", st.DischargeLimitW)
	}
	// heat pump off, nothing excluded: no limit
	st = State{HasBattery: true, LoadW: 800, Consumers: []Reading{{ID: "hp", PowerW: 0}, {ID: "tv", PowerW: 300}}}
	s.limits.at = s.limits.at.Add(-time10s)
	s.updateDischargeLimit(ctx, &st, 0)
	if st.DischargeLimitW != -1 || bat.discharge[len(bat.discharge)-1] != -1 {
		t.Fatalf("no exclusion: %v %v", st.DischargeLimitW, bat.discharge)
	}
}

const time10s = 11e9
