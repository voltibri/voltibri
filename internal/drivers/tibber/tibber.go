// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package tibber connects vehicles via the Tibber Data API and Tibber's
// end-customer prices via the Tibber price API.
//
// Vehicles: the car must be linked in the Tibber app (no Tibber electricity
// contract needed). Create an API client at
// https://data-api.tibber.com/clients/manage with the redirect URI shown in
// voltibri, then log in once from the voltibri web page (OAuth2 with PKCE).
// Tokens are kept in the device cache and refreshed automatically.
package tibber

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:         "tibber",
		Name:         "Tibber (Fahrzeuge)",
		Description:  "Fahrzeuge, die in der Tibber-App verbunden sind (Tibber Data API). API-Client unter data-api.tibber.com/clients/manage anlegen, danach in voltibri einmal bei Tibber anmelden.",
		Category:     sdk.CatVehicle,
		Capabilities: []sdk.Capability{sdk.CapVehicles, sdk.CapOAuth},
		SetupURL:     "https://data-api.tibber.com/clients/manage",
		Params: []sdk.Param{
			{Name: "clientId", Label: "Client ID", Type: sdk.TypeString, Required: true},
			{Name: "clientSecret", Label: "Client Secret", Type: sdk.TypeSecret, Required: true},
			{Name: "redirectUri", Label: "Redirect-URI", Type: sdk.TypeURL,
				Description: "Muss exakt so beim Tibber-Client hinterlegt sein. Leer: http://<voltibri>/api/oauth/callback"},
			{Name: "cache", Label: "Abfrageintervall", Type: sdk.TypeDuration, Default: "5m", Advanced: true},
			{Name: "authUrl", Label: "Auth-URL", Type: sdk.TypeURL, Default: authURL, Advanced: true},
			{Name: "tokenUrl", Label: "Token-URL", Type: sdk.TypeURL, Default: tokenURL, Advanced: true},
			{Name: "apiUrl", Label: "API-URL", Type: sdk.TypeURL, Default: apiURL, Advanced: true},
		},
		Factory: New,
	})
	sdk.RegisterDriver(sdk.Driver{
		Type:         "tibber-price",
		Name:         "Tibber (Strompreis)",
		Description:  "Endkundenpreise inkl. Steuern und Abgaben aus dem Tibber-Vertrag (persönlicher Token von developer.tibber.com). Als dynamischer Tarif ohne Aufschlag und Steuer verwenden.",
		Category:     sdk.CatTariff,
		Capabilities: []sdk.Capability{sdk.CapTariff},
		SetupURL:     "https://developer.tibber.com/settings/access-token",
		Params: []sdk.Param{
			{Name: "token", Label: "Persönlicher Token", Type: sdk.TypeSecret, Required: true},
			{Name: "homeId", Label: "Home-ID", Type: sdk.TypeString, Description: "Nur bei mehreren Zuhause nötig"},
			{Name: "url", Label: "API-URL", Type: sdk.TypeURL, Default: priceURL, Advanced: true},
		},
		Factory: NewPrice,
	})
}

const (
	authURL  = "https://thewall.tibber.com/connect/authorize"
	tokenURL = "https://thewall.tibber.com/connect/token"
	apiURL   = "https://data-api.tibber.com/v1"
	priceURL = "https://api.tibber.com/v1-beta/gql"
	scopes   = "openid profile email offline_access data-api-user-read data-api-vehicles-read"
)

var client = &http.Client{Timeout: 15 * time.Second}

// Device is a Tibber Data API account.
type Device struct {
	id                     string
	clientID, secret       string
	redirect               string
	authURL, tokenURL, api string
	ttl                    time.Duration
	cache                  sdk.Cache

	mu       sync.Mutex
	tok      token
	verifier map[string]string // state -> PKCE verifier
	homeOf   map[string]string // vehicle id -> home id
	vehicles []sdk.VehicleInfo
	listed   time.Time
	status   map[string]cached
}

type cached struct {
	st  sdk.VehicleStatus
	at  time.Time
	err error
}

type token struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh"`
	Expiry  time.Time `json:"expiry"`
}

// New creates the Tibber vehicle account.
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	id, err := cfg.String("clientId")
	if err != nil {
		return nil, err
	}
	sec, err := cfg.String("clientSecret")
	if err != nil {
		return nil, err
	}
	ttl, err := time.ParseDuration(cfg.StringOr("cache", "5m"))
	if err != nil {
		return nil, fmt.Errorf("%s: cache: %w", cfg.ID, err)
	}
	d := &Device{id: cfg.ID, clientID: id, secret: sec, redirect: cfg.StringOr("redirectUri", ""),
		authURL: cfg.StringOr("authUrl", authURL), tokenURL: cfg.StringOr("tokenUrl", tokenURL), api: strings.TrimRight(cfg.StringOr("apiUrl", apiURL), "/"),
		ttl: ttl, cache: cfg.Cache, verifier: map[string]string{}, homeOf: map[string]string{}, status: map[string]cached{}}
	if b, _, ok := cfg.Cache.Get("token"); ok {
		_ = json.Unmarshal(b, &d.tok)
	}
	return d, nil
}

