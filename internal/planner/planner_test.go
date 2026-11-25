package planner

import (
	"math"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/series"
)

var berlin, _ = time.LoadLocation("Europe/Berlin")

// constLoad forecasts a constant load.
type constLoad struct{ w float64 }

func (c constLoad) Ready() bool { return true }
func (c constLoad) Slot(t, cutoff time.Time, tempC float64) forecast.Estimate {
	return forecast.Estimate{P50: c.w, PQ: c.w * 1.2, N: 10}
}
func (c constLoad) Energy(from, to, cutoff time.Time, tempC float64) forecast.Estimate {
	h := to.Sub(from).Hours()
	return forecast.Estimate{P50: c.w * h, PQ: c.w * h * 1.2, N: 10}
}

// summerPV returns a bell shaped PV curve between 06:00 and 21:00 with the
// given peak in W.
func summerPV(t time.Time, peak float64) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60
	if h < 6 || h > 21 {
		return 0
	}
	return peak * math.Pow(math.Sin(math.Pi*(h-6)/15), 1.5)
}

func slots(start time.Time, hours int, pv func(time.Time) float64, spot func(time.Time) float64) []InputSlot {
	var out []InputSlot
	for t := start; t.Before(start.Add(time.Duration(hours) * time.Hour)); t = t.Add(series.Slot) {
		s := InputSlot{Start: t, PVW: pv(t), SpotCt: math.NaN()}
		if spot != nil {
			s.SpotCt = spot(t)
		}
		out = append(out, s)
	}
	return out
}

func battery(soc float64) Battery {
	return Battery{CapacityWh: 13800, SoC: soc, MaxChargeW: 6000, MaxDischargeW: 6000, ChargeEff: 0.95, DischargeEff: 0.95, MinSoC: 0.05, MaxSoC: 1}
}

func TestSunnyDayDelaysCharging(t *testing.T) {
	now := time.Date(2026, 6, 15, 8, 0, 0, 0, berlin)
	in := Input{Now: now, Battery: battery(0.25), TempC: math.NaN(),
		Slots: slots(now, 36, func(t time.Time) float64 { return summerPV(t, 8000) }, nil)}
	p := Make(in, constLoad{400}, DefaultConfig())
	if p.Mode != ModeDelay {
		t.Fatalf("mode = %s, reasons %v", p.Mode, p.Reasons)
	}
	if p.CurrentLimitW() != 0 {
		t.Errorf("at 08:00 the battery should wait, limit = %v", p.CurrentLimitW())
	}
	// night: 400 W * 1.2 (PQ) over the night; target well below 100 %
	if p.TargetSoC < 0.5 || p.TargetSoC > 0.9 {
		t.Errorf("unexpected target %.2f", p.TargetSoC)
	}
	// the battery must reach the target by the evening
	var atEvening float64
	for _, s := range p.Slots {
		if !s.Start.Before(*p.Evening) {
			break
		}
		atEvening = s.ExpectedSoC
	}
	if atEvening < p.TargetSoC-0.01 {
		t.Errorf("expected SoC at evening %.2f below target %.2f", atEvening, p.TargetSoC)
	}
	if max := maxSoCUntil(p, *p.Morning); max > p.TargetSoC+0.15 {
		t.Errorf("battery filled far beyond target: %.2f", max)
	}
	t.Logf("%v", p.Reasons)
}

func TestNegativePricesArePreferred(t *testing.T) {
	now := time.Date(2026, 6, 15, 8, 0, 0, 0, berlin)
	spot := func(t time.Time) float64 {
		if t.Day() == 15 && t.Hour() >= 12 && t.Hour() < 14 {
			return -2
		}
		return 8
	}
	in := Input{Now: now, Battery: battery(0.25), TempC: math.NaN(),
		Slots: slots(now, 36, func(t time.Time) float64 { return summerPV(t, 8000) }, spot)}
	// default: negative slots get the needed energy first (non-zero limits)
	p := Make(in, constLoad{400}, DefaultConfig())
	if p.Mode != ModeDelay {
		t.Fatalf("mode = %s", p.Mode)
	}
	for _, s := range p.Slots {
		if s.Start.Day() == 15 && s.Start.Hour() == 12 && s.ChargeLimitW <= 0 {
			t.Errorf("%s: negative price slot should get charge allocation, got %v", s.Start.Format("15:04"), s.ChargeLimitW)
		}
		if s.Start.Day() == 15 && s.Start.Hour() == 17 && s.ChargeLimitW != 0 {
			t.Errorf("%s: no charging expected after the negative window, got %v", s.Start.Format("15:04"), s.ChargeLimitW)
		}
	}
	// fillOnNegativePrice: unlimited in negative slots
	cfg := DefaultConfig()
	cfg.FillOnNegativePrice = true
	p = Make(in, constLoad{400}, cfg)
	for _, s := range p.Slots {
		h := s.Start.Hour()
		if s.Start.Day() == 15 && h >= 12 && h < 14 && s.ChargeLimitW != Unlimited {
			t.Errorf("%s: negative price slot must be unlimited, got %v", s.Start.Format("15:04"), s.ChargeLimitW)
		}
	}
}

