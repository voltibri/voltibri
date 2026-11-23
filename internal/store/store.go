// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package store persists configuration versions and measurement history in
// a SQLite database (pure Go driver, no cgo).
//
// Every configuration change creates a new, immutable version with author
// and comment; rolling back creates a new version with the old content, so
// the audit trail is never rewritten. Secret device parameters (passwords,
// tokens) are encrypted with AES-256-GCM; the key lives in a separate file
// (secret.key) or in the environment variable VOLTIBRI_SECRET_KEY.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/sdk"
)

// ErrNoConfig is returned when no configuration has been saved yet.
var ErrNoConfig = errors.New("no configuration saved yet")

// Redacted replaces secret values in exports and API responses. Saving a
// configuration that contains it keeps the previously stored secret.
const Redacted = "<secret>"

// Store is the voltibri database.
type Store struct {
	db  *sql.DB
	key []byte

	mu        sync.Mutex
	listeners []func(version int64)
}

// VersionInfo describes one configuration version.
type VersionInfo struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Author    string    `json:"author"`
	Comment   string    `json:"comment"`
}

var migrations = []string{
	`CREATE TABLE config_versions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at INTEGER NOT NULL,
		author     TEXT NOT NULL,
		comment    TEXT NOT NULL DEFAULT '',
		config     TEXT NOT NULL
	)`,
	`CREATE TABLE history (
		slot    INTEGER PRIMARY KEY, -- unix seconds of slot start
		pv_w    REAL NOT NULL,
		load_w  REAL NOT NULL,
		spot_ct REAL,
		temp_c  REAL
	)`,
	`CREATE TABLE cache (
		namespace TEXT NOT NULL, -- device id
		key       TEXT NOT NULL,
		stored_at INTEGER NOT NULL,
		value     BLOB NOT NULL,
		PRIMARY KEY (namespace, key)
	)`,
	`CREATE TABLE charging_sessions (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		loadpoint    TEXT NOT NULL,
		vehicle      TEXT NOT NULL,
		vehicle_name TEXT NOT NULL,
		guest        INTEGER NOT NULL,
		mode         TEXT NOT NULL,
		start        INTEGER NOT NULL, -- unix seconds
		end          INTEGER NOT NULL,
		energy_wh    REAL NOT NULL,
		solar_wh     REAL NOT NULL,
		cost_ct      REAL NOT NULL
	)`,
}

// Open opens (and creates or migrates) the database in dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	key, err := loadKey(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "voltibri.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps things simple and safe
	s := &Store{db: db, key: key}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// OnChange registers a callback invoked after a new version was saved.
func (s *Store) OnChange(f func(version int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, f)
}

// Save validates cfg and stores it as a new version. Secret parameters are
// encrypted; values equal to Redacted keep the currently stored secret.
func (s *Store) Save(ctx context.Context, cfg *config.Config, author, comment string) (int64, error) {
	cfg = cfg.Clone()
	if err := s.restoreRedacted(ctx, cfg); err != nil {
		return 0, err
	}
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	if err := s.transformSecrets(cfg, s.encrypt); err != nil {
		return 0, err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO config_versions (created_at, author, comment, config) VALUES (?, ?, ?, ?)`,
		time.Now().Unix(), author, comment, string(b))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	ls := append([]func(int64){}, s.listeners...)
	s.mu.Unlock()
	for _, f := range ls {
		f(id)
	}
	return id, nil
}

// Current returns the latest configuration (secrets decrypted).
func (s *Store) Current(ctx context.Context) (*config.Config, VersionInfo, error) {
	return s.load(ctx, `SELECT id, created_at, author, comment, config FROM config_versions ORDER BY id DESC LIMIT 1`)
}

// Version returns a specific configuration version (secrets decrypted).
func (s *Store) Version(ctx context.Context, id int64) (*config.Config, VersionInfo, error) {
	return s.load(ctx, `SELECT id, created_at, author, comment, config FROM config_versions WHERE id = ?`, id)
}

func (s *Store) load(ctx context.Context, q string, args ...any) (*config.Config, VersionInfo, error) {
	var vi VersionInfo
	var ts int64
	var raw string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&vi.ID, &ts, &vi.Author, &vi.Comment, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, vi, ErrNoConfig
	}
	if err != nil {
		return nil, vi, err
	}
	vi.CreatedAt = time.Unix(ts, 0)
	cfg := config.Default()
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, vi, fmt.Errorf("version %d: %w", vi.ID, err)
	}
	if err := s.transformSecrets(cfg, s.decrypt); err != nil {
		return nil, vi, fmt.Errorf("version %d: %w", vi.ID, err)
	}
	return cfg, vi, nil
}

// LatestVersion returns the id of the newest version (0 if none). Used to
// notice changes written by other processes (e.g. the CLI).
func (s *Store) LatestVersion(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM config_versions`).Scan(&id)
	return id, err
}

