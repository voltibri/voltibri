package meter

import (
	"math"
	"testing"

	"github.com/voltibri/voltibri/sdk"
)

func TestPower(t *testing.T) {
	goe := Correction{OffsetW: 200, ActiveAboveW: 50}
	cases := []struct {
		c        Correction
		raw, out float64
	}{
		{goe, 0, 0},         // idle stays 0
		{goe, 30, 30},       // below threshold
		{goe, 11000, 11200}, // charging
		{Correction{Factor: 1.03}, 1000, 1030},
		{Correction{Factor: 0.98, OffsetW: 10}, -2000, -1970}, // export: offset follows sign
		{Correction{}, 1234, 1234},
	}
	for _, tc := range cases {
		if got := tc.c.Power(tc.raw); math.Abs(got-tc.out) > 1e-9 {
			t.Errorf("%+v Power(%v) = %v, want %v", tc.c, tc.raw, got, tc.out)
		}
	}
}

func TestCurrents(t *testing.T) {
	c := Correction{OffsetW: 207, ActiveAboveW: 50}
	// three phase charging: 207 W / 230 V / 3 = 0.3 A per phase
	got := c.Currents(sdk.Phases{16, 16, 16}, 11000)
	for _, v := range got {
		if math.Abs(v-16.3) > 1e-9 {
			t.Fatalf("got %v", got)
		}
	}
	// single phase: everything on L1
	got = c.Currents(sdk.Phases{10, 0, 0}, 2300)
	if math.Abs(got[0]-10.9) > 1e-9 || got[1] != 0 {
		t.Errorf("got %v", got)
	}
	// idle: unchanged
	if got := c.Currents(sdk.Phases{0, 0, 0}, 0); got != (sdk.Phases{}) {
		t.Errorf("got %v", got)
	}
}

func TestIsZero(t *testing.T) {
	if !(Correction{}).IsZero() || !(Correction{Factor: 1}).IsZero() || (Correction{OffsetW: 1}).IsZero() {
		t.Error("IsZero wrong")
	}
}
