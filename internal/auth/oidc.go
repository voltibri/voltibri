// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OpenID Connect, authorization code flow with PKCE. The ID token comes
// straight from the provider's token endpoint over TLS, which OpenID
// Connect Core 3.1.3.7 accepts instead of checking its signature; issuer,
// audience, expiry and nonce are checked.

// Flow purposes.
const (
	FlowLogin  = "login"  // sign in with a linked login
	FlowLink   = "link"   // link the login to the signed-in user
	FlowReauth = "reauth" // confirm the login for the configuration
)

// OIDCClaims are the identity a provider confirmed.
type OIDCClaims struct {
	Subject string
	Email   string
	Name    string
}

type discovery struct {
	Issuer   string `json:"issuer"`
	AuthURL  string `json:"authorization_endpoint"`
	TokenURL string `json:"token_endpoint"`
	fetched  time.Time
}

// PendingFlow is a started login (state -> flow), kept for 10 minutes.
type PendingFlow struct {
	Provider string
	Purpose  string
	UserID   string // link/reauth: the signed-in user
	Next     string // local page to return to
	nonce    string
	verifier string
	redirect string
	created  time.Time
}

// OIDC runs the login flows.
type OIDC struct {
	Client *http.Client

	mu    sync.Mutex
	disc  map[string]discovery
	flows map[string]PendingFlow
}

// NewOIDC creates the client.
func NewOIDC() *OIDC {
	return &OIDC{Client: &http.Client{Timeout: 15 * time.Second}, disc: map[string]discovery{}, flows: map[string]PendingFlow{}}
}

func (o *OIDC) discover(ctx context.Context, issuer string) (discovery, error) {
	issuer = strings.TrimRight(issuer, "/")
	o.mu.Lock()
	d, ok := o.disc[issuer]
	o.mu.Unlock()
	if ok && time.Since(d.fetched) < time.Hour {
		return d, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return d, err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return d, fmt.Errorf("Anmeldedienst nicht erreichbar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d, fmt.Errorf("Anmeldedienst: %s antwortet %s", issuer, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return d, fmt.Errorf("Anmeldedienst: ungültige Konfiguration: %w", err)
	}
	if strings.TrimRight(d.Issuer, "/") != issuer || d.AuthURL == "" || d.TokenURL == "" {
		return d, fmt.Errorf("Anmeldedienst: Aussteller %q passt nicht zu %q", d.Issuer, issuer)
	}
	d.fetched = time.Now()
	o.mu.Lock()
	o.disc[issuer] = d
	o.mu.Unlock()
	return d, nil
}

// Start returns the URL that sends the browser to the provider.
func (o *OIDC) Start(ctx context.Context, p Provider, f PendingFlow, redirectURI string) (string, error) {
	d, err := o.discover(ctx, p.Issuer)
	if err != nil {
		return "", err
	}
	state, _ := NewToken()
	nonce, _ := NewToken()
	verifier, _ := NewToken()
	f.Provider, f.nonce, f.verifier, f.redirect, f.created = p.ID, nonce, verifier, redirectURI, time.Now()
	o.mu.Lock()
	for k, v := range o.flows {
		if time.Since(v.created) > 10*time.Minute {
			delete(o.flows, k)
		}
	}
	o.flows[state] = f
	o.mu.Unlock()
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	if f.Purpose == FlowReauth {
		q.Set("prompt", "login") // really confirm, not a silent single sign-on
		q.Set("max_age", "0")
	}
	sep := "?"
	if strings.Contains(d.AuthURL, "?") {
		sep = "&"
	}
	return d.AuthURL + sep + q.Encode(), nil
}

// Flow returns and removes a started flow.
func (o *OIDC) Flow(state string) (PendingFlow, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f, ok := o.flows[state]
	delete(o.flows, state)
	if ok && time.Since(f.created) > 10*time.Minute {
		return f, false
	}
	return f, ok
}

// Finish exchanges the code and returns the confirmed identity.
func (o *OIDC) Finish(ctx context.Context, p Provider, f PendingFlow, code string) (OIDCClaims, error) {
	d, err := o.discover(ctx, p.Issuer)
	if err != nil {
		return OIDCClaims{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirect},
		"client_id": {p.ClientID}, "code_verifier": {f.verifier}}
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return OIDCClaims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("Anmeldedienst nicht erreichbar: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return OIDCClaims{}, fmt.Errorf("Anmeldedienst: ungültige Antwort (%s)", resp.Status)
	}
	if resp.StatusCode != http.StatusOK || tok.IDToken == "" {
		return OIDCClaims{}, fmt.Errorf("Anmeldedienst lehnt ab: %s %s", tok.Error, tok.Desc)
	}
	return checkIDToken(tok.IDToken, d.Issuer, p.ClientID, f.nonce, time.Now())
}

// checkIDToken reads the claims of an ID token received from the token
// endpoint and checks issuer, audience, expiry and nonce.
func checkIDToken(raw, issuer, clientID, nonce string, now time.Time) (OIDCClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return OIDCClaims{}, errors.New("ID-Token: ungültiges Format")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return OIDCClaims{}, errors.New("ID-Token: ungültige Kodierung")
	}
	var c struct {
		Iss      string          `json:"iss"`
		Sub      string          `json:"sub"`
		Aud      json.RawMessage `json:"aud"`
		Exp      float64         `json:"exp"`
		Nonce    string          `json:"nonce"`
		Email    string          `json:"email"`
		Name     string          `json:"name"`
		Username string          `json:"preferred_username"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return OIDCClaims{}, errors.New("ID-Token: ungültiger Inhalt")
	}
	var auds []string
	if json.Unmarshal(c.Aud, &auds) != nil {
		var one string
		json.Unmarshal(c.Aud, &one)
		auds = []string{one}
	}
	audOK := false
	for _, a := range auds {
		audOK = audOK || a == clientID
	}
	switch {
	case strings.TrimRight(c.Iss, "/") != strings.TrimRight(issuer, "/"):
		return OIDCClaims{}, errors.New("ID-Token: falscher Aussteller")
	case !audOK:
		return OIDCClaims{}, errors.New("ID-Token: nicht für voltibri ausgestellt")
	case now.After(time.Unix(int64(c.Exp), 0).Add(time.Minute)):
		return OIDCClaims{}, errors.New("ID-Token: abgelaufen")
	case c.Nonce != nonce:
		return OIDCClaims{}, errors.New("ID-Token: falsche Nonce")
	case c.Sub == "":
		return OIDCClaims{}, errors.New("ID-Token: ohne Kennung")
	}
	name := c.Name
	if name == "" {
		name = c.Username
	}
	return OIDCClaims{Subject: c.Sub, Email: c.Email, Name: name}, nil
}
