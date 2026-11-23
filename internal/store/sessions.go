// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"

	"github.com/voltibri/voltibri/internal/charging"
)

// SaveSession stores a finished charging session.
func (s *Store) SaveSession(ctx context.Context, c charging.Session) (int64, error) {
	guest := 0
	if c.Guest {
		guest = 1
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO charging_sessions
		(loadpoint, vehicle, vehicle_name, guest, mode, start, end, energy_wh, solar_wh, cost_ct)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Loadpoint, c.Vehicle, c.VehicleName, guest, string(c.Mode), c.Start.Unix(), c.End.Unix(), c.EnergyWh, c.SolarWh, c.CostCt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Sessions returns the latest finished charging sessions, newest first.
func (s *Store) Sessions(ctx context.Context, limit int) ([]charging.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, loadpoint, vehicle, vehicle_name, guest, mode, start, end, energy_wh, solar_wh, cost_ct
		FROM charging_sessions ORDER BY start DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []charging.Session{}
	for rows.Next() {
		var c charging.Session
		var guest int
		var mode string
		var start, end int64
		if err := rows.Scan(&c.ID, &c.Loadpoint, &c.Vehicle, &c.VehicleName, &guest, &mode, &start, &end, &c.EnergyWh, &c.SolarWh, &c.CostCt); err != nil {
			return nil, err
		}
		c.Guest, c.Mode, c.Start, c.End = guest == 1, charging.Mode(mode), time.Unix(start, 0), time.Unix(end, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}
