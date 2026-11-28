// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 10 kWh PV, 2 kWh battery, 4 kWh grid at 7.35 ct feed-in and 31.26 ct grid
func shares() Shares { return Uniform(10000, 2000, 4000, 7.35, 31.26) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestDefault(t *testing.T) {
	var r *Rule
	c, note := r.Cost(shares(), "Dräger e-Up")
	want := 12*7.35 + 4*31.26
	if !near(c, want) {
		t.Fatalf("cost %.4f, want %.4f", c, want)
	}
	if strings.Contains(note, "Dräger") {
		t.Fatalf("default must not name the vehicle: %q", note)
	}
	if !r.Empty() || !(&Rule{PV: &Share{Mode: Inherit}}).Empty() {
		t.Fatal("nil and inherit-only rules are empty")
	}
}

func TestReplaceAndAdjust(t *testing.T) {
	r := &Rule{
		PV:      &Share{Mode: Replace, Base: BaseFixed, Ct: 15},
		Battery: &Share{Mode: Replace, Base: BaseFixed, Ct: 15, Percent: 10},
		Grid:    &Share{Mode: Adjust, Percent: 10},
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	c, note := r.Cost(shares(), "Dräger e-Up")
	want := 10*15.0 + 2*15*1.1 + 4*31.26*1.1
	if !near(c, want) {
		t.Fatalf("cost %.4f, want %.4f", c, want)
	}
	for _, s := range []string{"Dräger e-Up: ", "PV 15 ct fest", "Akku 15 ct fest +10 %", "Netz Tarif +10 %"} {
		if !strings.Contains(note, s) {
			t.Errorf("note %q lacks %q", note, s)
		}
	}
}

func TestAllAndSpecific(t *testing.T) {
	// one price for everything, but the grid share inherits the tariff
	r := &Rule{All: &Share{Mode: Replace, Base: BaseFixed, Ct: 30}, Grid: &Share{Mode: Inherit}}
	c, _ := r.Cost(shares(), "")
	// an explicit inherit on grid wins over All: grid at the tariff
	if want := 12*30.0 + 4*31.26; !near(c, want) {
		t.Fatalf("cost %.4f, want %.4f", c, want)
	}
	r = &Rule{All: &Share{Mode: Replace, Base: BaseFixed, Ct: 30}, Grid: &Share{Mode: Replace, Base: BaseGrid}}
	c, _ = r.Cost(shares(), "")
	if want := 12*30.0 + 4*31.26; !near(c, want) {
		t.Fatalf("cost %.4f, want %.4f", c, want)
	}
}

func TestBases(t *testing.T) {
	s := shares()
	for _, tc := range []struct {
		share Share
		want  float64 // cost of the PV share (10 kWh)
	}{
		{Share{Mode: Replace, Base: BaseFree}, 0},
		{Share{Mode: Replace, Base: BaseGrid}, 10 * 31.26},
		{Share{Mode: Replace, Base: BaseFeedIn, AddCt: 2}, 10 * (7.35 + 2)},
		{Share{Mode: Adjust, AddCt: -1}, 10 * 6.35},
	} {
		sh := tc.share
		r := &Rule{PV: &sh, Battery: &Share{Mode: Replace, Base: BaseFree}, Grid: &Share{Mode: Replace, Base: BaseFree}}
		if err := r.Validate(); err != nil {
			t.Fatalf("%+v: %v", tc.share, err)
		}
		if c, _ := r.Cost(s, ""); !near(c, tc.want) {
			t.Errorf("%+v: cost %.4f, want %.4f", tc.share, c, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []Share{
		{Mode: "half"},
		{Mode: Replace},
		{Mode: Replace, Base: "spot"},
		{Mode: Replace, Base: BaseFixed, Ct: -1},
		{Mode: Replace, Base: BaseGrid, Ct: 5},
		{Mode: Adjust},
		{Mode: Adjust, Base: BaseFixed, Ct: 3, Percent: 5},
		{Mode: Inherit, Percent: 5},
		{Mode: Adjust, Percent: -200},
	} {
		sh := s
		if err := (&Rule{PV: &sh}).Validate(); err == nil {
			t.Errorf("%+v: expected an error", s)
		}
	}
}

func TestFromAttrs(t *testing.T) {
	s := shares()
	b, _ := json.Marshal(map[string]any{AttrKey: s, "odometerKm": 1200})
	var attrs map[string]any
	_ = json.Unmarshal(b, &attrs)
	got, ok := FromAttrs(attrs)
	if !ok || !near(got.Grid.GridCt, s.Grid.GridCt) || !near(got.PV.Wh, 10000) {
		t.Fatalf("decoded %+v (%v)", got, ok)
	}
	if _, ok := FromAttrs(map[string]any{}); ok {
		t.Fatal("no shares expected")
	}
}

func TestUnit(t *testing.T) {
	r := &Rule{PV: &Share{Mode: Replace, Base: BaseFixed, Ct: 15.5}}
	if u := r.Unit(7.35, 31.26); u != (Unit{GridCt: 31.26, BatteryCt: 7.35, PVCt: 15.5}) {
		t.Fatalf("unit %+v", u)
	}
	var none *Rule
	if u := none.Unit(8, 30); u != (Unit{GridCt: 30, BatteryCt: 8, PVCt: 8}) {
		t.Fatalf("default %+v", u)
	}
}

// PV taken while the home battery had priority costs the grid price (by
// the grid rule), whatever the PV rule says.
func TestPVPriority(t *testing.T) {
	var s Shares
	s.PV.Add(1000, 7.35, 31.26)
	s.PVPriority.Add(2000, 7.35, 31.26)
	s.Grid.Add(1000, 7.35, 31.26)
	r := &Rule{PV: &Share{Mode: Replace, Base: BaseFixed, Ct: 15}}
	c, note := r.Cost(s, "e-Up")
	if math.Abs(c-(15+2*31.26+31.26)) > 1e-9 || !strings.Contains(note, "PV bei Akku-Vorrang Tarif") {
		t.Fatalf("cost %v %q", c, note)
	}
	if c, _ := (*Rule)(nil).Cost(s, ""); math.Abs(c-(7.35+3*31.26)) > 1e-9 {
		t.Fatalf("default %v", c)
	}
}