func TestCloudyDayChargesImmediately(t *testing.T) {
	now := time.Date(2026, 6, 15, 8, 0, 0, 0, berlin)
	in := Input{Now: now, Battery: battery(0.2), TempC: math.NaN(),
		Slots: slots(now, 36, func(t time.Time) float64 { return summerPV(t, 1800) }, nil)}
	p := Make(in, constLoad{400}, DefaultConfig())
	if p.Mode != ModeImmediate {
		t.Fatalf("mode = %s, reasons %v", p.Mode, p.Reasons)
	}
	if p.CurrentLimitW() != Unlimited {
		t.Errorf("limit = %v", p.CurrentLimitW())
	}
}

func TestNight(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 0, 0, 0, berlin)
	in := Input{Now: now, Battery: battery(0.6), TempC: math.NaN(),
		Slots: slots(now, 24, func(t time.Time) float64 { return summerPV(t, 8000) }, nil)}
	p := Make(in, constLoad{400}, DefaultConfig())
	if p.Mode != ModeNight || p.CurrentLimitW() != Unlimited {
		t.Fatalf("mode = %s limit %v", p.Mode, p.CurrentLimitW())
	}
}

func TestNoForecast(t *testing.T) {
	p := Make(Input{Now: time.Now()}, nil, DefaultConfig())
	if p.Mode != ModeNoData || p.CurrentLimitW() != Unlimited {
		t.Fatalf("mode = %s", p.Mode)
	}
}

func maxSoCUntil(p Plan, until time.Time) float64 {
	m := 0.0
	for _, s := range p.Slots {
		if !s.Start.Before(until) {
			break
		}
		m = math.Max(m, s.ExpectedSoC)
	}
	return m
}

func TestAutoTargets(t *testing.T) {
	ac := AutoConfig{FloorSunny: 0.05, FloorCloudy: 0.2}
	now := time.Date(2026, 6, 15, 14, 0, 0, 0, berlin)
	day := func(peak float64) Input {
		return Input{Now: now, Battery: battery(0.5), TempC: math.NaN(),
			Slots: slots(now, 40, func(t time.Time) float64 { return summerPV(t, peak) }, nil)}
	}
	// sunny tomorrow: empty down to 5 %, charge only what the night needs:
	// 400 W × ~8.5 h × 1.2 = ~4.1 kWh of 13.8 kWh (÷0.95) ≈ 31 % + 5 % + 5 % -> 45 %
	sun := AutoTargets(day(8000), constLoad{400}, DefaultConfig(), ac)
	if sun.MinSoC != 0.05 || sun.MaxSoC < 0.35 || sun.MaxSoC > 0.5 || sun.DayFill == nil || *sun.DayFill < 1 || sun.Morning == nil || sun.Morning.Day() != 16 {
		t.Fatalf("sunny: %+v", sun)
	}
	// more consumption in the night: higher
	if got := AutoTargets(day(8000), constLoad{800}, DefaultConfig(), ac); got.MaxSoC <= sun.MaxSoC {
		t.Fatalf("night need: %+v vs %+v", got, sun)
	}
	// dull tomorrow: keep 20 %, charge fully
	dull := AutoTargets(day(900), constLoad{400}, DefaultConfig(), ac)
	if dull.MinSoC != 0.2 || dull.MaxSoC != 1 {
		t.Fatalf("dull: %+v", dull)
	}
	// in between: interpolated, charges fully
	mid := AutoTargets(day(2500), constLoad{400}, DefaultConfig(), ac)
	if mid.MinSoC <= 0.05 || mid.MinSoC >= 0.2 || mid.MaxSoC != 1 {
		t.Fatalf("between: %+v", mid)
	}
	// a fixed upper target applies always
	fixed := ac
	fixed.MaxSoC = 0.8
	if got := AutoTargets(day(8000), constLoad{400}, DefaultConfig(), fixed); got.MaxSoC != 0.8 || got.MinSoC != 0.05 {
		t.Fatalf("fixed sunny: %+v", got)
	}
	if got := AutoTargets(day(900), constLoad{400}, DefaultConfig(), fixed); got.MaxSoC != 0.8 {
		t.Fatalf("fixed dull: %+v", got)
	}
	// no forecast
	if got := AutoTargets(Input{Now: now, Battery: battery(0.5)}, constLoad{400}, DefaultConfig(), ac); got.MinSoC != 0.2 || got.MaxSoC != 1 {
		t.Fatalf("no data: %+v", got)
	}
}
