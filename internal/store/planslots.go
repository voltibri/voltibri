// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"math"
	"time"
)

// PlanSlot is the plan ("Soll") and the measured state of charge of one
// 15-minute slot. Unknown values are NaN.
type PlanSlot struct {
	Start     time.Time
	PlanAt    time.Time // zero: no plan stored
	PlanPVW   float64
	PlanLoadW float64
	PlanSoC   float64
	SoC       float64
}

// SavePlan stores the planned values of slots that have none yet: each slot
// keeps the first plan made on its day.
func (s *Store) SavePlan(ctx context.Context, madeAt time.Time, slots []PlanSlot) error {
	if len(slots) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO plan_slots (slot, plan_at, plan_pv_w, plan_load_w, plan_soc) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(slot) DO UPDATE SET plan_at = excluded.plan_at, plan_pv_w = excluded.plan_pv_w,
			plan_load_w = excluded.plan_load_w, plan_soc = excluded.plan_soc WHERE plan_slots.plan_at IS NULL`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range slots {
		if _, err := stmt.ExecContext(ctx, p.Start.Unix(), madeAt.Unix(), nullable(p.PlanPVW), nullable(p.PlanLoadW), nullable(p.PlanSoC)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveSlotSoC stores the measured state of charge at the end of a slot.
func (s *Store) SaveSlotSoC(ctx context.Context, slot time.Time, soc float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO plan_slots (slot, soc) VALUES (?, ?) ON CONFLICT(slot) DO UPDATE SET soc = excluded.soc`,
		slot.Unix(), nullable(soc))
	return err
}

// PlanSlots returns the stored slots in [from, to).
func (s *Store) PlanSlots(ctx context.Context, from, to time.Time) ([]PlanSlot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slot, plan_at, plan_pv_w, plan_load_w, plan_soc, soc FROM plan_slots
		WHERE slot >= ? AND slot < ? ORDER BY slot`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	f := func(v sql.NullFloat64) float64 {
		if v.Valid {
			return v.Float64
		}
		return math.NaN()
	}
	var out []PlanSlot
	for rows.Next() {
		var slot int64
		var at sql.NullInt64
		var pv, load, psoc, soc sql.NullFloat64
		if err := rows.Scan(&slot, &at, &pv, &load, &psoc, &soc); err != nil {
			return nil, err
		}
		p := PlanSlot{Start: time.Unix(slot, 0), PlanPVW: f(pv), PlanLoadW: f(load), PlanSoC: f(psoc), SoC: f(soc)}
		if at.Valid {
			p.PlanAt = time.Unix(at.Int64, 0)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
