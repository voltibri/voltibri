package drivers_test

import (
	"context"
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
		"planes": []any{map[string]any{"dec": 30, "az": 0, "kwp": 5}, map[string]any{"dec": 30, "az": 90, "kwp": 5}},
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
	if len(calls) != 2 {
		t.Errorf("expected one request per plane, got %v", calls)
	}
}
