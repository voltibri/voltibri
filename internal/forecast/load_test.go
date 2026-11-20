package forecast

import (
	"math"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/series"
)

var berlin, _ = time.LoadLocation("Europe/Berlin")

// build creates n days of history ending the day before end. load(t) gives
// the load in W, temp(t) the temperature.
func build(t *testing.T, end time.Time, n int, load func(time.Time) float64, temp func(time.Time) float64) *LoadModel {
	t.Helper()
	cal, _ := calendar.New()
	m := NewLoadModel(DefaultConfig(), cal, berlin)
	start := end.AddDate(0, 0, -n)
	for ts := start; ts.Before(end); ts = ts.Add(series.Slot) {
		r := series.Record{Time: ts, LoadW: load(ts), PVW: 0, SpotCt: math.NaN(), TempC: math.NaN()}
		if temp != nil {
			r.TempC = temp(ts)
		}
		m.Add(r)
	}
	return m
}

func TestDayTypeProfiles(t *testing.T) {
	end := time.Date(2026, 6, 17, 0, 0, 0, 0, berlin) // Wednesday
	m := build(t, end, 42, func(ts time.Time) float64 {
		if ts.Weekday() == time.Saturday || ts.Weekday() == time.Sunday {
			return 800
		}
		if ts.Hour() >= 18 && ts.Hour() < 22 {
			return 1500 // evening peak on workdays
		}
		return 400
	}, nil)
	if !m.Ready() {
		t.Fatal("model should be ready")
	}
	wed := end.Add(19 * time.Hour)
	if e := m.Slot(wed, end, math.NaN()); math.Abs(e.P50-1500) > 1 || e.N == 0 {
		t.Errorf("workday evening: got %+v, want 1500", e)
	}
	sat := time.Date(2026, 6, 20, 19, 0, 0, 0, berlin)
	if e := m.Slot(sat, end, math.NaN()); math.Abs(e.P50-800) > 1 {
		t.Errorf("saturday: got %+v, want 800", e)
	}
	// night Wed 18:00 -> Thu 07:00: 4h*1500 + 9h*400 = 9600 Wh
	e := m.Energy(end.Add(18*time.Hour), end.Add(31*time.Hour), end, math.NaN())
	if math.Abs(e.P50-9600) > 1 {
		t.Errorf("night energy: got %+v, want 9600", e)
	}
}

func TestQuantileReflectsSpread(t *testing.T) {
	end := time.Date(2026, 6, 17, 0, 0, 0, 0, berlin)
	m := build(t, end, 42, func(ts time.Time) float64 {
		if ts.Day()%2 == 0 {
			return 1000
		}
		return 500
	}, nil)
	e := m.Energy(end.Add(20*time.Hour), end.Add(24*time.Hour), end, math.NaN())
	if !(e.PQ > e.P50) || e.PQ > 4000+1 || e.P50 < 2000-1 {
		t.Errorf("expected P50 < PQ within [2000,4000], got %+v", e)
	}
}

func TestTemperatureCorrection(t *testing.T) {
	end := time.Date(2026, 1, 20, 0, 0, 0, 0, berlin)
	// daily temperature varies between -5 and 15 °C; heat pump draws
	// 100 W per degree below 15 °C
	tempOf := func(ts time.Time) float64 { return 5 + 10*math.Sin(float64(ts.YearDay())) }
	m := build(t, end, 60, func(ts time.Time) float64 {
		return 300 + 100*math.Max(0, 15-tempOf(ts))
	}, tempOf)
	coef := m.TempCoefWh()
	if math.Abs(coef-2400) > 50 { // 100 W * 24 h per degree day
		t.Fatalf("temperature coefficient: got %.0f Wh/K, want ~2400", coef)
	}
	// a very cold day must be forecast higher than a mild one
	cold := m.Energy(end, end.Add(24*time.Hour), end, -10)
	mild := m.Energy(end, end.Add(24*time.Hour), end, 14)
	if math.Abs(cold.P50-(300+2500)*24) > 0.05*cold.P50 {
		t.Errorf("cold day: got %.0f Wh, want ~%.0f", cold.P50, float64((300+2500)*24))
	}
	if !(cold.P50 > mild.P50*3) {
		t.Errorf("cold %.0f should be far above mild %.0f", cold.P50, mild.P50)
	}
}

func TestWeightedQuantile(t *testing.T) {
	v := []float64{1, 2, 3, 4}
	w := []float64{1, 1, 1, 1}
	if q := weightedQuantile(v, w, 0.5); q != 2.5 {
		t.Errorf("median = %v, want 2.5", q)
	}
	if q := weightedQuantile(v, w, 1); q != 4 {
		t.Errorf("max = %v", q)
	}
}
