// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/voltibri/voltibri/internal/auth"
)

// The store implements auth.Store.
var (
	_ auth.Store         = (*Store)(nil)
	_ auth.ProviderStore = (*Store)(nil)
)

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrNotFound
	}
	return err
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

const userCols = `u.id, u.login, u.name, u.email, u.role, u.password, u.disabled, u.created,
	(SELECT MAX(last_seen) FROM sessions s WHERE s.user_id = u.id)`

func (s *Store) scanUsers(ctx context.Context, where string, args ...any) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users u `+where+` ORDER BY u.role, u.name COLLATE NOCASE, u.login`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var disabled int
		var created int64
		var seen sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.Role, &u.hash, &disabled, &created, &seen); err != nil {
			return nil, err
		}
		u.Disabled, u.Created, u.HasPassword, u.LastSeen = disabled == 1, fromUnix(created), u.hash != "", fromUnix(seen.Int64)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		g, err := s.userGroups(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Groups = g
	}
	return out, nil
}

// User is auth.User plus the password hash (never leaves the store).
type User struct {
	auth.User
	hash string
}

func (s *Store) userGroups(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT group_id FROM user_groups WHERE user_id = ? ORDER BY group_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Users lists all users.
func (s *Store) Users(ctx context.Context) ([]auth.User, error) {
	us, err := s.scanUsers(ctx, "")
	out := make([]auth.User, len(us))
	for i, u := range us {
		out[i] = u.User
	}
	return out, err
}

// User returns one user.
func (s *Store) User(ctx context.Context, id string) (auth.User, error) {
	us, err := s.scanUsers(ctx, "WHERE u.id = ?", id)
	if err != nil {
		return auth.User{}, err
	}
	if len(us) == 0 {
		return auth.User{}, auth.ErrNotFound
	}
	return us[0].User, nil
}

// UserByLogin returns a user and the password hash (login is case-insensitive).
func (s *Store) UserByLogin(ctx context.Context, login string) (auth.User, string, error) {
	us, err := s.scanUsers(ctx, "WHERE u.login = ? COLLATE NOCASE", login)
	if err != nil {
		return auth.User{}, "", err
	}
	if len(us) == 0 {
		return auth.User{}, "", auth.ErrNotFound
	}
	return us[0].User, us[0].hash, nil
}

// SaveUser creates or updates a user and the group memberships (not the password).
func (s *Store) SaveUser(ctx context.Context, u auth.User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, login, name, email, role, disabled, created) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET login = excluded.login, name = excluded.name, email = excluded.email, role = excluded.role, disabled = excluded.disabled`,
		u.ID, u.Login, u.Name, u.Email, u.Role, b2i(u.Disabled), unix(u.Created)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE user_id = ?`, u.ID); err != nil {
		return err
	}
	for _, g := range u.Groups {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_groups (user_id, group_id) SELECT ?, id FROM groups WHERE id = ?`, u.ID, g); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetPassword stores a password hash.
func (s *Store) SetPassword(ctx context.Context, userID, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password = ? WHERE id = ?`, hash, userID)
	return err
}

// DeleteUser removes a user (sessions, identities, invites cascade).
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

type groupPerms struct {
	Overview   bool     `json:"overview"`
	Reports    bool     `json:"reports"`
	Loadpoints []string `json:"loadpoints"`
	Vehicles   []string `json:"vehicles"`
}

// Groups lists all groups.
func (s *Store) Groups(ctx context.Context) ([]auth.Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, perms FROM groups ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auth.Group{}
	for rows.Next() {
		var g auth.Group
		var perms string
		if err := rows.Scan(&g.ID, &g.Name, &perms); err != nil {
			return nil, err
		}
		var p groupPerms
		_ = json.Unmarshal([]byte(perms), &p)
		g.Overview, g.Reports, g.Loadpoints, g.Vehicles = p.Overview, p.Reports, p.Loadpoints, p.Vehicles
		if g.Loadpoints == nil {
			g.Loadpoints = []string{}
		}
		if g.Vehicles == nil {
			g.Vehicles = []string{}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SaveGroup creates or updates a group.
func (s *Store) SaveGroup(ctx context.Context, g auth.Group) error {
	b, _ := json.Marshal(groupPerms{Overview: g.Overview, Reports: g.Reports, Loadpoints: g.Loadpoints, Vehicles: g.Vehicles})
	_, err := s.db.ExecContext(ctx, `INSERT INTO groups (id, name, perms) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, perms = excluded.perms`, g.ID, g.Name, string(b))
	return err
}

// DeleteGroup removes a group (memberships cascade).
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
	return err
}

func (s *Store) scanIdentities(ctx context.Context, where string, args ...any) ([]auth.Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, subject, user_id, email, name, created FROM identities `+where+` ORDER BY provider, created`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auth.Identity{}
	for rows.Next() {
		var i auth.Identity
		var created int64
		if err := rows.Scan(&i.Provider, &i.Subject, &i.UserID, &i.Email, &i.Name, &created); err != nil {
			return nil, err
		}
		i.Created = fromUnix(created)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Identities lists the external logins of a user.
func (s *Store) Identities(ctx context.Context, userID string) ([]auth.Identity, error) {
	return s.scanIdentities(ctx, "WHERE user_id = ?", userID)
}

// IdentityBySubject finds the user of an external login.
func (s *Store) IdentityBySubject(ctx context.Context, provider, subject string) (auth.Identity, error) {
	is, err := s.scanIdentities(ctx, "WHERE provider = ? AND subject = ?", provider, subject)
	if err != nil {
		return auth.Identity{}, err
	}
	if len(is) == 0 {
		return auth.Identity{}, auth.ErrNotFound
	}
	return is[0], nil
}

// SaveIdentity links an external login (or updates its name and e-mail).
func (s *Store) SaveIdentity(ctx context.Context, i auth.Identity) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO identities (provider, subject, user_id, email, name, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, subject) DO UPDATE SET email = excluded.email, name = excluded.name WHERE identities.user_id = excluded.user_id`,
		i.Provider, i.Subject, i.UserID, i.Email, i.Name, unix(i.Created))
	return err
}

// DeleteIdentity removes an external login of a user.
func (s *Store) DeleteIdentity(ctx context.Context, userID, provider, subject string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM identities WHERE user_id = ? AND provider = ? AND subject = ?`, userID, provider, subject)
	return err
}

