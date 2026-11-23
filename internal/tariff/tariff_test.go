// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package tariff

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

func TestFixedZones(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	tf, err := New(&Config{Type: "fixed", Price: 30, Zones: []Zone{
		{Days: "Mo-Fr", Hours: "22-6", Price: 20},
		{Days: "Sa,So", Price: 25},
	}}, loc, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   string
		want float64
	}{
		{"2026-11-20 12:00", 30}, // Friday noon
		{"2026-11-20 23:00", 20}, // Friday night
		{"2026-11-21 03:00", 20}, // early Saturday, still Friday's night zone
		{"2026-11-21 12:00", 25}, // weekend
		{"2026-11-24 05:45", 20}, // Tuesday early
		{"2026-11-24 06:00", 30},
	}
	for _, c := range cases {
		at, _ := time.ParseInLocation("2006-01-02 15:04", c.at, loc)
		p, _ := tf.Prices(context.Background(), at, at.Add(Slot))
		if len(p) != 1 || p[0] != c.want {
			t.Errorf("%s: got %v, want %v", c.at, p, c.want)
		}
	}
}

type src []sdk.Slot

func (s src) ID() string { return "spot" }
func (s src) Prices(context.Context, time.Time, time.Time) ([]sdk.Slot, error) {
	return s, nil
}

func TestDynamic(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	devs := map[string]sdk.Device{"spot": src{{Start: start, Duration: time.Hour, Value: 10}}}
	tf, err := New(&Config{Type: "dynamic", Source: "spot", Charges: 15, Tax: 0.19}, time.UTC, devs)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tf.Prices(context.Background(), start, start.Add(90*time.Minute))
	if len(p) != 6 || math.Abs(p[0]-29.75) > 1e-9 || math.Abs(p[3]-29.75) > 1e-9 || !math.IsNaN(p[4]) {
		t.Fatalf("got %v", p)
	}
}

func TestValidate(t *testing.T) {
	if err := (&Config{Type: "fixed", Zones: []Zone{{Days: "Xy"}}}).Validate(nil); err == nil {
		t.Error("bad weekday accepted")
	}
	if err := (&Config{Type: "dynamic", Source: "x"}).Validate(map[string]bool{"spot": true}); err == nil {
		t.Error("unknown source accepted")
	}
}
