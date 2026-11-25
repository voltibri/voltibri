package site

import (
	"context"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/sdk"
)

func TestSocGuard(t *testing.T) {
	var g socGuard
	bs := BatterySettings{MaxSoC: 0.8, MinSoC: 0.2, MaxDischargeW: 2000}
	// in between: charging free, discharge limited to the maximum
	if c, d := g.apply(bs, 0.5, -1, -1); c != -1 || d != 2000 {
		t.Fatalf("middle: %v %v", c, d)
	}
	// a lower limit (excluded loads) stays
	if _, d := g.apply(bs, 0.5, -1, 300); d != 300 {
		t.Fatalf("excluded loads: %v", d)
	}
	// upper target reached: no charging, until 2 % below it
	if c, _ := g.apply(bs, 0.8, 3000, -1); c != 0 {
		t.Fatalf("full: %v", c)
	}
	if c, _ := g.apply(bs, 0.79, 3000, -1); c != 0 {
		t.Fatalf("hysteresis: %v", c)
	}
	if c, _ := g.apply(bs, 0.77, 3000, -1); c != 3000 {
		t.Fatalf("below hysteresis: %v", c)
	}
	// lower target reached: no discharging, until 2 % above it
	if _, d := g.apply(bs, 0.2, -1, -1); d != 0 {
		t.Fatalf("empty: %v", d)
	}
	if _, d := g.apply(bs, 0.21, -1, -1); d != 0 {
		t.Fatalf("empty hysteresis: %v", d)
	}
	if _, d := g.apply(bs, 0.23, -1, -1); d != 2000 {
		t.Fatalf("above hysteresis: %v", d)
	}
	// no settings: untouched
	if c, d := g.apply(BatterySettings{}, 0.99, -1, -1); c != -1 || d != -1 {
		t.Fatalf("none: %v %v", c, d)
	}
}

type ctlOnly struct{}

func (ctlOnly) SetChargeLimit(context.Context, float64) error { return nil }
func (ctlOnly) ResetLimits(context.Context) error             { return nil }

type forceBat struct {
	fakeBat
	forced []float64
}

func (f *forceBat) ForceCharge(_ context.Context, w float64) error {
	f.forced = append(f.forced, w)
	return nil
}

func TestBatterySettingsByCapability(t *testing.T) {
	cfg := config.Default()
	// only a charge limit: upper target yes, nothing else
	s := &Site{cfg: cfg, batCtl: ctlOnly{}}
	if f := s.batteryFeatures(); !f.MaxSoC || f.MinSoC || f.MaxDischarge || f.ForceCharge {
		t.Fatalf("charge limit only: %+v", f)
	}
	if err := s.SetBatterySettings(BatterySettings{MaxSoC: 0.9}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBatterySettings(BatterySettings{MinSoC: 0.1}); err == nil {
		t.Fatal("lower target accepted without discharge limit")
	}
	if err := s.StartForceCharge(0.8); err == nil {
		t.Fatal("force charging accepted without force charger")
	}
	// no control at all
	s = &Site{cfg: cfg}
	if s.batteryFeatures().Any() {
		t.Fatal("features without control")
	}
	// everything
	bat := &forceBat{}
	s = &Site{cfg: cfg, batCtl: bat, forceCh: bat}
	if f := s.batteryFeatures(); !f.MaxSoC || !f.MinSoC || !f.MaxDischarge || !f.ForceCharge {
		t.Fatalf("all: %+v", f)
	}
	for _, bad := range []BatterySettings{{MaxSoC: 0.5, MinSoC: 0.45}, {MinSoC: 0.95}, {MaxSoC: 0.1}, {MaxDischargeW: -1}} {
		if err := s.SetBatterySettings(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := s.SetBatterySettings(BatterySettings{MaxSoC: 1, MinSoC: 0.2, MaxDischargeW: 2020}); err != nil {
		t.Fatal(err)
	}
	if s.batSet.MaxSoC != 0 || s.batSet.MaxDischargeW != 2000 {
		t.Fatalf("normalised: %+v", s.batSet)
	}
}

func TestForceCharge(t *testing.T) {
	cfg := config.Default()
	cfg.Battery.MaxChargeW = 4000
	bat := &forceBat{}
	cache := sdk.NewMemCache()
	s := &Site{cfg: cfg, batCtl: bat, forceCh: bat, batCache: cache, discharge: -1, plan: planner.Plan{}}
	s.state.SoC = 0.4
	if err := s.StartForceCharge(0.3); err == nil {
		t.Fatal("force charging below the SoC accepted")
	}
	if err := s.StartForceCharge(0.8); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.updateForce(ctx, 0.4, 0)
	if len(bat.forced) != 1 || bat.forced[0] != 4000 || !s.forcing {
		t.Fatalf("forced %v forcing %v", bat.forced, s.forcing)
	}
	// the limits are not written while forcing
	s.writeLimits(ctx, true)
	if len(bat.charge) != 0 {
		t.Fatalf("limits written during force charging: %v", bat.charge)
	}
	// a new site (configuration change) continues the force charging
	s2 := &Site{cfg: cfg, batCtl: bat, forceCh: bat, batCache: cache}
	s2.loadBattery()
	if s2.force == nil || s2.force.TargetSoC != 0.8 {
		t.Fatalf("force charging not restored: %+v", s2.force)
	}
	// target reached: force charging ends, back to the plan
	s.updateForce(ctx, 0.8, 0)
	if s.force != nil || s.forcing || len(bat.charge) != 1 {
		t.Fatalf("force %+v forcing %v limits %v", s.force, s.forcing, bat.charge)
	}
	s3 := &Site{cfg: cfg, batCtl: bat, forceCh: bat, batCache: cache}
	s3.loadBattery()
	if s3.force != nil {
		t.Fatal("ended force charging restored")
	}
	// an old force charging expires
	s.force = &ForceCharge{TargetSoC: 0.9, Since: time.Now().Add(-forceMax)}
	if w := s.forceWant(0.5, 0, -1); w != 0 || s.force != nil {
		t.Fatalf("expired force charging: %v %+v", w, s.force)
	}
}
