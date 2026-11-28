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
	a, by := tr.Headroom("carport", Phases{16, 16, 16}, 3)
	if a != 22 || by != "Wallboxen" {
		t.Fatalf("headroom %v %q", a, by)
	}
	// carport goes up to 22 A -> zaun may keep only 10 A
	tr.Commit("carport", Phases{16, 16, 16}, Phases{22, 22, 22})
	if a, _ := tr.Headroom("zaun", Phases{10, 10, 10}, 3); a != 10 {
		t.Fatalf("after commit %v", a)
	}
	// device without circuit is limited by the house connection
	if a, by := tr.Headroom("other", Phases{}, 3); a != 63-42 || by != "Hausanschluss" {
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
	if a, _ := tr.Headroom("nobody", Phases{}, 3); !math.IsInf(a, 1) {
		t.Errorf("root without fuse limits: %v", a)
	}
}

// Unbalanced load (Schieflast): a 2-phase car at a house that already
// draws more on L1 may add only up to 20 A above the lowest other phase.
func TestImbalance(t *testing.T) {
	nodes := []Node{{ID: Root, Name: "Hausanschluss", Phases: 3, Meter: true, ImbalanceA: 20, Devices: []string{"wb"}}}
	// the car draws 16 A on L1 and L2, the house 8 A on L1 and 2 A on L2 and L3
	r := map[string]Reading{"wb": {Currents: Phases{16, 16, 0}, HasCurrents: true}}
	tr := Compute(nodes, r, map[string]Phases{Root: {24, 18, 2}})
	g := tr.Statuses()[0]
	if g.MaxImbalanceA != 20 || g.ImbalanceA != 22 || !g.ImbalanceOver {
		t.Fatalf("root %+v", g)
	}
	// others: 8 / 2 / 2 -> the car may draw 20 + 2 - 8 = 14 A on L1 and L2
	if a, by := tr.Headroom("wb", Phases{16, 16, 0}, 2); a != 14 || by != "Schieflast am Hausanschluss" {
		t.Fatalf("2 phases: %v %q", a, by)
	}
	// on all three phases the imbalance does not change
	if a, _ := tr.Headroom("wb", Phases{16, 16, 16}, 3); !math.IsInf(a, 1) {
		t.Fatalf("3 phases: %v", a)
	}
	// not measured per phase: L1 (and L2) assumed
	if a, _ := tr.Headroom("wb", Phases{}, 1); a != 0 { // others 24 / 18 / 2: L1 is 22 A above L3 already
		t.Fatalf("1 phase unmeasured: %v", a)
	}
	// without a grid meter the house is not known: not watched
	tr = Compute(nodes, r, nil)
	if a, _ := tr.Headroom("wb", Phases{16, 16, 0}, 2); !math.IsInf(a, 1) || tr.Statuses()[0].MaxImbalanceA != 0 {
		t.Fatalf("without meter: %v", a)
	}
}