const sessionCols = `id, user_id, created, last_seen, auth_at, elevated_until, method, agent`

func scanSession(sc interface{ Scan(...any) error }) (auth.Session, error) {
	var x auth.Session
	var created, seen, authAt, elev int64
	err := sc.Scan(&x.ID, &x.UserID, &created, &seen, &authAt, &elev, &x.Method, &x.Agent)
	x.Created, x.LastSeen, x.AuthAt, x.ElevatedUntil = fromUnix(created), fromUnix(seen), fromUnix(authAt), fromUnix(elev)
	return x, err
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, x auth.Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		x.ID, x.UserID, unix(x.Created), unix(x.LastSeen), unix(x.AuthAt), unix(x.ElevatedUntil), x.Method, x.Agent)
	return err
}

// Session returns a session by id (token hash).
func (s *Store) Session(ctx context.Context, id string) (auth.Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	return x, notFound(err)
}

// UpdateSession stores last use and elevation.
func (s *Store) UpdateSession(ctx context.Context, x auth.Session) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen = ?, auth_at = ?, elevated_until = ?, method = ? WHERE id = ?`,
		unix(x.LastSeen), unix(x.AuthAt), unix(x.ElevatedUntil), x.Method, x.ID)
	return err
}

// Sessions lists the sessions of a user, latest first.
func (s *Store) Sessions(ctx context.Context, userID string) ([]auth.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE user_id = ? ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auth.Session{}
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteSession ends a session.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteSessions ends all sessions of a user.
func (s *Store) DeleteSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// CreateInvite stores an invite link (token hash).
func (s *Store) CreateInvite(ctx context.Context, tokenHash, userID string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO invites (token, user_id, expires) VALUES (?, ?, ?)`, tokenHash, userID, unix(expires))
	return err
}

// Invite returns user and expiry of an invite.
func (s *Store) Invite(ctx context.Context, tokenHash string) (string, time.Time, error) {
	var id string
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires FROM invites WHERE token = ?`, tokenHash).Scan(&id, &exp)
	return id, fromUnix(exp), notFound(err)
}

// DeleteInvites removes the invites of a user.
func (s *Store) DeleteInvites(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE user_id = ?`, userID)
	return err
}

// Providers lists the login services with decrypted secrets.
func (s *Store) Providers(ctx context.Context) ([]auth.Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, issuer, client_id, client_secret, created FROM login_providers ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auth.Provider{}
	for rows.Next() {
		var p auth.Provider
		var secret string
		var created int64
		if err := rows.Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &secret, &created); err != nil {
			return nil, err
		}
		if secret != "" {
			if p.ClientSecret, err = s.decrypt(secret); err != nil {
				return nil, err
			}
		}
		p.HasSecret, p.Created = secret != "", fromUnix(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveProvider creates or updates a login service; an empty secret keeps
// the stored one.
func (s *Store) SaveProvider(ctx context.Context, p auth.Provider) error {
	secret := ""
	if p.ClientSecret != "" {
		var err error
		if secret, err = s.encrypt(p.ClientSecret); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO login_providers (id, name, issuer, client_id, client_secret, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, issuer = excluded.issuer, client_id = excluded.client_id,
			client_secret = CASE WHEN excluded.client_secret = '' THEN login_providers.client_secret ELSE excluded.client_secret END`,
		p.ID, p.Name, p.Issuer, p.ClientID, secret, unix(p.Created))
	return err
}

// DeleteProvider removes a login service and the logins linked through it.
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE provider = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_providers WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
