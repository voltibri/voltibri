package importer

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestConvertHA(t *testing.T) {
	in := `entity_id,state,last_changed
sensor.pv,1000,2026-06-15T10:00:00.000Z
sensor.grid,-500,2026-06-15T10:00:00.000Z
sensor.pv,3000,2026-06-15T10:22:30.000Z
sensor.grid,unavailable,2026-06-15T10:20:00.000Z
sensor.pv,0,2026-06-15T11:00:00.000Z
sensor.grid,200,2026-06-15T11:00:00.000Z
`
	s, err := ConvertHA(strings.NewReader(in), Mapping{PV: []string{"sensor.pv"}, Grid: "sensor.grid"}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// slots from 10:15 to 11:00 (first full slot after the first sample)
	if len(s) != 3 {
		t.Fatalf("got %d slots: %+v", len(s), s)
	}
	// 10:15-10:30: 7.5 min at 1000 W, 7.5 min at 3000 W -> 2000 W
	if math.Abs(s[0].PVW-2000) > 0.01 {
		t.Errorf("pv avg = %v, want 2000", s[0].PVW)
	}
	// load = grid + pv = -500 + 2000
	if math.Abs(s[0].LoadW-1500) > 0.01 {
		t.Errorf("load = %v, want 1500", s[0].LoadW)
	}
}
