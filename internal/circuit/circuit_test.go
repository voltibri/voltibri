package circuit

import (
	"math"
	"testing"
)

func TestComputeAndHeadroom(t *testing.T) {
	nodes := []Node{
		{ID: Root, Name: "Hausanschluss", MaxA: 63, Phases: 3},
		{ID: "wb", Name: "Wallboxen", MaxA: 32, Phases: 3, Devices: []string{"carport", "zaun"}},
		{ID: "wp", Name: "Wärmepumpe", Phases: 1, Devices: []string{"heat"}},
	}
	r := map[string]Reading{
		"carport": {Currents: Phases{16, 16, 16}, HasCurrents: true, PowerW: 11000},
		"zaun":    {Currents: Phases{10, 10, 10}, HasCurrents: true, PowerW: 6900},
		"heat":    {PowerW: 2300, Phases: 1}, // 10 A estimated on L1
	}
	tr := Compute(nodes, r, nil)
	st := map[string]Status{}
	for _, s := range tr.Statuses() {
		st[s.ID] = s
	}
	if wb := st["wb"]; wb.CurrentsA != (Phases{26, 26, 26}) || wb.Over || math.Abs(wb.Utilization-26.0/32) > 1e-9 {
		t.Fatalf("wallbox circuit %+v", wb)
	}
	if g := st[Root]; g.CurrentsA != (Phases{36, 26, 26}) || !g.Estimated || g.PowerW != 20200 {
		t.Fatalf("root %+v", g)
	}
	if tr.Statuses()[0].ID != Root {
		t.Error("root not first")
	}

	// carport may use what zaun leaves: 32 - 10 = 22 A
	a, by := tr.Headroom("carport", Phases{16, 16, 16})
	if a != 22 || by != "Wallboxen" {
		t.Fatalf("headroom %v %q", a, by)
	}
	// carport goes up to 22 A -> zaun may keep only 10 A
	tr.Commit("carport", Phases{16, 16, 16}, Phases{22, 22, 22})
	if a, _ := tr.Headroom("zaun", Phases{10, 10, 10}); a != 10 {
		t.Fatalf("after commit %v", a)
	}
	// device without circuit is limited by the house connection
	if a, by := tr.Headroom("other", Phases{}); a != 63-42 || by != "Hausanschluss" {
		t.Fatalf("root headroom %v %q", a, by)
	}
}

func TestMeterWins(t *testing.T) {
	tr := Compute([]Node{{ID: "wb", MaxA: 32, Devices: []string{"a"}}}, map[string]Reading{"a": {PowerW: 5000}},
		map[string]Phases{"wb": {20, 21, 22}, Root: {30, 30, 30}})
	s := tr.Statuses()
	if s[0].CurrentsA != (Phases{30, 30, 30}) || s[1].CurrentsA != (Phases{20, 21, 22}) || s[1].Estimated {
		t.Fatalf("%+v", s)
	}
	if a, _ := tr.Headroom("nobody", Phases{}); !math.IsInf(a, 1) {
		t.Errorf("root without fuse limits: %v", a)
	}
}