func (d *Device) ID() string { return d.id }

// RedirectURI returns the configured redirect URI ("" = core default).
func (d *Device) RedirectURI() string { return d.redirect }

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL implements sdk.OAuthAuthorizer (PKCE S256).
func (d *Device) AuthURL(redirectURI string) (string, string, error) {
	if d.redirect != "" {
		redirectURI = d.redirect
	}
	state, verifier := random(16), random(32)
	sum := sha256.Sum256([]byte(verifier))
	d.mu.Lock()
	d.verifier[state] = verifier + " " + redirectURI
	d.mu.Unlock()
	q := url.Values{"response_type": {"code"}, "client_id": {d.clientID}, "redirect_uri": {redirectURI}, "scope": {scopes},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	return d.authURL + "?" + q.Encode(), state, nil
}

// Exchange implements sdk.OAuthAuthorizer.
func (d *Device) Exchange(ctx context.Context, code, state string) error {
	d.mu.Lock()
	v, ok := d.verifier[state]
	delete(d.verifier, state)
	d.mu.Unlock()
	if !ok {
		return errors.New("tibber: unknown or expired login (state), please start again")
	}
	verifier, redirect, _ := strings.Cut(v, " ")
	return d.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {verifier}})
}

// Authorized implements sdk.OAuthAuthorizer.
func (d *Device) Authorized() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tok.Refresh != "" || (d.tok.Access != "" && time.Now().Before(d.tok.Expiry))
}

func (d *Device) tokenRequest(ctx context.Context, form url.Values) error {
	form.Set("client_id", d.clientID)
	form.Set("client_secret", d.secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("tibber token: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	var r struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("tibber token: %w", err)
	}
	d.mu.Lock()
	d.tok.Access = r.Access
	if r.Refresh != "" {
		d.tok.Refresh = r.Refresh
	}
	d.tok.Expiry = time.Now().Add(time.Duration(r.Expires)*time.Second - time.Minute)
	b, _ := json.Marshal(d.tok)
	d.mu.Unlock()
	return d.cache.Put("token", b)
}

// accessToken returns a valid access token, refreshing it if needed.
func (d *Device) accessToken(ctx context.Context) (string, error) {
	d.mu.Lock()
	tok := d.tok
	d.mu.Unlock()
	if tok.Access != "" && time.Now().Before(tok.Expiry) {
		return tok.Access, nil
	}
	if tok.Refresh == "" {
		return "", errors.New("tibber: not logged in – log in once on the voltibri page")
	}
	if err := d.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}}); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tok.Access, nil
}

// get performs an authorised GET with retries on 429/5xx (Retry-After).
func (d *Device) get(ctx context.Context, path string, v any) error {
	for attempt := 0; ; attempt++ {
		at, err := d.accessToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.api+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+at)
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusOK:
			return json.Unmarshal(body, v)
		case res.StatusCode == http.StatusUnauthorized && attempt == 0:
			d.mu.Lock()
			d.tok.Expiry = time.Time{} // force refresh
			d.mu.Unlock()
			continue
		case (res.StatusCode == 429 || res.StatusCode >= 500) && attempt < 2:
			wait := time.Duration(1<<attempt) * time.Second
			if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s < 30 {
				wait = time.Duration(s) * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return fmt.Errorf("tibber %s: %s", path, res.Status)
	}
}

type apiDevice struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
	Info       struct {
		Name  string `json:"name"`
		Brand string `json:"brand"`
		Model string `json:"model"`
	} `json:"info"`
	Capabilities []struct {
		ID    string `json:"id"`
		Value any    `json:"value"`
		Unit  string `json:"unit"`
	} `json:"capabilities"`
}

// Vehicles implements sdk.VehicleProvider. Vehicles appear under every
// home; they are de-duplicated by id.
func (d *Device) Vehicles(ctx context.Context) ([]sdk.VehicleInfo, error) {
	d.mu.Lock()
	if time.Since(d.listed) < time.Hour && d.vehicles != nil {
		defer d.mu.Unlock()
		return d.vehicles, nil
	}
	d.mu.Unlock()
	var homes struct {
		Homes []struct {
			ID string `json:"id"`
		} `json:"homes"`
	}
	if err := d.get(ctx, "/homes", &homes); err != nil {
		return nil, err
	}
	out := []sdk.VehicleInfo{}
	homeOf := map[string]string{}
	for _, h := range homes.Homes {
		var devs struct {
			Devices []apiDevice `json:"devices"`
		}
		if err := d.get(ctx, "/homes/"+url.PathEscape(h.ID)+"/devices", &devs); err != nil {
			return nil, err
		}
		for _, v := range devs.Devices {
			if _, seen := homeOf[v.ID]; seen {
				continue
			}
			homeOf[v.ID] = h.ID
			vin := v.ExternalID
			if _, after, ok := strings.Cut(vin, ":"); ok {
				vin = after
			}
			out = append(out, sdk.VehicleInfo{ID: v.ID, Name: v.Info.Name, Brand: v.Info.Brand, Model: v.Info.Model, VIN: vin})
		}
	}
	d.mu.Lock()
	d.vehicles, d.homeOf, d.listed = out, homeOf, time.Now()
	d.mu.Unlock()
	return out, nil
}