// Versions lists the most recent configuration versions, newest first.
func (s *Store) Versions(ctx context.Context, limit int) ([]VersionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, author, comment FROM config_versions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionInfo{}
	for rows.Next() {
		var vi VersionInfo
		var ts int64
		if err := rows.Scan(&vi.ID, &ts, &vi.Author, &vi.Comment); err != nil {
			return nil, err
		}
		vi.CreatedAt = time.Unix(ts, 0)
		out = append(out, vi)
	}
	return out, rows.Err()
}

// Rollback stores the content of version id as a new version.
func (s *Store) Rollback(ctx context.Context, id int64, author string) (int64, error) {
	cfg, _, err := s.Version(ctx, id)
	if err != nil {
		return 0, err
	}
	return s.Save(ctx, cfg, author, fmt.Sprintf("Zurückgesetzt auf Version %d", id))
}

// Redact returns a copy of cfg with all secrets replaced by Redacted.
func Redact(cfg *config.Config) *config.Config {
	out := cfg.Clone()
	for i, d := range out.Devices {
		drv, ok := sdk.Lookup(d.Type)
		if !ok {
			continue
		}
		for _, name := range drv.SecretParams() {
			if _, ok := d.Params[name]; ok {
				out.Devices[i].Params[name] = Redacted
			}
		}
	}
	return out
}

// restoreRedacted replaces Redacted placeholders with the stored secrets.
func (s *Store) restoreRedacted(ctx context.Context, cfg *config.Config) error {
	var cur *config.Config
	for i, d := range cfg.Devices {
		for k, v := range d.Params {
			if v != Redacted {
				continue
			}
			if cur == nil {
				c, _, err := s.Current(ctx)
				if err != nil && !errors.Is(err, ErrNoConfig) {
					return err
				}
				if c == nil {
					c = config.Default()
				}
				cur = c
			}
			old, ok := findParam(cur, d.ID, k)
			if !ok {
				return fmt.Errorf("device %q: secret %q has no stored value, please enter it", d.ID, k)
			}
			cfg.Devices[i].Params[k] = old
		}
	}
	return nil
}

func findParam(cfg *config.Config, id, key string) (any, bool) {
	for _, d := range cfg.Devices {
		if d.ID == id {
			v, ok := d.Params[key]
			return v, ok
		}
	}
	return nil, false
}

// transformSecrets applies f to every secret parameter value.
func (s *Store) transformSecrets(cfg *config.Config, f func(string) (string, error)) error {
	for _, d := range cfg.Devices {
		drv, ok := sdk.Lookup(d.Type)
		if !ok {
			continue
		}
		for _, name := range drv.SecretParams() {
			v, ok := d.Params[name].(string)
			if !ok || v == "" {
				continue
			}
			nv, err := f(v)
			if err != nil {
				return fmt.Errorf("device %q, parameter %q: %w", d.ID, name, err)
			}
			d.Params[name] = nv
		}
	}
	return nil
}

// --- history ---

func nullable(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return v
}

// AppendHistory stores (or replaces) one 15-minute record.
func (s *Store) AppendHistory(ctx context.Context, r series.Record) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO history (slot, pv_w, load_w, spot_ct, temp_c) VALUES (?, ?, ?, ?, ?)`,
		r.Time.Unix(), r.PVW, r.LoadW, nullable(r.SpotCt), nullable(r.TempC))
	return err
}

// ImportHistory stores imported records in one transaction. Slots that
// already exist (voltibri's own measurements) are kept unless overwrite is
// set. It returns the number of records written.
func (s *Store) ImportHistory(ctx context.Context, recs series.Series, overwrite bool) (int, error) {
	verb := "INSERT OR IGNORE"
	if overwrite {
		verb = "INSERT OR REPLACE"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, verb+` INTO history (slot, pv_w, load_w, spot_ct, temp_c) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, r := range recs {
		res, err := stmt.ExecContext(ctx, r.Time.Unix(), r.PVW, r.LoadW, nullable(r.SpotCt), nullable(r.TempC))
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// HistoryInfo summarises the stored history.
type HistoryInfo struct {
	Slots       int
	First, Last time.Time
}

// HistoryInfo returns the number of slots and the covered period.
func (s *Store) HistoryInfo(ctx context.Context) (HistoryInfo, error) {
	var hi HistoryInfo
	var first, last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(slot), MAX(slot) FROM history`).Scan(&hi.Slots, &first, &last)
	if first.Valid {
		hi.First, hi.Last = time.Unix(first.Int64, 0), time.Unix(last.Int64, 0)
	}
	return hi, err
}

// LoadHistory returns all records since the given time.
func (s *Store) LoadHistory(ctx context.Context, since time.Time, loc *time.Location) (series.Series, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slot, pv_w, load_w, spot_ct, temp_c FROM history WHERE slot >= ? ORDER BY slot`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out series.Series
	for rows.Next() {
		var slot int64
		var r series.Record
		var spot, temp sql.NullFloat64
		if err := rows.Scan(&slot, &r.PVW, &r.LoadW, &spot, &temp); err != nil {
			return nil, err
		}
		r.Time = time.Unix(slot, 0).In(loc)
		r.SpotCt, r.TempC = math.NaN(), math.NaN()
		if spot.Valid {
			r.SpotCt = spot.Float64
		}
		if temp.Valid {
			r.TempC = temp.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
