package sim

import (
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/planner"
)

func params(cal *calendar.Calendar, loc *time.Location) Params {
	return Params{
		Battery:  planner.Battery{CapacityWh: 13800, SoC: 0.5, MaxChargeW: 6000, MaxDischargeW: 6000, ChargeEff: 0.95, DischargeEff: 0.95, MinSoC: 0.05, MaxSoC: 1},
		FeedInCt: 8, ImportCt: 30, PVForecastError: 0.15, Seed: 1,
		Planner: planner.DefaultConfig(), Forecast: forecast.DefaultConfig(),
		Calendar: cal, Location: loc,
	}
}

// TestSummerComparison checks the core promise on synthetic summer data:
// far fewer hours at high SoC and more battery charging during negative
// prices, at nearly the same grid import.
func TestSummerComparison(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	cal, _ := calendar.New()
	s := Synthesize(time.Date(2026, 4, 1, 0, 0, 0, 0, loc), 120, DefaultSynth(), cal)
	p := params(cal, loc)

	imm := Run(s, Immediate{}, p)
	planned := NewPlanned(s, p)
	pl := Run(s, planned, p)
	t.Logf("sofort:   import %.0f kWh, export %.0f, >=90%% %.0f h, full %.0f h, morning SoC %.0f%%, neg-charge %.0f kWh, short nights %d",
		imm.ImportKWh, imm.ExportKWh, imm.HoursAbove90, imm.HoursFull, 100*imm.MorningSoCMean, imm.ChargedNegKWh, imm.ShortNights)
	t.Logf("voltibri: import %.0f kWh, export %.0f, >=90%% %.0f h, full %.0f h, morning SoC %.0f%%, neg-charge %.0f kWh, short nights %d, modes %v",
		pl.ImportKWh, pl.ExportKWh, pl.HoursAbove90, pl.HoursFull, 100*pl.MorningSoCMean, pl.ChargedNegKWh, pl.ShortNights, planned.Plans)

	if pl.HoursAbove90 > imm.HoursAbove90*0.6 {
		t.Errorf("expected clearly fewer hours above 90%%: %.0f vs %.0f", pl.HoursAbove90, imm.HoursAbove90)
	}
	if pl.ImportKWh > imm.ImportKWh*1.10 {
		t.Errorf("grid import grew too much: %.0f vs %.0f kWh", pl.ImportKWh, imm.ImportKWh)
	}
	if pl.ChargedNegKWh <= imm.ChargedNegKWh {
		t.Errorf("expected more charging during negative prices: %.0f vs %.0f", pl.ChargedNegKWh, imm.ChargedNegKWh)
	}
}

func TestBatteryEnergyBalance(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	cal, _ := calendar.New()
	s := Synthesize(time.Date(2026, 6, 1, 0, 0, 0, 0, loc), 10, DefaultSynth(), cal)
	p := params(cal, loc)
	r := Run(s, Immediate{}, p)
	// PV + import = load + export + battery losses/storage (within a few kWh)
	balance := r.PVKWh + r.ImportKWh - r.LoadKWh - r.ExportKWh
	if balance < -1 || balance > 0.15*r.ThroughputKWh+p.Battery.CapacityWh/1000 {
		t.Errorf("energy balance off: %.2f kWh (throughput %.1f)", balance, r.ThroughputKWh)
	}
}
