// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/store"
)

func TestDeviceHistory(t *testing.T) {
	loc := time.UTC
	mid := time.Date(2026, 11, 26, 0, 0, 0, 0, loc)
	day := func(n int) time.Time { return mid.AddDate(0, 0, n) }
	sums := []store.ProfileSum{
		{Asset: "wp", Start: day(0), ImportWh: 4000, CostCt: 100},
		{Asset: "wp", Start: day(-1), ImportWh: 10000, CostCt: 300},
		{Asset: "wp", Start: day(-2), ImportWh: 6000, CostCt: 150},
		{Asset: "wp", Start: day(-3), ImportWh: 9000, CostCt: 200}, // recording started during this day
		{Asset: "inv.grid", Start: day(-1), ImportWh: 5000, CostCt: 120},
		{Asset: "pv", Start: day(-1), ImportWh: 10},
	}
	starts := map[string]time.Time{"wp": day(-3).Add(6 * time.Hour)}
	got := deviceHistory(sums, starts, mid, map[string]bool{"pv": true})
	if len(got) != 1 || got["wp"] == nil {
		t.Fatalf("devices = %v, want only wp", got)
	}
	d := got["wp"]
	if d.TodayWh != 4000 || d.TodayCt != 100 || d.YesterdayWh != 10000 || d.YesterdayCt != 300 {
		t.Errorf("today/yesterday = %+v", d)
	}
	if d.AvgDays != 2 || d.AvgWh != 8000 || d.AvgCt != 225 {
		t.Errorf("average = %v days, %v Wh, %v ct; want 2 days, 8000 Wh, 225 ct", d.AvgDays, d.AvgWh, d.AvgCt)
	}
}
