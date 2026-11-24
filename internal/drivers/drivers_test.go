package drivers_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/voltibri/voltibri/internal/drivers/all"
	"github.com/voltibri/voltibri/sdk"
)

func server(t *testing.T, routes map[string]string, calls *[]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.URL.RequestURI())
		for prefix, body := range routes {
			if strings.HasPrefix(r.URL.RequestURI(), prefix) {
				w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDev(t *testing.T, typ string, mode sdk.ControlMode, params map[string]any) sdk.Device {
	d, err := sdk.New(sdk.DeviceConfig{ID: typ, Type: typ, Mode: mode, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGoE(t *testing.T) {
	var calls []string
	srv := server(t, map[string]string{
		"/api/status": `{"car":2,"amp":10,"pnp":3,"nrg":[230,230,230,0,10,10,10,0,2300,2300,2300,0,6900]}`,
		"/api/set":    `{"amp":true}`,
	}, &calls)
	d := newDev(t, "go-e", sdk.ModeOwned, map[string]any{"url": srv.URL})
	st, err := d.(sdk.Charger).ChargerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Charging || !st.Connected || st.Phases != 3 || st.CurrentA != 10 {
		t.Errorf("unexpected status %+v", st)
	}
	if err := d.(sdk.ChargerController).SetCurrent(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "/api/set?amp=16" {
		t.Errorf("unexpected call %s", calls[len(calls)-1])
	}
	ph, err := d.(sdk.PhaseCurrents).Currents(context.Background())
	if err != nil || ph != (sdk.Phases{10, 10, 10}) {
		t.Errorf("currents %v %v", ph, err)
	}
	if err := d.(sdk.PhaseSwitcher).SetPhases(context.Background(), 1); err != nil || calls[len(calls)-1] != "/api/set?psm=1" {
		t.Errorf("phase switch: %v %s", err, calls[len(calls)-1])
	}
}

// The forecast survives a restart (shared persistent cache) and bridges API
// errors such as the rate limit with the cached answer.
func TestForecastSolarCache(t *testing.T) {
	limited := false
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if limited {
			http.Error(w, `{"message":"Rate limit"}`, http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"result":{"watts":{"2026-06-15 12:00:00":3000,"2026-06-15 13:00:00":3000}}}`))
	}))
	defer srv.Close()
	cache := sdk.NewMemCache()
	mk := func(refresh string) sdk.PVForecaster {
		d, err := sdk.New(sdk.DeviceConfig{ID: "pv", Type: "forecast.solar", Cache: cache, Params: map[string]any{
			"url": srv.URL, "lat": 48.6, "lon": 9.0, "refresh": refresh,
			"planes": []any{map[string]any{"kwp": 5, "az": -118}, map[string]any{"kwp": 5, "az": 62}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return d.(sdk.PVForecaster)
	}
	loc, _ := time.LoadLocation("Europe/Berlin")
	from := time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	get := func(f sdk.PVForecaster) float64 {
		s, err := f.PVForecast(context.Background(), from, from.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s[0].Value
	}

	if v := get(mk("1h")); v != 6000 || calls != 2 {
		t.Fatalf("first start: %v W, %d calls", v, calls)
	}
	// "restart": new device, same cache -> no requests
	if v := get(mk("1h")); v != 6000 || calls != 2 {
		t.Fatalf("after restart: %v W, %d calls (expected none)", v, calls)
	}
	// cache expired and API rate limited -> stale cached forecast is used
	limited = true
	time.Sleep(5 * time.Millisecond)
	if v := get(mk("1ms")); v != 6000 || calls != 4 {
		t.Fatalf("rate limited: %v W, %d calls", v, calls)
	}
}

// Every built-in driver must describe itself for the configuration UI.
func TestDriversHaveSchemas(t *testing.T) {
	for _, d := range sdk.Drivers() {
		if d.Name == d.Type && d.Description == "" || d.Category == "" || (len(d.Capabilities) == 0 && d.Category != sdk.CatNone) || d.Params == nil {
			t.Errorf("driver %s lacks metadata: %+v", d.Type, d)
		}
		for _, p := range d.Params {
			if p.Label == "" {
				t.Errorf("driver %s: parameter %s has no label", d.Type, p.Name)
			}
		}
	}
}

func TestShellyObservedRefusesWrites(t *testing.T) {
	var calls []string
	srv := server(t, map[string]string{"/rpc/Switch.GetStatus": `{"output":true,"apower":812.5}`}, &calls)
	d := newDev(t, "shelly", sdk.ModeObserved, map[string]any{"url": srv.URL})
	on, err := d.(sdk.Switch).SwitchState(context.Background())
	if err != nil || !on {
		t.Fatalf("state %v %v", on, err)
	}
	if p, _ := d.(sdk.PowerConsumer).ConsumerPower(context.Background()); p != 812.5 {
		t.Errorf("power %v", p)
	}
	if err := d.(sdk.Switch).SetSwitch(context.Background(), false); err == nil {
		t.Error("observed shelly must refuse to switch")
	}
}

func TestShellyPM(t *testing.T) {
	var calls []string
	// response captured from a Shelly PM Mini Gen3
	srv := server(t, map[string]string{"/rpc/PM1.GetStatus?id=0": `{"id":0, "voltage":237.9, "current":3.341, "apower":617.5 ,"freq":50.0,"aenergy":{"total":482705.879,"by_minute":[1,2,3],"minute_ts":1795169880},"ret_aenergy":{"total":0.000}}`}, &calls)
	d := newDev(t, "shelly-pm", sdk.ModeObserved, map[string]any{"url": srv.URL})
	if p, err := d.(sdk.PowerConsumer).ConsumerPower(context.Background()); err != nil || p != 617.5 {
		t.Errorf("power %v %v", p, err)
	}
	if ph, _ := d.(sdk.PhaseCurrents).Currents(context.Background()); ph != (sdk.Phases{3.341, 0, 0}) {
		t.Errorf("currents %v", ph)
	}
	if e, _ := d.(sdk.EnergyMeter).EnergyWh(context.Background()); e != 482705.879 {
		t.Errorf("energy %v", e)
	}
	if _, ok := d.(sdk.Switch); ok {
		t.Error("a meter without relay must not offer switching")
	}
}

func TestOpenDTU(t *testing.T) {
	var calls []string
	srv := server(t, map[string]string{"/api/livedata/status": `{"total":{"Power":{"v":1234.5,"u":"W","d":1}}}`}, &calls)
	d := newDev(t, "opendtu", sdk.ModeObserved, map[string]any{"url": srv.URL})
	if p, err := d.(sdk.PVProducer).PVPower(context.Background()); err != nil || p != 1234.5 {
		t.Errorf("pv %v %v", p, err)
	}
}

func TestForecastSolar(t *testing.T) {
	var calls []string
	srv := server(t, map[string]string{"/estimate/": `{"result":{"watts":{
		"2026-06-15 05:00:00":0,"2026-06-15 12:00:00":6000,"2026-06-15 13:00:00":6000,"2026-06-15 21:00:00":0}}}`}, &calls)
	d := newDev(t, "forecast.solar", sdk.ModeObserved, map[string]any{
		"url": srv.URL, "lat": 51.0, "lon": 10.0,
		"planes": []any{map[string]any{"dec": 30, "az": 0, "kwp": 5}, map[string]any{"dec": 30, "az": 90, "kwp": 5, "horizon": "0,0,10, 20"}},
	})
	loc, _ := time.LoadLocation("Europe/Berlin")
	from := time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	slots, err := d.(sdk.PVForecaster).PVForecast(context.Background(), from, from.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 4 || slots[0].Value != 12000 { // two planes summed
		t.Errorf("unexpected slots %+v", slots)
	}
	if len(calls) != 2 || strings.Contains(calls[0], "horizon") || !strings.HasSuffix(calls[1], "?horizon=0%2C0%2C10%2C20") {
		t.Errorf("expected one request per plane with horizon on the second, got %v", calls)
	}
	// cached: no new requests within the refresh interval
	d.(sdk.PVForecaster).PVForecast(context.Background(), from, from.Add(time.Hour))
	if len(calls) != 2 {
		t.Errorf("forecast not cached: %v", calls)
	}
}

func TestManualVehicle(t *testing.T) {
	dev, err := sdk.New(sdk.DeviceConfig{ID: "e-up", Name: "Dräger e-Up", Type: "vehicle", Params: map[string]any{"capacityKWh": 32.3, "phases": 2.0}})
	if err != nil {
		t.Fatal(err)
	}
	vp := dev.(sdk.VehicleProvider)
	vs, _ := vp.Vehicles(context.Background())
	if len(vs) != 1 || vs[0].Name != "Dräger e-Up" || vs[0].CapacityWh != 32300 || vs[0].MaxPowerW != 7360 {
		t.Fatalf("vehicle %+v", vs)
	}
	st, _ := vp.VehicleStatus(context.Background(), "e-up")
	if !math.IsNaN(st.SoC) || st.Connected {
		t.Fatalf("status %+v", st)
	}
}
