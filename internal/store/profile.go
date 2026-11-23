// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProfileValue is the energy of one device in one 15-minute slot.
type ProfileValue struct {
	Asset     string
	ImportWh  float64 // drawn
	ExportWh  float64 // delivered (PV production, battery discharge, feed-in)
	PVWh      float64 // import share from PV
	BatteryWh float64 // import share from the home battery
	GridWh    float64 // import share from the grid
	CostCt    float64 // cost of the import
}

func (v ProfileValue) zero() bool {
	return math.Abs(v.ImportWh) < 0.05 && math.Abs(v.ExportWh) < 0.05
}

// assetIDs maps device ids to the small integers used in load_profile.
type assetIDs struct {
	mu sync.Mutex
	m  map[string]int64
}

func (s *Store) assetID(ctx context.Context, tx *sql.Tx, asset string) (int64, error) {
	s.ids.mu.Lock()
	defer s.ids.mu.Unlock()
	if s.ids.m == nil {
		s.ids.m = map[string]int64{}
	}
	if id, ok := s.ids.m[asset]; ok {
		return id, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO assets (asset) VALUES (?)`, asset); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM assets WHERE asset = ?`, asset).Scan(&id); err != nil {
		return 0, err
	}
	s.ids.m[asset] = id
	return id, nil
}

func dwh(wh float64) int64 { return int64(math.Round(wh * 10)) }

// AppendProfile adds the measured energy of all devices to one slot (a
// slot can be written in parts, e.g. around a restart). Slots without
// energy are not stored (a missing value means 0).
func (s *Store) AppendProfile(ctx context.Context, slot time.Time, values []ProfileValue) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range values {
		if v.zero() {
			continue
		}
		id, err := s.assetID(ctx, tx, v.Asset)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO load_profile
			(asset_id, slot, import_dwh, export_dwh, pv_dwh, battery_dwh, grid_dwh, cost_mct) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (asset_id, slot) DO UPDATE SET import_dwh = import_dwh + excluded.import_dwh, export_dwh = export_dwh + excluded.export_dwh,
			pv_dwh = pv_dwh + excluded.pv_dwh, battery_dwh = battery_dwh + excluded.battery_dwh, grid_dwh = grid_dwh + excluded.grid_dwh,
			cost_mct = cost_mct + excluded.cost_mct`,
			id, slot.Unix(), dwh(v.ImportWh), dwh(v.ExportWh), dwh(v.PVWh), dwh(v.BatteryWh), dwh(v.GridWh), int64(math.Round(v.CostCt*1000))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProfileStarts returns the first slot with a load profile per device.
func (s *Store) ProfileStarts(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.asset, MIN(p.slot) FROM load_profile p JOIN assets a ON a.id = p.asset_id GROUP BY a.asset`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var a string
		var first int64
		if err := rows.Scan(&a, &first); err != nil {
			return nil, err
		}
		out[a] = time.Unix(first, 0)
	}
	return out, rows.Err()
}

// ProfileSum is the summed load profile of one device in one bucket.
type ProfileSum struct {
	Asset     string
	Key       string // bucket key as in RunBuckets
	Start     time.Time
	ImportWh  float64
	ExportWh  float64
	PVWh      float64
	BatteryWh float64
	GridWh    float64
	CostCt    float64
}

// ProfileSums sums the load profile of the given devices (all if empty)
// in [from, to) per device and bucket (hour, day, month, or "" for one sum
// per device).
func (s *Store) ProfileSums(ctx context.Context, assets []string, from, to time.Time, loc *time.Location, bucket string) ([]ProfileSum, error) {
	q := `SELECT a.asset, p.slot, p.import_dwh, p.export_dwh, p.pv_dwh, p.battery_dwh, p.grid_dwh, p.cost_mct
		FROM load_profile p JOIN assets a ON a.id = p.asset_id WHERE 1 = 1`
	var args []any
	if len(assets) > 0 {
		q += ` AND a.asset IN (?` + strings.Repeat(`, ?`, len(assets)-1) + `)`
		for _, a := range assets {
			args = append(args, a)
		}
	}
	if !from.IsZero() {
		q, args = q+` AND p.slot >= ?`, append(args, from.Unix())
	}
	if !to.IsZero() {
		q, args = q+` AND p.slot < ?`, append(args, to.Unix())
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[[2]string]int{}
	var out []ProfileSum
	for rows.Next() {
		var a string
		var slot, imp, exp, pv, bat, grid, cost int64
		if err := rows.Scan(&a, &slot, &imp, &exp, &pv, &bat, &grid, &cost); err != nil {
			return nil, err
		}
		key, start := "", time.Time{}
		if bucket != "" {
			key, start = bucketOf(time.Unix(slot, 0), loc, bucket)
		}
		k := [2]string{a, key}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, ProfileSum{Asset: a, Key: key, Start: start})
		}
		o := &out[i]
		o.ImportWh += float64(imp) / 10
		o.ExportWh += float64(exp) / 10
		o.PVWh += float64(pv) / 10
		o.BatteryWh += float64(bat) / 10
		o.GridWh += float64(grid) / 10
		o.CostCt += float64(cost) / 1000
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Asset < out[j].Asset
	})
	return out, rows.Err()
}

// bucketOf returns key and start of the bucket containing t.
func bucketOf(t time.Time, loc *time.Location, bucket string) (string, time.Time) {
	t = t.In(loc)
	switch bucket {
	case BucketHour:
		b := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		return b.Format("2006-01-02T15"), b
	case BucketDay:
		b := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		return b.Format("2006-01-02"), b
	}
	b := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	return b.Format("2006-01"), b
}
