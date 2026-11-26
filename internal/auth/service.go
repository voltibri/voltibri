// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Errors shown to the user.
var (
	ErrBadLogin    = errors.New("Benutzername oder Passwort falsch.")
	ErrThrottled   = errors.New("Zu viele Fehlversuche – bitte in ein paar Minuten erneut versuchen.")
	ErrNoSession   = errors.New("nicht angemeldet")
	ErrInvite      = errors.New("Der Einladungslink ist ungültig oder abgelaufen.")
	ErrLastAdmin   = errors.New("Der letzte Administrator kann nicht entfernt oder herabgestuft werden.")
	ErrLoginExists = errors.New("Diesen Benutzernamen gibt es schon.")
)

// Service implements sign-in, sessions and user management.
type Service struct {
	st  Store
	now func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time // key (ip or login) -> recent failed logins
}

// NewService creates the service.
func NewService(st Store) *Service {
	return &Service{st: st, now: time.Now, failures: map[string][]time.Time{}}
}

// Store returns the underlying store.
func (s *Service) Store() Store { return s.st }

// HasUsers reports whether anyone can sign in (else setup is open).
func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	us, err := s.st.Users(ctx)
	return len(us) > 0, err
}

const (
	maxFailures   = 8
	failureWindow = 15 * time.Minute
)

func (s *Service) throttled(keys ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, k := range keys {
		var keep []time.Time
		for _, t := range s.failures[k] {
			if now.Sub(t) < failureWindow {
				keep = append(keep, t)
			}
		}
		s.failures[k] = keep
		if len(keep) >= maxFailures {
			return true
		}
	}
	return false
}

func (s *Service) fail(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.failures[k] = append(s.failures[k], s.now())
	}
}

// CheckPassword verifies login and password (rate limited per IP and login).
func (s *Service) CheckPassword(ctx context.Context, login, pw, ip string) (User, error) {
	login = strings.TrimSpace(login)
	keys := []string{"ip:" + ip, "login:" + strings.ToLower(login)}
	if s.throttled(keys...) {
		return User{}, ErrThrottled
	}
	u, hash, err := s.st.UserByLogin(ctx, login)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return User{}, err
	}
	if !VerifyPassword(hash, pw) || u.Disabled {
		s.fail(keys...)
		return User{}, ErrBadLogin
	}
	return u, nil
}

// StartSession signs a user in and returns the cookie token. A fresh login
// also unlocks the configuration for a while.
func (s *Service) StartSession(ctx context.Context, u User, method, agent string) (string, error) {
	token, hash := NewToken()
	now := s.now()
	ss := Session{ID: hash, UserID: u.ID, Created: now, LastSeen: now, AuthAt: now, Method: method, Agent: shorten(agent, 200)}
	if u.Role == RoleAdmin {
		ss.ElevatedUntil = now.Add(ElevatedFor)
	}
	return token, s.st.CreateSession(ctx, ss)
}

// Resolve returns the principal of a session token.
func (s *Service) Resolve(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	ss, err := s.st.Session(ctx, HashToken(token))
	if err != nil {
		return nil, ErrNoSession
	}
	now := s.now()
	if now.Sub(ss.LastSeen) > SessionIdle {
		s.st.DeleteSession(ctx, ss.ID)
		return nil, ErrNoSession
	}
	u, err := s.st.User(ctx, ss.UserID)
	if err != nil || u.Disabled {
		return nil, ErrNoSession
	}
	if now.Sub(ss.LastSeen) > touchInterval {
		ss.LastSeen = now
		s.st.UpdateSession(ctx, ss)
	}
	groups, err := s.st.Groups(ctx)
	if err != nil {
		return nil, err
	}
	return NewPrincipal(u, ss, groups), nil
}

