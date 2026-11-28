// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package soc

import (
	"math"
	"testing"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

func TestEstimate(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	e := New(0.4, t0, SourceManual, math.NaN())
	e.Charge(10000/0.85, 0.85, 50000) // 10 kWh into a 50 kWh battery
	if v := e.Value(50000); math.Abs(v-0.6) > 1e-9 || !e.Interpolated() {
		t.Fatalf("value %v", v)
	}
	if v := e.Value(0); v != 0.4 {
		t.Fatalf("without capacity: %v", v)
	}
	e.Charge(1e6, 1, 50000)
	if v := e.Value(50000); v != 1 {
		t.Fatalf("clamped: %v", v)
	}
	e.Discharge(0.95*5000, 0.95, 50000) // full stays full: 10 % less
	if v := e.Value(50000); math.Abs(v-0.9) > 1e-9 {
		t.Fatalf("after full: %v", v)
	}
	e.Discharge(1e6, 0.95, 50000)
	if v := e.Value(50000); v != 0 {
		t.Fatalf("clamped low: %v", v)
	}
	if !e.APIChanged(0.3) || e.APIChanged(math.NaN()) {
		t.Fatal("no API value seen: any API value is new")
	}
	e.Rebase(0.3, t0.Add(time.Hour), SourceAPI, 0.3)
	if e.APIChanged(0.302) || !e.APIChanged(0.32) || e.Value(50000) != 0.3 {
		t.Fatalf("after rebase: %+v", e)
	}

	c := sdk.NewMemCache()
	Save(c, "soc:x", e)
	if l := Load(c, "soc:x"); l == nil || l.Base != 0.3 || l.Source != SourceAPI || l.APIRef == nil || *l.APIRef != 0.3 {
		t.Fatalf("load: %+v", l)
	}
	Save(c, "soc:x", nil)
	if Load(c, "soc:x") != nil {
		t.Fatal("removed")
	}
	if Validate(1.1) == nil || Validate(-0.1) == nil || Validate(0.5) != nil {
		t.Fatal("validate")
	}
}