// VehicleStatus implements sdk.VehicleProvider (cached for the configured interval).
func (d *Device) VehicleStatus(ctx context.Context, id string) (sdk.VehicleStatus, error) {
	d.mu.Lock()
	c, ok := d.status[id]
	home := d.homeOf[id]
	d.mu.Unlock()
	if ok && time.Since(c.at) < d.ttl {
		return c.st, c.err
	}
	if home == "" {
		if _, err := d.Vehicles(ctx); err != nil {
			return sdk.VehicleStatus{}, err
		}
		d.mu.Lock()
		home = d.homeOf[id]
		d.mu.Unlock()
		if home == "" {
			return sdk.VehicleStatus{}, fmt.Errorf("tibber: unknown vehicle %q", id)
		}
	}
	var v apiDevice
	err := d.get(ctx, "/homes/"+url.PathEscape(home)+"/devices/"+url.PathEscape(id), &v)
	st := parseStatus(v)
	d.mu.Lock()
	d.status[id] = cached{st: st, at: time.Now(), err: err}
	d.mu.Unlock()
	return st, err
}

func parseStatus(v apiDevice) sdk.VehicleStatus {
	st := sdk.VehicleStatus{SoC: math.NaN(), LimitSoC: math.NaN(), RangeKm: math.NaN(), Updated: time.Now()}
	for _, c := range v.Capabilities {
		f, isNum := number(c.Value)
		s, _ := c.Value.(string)
		switch c.ID {
		case "storage.stateOfCharge":
			if isNum {
				st.SoC = f / 100
			}
		case "storage.targetStateOfCharge":
			if isNum {
				st.LimitSoC = f / 100
			}
		case "range.remaining":
			if isNum {
				switch c.Unit {
				case "m":
					f /= 1000
				case "mi", "mile", "miles":
					f *= 1.609344
				}
				st.RangeKm = f
			}
		case "connector.status":
			st.Connected = s == "connected"
		case "charging.status":
			st.Charging = s == "charging"
		}
	}
	if st.Charging {
		st.Connected = true
	}
	return st
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// Price is the Tibber end-customer price source.
type Price struct {
	id, token, home, url string

	mu      sync.Mutex
	slots   []sdk.Slot
	fetched time.Time
}

// NewPrice creates the Tibber price source.
func NewPrice(cfg sdk.DeviceConfig) (sdk.Device, error) {
	tok, err := cfg.String("token")
	if err != nil {
		return nil, err
	}
	return &Price{id: cfg.ID, token: tok, home: cfg.StringOr("homeId", ""), url: cfg.StringOr("url", priceURL)}, nil
}

func (p *Price) ID() string { return p.id }

const priceQuery = `{ viewer { homes { id currentSubscription { priceInfo(resolution: QUARTER_HOURLY) { today { total startsAt } tomorrow { total startsAt } } } } } }`

// Prices implements sdk.TariffSource (ct/kWh incl. taxes). Prices are
// fetched at most every 30 minutes.
func (p *Price) Prices(ctx context.Context, from, to time.Time) ([]sdk.Slot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.fetched) > 30*time.Minute {
		slots, err := p.fetch(ctx)
		if err != nil && p.slots == nil {
			return nil, err
		}
		if err == nil {
			p.slots, p.fetched = slots, time.Now()
		}
	}
	var out []sdk.Slot
	for _, s := range p.slots {
		if s.Start.Add(s.Duration).After(from) && s.Start.Before(to) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (p *Price) fetch(ctx context.Context) ([]sdk.Slot, error) {
	body, _ := json.Marshal(map[string]string{"query": priceQuery})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tibber prices: %s", res.Status)
	}
	type pt struct {
		Total    float64   `json:"total"`
		StartsAt time.Time `json:"startsAt"`
	}
	var r struct {
		Data struct {
			Viewer struct {
				Homes []struct {
					ID                  string `json:"id"`
					CurrentSubscription *struct {
						PriceInfo struct {
							Today, Tomorrow []pt
						} `json:"priceInfo"`
					} `json:"currentSubscription"`
				} `json:"homes"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("tibber prices: %w", err)
	}
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("tibber prices: %s", r.Errors[0].Message)
	}
	for _, h := range r.Data.Viewer.Homes {
		if h.CurrentSubscription == nil || (p.home != "" && h.ID != p.home) {
			continue
		}
		all := append(h.CurrentSubscription.PriceInfo.Today, h.CurrentSubscription.PriceInfo.Tomorrow...)
		out := make([]sdk.Slot, 0, len(all))
		for i, x := range all {
			dur := 15 * time.Minute
			if i+1 < len(all) {
				dur = all[i+1].StartsAt.Sub(x.StartsAt)
			}
			out = append(out, sdk.Slot{Start: x.StartsAt, Duration: dur, Value: x.Total * 100})
		}
		return out, nil
	}
	return nil, errors.New("tibber prices: no home with an active subscription")
}
