// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package auth knows users, groups, login identities and sessions, and
// decides what a signed-in user may see and do.
//
// Roles: an administrator may do everything; a user only what the groups
// grant (overview, reports, wallboxes, vehicles). Without a session nothing
// but the login page is reachable.
//
// Sessions run long (everyday use: switching a wallbox from the phone), but
// the configuration needs a recent login: a session is "elevated" for a
// short time after signing in and must be confirmed again afterwards.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Lifetimes.
var (
	SessionIdle   = 180 * 24 * time.Hour // a session ends after this long unused
	ElevatedFor   = 30 * time.Minute     // configuration access after a login (sliding)
	ElevatedMax   = 8 * time.Hour        // ... but at most this long after the login
	InviteFor     = 7 * 24 * time.Hour
	touchInterval = 10 * time.Minute // last-seen updates are written at most this often
)

// User is a person who can sign in.
type User struct {
	ID          string    `json:"id"`
	Login       string    `json:"login"` // user name for the built-in login
	Name        string    `json:"name"`
	Email       string    `json:"email,omitempty"`
	Role        string    `json:"role"`
	Groups      []string  `json:"groups"`
	Disabled    bool      `json:"disabled,omitempty"`
	HasPassword bool      `json:"hasPassword"`
	Created     time.Time `json:"created"`
	LastSeen    time.Time `json:"lastSeen,omitzero"`
}

// DisplayName is the name or else the login.
func (u User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	return u.Login
}

// Group grants access to parts of voltibri.
type Group struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Overview   bool     `json:"overview"`   // energy flow, battery, heating, today
	Reports    bool     `json:"reports"`    // charging reports of the group's vehicles
	Loadpoints []string `json:"loadpoints"` // wallboxes (charger device ids)
	Vehicles   []string `json:"vehicles"`   // vehicle asset ids
}

// Identity links a login at an external provider to a user.
type Identity struct {
	Provider string    `json:"provider"`
	Subject  string    `json:"subject"`
	UserID   string    `json:"userId"`
	Email    string    `json:"email,omitempty"`
	Name     string    `json:"name,omitempty"`
	Created  time.Time `json:"created"`
}

// Session is a signed-in browser.
type Session struct {
	ID            string    `json:"id"` // hash of the token (the token itself is only in the cookie)
	UserID        string    `json:"userId"`
	Created       time.Time `json:"created"`
	LastSeen      time.Time `json:"lastSeen"`
	AuthAt        time.Time `json:"authAt"` // last time the user proved who they are
	ElevatedUntil time.Time `json:"elevatedUntil"`
	Method        string    `json:"method"` // "password" or the provider id
	Agent         string    `json:"agent"`
}

// Store persists users, groups, identities, sessions and invites
// (implemented by store.Store).
type Store interface {
	Users(ctx context.Context) ([]User, error)
	User(ctx context.Context, id string) (User, error)
	UserByLogin(ctx context.Context, login string) (User, string, error) // user and password hash
	SaveUser(ctx context.Context, u User) error
	SetPassword(ctx context.Context, userID, hash string) error
	DeleteUser(ctx context.Context, id string) error

	Groups(ctx context.Context) ([]Group, error)
	SaveGroup(ctx context.Context, g Group) error
	DeleteGroup(ctx context.Context, id string) error

	Identities(ctx context.Context, userID string) ([]Identity, error)
	IdentityBySubject(ctx context.Context, provider, subject string) (Identity, error)
	SaveIdentity(ctx context.Context, i Identity) error
	DeleteIdentity(ctx context.Context, userID, provider, subject string) error

	CreateSession(ctx context.Context, s Session) error
	Session(ctx context.Context, id string) (Session, error)
	UpdateSession(ctx context.Context, s Session) error
	Sessions(ctx context.Context, userID string) ([]Session, error)
	DeleteSession(ctx context.Context, id string) error
	DeleteSessions(ctx context.Context, userID string) error

	CreateInvite(ctx context.Context, tokenHash, userID string, expires time.Time) error
	Invite(ctx context.Context, tokenHash string) (string, time.Time, error) // user id, expiry
	DeleteInvites(ctx context.Context, userID string) error
}

// ErrNotFound is returned by the store for unknown records.
var ErrNotFound = errors.New("not found")

// Principal is the signed-in user with the session and the effective
// permissions of the user's groups.
type Principal struct {
	User       User
	Session    Session
	overview   bool
	reports    bool
	loadpoints map[string]bool
	vehicles   map[string]bool
}

// NewPrincipal combines the user's groups.
func NewPrincipal(u User, s Session, groups []Group) *Principal {
	p := &Principal{User: u, Session: s, loadpoints: map[string]bool{}, vehicles: map[string]bool{}}
	for _, g := range groups {
		if !slices.Contains(u.Groups, g.ID) {
			continue
		}
		p.overview = p.overview || g.Overview
		p.reports = p.reports || g.Reports
		for _, l := range g.Loadpoints {
			p.loadpoints[l] = true
		}
		for _, v := range g.Vehicles {
			p.vehicles[v] = true
		}
	}
	return p
}

// Admin reports whether the user is an administrator.
func (p *Principal) Admin() bool { return p != nil && p.User.Role == RoleAdmin }

// Elevated reports whether the session may change the configuration now.
func (p *Principal) Elevated(now time.Time) bool {
	return p.Admin() && now.Before(p.Session.ElevatedUntil)
}

// Overview: energy flow, battery plan, heating.
func (p *Principal) Overview() bool { return p.Admin() || (p != nil && p.overview) }

// Reports: charging reports (admins: all, users: their vehicles).
func (p *Principal) Reports() bool { return p.Admin() || (p != nil && p.reports) }

// Loadpoint reports whether the user may use the wallbox.
func (p *Principal) Loadpoint(id string) bool { return p.Admin() || (p != nil && p.loadpoints[id]) }

// Vehicle reports whether the user may see and control the vehicle.
func (p *Principal) Vehicle(key string) bool { return p.Admin() || (p != nil && p.vehicles[key]) }

// Vehicles lists the vehicles of a user (nil for admins: all).
func (p *Principal) Vehicles() []string {
	if p.Admin() {
		return nil
	}
	out := []string{}
	for v := range p.vehicles {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// Loadpoints lists the wallboxes of a user (nil for admins: all).
func (p *Principal) Loadpoints() []string {
	if p.Admin() {
		return nil
	}
	out := []string{}
	for l := range p.loadpoints {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

type ctxKey struct{}

// With returns a context carrying the principal.
func With(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the principal of a request (nil: not signed in).
func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// NewToken returns a random token for a cookie or link and its hash for the
// database (tokens are never stored).
func NewToken() (token, hash string) {
	b := make([]byte, 32)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken hashes a session or invite token.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// NewID returns a short random id.
func NewID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

// Provider is an external login service (OpenID Connect).
type Provider struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`   // shown on the button, e.g. "Pocket ID"
	Issuer       string    `json:"issuer"` // e.g. https://id.example.com or https://accounts.google.com
	ClientID     string    `json:"clientId"`
	ClientSecret string    `json:"clientSecret,omitempty"` // only when saving; never returned
	HasSecret    bool      `json:"hasSecret"`
	Created      time.Time `json:"created"`
}

// ProviderStore persists the login services (implemented by store.Store).
type ProviderStore interface {
	Providers(ctx context.Context) ([]Provider, error)  // with decrypted secrets
	SaveProvider(ctx context.Context, p Provider) error // empty secret keeps the stored one
	DeleteProvider(ctx context.Context, id string) error
}
