// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/run"
)

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// notShadowed hides runs measured by voltibri that overlap an imported run
// of the same asset (both systems recorded the same session; the import is
// complete, voltibri may only have seen part of it). Undoing the import
// shows them again.
const notShadowed = `NOT (r.source = 'voltibri' AND EXISTS (SELECT 1 FROM runs i WHERE i.asset = r.asset AND i.source != 'voltibri'
	AND i.start < r.end AND i.end > r.start))`

const runColumns = `asset, kind, vehicle, vehicle_name, guest, mode, start, end, energy_wh, pv_wh, battery_wh, grid_wh, cost_ct, cost_known, price_note, attrs`

func runValues(r run.Run) []any {
	attrs := "{}"
	if len(r.Attrs) > 0 {
		if b, err := json.Marshal(r.Attrs); err == nil {
			attrs = string(b)
		}
	}
	return []any{r.Asset, r.Kind, r.Vehicle, r.VehicleName, b2i(r.Guest), r.Mode, r.Start.Unix(), r.End.Unix(),
		r.EnergyWh, r.PVWh, r.BatteryWh, r.GridWh, r.CostCt, b2i(!r.CostUnknown), r.PriceNote, attrs}
}

// SaveRun stores a finished run measured by voltibri.
func (s *Store) SaveRun(ctx context.Context, r run.Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (`+runColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, runValues(r)...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Runs returns finished runs, newest first.
func (s *Store) Runs(ctx context.Context, f run.Filter) ([]run.Run, error) {
	where, args := runWhere(f)
	q := `SELECT id, ` + runColumns + `, source, COALESCE(external_id, ''), COALESCE(import_id, 0) FROM runs r`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += " ORDER BY start DESC LIMIT ? OFFSET ?"
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []run.Run{}
	for rows.Next() {
		var r run.Run
		var guest, known int
		var start, end int64
		var attrs, source string
		if err := rows.Scan(&r.ID, &r.Asset, &r.Kind, &r.Vehicle, &r.VehicleName, &guest, &r.Mode, &start, &end,
			&r.EnergyWh, &r.PVWh, &r.BatteryWh, &r.GridWh, &r.CostCt, &known, &r.PriceNote, &attrs, &source, &r.ExternalID, &r.ImportID); err != nil {
			return nil, err
		}
		r.Guest, r.CostUnknown, r.Start, r.End = guest == 1, known == 0, time.Unix(start, 0), time.Unix(end, 0)
		if attrs != "{}" && attrs != "" {
			_ = json.Unmarshal([]byte(attrs), &r.Attrs)
		}
		if source != "voltibri" {
			r.Source = source
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunSummary aggregates the runs of one asset.
type RunSummary struct {
	Asset    string    `json:"asset"`
	Kind     string    `json:"kind"`
	Count    int       `json:"count"`
	Imported int       `json:"imported"` // of Count
	EnergyWh float64   `json:"energyWh"`
	SolarWh  float64   `json:"solarWh"` // PV + battery
	CostCt   float64   `json:"costCt"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
}

// RunSummaries returns one summary per asset and run kind for the runs
// selected by f (Limit and Offset are ignored).
func (s *Store) RunSummaries(ctx context.Context, f run.Filter) ([]RunSummary, error) {
	where, args := runWhere(f)
	rows, err := s.db.QueryContext(ctx, `SELECT asset, kind, COUNT(*), SUM(source != 'voltibri'), SUM(energy_wh), SUM(pv_wh + battery_wh),
		SUM(CASE WHEN cost_known = 1 THEN cost_ct ELSE 0 END), MIN(start), MAX(start) FROM runs r WHERE `+strings.Join(where, " AND ")+` GROUP BY asset, kind ORDER BY asset, kind`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunSummary{}
	for rows.Next() {
		var r RunSummary
		var first, last int64
		if err := rows.Scan(&r.Asset, &r.Kind, &r.Count, &r.Imported, &r.EnergyWh, &r.SolarWh, &r.CostCt, &first, &last); err != nil {
			return nil, err
		}
		r.First, r.Last = time.Unix(first, 0), time.Unix(last, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Import describes one import run.
type Import struct {
	ID          int64          `json:"id"`
	CreatedAt   time.Time      `json:"createdAt"`
	Source      string         `json:"source"` // e.g. "evcc"
	Kind        string         `json:"kind"`   // "runs"
	Author      string         `json:"author"`
	Description string         `json:"description"`
	Params      map[string]any `json:"params,omitempty"`
	Count       int            `json:"count"` // records added
	UndoneAt    *time.Time     `json:"undoneAt,omitempty"`
}

// ImportRuns stores runs from another system as one import. Every run
// needs an ExternalID; runs already imported from the same source (by any
// import) are skipped. Imports that add nothing are not recorded (ID 0).
func (s *Store) ImportRuns(ctx context.Context, imp Import, runs []run.Run) (Import, int, error) {
	if imp.Source == "" || imp.Source == "voltibri" {
		return imp, 0, errors.New("import needs a source other than voltibri")
	}
	params, err := json.Marshal(imp.Params)
	if err != nil {
		return imp, 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return imp, 0, err
	}
	defer tx.Rollback()
	imp.CreatedAt, imp.Kind = time.Now().Truncate(time.Second), "runs"
	res, err := tx.ExecContext(ctx, `INSERT INTO imports (created_at, source, kind, author, description, params) VALUES (?, ?, ?, ?, ?, ?)`,
		imp.CreatedAt.Unix(), imp.Source, imp.Kind, imp.Author, imp.Description, string(params))
	if err != nil {
		return imp, 0, err
	}
	if imp.ID, err = res.LastInsertId(); err != nil {
		return imp, 0, err
	}
	skipped := 0
	for _, r := range runs {
		if r.ExternalID == "" {
			return imp, 0, fmt.Errorf("run from %s at %s without external id", imp.Source, r.Start)
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runs (`+runColumns+`, source, external_id, import_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(runValues(r), imp.Source, r.ExternalID, imp.ID)...)
		if err != nil {
			return imp, 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			skipped++
		} else {
			imp.Count++
		}
	}
	if imp.Count == 0 {
		return Import{}, skipped, nil // rollback: nothing to remember
	}
	if _, err := tx.ExecContext(ctx, `UPDATE imports SET count = ? WHERE id = ?`, imp.Count, imp.ID); err != nil {
		return imp, 0, err
	}
	return imp, skipped, tx.Commit()
}

// ImportedIDs returns the external ids already imported from source.
func (s *Store) ImportedIDs(ctx context.Context, source string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT external_id FROM runs WHERE source = ? AND external_id IS NOT NULL`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Imports lists the imports, newest first.
func (s *Store) Imports(ctx context.Context) ([]Import, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, source, kind, author, description, params, count, undone_at FROM imports ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Import{}
	for rows.Next() {
		var i Import
		var created int64
		var params string
		var undone sql.NullInt64
		if err := rows.Scan(&i.ID, &created, &i.Source, &i.Kind, &i.Author, &i.Description, &params, &i.Count, &undone); err != nil {
			return nil, err
		}
		i.CreatedAt = time.Unix(created, 0)
		_ = json.Unmarshal([]byte(params), &i.Params)
		if undone.Valid {
			t := time.Unix(undone.Int64, 0)
			i.UndoneAt = &t
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ErrUndone is returned when an import was already undone.
var ErrUndone = errors.New("import already undone")

// UndoImport removes all records of an import. The import itself stays in
// the list (marked as undone), so it is clear what happened.
func (s *Store) UndoImport(ctx context.Context, id int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var undone sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT undone_at FROM imports WHERE id = ?`, id).Scan(&undone); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("import %d not found", id)
		}
		return 0, err
	}
	if undone.Valid {
		return 0, ErrUndone
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE import_id = ?`, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE imports SET undone_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// RunBucket aggregates runs per hour, day or month (in the site time zone).
type RunBucket struct {
	Key       string    `json:"key"`   // 2006-01-02T15, 2006-01-02 or 2006-01
	Start     time.Time `json:"start"` // start of the bucket
	Count     int       `json:"count"`
	EnergyWh  float64   `json:"energyWh"`
	PVWh      float64   `json:"pvWh"`
	BatteryWh float64   `json:"batteryWh"`
	GridWh    float64   `json:"gridWh"`
	CostCt    float64   `json:"costCt"`
}

// Buckets for RunBuckets.
const (
	BucketHour  = "hour"
	BucketDay   = "day"
	BucketMonth = "month"
)

// RunBuckets returns totals per bucket of the runs selected by f (Limit and
// Offset are ignored), oldest first. A run counts in the bucket it started.
func (s *Store) RunBuckets(ctx context.Context, f run.Filter, loc *time.Location, bucket string) ([]RunBucket, error) {
	startOf := func(t time.Time) (string, time.Time) { return bucketOf(t, loc, bucket) }
	f.Limit, f.Offset = 1<<30, 0
	var out []RunBucket
	idx := map[string]int{}
	for page := 0; ; page++ {
		f.Offset = page * 5000
		f.Limit = 5000
		rs, err := s.Runs(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			m, start := startOf(r.Start)
			i, ok := idx[m]
			if !ok {
				i = len(out)
				idx[m] = i
				out = append(out, RunBucket{Key: m, Start: start})
			}
			o := &out[i]
			o.Count++
			o.EnergyWh += r.EnergyWh
			o.PVWh += r.PVWh
			o.BatteryWh += r.BatteryWh
			o.GridWh += r.GridWh
			if !r.CostUnknown {
				o.CostCt += r.CostCt
			}
		}
		if len(rs) < 5000 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// runWhere builds the conditions for a filter (always at least one).
func runWhere(f run.Filter) ([]string, []any) {
	where := []string{notShadowed}
	if f.All {
		where = []string{"1 = 1"}
	}
	var args []any
	if f.Asset != "" {
		where, args = append(where, "asset = ?"), append(args, f.Asset)
	}
	if f.Vehicle != "" {
		where, args = append(where, "vehicle_name = ?"), append(args, f.Vehicle)
	}
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, f.Kind)
	}
	if !f.From.IsZero() {
		where, args = append(where, "start >= ?"), append(args, f.From.Unix())
	}
	if !f.To.IsZero() {
		where, args = append(where, "start < ?"), append(args, f.To.Unix())
	}
	for a, from := range f.ProfiledFrom {
		where, args = append(where, "NOT (asset = ? AND start >= ?)"), append(args, a, from.Unix())
	}
	return where, args
}

// RunVehicle is a vehicle that appears in runs.
type RunVehicle struct {
	Name     string    `json:"name"`
	Guest    bool      `json:"guest"`
	Count    int       `json:"count"`
	EnergyWh float64   `json:"energyWh"`
	Last     time.Time `json:"last"`
}

// RunVehicles lists the vehicles of the runs selected by f (vehicle and
// paging ignored), most used first.
func (s *Store) RunVehicles(ctx context.Context, f run.Filter) ([]RunVehicle, error) {
	f.Vehicle = ""
	where, args := runWhere(f)
	rows, err := s.db.QueryContext(ctx, `SELECT vehicle_name, MAX(guest), COUNT(*), SUM(energy_wh), MAX(start) FROM runs r
		WHERE `+strings.Join(where, " AND ")+` AND vehicle_name != '' GROUP BY vehicle_name ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunVehicle{}
	for rows.Next() {
		var v RunVehicle
		var guest int
		var last int64
		if err := rows.Scan(&v.Name, &guest, &v.Count, &v.EnergyWh, &last); err != nil {
			return nil, err
		}
		v.Guest, v.Last = guest == 1, time.Unix(last, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}
