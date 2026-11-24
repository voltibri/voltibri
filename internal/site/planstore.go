// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/store"
)

// PlanStore keeps plan and actual per slot for the plan/actual chart
// (implemented by store.Store; optional).
type PlanStore interface {
	SavePlan(ctx context.Context, madeAt time.Time, slots []store.PlanSlot) error
	SaveSlotSoC(ctx context.Context, slot time.Time, soc float64) error
}

// savePlan records the plan for the slots of today: the first plan of a
// day is the reference ("Soll") the measured values are compared with.
func (s *Site) savePlan(ctx context.Context, plan planner.Plan) {
	ps, ok := s.hist.(PlanStore)
	if !ok || len(plan.Slots) == 0 {
		return
	}
	now := time.Now().In(s.loc)
	y, m, d := now.Date()
	end := time.Date(y, m, d+1, 0, 0, 0, 0, s.loc)
	var out []store.PlanSlot
	for _, sl := range plan.Slots {
		if !sl.Start.Before(end) {
			break
		}
		p := store.PlanSlot{Start: sl.Start, PlanPVW: sl.PVW, PlanLoadW: sl.LoadW, PlanSoC: sl.ExpectedSoC}
		if !s.state.HasBattery {
			p.PlanSoC = math.NaN()
		}
		out = append(out, p)
	}
	if err := ps.SavePlan(ctx, now, out); err != nil {
		slog.Error("writing plan failed", "err", err)
	}
}