// Extend keeps the configuration unlocked while an administrator works
// (sliding, up to ElevatedMax after the last login).
func (s *Service) Extend(ctx context.Context, p *Principal) {
	now := s.now()
	if !p.Elevated(now) {
		return
	}
	until := now.Add(ElevatedFor)
	if limit := p.Session.AuthAt.Add(ElevatedMax); until.After(limit) {
		until = limit
	}
	if until.Sub(p.Session.ElevatedUntil) > time.Minute {
		p.Session.ElevatedUntil = until
		s.st.UpdateSession(ctx, p.Session)
	}
}

// Elevate unlocks the configuration after the user confirmed the login.
func (s *Service) Elevate(ctx context.Context, p *Principal, method string) error {
	now := s.now()
	p.Session.AuthAt, p.Session.Method = now, method
	if p.Admin() {
		p.Session.ElevatedUntil = now.Add(ElevatedFor)
	}
	return s.st.UpdateSession(ctx, p.Session)
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.st.DeleteSession(ctx, HashToken(token))
}

var loginRe = regexp.MustCompile(`^[\p{L}\p{N}._@-]{2,64}$`)

// SaveUser creates or updates a user. It keeps at least one active
// administrator.
func (s *Service) SaveUser(ctx context.Context, u User) (User, error) {
	u.Login, u.Name, u.Email = strings.TrimSpace(u.Login), strings.TrimSpace(u.Name), strings.TrimSpace(u.Email)
	if !loginRe.MatchString(u.Login) {
		return u, errors.New("Benutzername: 2–64 Zeichen, nur Buchstaben, Ziffern und . _ @ -")
	}
	if u.Role != RoleAdmin {
		u.Role = RoleUser
	}
	if u.Groups == nil {
		u.Groups = []string{}
	}
	if other, _, err := s.st.UserByLogin(ctx, u.Login); err == nil && other.ID != u.ID {
		return u, ErrLoginExists
	}
	if u.ID == "" {
		u.ID, u.Created = NewID(), s.now()
	} else {
		old, err := s.st.User(ctx, u.ID)
		if err != nil {
			return u, err
		}
		u.Created = old.Created
		if old.Role == RoleAdmin && !old.Disabled && (u.Role != RoleAdmin || u.Disabled) {
			if err := s.keepAdmin(ctx, u.ID); err != nil {
				return u, err
			}
		}
	}
	if err := s.st.SaveUser(ctx, u); err != nil {
		return u, err
	}
	if u.Disabled {
		s.st.DeleteSessions(ctx, u.ID)
	}
	return u, nil
}

// keepAdmin fails if id is the only active administrator.
func (s *Service) keepAdmin(ctx context.Context, id string) error {
	us, err := s.st.Users(ctx)
	if err != nil {
		return err
	}
	for _, o := range us {
		if o.ID != id && o.Role == RoleAdmin && !o.Disabled {
			return nil
		}
	}
	return ErrLastAdmin
}

// DeleteUser removes a user with all sessions and identities.
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	u, err := s.st.User(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == RoleAdmin {
		if err := s.keepAdmin(ctx, id); err != nil {
			return err
		}
	}
	return s.st.DeleteUser(ctx, id)
}

// SetPassword sets a new password and ends all other sessions of the user.
func (s *Service) SetPassword(ctx context.Context, userID, pw, keepSession string) error {
	if err := CheckPasswordRules(pw); err != nil {
		return err
	}
	if err := s.st.SetPassword(ctx, userID, HashPassword(pw)); err != nil {
		return err
	}
	ss, err := s.st.Sessions(ctx, userID)
	if err != nil {
		return err
	}
	for _, x := range ss {
		if x.ID != keepSession {
			s.st.DeleteSession(ctx, x.ID)
		}
	}
	return nil
}

// Invite returns a one-time link token with which the user sets a password
// (also used to reset a forgotten one).
func (s *Service) Invite(ctx context.Context, userID string) (string, time.Time, error) {
	if _, err := s.st.User(ctx, userID); err != nil {
		return "", time.Time{}, err
	}
	if err := s.st.DeleteInvites(ctx, userID); err != nil {
		return "", time.Time{}, err
	}
	token, hash := NewToken()
	exp := s.now().Add(InviteFor)
	return token, exp, s.st.CreateInvite(ctx, hash, userID, exp)
}

