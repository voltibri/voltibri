package fronius

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

const flowJSON = `{"Body":{"Data":{"Site":{"P_Akku":-1500.5,"P_Grid":-2300,"P_Load":-700,"P_PV":4500},
"Inverters":{"1":{"SOC":42.5}}}}}`

func TestSolarAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/solar_api/v1/GetPowerFlowRealtimeData.fcgi":
			w.Write([]byte(flowJSON))
		case "/solar_api/v1/GetMeterRealtimeData.cgi":
			w.Write([]byte(`{"Body":{"Data":{"0":{"Current_AC_Phase_1":-3.5,"Current_AC_Phase_2":1.25,"Current_AC_Phase_3":10,"Meter_Location_Current":0}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dev, err := New(sdk.DeviceConfig{ID: "f", Mode: sdk.ModeObserved, Params: map[string]any{"host": "x", "url": srv.URL, "capacityWh": 13800}})
	if err != nil {
		t.Fatal(err)
	}
	d := dev.(*Device)
	ctx := context.Background()
	if v, _ := d.GridPower(ctx); v != -2300 {
		t.Errorf("grid %v", v)
	}
	if v, _ := d.PVPower(ctx); v != 4500 {
		t.Errorf("pv %v", v)
	}
	if v, _ := d.SoC(ctx); v != 0.425 {
		t.Errorf("soc %v", v)
	}
	if v, _ := d.BatteryPower(ctx); v != -1500.5 {
		t.Errorf("battery %v", v)
	}
	if ph, err := d.Currents(ctx); err != nil || ph != (sdk.Phases{3.5, 1.25, 10}) {
		t.Errorf("currents %v %v", ph, err)
	}
	if err := d.SetChargeLimit(ctx, 1000); err == nil {
		t.Error("observed device must not accept control")
	}
}

// fakeRegs emulates a SunSpec register map: header, model 1 (66 regs),
// model 103 (50 regs), model 124 (24 regs), end marker.
type fakeRegs struct {
	m map[uint16]uint16
}

func newFake() *fakeRegs {
	f := &fakeRegs{m: map[uint16]uint16{}}
	a := uint16(40000)
	f.m[a], f.m[a+1] = 0x5375, 0x6e53
	a += 2
	for _, mdl := range [][2]uint16{{1, 66}, {103, 50}, {124, 24}} {
		f.m[a], f.m[a+1] = mdl[0], mdl[1]
		if mdl[0] == 124 {
			base := a + 2
			f.m[base+offWChaMax] = 5000
			f.m[base+offWChaMaxSF] = 0
			f.m[base+offInOutWRteSF] = uint16(0xFFFE) // -2
		}
		a += 2 + mdl[1]
	}
	f.m[a] = 0xFFFF
	return f
}

func (f *fakeRegs) read(addr, qty uint16) ([]uint16, error) {
	out := make([]uint16, qty)
	for i := range out {
		out[i] = f.m[addr+uint16(i)]
	}
	return out, nil
}

func (f *fakeRegs) write(addr uint16, v []uint16) error {
	for i, x := range v {
		f.m[addr+uint16(i)] = x
	}
	return nil
}

func TestSunspecChargeLimit(t *testing.T) {
	f := newFake()
	s := &sunspec{regs: f}
	if err := s.setChargeLimit(1250, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	base := uint16(40000 + 2 + 2 + 66 + 2 + 50 + 2)
	if s.base != base {
		t.Fatalf("model 124 at %d, want %d", s.base, base)
	}
	// 1250 of 5000 W = 25 % -> raw 2500 with SF -2
	if got := f.m[base+offInWRte]; got != 2500 {
		t.Errorf("InWRte = %d, want 2500", got)
	}
	if got := f.m[base+offRvrtTms]; got != 300 {
		t.Errorf("revert = %d, want 300", got)
	}
	if got := f.m[base+offStorCtlMod]; got&storCtlCharge == 0 {
		t.Errorf("StorCtl_Mod = %d, charge bit missing", got)
	}
	if err := s.reset(); err != nil {
		t.Fatal(err)
	}
	if got := f.m[base+offStorCtlMod]; got != 0 {
		t.Errorf("after reset StorCtl_Mod = %d", got)
	}
}

func TestSunspecLimits(t *testing.T) {
	f := newFake()
	s := &sunspec{regs: f}
	// only a discharge limit: 800 of 5000 W = 16 % -> raw 1600, charging free
	if err := s.setLimits(-1, 800, time.Minute); err != nil {
		t.Fatal(err)
	}
	base := s.base
	if f.m[base+offOutWRte] != 1600 || f.m[base+offStorCtlMod] != storCtlDischarge || f.m[base+offRvrtTms] != 60 {
		t.Fatalf("discharge only: OutWRte %d mode %d revert %d", f.m[base+offOutWRte], f.m[base+offStorCtlMod], f.m[base+offRvrtTms])
	}
	// both
	if err := s.setLimits(0, 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if f.m[base+offStorCtlMod] != storCtlCharge|storCtlDischarge || f.m[base+offInWRte] != 0 || f.m[base+offOutWRte] != 0 {
		t.Fatalf("both: mode %d", f.m[base+offStorCtlMod])
	}
	// none: autonomous
	if err := s.setLimits(-1, -1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if f.m[base+offStorCtlMod] != 0 {
		t.Fatalf("none: mode %d", f.m[base+offStorCtlMod])
	}
}

func TestSunspecForceCharge(t *testing.T) {
	f := newFake()
	s := &sunspec{regs: f}
	// 2000 of 5000 W = 40 % -> OutWRte raw -4000 with SF -2
	if err := s.forceCharge(2000, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	base := s.base
	if got := int16(f.m[base+offOutWRte]); got != -4000 {
		t.Errorf("OutWRte = %d, want -4000", got)
	}
	if got := f.m[base+offInWRte]; got != 10000 {
		t.Errorf("InWRte = %d, want 10000", got)
	}
	if f.m[base+offChaGriSet] != chaGriSetGrid || f.m[base+offStorCtlMod] != storCtlCharge|storCtlDischarge || f.m[base+offRvrtTms] != 120 {
		t.Errorf("ChaGriSet %d mode %d revert %d", f.m[base+offChaGriSet], f.m[base+offStorCtlMod], f.m[base+offRvrtTms])
	}
}