// InviteUser returns the user of a valid invite token.
func (s *Service) InviteUser(ctx context.Context, token string) (User, error) {
	id, exp, err := s.st.Invite(ctx, HashToken(token))
	if err != nil || s.now().After(exp) {
		return User{}, ErrInvite
	}
	u, err := s.st.User(ctx, id)
	if err != nil || u.Disabled {
		return User{}, ErrInvite
	}
	return u, nil
}

// AcceptInvite sets the password of the invited user, signs them in and
// invalidates the link.
func (s *Service) AcceptInvite(ctx context.Context, token, pw, agent string) (string, User, error) {
	u, err := s.InviteUser(ctx, token)
	if err != nil {
		return "", u, err
	}
	if err := s.SetPassword(ctx, u.ID, pw, ""); err != nil {
		return "", u, err
	}
	s.st.DeleteInvites(ctx, u.ID)
	u.HasPassword = true
	t, err := s.StartSession(ctx, u, "password", agent)
	return t, u, err
}

// CreateFirstAdmin creates the first administrator (only while there is
// no user).
func (s *Service) CreateFirstAdmin(ctx context.Context, login, name, pw string) (User, error) {
	has, err := s.HasUsers(ctx)
	if err != nil {
		return User{}, err
	}
	if has {
		return User{}, errors.New("Es gibt schon Benutzer.")
	}
	if err := CheckPasswordRules(pw); err != nil {
		return User{}, err
	}
	u, err := s.SaveUser(ctx, User{Login: login, Name: name, Role: RoleAdmin})
	if err != nil {
		return u, err
	}
	if err := s.st.SetPassword(ctx, u.ID, HashPassword(pw)); err != nil {
		return u, err
	}
	u.HasPassword = true
	return u, nil
}

var groupIDRe = regexp.MustCompile(`[^a-z0-9]+`)

// SaveGroup creates or updates a group.
func (s *Service) SaveGroup(ctx context.Context, g Group) (Group, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return g, errors.New("Die Gruppe braucht einen Namen.")
	}
	if g.ID == "" {
		base := strings.Trim(groupIDRe.ReplaceAllString(strings.ToLower(strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss").Replace(g.Name)), "-"), "-")
		if base == "" {
			base = "gruppe"
		}
		gs, err := s.st.Groups(ctx)
		if err != nil {
			return g, err
		}
		g.ID = base
		for n := 2; slicesContainsGroup(gs, g.ID); n++ {
			g.ID = fmt.Sprintf("%s-%d", base, n)
		}
	}
	if g.Loadpoints == nil {
		g.Loadpoints = []string{}
	}
	if g.Vehicles == nil {
		g.Vehicles = []string{}
	}
	return g, s.st.SaveGroup(ctx, g)
}

func slicesContainsGroup(gs []Group, id string) bool {
	for _, g := range gs {
		if g.ID == id {
			return true
		}
	}
	return false
}

func shorten(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CreateToken creates an access token for u named name. A write token of
// an administrator may change the configuration without re-authentication.
func (s *Service) CreateToken(ctx context.Context, u User, name string, write bool) (string, Session, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Session{}, errors.New("Bitte einen Namen für den Zugang angeben.")
	}
	token, _ := NewToken()
	token = TokenPrefix + token
	now := s.now()
	ss := Session{ID: HashToken(token), UserID: u.ID, Created: now, LastSeen: now, AuthAt: now, Method: MethodToken, Agent: shorten(name, 80)}
	if write {
		ss.Method = MethodTokenWrite
		if u.Role == RoleAdmin {
			ss.ElevatedUntil = now.AddDate(100, 0, 0)
		}
	}
	return token, ss, s.st.CreateSession(ctx, ss)
}
