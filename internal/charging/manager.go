// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package charging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/circuit"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/pricing"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/tariff"
	"github.com/voltibri/voltibri/sdk"
)

// Guest is the vehicle key of an unknown vehicle.
const Guest = "guest"

// DetectTimeout: how long voltibri waits for the vehicle cloud to report a
// plugged-in vehicle before treating it as a guest.
var DetectTimeout = 10 * time.Minute

// VehicleSettings are the effective settings of a vehicle. Mode and plans
// are the user's (stored in the database, changed from the web page without
// a config version); capacity, charger limit and the home battery rule come
// from the vehicle's configuration and are filled in by vehicleSettings.
type VehicleSettings struct {
	Name       string  `json:"name,omitempty"`       // display name override
	CapacityWh float64 `json:"capacityWh,omitempty"` // needed for SoC plans
	Mode       Mode    `json:"mode,omitempty"`       // default pv
	MinPowerW  float64 `json:"minPowerW,omitempty"`  // minpv minimum (0 = minimum current)
	MaxPowerW  float64 `json:"maxPowerW,omitempty"`  // onboard charger limit (0 = none)
	Plans      []Plan  `json:"plans,omitempty"`
	// NoBatteryModes: in these charge modes the home battery is held back
	// (the car charges from PV and grid only).
	NoBatteryModes []Mode `json:"noBatteryModes,omitempty"`
}

// Vehicle is a known vehicle with its state.
type Vehicle struct {
	Key       string          `json:"key"`                // asset id, or account/id for vehicles without an asset
	Asset     bool            `json:"asset"`              // configured as its own vehicle
	Provider  string          `json:"provider"`           // account the live data comes from ("" = manual)
	SourceID  string          `json:"sourceId,omitempty"` // vehicle id within the account
	Info      sdk.VehicleInfo `json:"info"`
	Name      string          `json:"name"`
	Settings  VehicleSettings `json:"settings"`
	SoC       *float64        `json:"soc,omitempty"`
	LimitSoC  *float64        `json:"limitSoc,omitempty"`
	RangeKm   *float64        `json:"rangeKm,omitempty"`
	Connected bool            `json:"connected"`
	Charging  bool            `json:"charging"`
	Updated   time.Time       `json:"updated,omitzero"`
	Error     string          `json:"error,omitempty"`
	NextPlan  *Plan           `json:"nextPlan,omitempty"`
	status    sdk.VehicleStatus
}

// MinSessionWh: sessions with less energy (plugged in, nothing charged)
// are not stored.
const MinSessionWh = 50.0

// Session is one charging session (plug-in to unplug): a run of kind
// "charging" of the loadpoint asset.
type Session = run.Run

// Loadpoint is the public view of a charge point.
type Loadpoint struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Owned           bool          `json:"owned"`          // voltibri may write to the charger
	CanSwitch       bool          `json:"canSwitch"`      // charger can switch between 1 and 3 phases
	Off             bool          `json:"off"`            // switched off by the user (any vehicle)
	Card            string        `json:"card,omitempty"` // charging card of this session
	SessionMode     bool          `json:"sessionMode"`    // mode chosen for this session (until unplugged)
	Phases          int           `json:"phases"`
	MinCurrentA     float64       `json:"minCurrent"`
	MaxCurrentA     float64       `json:"maxCurrent"`
	Guest           string        `json:"guestPolicy"`
	Separate        bool          `json:"separateMeter"`
	Connected       bool          `json:"connected"`
	Charging        bool          `json:"charging"`
	PowerW          float64       `json:"powerW"`
	Vehicle         string        `json:"vehicle"` // key, "guest" or ""
	VehicleName     string        `json:"vehicleName"`
	Manual          bool          `json:"manual"`               // vehicle chosen by the user
	ManualUntil     time.Time     `json:"manualUntil,omitzero"` // chosen before plugging in: plug in by then, else the choice ends
	Detecting       bool          `json:"detecting"`            // still waiting for the vehicle cloud
	Mode            Mode          `json:"mode"`
	Target          State         `json:"target"` // what voltibri wants
	Reason          string        `json:"reason"`
	Plan            *Schedule     `json:"plan,omitempty"`
	Session         *Session      `json:"session,omitempty"`
	PriceCt         *float64      `json:"priceCt,omitempty"`         // current price at this loadpoint
	Prices          *pricing.Unit `json:"prices,omitempty"`          // what a kWh costs the chosen vehicle now, per share (nil: no vehicle chosen yet)
	LimitA          *float64      `json:"limitA,omitempty"`          // load management: max current per phase right now
	LimitBy         string        `json:"limitBy,omitempty"`         // circuit that limits
	BatteryExcluded bool          `json:"batteryExcluded,omitempty"` // the home battery is held back for this charging
	Boosting        bool          `json:"boosting,omitempty"`        // the home battery adds power (boost)
	Priority        int           `json:"priority"`                  // surplus order: higher first (0-10)
	Top             bool          `json:"top,omitempty"`             // first before all others for today
	Foreign         bool          `json:"foreign,omitempty"`         // set by the API: another user's vehicle is connected (details hidden)
	ActivePhases    int           `json:"activePhases"`              // phases the car draws on right now, measured (0 = not charging or the charger does not measure)
	PhaseCurrentA   float64       `json:"phaseCurrentA"`             // current per active phase right now (with ActivePhases)
	CarPhases       int           `json:"carPhases,omitempty"`       // phases the car draws on as far as known (measured in this session or set at the vehicle; 0 = unknown)
	Finish          *Finish       `json:"finish,omitempty"`          // expected end of charging (own vehicle with state of charge)
}

// Finish is the expected end of a charging session: when the vehicle reaches
// its target at the current power. Basis says where the target comes from:
// "plan" (charge plan), "limit" (charge limit set in the car) or "full".
type Finish struct {
	At        time.Time `json:"at,omitzero"` // zero while not charging
	TargetSoC float64   `json:"targetSoc"`
	Basis     string    `json:"basis"`
	NeedWh    float64   `json:"needWh"`
	Reached   bool      `json:"reached,omitempty"`
}

// SiteInput is the site state for one control tick.
type SiteInput struct {
	Now             time.Time
	GridW, BatteryW float64
	PVW, LoadW      float64
	Chargers        map[string]sdk.ChargerStatus // corrected power
	BatteryPriority bool                         // PV goes to the home battery first
	Circuits        *circuit.Tree                // load management (nil: no fuses configured)
	// Alloc shares the surplus between all surplus loads (wallboxes and
	// heating rods) in priority order; nil: the loadpoints in configuration
	// order, each sees the whole surplus minus what the ones before took.
	Alloc Allocator
	// BoostW: power the home battery may add for boosted loads now (0 =
	// the battery is at its boost limit); AutoBoost: the battery's automatic
	// boost runs, every surplus mode gets the boost.
	BoostW    float64
	AutoBoost bool
	Priority  map[string]int  // priority per loadpoint (shown only)
	Top       map[string]bool // first before all others for today (shown only)
}

// Allocator shares the surplus between the surplus loads in priority order.
type Allocator interface {
	// Rank is the position of the loadpoint in the order (lower first).
	Rank(id string) int
	// Avail is the surplus for the loadpoint including what it draws now;
	// boost: the battery's boost power on top.
	Avail(id string, boost bool) float64
	// Take books what the loadpoint draws from now on.
	Take(id string, w float64, boost bool)
}

// Result tells the site about guest charging.
type Result struct {
	GuestW        float64 // guest charging power (kept out of the load forecast)
	ForceBatteryW float64 // > 0: charge the battery with this power (forceBattery policy)
	ExcludedW     float64 // charging power the home battery must not supply
}

type lpRuntime struct {
	card       string // charging card seen in this session
	view       Loadpoint
	cfg        config.Loadpoint
	charger    sdk.Charger
	ctl        sdk.ChargerController
	phaseSw    sdk.PhaseSwitcher
	state      State
	applied    *State // last state written to the charger
	plugged    time.Time
	session    *Session
	lastTick   time.Time
	manual     string
	manualTo   time.Time // chosen before plugging in: the choice ends then unless a vehicle is plugged in
	wasLocked  bool      // voltibri disabled the charger
	observedLg string
	ticked     bool
	carPhases  int       // phases the car drew on in this session (measured, 0 = not yet)
	released   int       // phases released by the last decision
	releasedAt time.Time // since then
}

// Manager runs all loadpoints.
type Manager struct {
	batteryNever map[string]bool // devices the home battery must not supply
	loc          *time.Location
	cfg          *config.Config
	providers    map[string]sdk.VehicleProvider // vehicle accounts
	own          []ownVehicle                   // vehicle assets
	alias        map[string]string              // former vehicle key -> asset id
	settings     sdk.Cache
	sessions     run.Store
	grid         tariff.Tariff
	feedin       tariff.Tariff
	chargingT    tariff.Tariff
	dynGrid      bool
	now          func() time.Time // clock for choices made by the user (tests)

	mu       sync.RWMutex
	lps      []*lpRuntime
	vehicles map[string]*Vehicle
	prices   map[string]priceCache
}

// ownVehicle is a vehicle asset (driver "vehicle").
type ownVehicle struct {
	dev config.Device
	p   sdk.VehicleProvider
}

type priceCache struct {
	slot time.Time
	v    float64
}

// New creates the manager. devices are all configured devices; ids lists
// the devices with a site role (grid, pv, battery) which are never
// loadpoints.
func New(cfg *config.Config, loc *time.Location, devices map[string]sdk.Device, settings sdk.Cache, sessions run.Store) (*Manager, error) {
	if settings == nil {
		settings = sdk.NewMemCache()
	}
	m := &Manager{loc: loc, cfg: cfg, providers: map[string]sdk.VehicleProvider{}, settings: settings, sessions: sessions, now: time.Now,
		vehicles: map[string]*Vehicle{}, prices: map[string]priceCache{}, batteryNever: map[string]bool{}, alias: map[string]string{}}
	for _, d := range cfg.Devices {
		if d.BatteryUse == config.BatteryNever {
			m.batteryNever[d.ID] = true
		}
	}
	var err error
	if m.grid, err = tariff.New(cfg.Tariffs.Grid, loc, devices); err != nil {
		return nil, fmt.Errorf("tariffs.grid: %w", err)
	}
	if m.feedin, err = tariff.New(cfg.Tariffs.FeedIn, loc, devices); err != nil {
		return nil, fmt.Errorf("tariffs.feedin: %w", err)
	}
	if m.chargingT, err = tariff.New(cfg.Tariffs.Charging, loc, devices); err != nil {
		return nil, fmt.Errorf("tariffs.charging: %w", err)
	}
	m.dynGrid = tariff.Dynamic(cfg.Tariffs.Grid)
	roles := map[string]bool{cfg.Roles.Grid: true, cfg.Roles.Battery: true}
	for _, id := range cfg.Roles.PV {
		roles[id] = true
	}
	for _, dc := range cfg.Devices {
		dev := devices[dc.ID]
		if vp, ok := dev.(sdk.VehicleProvider); ok {
			if dc.IsVehicle() {
				m.own = append(m.own, ownVehicle{dev: dc, p: vp})
				m.alias[dc.ID+"/"+dc.ID] = dc.ID // key of manual vehicles before they became assets
				if b := dc.Vehicle(); b.Source != "" {
					m.alias[b.Source+"/"+b.SourceVehicle] = dc.ID
				}
			} else {
				m.providers[dc.ID] = vp
			}
		}
		ch, ok := dev.(sdk.Charger)
		if !ok || roles[dc.ID] {
			continue
		}
		lc := cfg.LoadpointFor(dc.ID)
		lp := &lpRuntime{cfg: lc, charger: ch}
		owned := sdk.ControlMode(dc.Mode) == sdk.ModeOwned
		if ctl, ok := dev.(sdk.ChargerController); ok && owned {
			lp.ctl = ctl
		}
		if ps, ok := dev.(sdk.PhaseSwitcher); ok && owned {
			lp.phaseSw = ps
		}
		name := dc.Name
		if name == "" {
			name = dc.ID
		}
		lp.view = Loadpoint{ID: dc.ID, Name: name, Owned: lp.ctl != nil, CanSwitch: lp.phaseSw != nil, Phases: lc.Phases, MinCurrentA: lc.MinCurrentA,
			MaxCurrentA: lc.MaxCurrentA, Guest: lc.Guest, Separate: lc.SeparateMeter}
		lp.manual = m.loadString("lp:" + dc.ID)
		if t, err := time.Parse(time.RFC3339, m.loadString("lpUntil:"+dc.ID)); err == nil {
			lp.manualTo = t
		}
		m.lps = append(m.lps, lp)
	}
	for _, lp := range m.lps { // aliases are complete only now
		lp.manual = m.resolve(lp.manual)
	}
	return m, nil
}

func (m *Manager) loadString(key string) string {
	if m.settings == nil {
		return ""
	}
	b, _, ok := m.settings.Get(key)
	if !ok {
		return ""
	}
	return string(b)
}

func (m *Manager) vehicleSettings(key string) VehicleSettings {
	var s VehicleSettings
	if m.settings != nil {
		b, _, ok := m.settings.Get("vehicle:" + key)
		for old, k := range m.alias { // settings saved under a former key
			if !ok && k == key {
				b, _, ok = m.settings.Get("vehicle:" + old)
			}
		}
		if ok {
			_ = json.Unmarshal(b, &s)
		}
	}
	if s.Mode == "" {
		s.Mode = ModePV
	}
	// battery size, charger limit and the home battery rule are configuration
	// (vehicle asset), not settings; the Min+PV minimum is not adjustable yet
	s.CapacityWh, s.MaxPowerW, s.MinPowerW, s.NoBatteryModes = 0, 0, 0, nil
	if v, ok := m.vehicles[key]; ok {
		s.CapacityWh, s.MaxPowerW = v.Info.CapacityWh, v.Info.MaxPowerW
	}
	for _, a := range m.own {
		if a.dev.ID == key {
			for _, md := range a.dev.NoBatteryModes {
				s.NoBatteryModes = append(s.NoBatteryModes, Mode(md))
			}
		}
	}
	return s
}

// HasLoadpoints reports whether any charger exists.
func (m *Manager) HasLoadpoints() bool { return len(m.lps) > 0 }

// Run refreshes the vehicle list and states in the background (vehicle
// clouds are slow; the control loop only uses the latest known state).
func (m *Manager) Run(ctx context.Context) {
	if len(m.providers) == 0 && len(m.own) == 0 {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		m.refreshVehicles(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) refreshVehicles(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	m.mu.RLock()
	old := m.vehicles
	m.mu.RUnlock()
	// 1. vehicles of the accounts (Tibber, vendor clouds)
	acct := map[string]*Vehicle{}
	failed := map[string]string{}
	for pid, p := range m.providers {
		if a, ok := p.(sdk.OAuthAuthorizer); ok && !a.Authorized() {
			failed[pid] = "nicht angemeldet"
			continue
		}
		infos, err := p.Vehicles(ctx)
		if err != nil {
			slog.Warn("vehicles unavailable", "provider", pid, "err", err)
			failed[pid] = err.Error()
			for _, v := range old { // keep the last known ones
				if v.Provider == pid && v.SourceID != "" {
					c := *v
					acct[pid+"/"+v.SourceID] = &c
				}
			}
			continue
		}
		for _, info := range infos {
			key := pid + "/" + info.ID
			v := &Vehicle{Key: key, Provider: pid, SourceID: info.ID, Info: info}
			st, err := p.VehicleStatus(ctx, info.ID)
			if err != nil {
				v.Error = err.Error()
				if o, ok := old[m.resolve(key)]; ok {
					st = o.status
				}
			}
			v.status = st
			acct[key] = v
		}
	}
	found := map[string]*Vehicle{}
	// 2. vehicle assets; a bound account vehicle supplies the live data
	for _, a := range m.own {
		infos, _ := a.p.Vehicles(ctx)
		v := &Vehicle{Key: a.dev.ID, Asset: true, status: sdk.VehicleStatus{SoC: math.NaN(), LimitSoC: math.NaN(), RangeKm: math.NaN()}}
		if len(infos) > 0 {
			v.Info = infos[0]
		}
		if b := a.dev.Vehicle(); b.Source != "" {
			v.Provider, v.SourceID = b.Source, b.SourceVehicle
			if src, ok := acct[b.Source+"/"+b.SourceVehicle]; ok {
				delete(acct, b.Source+"/"+b.SourceVehicle)
				v.status, v.Error = src.status, src.Error
				v.Info.VIN = src.Info.VIN
				if v.Info.Brand == "" && v.Info.Model == "" {
					v.Info.Brand, v.Info.Model = src.Info.Brand, src.Info.Model
				}
			} else if f, ok := failed[b.Source]; ok {
				v.Error = "Datenquelle " + b.Source + ": " + f
			} else {
				v.Error = "Datenquelle " + b.Source + " kennt dieses Fahrzeug nicht (mehr)"
			}
		}
		found[v.Key] = v
	}
	// 3. account vehicles without an asset stay usable (automatic detection)
	for k, v := range acct {
		found[k] = v
	}
	m.mu.Lock()
	m.vehicles = found
	m.mu.Unlock()
}

// resolve maps a former vehicle key (account/id, or the key of a manual
// vehicle before vehicles became assets) to the key of its vehicle asset.
func (m *Manager) resolve(key string) string {
	if k, ok := m.alias[key]; ok {
		return k
	}
	return key
}

func optional(f float64) *float64 {
	if math.IsNaN(f) {
		return nil
	}
	return &f
}

// Vehicles returns all known vehicles with settings, sorted by name.
func (m *Manager) Vehicles() []Vehicle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	out := make([]Vehicle, 0, len(m.vehicles))
	for _, v := range m.vehicles {
		c := *v
		c.Settings = m.vehicleSettings(v.Key)
		c.Name = displayName(c)
		c.SoC, c.LimitSoC, c.RangeKm = optional(v.status.SoC), optional(v.status.LimitSoC), optional(v.status.RangeKm)
		c.Connected, c.Charging, c.Updated = v.status.Connected, v.status.Charging, v.status.Updated
		if p, t := nextPlan(c.Settings.Plans, now, m.loc); !t.IsZero() {
			c.NextPlan = &p
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func displayName(v Vehicle) string {
	switch {
	case v.Settings.Name != "":
		return v.Settings.Name
	case v.Info.Name != "":
		return v.Info.Name
	case v.Info.Brand != "":
		return strings.TrimSpace(v.Info.Brand + " " + v.Info.Model)
	}
	return v.Info.ID
}

func nextPlan(plans []Plan, now time.Time, loc *time.Location) (Plan, time.Time) {
	var best Plan
	var bt time.Time
	for _, p := range plans {
		if t := p.Next(now, loc); !t.IsZero() && (bt.IsZero() || t.Before(bt)) {
			best, bt = p, t
		}
	}
	return best, bt
}

// Loadpoints returns the current loadpoint views.
func (m *Manager) Loadpoints() []Loadpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Loadpoint, len(m.lps))
	for i, lp := range m.lps {
		out[i] = lp.view
		if lp.session != nil {
			s := *lp.session
			out[i].Session = &s
		}
	}
	return out
}

// SetVehicleSettings stores the settings of a vehicle.
func (m *Manager) SetVehicleSettings(key string, s VehicleSettings) error {
	key = m.resolve(key)
	m.mu.RLock()
	_, ok := m.vehicles[key]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown vehicle %q", key)
	}
	if s.Mode != "" && !s.Mode.Valid() {
		return fmt.Errorf("unknown mode %q", s.Mode)
	}
	// configuration, not settings (see vehicleSettings)
	s.CapacityWh, s.MaxPowerW, s.MinPowerW, s.NoBatteryModes = 0, 0, 0, nil
	for i := range s.Plans {
		if s.Plans[i].ID == "" {
			s.Plans[i].ID = fmt.Sprintf("p%d", time.Now().UnixNano()+int64(i))
		}
		if err := s.Plans[i].Validate(); err != nil {
			return fmt.Errorf("plan %d: %w", i+1, err)
		}
	}
	b, _ := json.Marshal(s)
	return m.settings.Put("vehicle:"+key, b)
}

// PreselectTimeout: a vehicle chosen before plugging in must be plugged in
// within this time, else the choice ends and the wallbox is locked again
// (guest settings).
const PreselectTimeout = 10 * time.Minute

// SetLoadpointVehicle assigns a vehicle manually ("" = automatic, "guest").
// The assignment ends when the vehicle is unplugged; chosen before plugging
// in, it also ends when nothing is plugged in within PreselectTimeout.
func (m *Manager) SetLoadpointVehicle(id, key string) error {
	key = m.resolve(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	if key != "" && key != Guest {
		if _, ok := m.vehicles[key]; !ok {
			return fmt.Errorf("unknown vehicle %q", key)
		}
	}
	for _, lp := range m.lps {
		if lp.view.ID == id {
			var until time.Time
			if key != "" && !lp.view.Connected {
				until = m.now().Add(PreselectTimeout)
			}
			m.setManual(lp, key, until)
			m.preselectView(lp, m.now())
			return nil
		}
	}
	return fmt.Errorf("unknown loadpoint %q", id)
}

// setManual stores the manual vehicle choice of a loadpoint and its deadline
// (zero: until unplugged).
func (m *Manager) setManual(lp *lpRuntime, key string, until time.Time) {
	lp.manual, lp.manualTo = key, until
	_ = m.settings.Put("lp:"+lp.view.ID, []byte(key))
	var b []byte
	if !until.IsZero() {
		b = []byte(until.UTC().Format(time.RFC3339))
	}
	_ = m.settings.Put("lpUntil:"+lp.view.ID, b)
}

// preselectView shows a vehicle chosen before plugging in at a free
// loadpoint: who may plug in and until when.
func (m *Manager) preselectView(lp *lpRuntime, now time.Time) {
	v := &lp.view
	v.Manual = lp.manual != ""
	v.ManualUntil = time.Time{}
	if v.Connected {
		return
	}
	v.Vehicle, v.VehicleName = "", ""
	if lp.manual == "" || lp.manualTo.IsZero() {
		return
	}
	v.Vehicle, v.ManualUntil = lp.manual, lp.manualTo
	v.VehicleName = "Gastfahrzeug"
	if veh, ok := m.vehicles[lp.manual]; ok {
		v.VehicleName = displayName(Vehicle{Info: veh.Info, Settings: m.vehicleSettings(lp.manual)})
	}
	left := lp.manualTo.Sub(now)
	v.Reason = fmt.Sprintf("%s gewählt – bis %s einstecken (noch %d min), sonst wird die Wallbox wieder gesperrt",
		v.VehicleName, lp.manualTo.In(m.loc).Format("15:04"), int(math.Ceil(math.Max(0, left.Minutes()))))
}

// SetSessionMode sets the charge mode of the current session (until the car
// is unplugged); it wins over the vehicle's default mode.
func (m *Manager) SetSessionMode(id string, mode Mode) error { return m.SetGuestMode(id, mode) }

// SetGuestMode changes the charge mode used for guests at a loadpoint.
func (m *Manager) SetGuestMode(id string, mode Mode) error {
	if !mode.Valid() {
		return fmt.Errorf("unknown mode %q", mode)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lp := range m.lps {
		if lp.view.ID == id {
			return m.settings.Put("mode:"+id, []byte(mode))
		}
	}
	return fmt.Errorf("unknown loadpoint %q", id)
}

// Sessions returns the latest sessions (open ones first).
func (m *Manager) Sessions(ctx context.Context, limit int) ([]Session, error) {
	var out []Session
	for _, lp := range m.Loadpoints() {
		if lp.Session != nil {
			out = append(out, *lp.Session)
		}
	}
	if m.sessions == nil {
		return out, nil
	}
	done, err := m.sessions.Runs(ctx, run.Filter{Kind: config.RunCharging, Limit: limit})
	return append(out, done...), err
}

// price returns the current price of a tariff (cached per slot).
func (m *Manager) price(ctx context.Context, name string, t tariff.Tariff, now time.Time) float64 {
	if t == nil {
		return math.NaN()
	}
	slot := now.Truncate(tariff.Slot)
	if c, ok := m.prices[name]; ok && c.slot.Equal(slot) {
		return c.v
	}
	v := math.NaN()
	if ps, err := t.Prices(ctx, slot, slot.Add(tariff.Slot)); err == nil && len(ps) > 0 {
		v = ps[0]
	}
	m.prices[name] = priceCache{slot: slot, v: v}
	return v
}

// detect picks the vehicle at a loadpoint: manual choice, else the only
// vehicle the clouds report as plugged in (and not at another loadpoint).
func (m *Manager) detect(lp *lpRuntime, now time.Time, taken map[string]bool) (key string, detecting bool) {
	if lp.manual != "" {
		return lp.manual, false
	}
	var cands []string
	for k, v := range m.vehicles {
		if v.status.Connected && !taken[k] {
			cands = append(cands, k)
		}
	}
	if len(cands) == 1 {
		return cands[0], false
	}
	if len(m.providers) > 0 && now.Sub(lp.plugged) < DetectTimeout {
		return Guest, true
	}
	return Guest, false
}

// Update runs one control tick for all loadpoints.
func (m *Manager) Update(ctx context.Context, in SiteInput) Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res Result
	gridPrice := m.price(ctx, "grid", m.grid, in.Now)
	feedPrice := m.price(ctx, "feedin", m.feedin, in.Now)
	chargePrice := m.price(ctx, "charging", m.chargingT, in.Now)
	remaining := math.Inf(1) // surplus not yet given to another loadpoint
	taken := map[string]bool{}
	for _, lp := range m.lps {
		if lp.view.Connected && lp.view.Vehicle != "" && lp.view.Vehicle != Guest {
			taken[lp.view.Vehicle] = true
		}
	}
	lps := m.lps
	if in.Alloc != nil {
		lps = append([]*lpRuntime(nil), m.lps...)
		sort.SliceStable(lps, func(i, j int) bool { return in.Alloc.Rank(lps[i].view.ID) < in.Alloc.Rank(lps[j].view.ID) })
	}
	for _, lp := range lps {
		st := in.Chargers[lp.view.ID]
		v := &lp.view
		v.Priority, v.Top = in.Priority[v.ID], in.Top[v.ID]
		wasConnected := v.Connected
		v.Connected, v.Charging, v.PowerW = st.Connected, st.Charging, st.PowerW
		if !lp.ticked { // first tick: continue a session interrupted by a restart
			lp.ticked = true
			// read only now, not in New: on a configuration change the new
			// manager is built while the old one still runs and keeps its
			// open session only when it stops
			if r := m.openSession(v.ID); r != nil {
				_ = m.settings.Put("open:"+v.ID, []byte{})
				if v.Connected && in.Now.Sub(r.Start) < 48*time.Hour {
					lp.session, lp.plugged, wasConnected = r, r.Start, true
					lp.lastTick = in.Now
				} else if m.sessions != nil && r.EnergyWh >= MinSessionWh {
					r.End = in.Now
					if _, err := m.sessions.SaveRun(ctx, *r); err != nil {
						slog.Error("saving charging session failed", "err", err)
					}
				}
			}
		}
		if v.Connected && !wasConnected {
			lp.plugged = in.Now
			if !lp.manualTo.IsZero() { // plugged in in time: the choice holds until unplugged
				m.setManual(lp, lp.manual, time.Time{})
			}
		}
		if !v.Connected && wasConnected {
			m.endSession(ctx, lp, in.Now)
			m.setManual(lp, "", time.Time{})
			_ = m.settings.Put("mode:"+v.ID, []byte{})
			lp.card = ""
		}
		if !v.Connected && lp.manual != "" {
			switch {
			case lp.manualTo.IsZero(): // chosen before this rule existed: give it the full time
				m.setManual(lp, lp.manual, in.Now.Add(PreselectTimeout))
			case !in.Now.Before(lp.manualTo):
				slog.Info("vehicle chosen but not plugged in, choice ended", "loadpoint", v.ID, "vehicle", lp.manual)
				m.setManual(lp, "", time.Time{})
			}
		}

		// vehicle and mode
		if wasConnected { // only a plugged-in vehicle is taken
			delete(taken, v.Vehicle)
		}
		v.Vehicle, v.Detecting, v.Manual, v.ManualUntil = "", false, lp.manual != "", time.Time{}
		vs := VehicleSettings{}
		// a card held to the wallbox chooses vehicle and mode for this session
		if v.Connected && st.Card != "" && lp.card != st.Card {
			lp.card = st.Card
			if c, ok := m.cardFor(st.Card); ok {
				if c.Vehicle != "" {
					lp.manual = c.Vehicle
					_ = m.settings.Put("lp:"+v.ID, []byte(c.Vehicle))
				}
				if Mode(c.Mode).Valid() {
					_ = m.settings.Put("mode:"+v.ID, []byte(c.Mode))
				}
				slog.Info("charging card", "loadpoint", v.ID, "card", st.Card, "vehicle", c.Vehicle, "mode", c.Mode)
			}
		}
		v.Card = lp.card
		v.Mode = Mode(lp.cfg.GuestMode) // guests: default of the wallbox
		if v.Connected {
			v.Vehicle, v.Detecting = m.detect(lp, in.Now, taken)
			taken[v.Vehicle] = v.Vehicle != Guest
			if veh, ok := m.vehicles[v.Vehicle]; ok {
				vs = m.vehicleSettings(v.Vehicle)
				v.Mode = vs.Mode
				v.VehicleName = displayName(Vehicle{Info: veh.Info, Settings: vs})
			} else {
				v.VehicleName = "Gastfahrzeug"
				if v.Detecting {
					v.VehicleName = "Fahrzeug wird erkannt …"
				}
			}
		} else {
			v.VehicleName = ""
		}
		// mode chosen for this session (wallbox buttons or card), until unplugged
		v.SessionMode = false
		if sm := Mode(m.loadString("mode:" + v.ID)); sm.Valid() && v.Connected {
			v.Mode, v.SessionMode = sm, true
		}
		// the wallbox itself switched off: wins over every vehicle mode
		v.Off = m.loadString("off:"+v.ID) == "1"
		if v.Off {
			v.Mode = ModeOff
		}
		guest := v.Connected && v.Vehicle == Guest
		carPhases := m.carPhases(lp, st, v.Vehicle, in.Now)
		v.CarPhases = carPhases

		// session bookkeeping
		if v.Connected && lp.session == nil {
			lp.session = &Session{Asset: v.ID, Kind: config.RunCharging, Start: in.Now}
		}
		if s := lp.session; s != nil {
			s.Vehicle, s.VehicleName, s.Guest, s.Mode = v.Vehicle, v.VehicleName, guest, string(v.Mode)
			if !lp.lastTick.IsZero() {
				dt := math.Min(in.Now.Sub(lp.lastTick).Hours(), 1.0/60)
				e := v.PowerW * dt
				// grid first, then the discharging home battery, the rest is PV
				fromGrid := math.Min(v.PowerW, math.Max(0, in.GridW)) * dt
				fromBattery := math.Min(e-fromGrid, math.Max(0, in.BatteryW)*dt)
				s.EnergyWh += e
				s.GridWh += fromGrid
				s.BatteryWh += fromBattery
				s.PVWh += e - fromGrid - fromBattery
				// the shares at both tariffs, to price them by the vehicle rule
				// now and to recalculate later
				sh, _ := pricing.FromAttrs(s.Attrs)
				sh.PV.Add(e-fromGrid-fromBattery, feedPrice, gridPrice)
				sh.Battery.Add(fromBattery, feedPrice, gridPrice)
				sh.Grid.Add(fromGrid, feedPrice, gridPrice)
				if s.Attrs == nil {
					s.Attrs = map[string]any{}
				}
				s.Attrs[pricing.AttrKey] = sh
				s.PriceNote = priceNote(lp.cfg.SeparateMeter, guest)
				switch {
				case lp.cfg.SeparateMeter:
					s.CostCt += nz(chargePrice) * e / 1000
				case guest:
					s.CostCt += nz(gridPrice) * e / 1000 // guests pay grid price for everything
				default:
					// PV and battery at the lost feed-in tariff, grid at the
					// tariff – unless the vehicle has its own price rule
					rule, name := m.priceRule(v.Vehicle)
					s.CostCt, _ = rule.Cost(sh, "")
					if !rule.Empty() {
						_, s.PriceNote = rule.Cost(sh, name)
					}
				}
			}
		}
		lp.lastTick = in.Now
		if lp.cfg.SeparateMeter {
			v.PriceCt = optional(chargePrice)
		} else {
			v.PriceCt = optional(gridPrice)
		}

		// plan
		v.Plan = nil
		planSlot := false
		if veh, ok := m.vehicles[v.Vehicle]; ok && v.Connected && v.Mode.Surplus() {
			if p, target := nextPlan(vs.Plans, in.Now, m.loc); !target.IsZero() {
				powerW := lp.cfg.MaxCurrentA * Voltage * float64(DrawPhases(lp.cfg.Phases, carPhases))
				if vs.MaxPowerW > 0 {
					powerW = math.Min(powerW, vs.MaxPowerW)
				}
				need := NeedWh(veh.status.SoC, p.SoC, vs.CapacityWh)
				var sch Schedule
				if math.IsNaN(need) {
					sch = Schedule{Target: target, SoC: p.SoC, Reason: "Ladestand oder Akkukapazität unbekannt – Plan kann nicht berechnet werden"}
				} else {
					var prices []float64
					t := m.grid
					if lp.cfg.SeparateMeter {
						t = m.chargingT
					}
					if t != nil {
						prices, _ = t.Prices(ctx, in.Now, target)
					}
					sch = MakeSchedule(in.Now, target, need, powerW, prices) // need is grid energy, powerW grid power
					sch.SoC = p.SoC
				}
				v.Plan = &sch
				planSlot = sch.Active(in.Now)
			}
		}

		// guest policies
		mode := v.Mode
		prio := in.BatteryPriority
		if guest {
			switch lp.cfg.Guest {
			case config.GuestBatteryFirst:
				prio = true
				if mode == ModeFast {
					mode = ModeMinPV
				}
			case config.GuestForceBattery:
				if v.Charging {
					res.ForceBatteryW = math.Max(res.ForceBatteryW, in.PVW-(in.LoadW-v.PowerW))
				}
			}
			res.GuestW += v.PowerW
		}

		// home battery held back? (device setting, guests, vehicle per mode)
		excl := m.batteryNever[v.ID] || (guest && lp.cfg.GuestNoBattery) || (!guest && hasMode(vs.NoBatteryModes, v.Mode))
		v.BatteryExcluded = excl && v.Connected
		if v.BatteryExcluded {
			res.ExcludedW += v.PowerW
		}

		// boost: by hand (mode) or the battery's automatic boost
		wantBoost := mode == ModeBoost || (in.AutoBoost && mode.Surplus())
		boosted := wantBoost && in.BoostW > 0 && !v.BatteryExcluded
		v.Boosting = boosted && v.Connected
		var surplus float64
		if in.Alloc != nil {
			surplus = in.Alloc.Avail(v.ID, boosted)
			if prio && !in.BatteryPriority { // the battery's charging power stays with it
				surplus -= math.Max(0, -in.BatteryW)
			}
		} else {
			surplus = Surplus(in.GridW, in.BatteryW, v.PowerW, prio && !boosted)
			if boosted {
				surplus += in.BoostW
			}
			surplus = math.Min(surplus, remaining)
		}
		// load management: the circuits (sub-distribution, house fuse) cap the current
		maxA, own := lp.cfg.MaxCurrentA, circuit.Phases{}
		v.LimitA, v.LimitBy = nil, ""
		if in.Circuits != nil {
			var ok bool
			if own, ok = in.Circuits.Draw(v.ID); !ok {
				own = circuit.Spread(v.PowerW/Voltage/float64(lp.cfg.Phases), lp.cfg.Phases)
			}
			released := lp.state.Phases
			if released == 0 {
				released = lp.cfg.Phases
			}
			if a, by := in.Circuits.Headroom(v.ID, own, DrawPhases(released, carPhases)); a < maxA {
				maxA, v.LimitBy = math.Floor(a), by
				v.LimitA = &maxA
			}
		}
		d := Decide(Input{Now: in.Now, Mode: mode, PlanSlot: planSlot, Connected: v.Connected, SurplusW: surplus,
			MinA: lp.cfg.MinCurrentA, MaxA: math.Max(maxA, lp.cfg.MinCurrentA), Phases: lp.cfg.Phases, CarPhases: carPhases, CanSwitch: lp.phaseSw != nil,
			MinPowerW: vs.MinPowerW, MaxPowerW: vs.MaxPowerW, Prev: lp.state})
		if guest && lp.cfg.Guest == config.GuestBatteryFirst && v.Mode == ModeFast {
			d.Reason = "Gast: Batterie hat Vorrang – " + d.Reason
		}
		switch {
		case boosted && d.Enabled && !planSlot:
			d.Reason += fmt.Sprintf(" · Boost: Hausbatterie gibt bis zu %.1f kW dazu", in.BoostW/1000)
		case mode == ModeBoost && v.BatteryExcluded:
			d.Reason = "Boost ohne Hausbatterie (für dieses Fahrzeug ausgeschlossen) – " + d.Reason
		case mode == ModeBoost && in.BoostW <= 0:
			d.Reason = "Boost: Hausbatterie ist an ihrer Boost-Grenze – " + d.Reason
		}
		if v.Detecting {
			d.Reason = "Fahrzeug wird erkannt (bis zu 10 min), solange gelten die Gast-Einstellungen – " + d.Reason
		}
		if v.LimitBy != "" && mode != ModeExternal && d.Enabled {
			if maxA < lp.cfg.MinCurrentA {
				d.Enabled, d.CurrentA = false, 0
				d.Reason = fmt.Sprintf("Lastmanagement: %s ist ausgelastet (frei %.0f A, Minimum %.0f A) – Laden pausiert", v.LimitBy, maxA, lp.cfg.MinCurrentA)
			} else if d.CurrentA >= maxA {
				d.Reason += fmt.Sprintf(" · Lastmanagement: höchstens %.0f A (%s)", maxA, v.LimitBy)
			}
		}
		if mode == ModeOff { // say why it is off and how to start
			d.Reason = offReason(v.Off, v.SessionMode, guest, v.Connected)
		}
		lp.state = d.State
		if d.Phases != lp.released {
			lp.released, lp.releasedAt = d.Phases, in.Now
		}
		v.Target, v.Reason = d.State, d.Reason
		if !v.Connected { // a vehicle chosen before plugging in
			why := v.Reason
			m.preselectView(lp, in.Now)
			if v.Off {
				v.Reason = why
			}
		}
		v.ActivePhases, v.PhaseCurrentA, v.Finish = livePhases(st, v.Charging), 0, nil
		if v.ActivePhases > 0 {
			v.PhaseCurrentA = v.PowerW / Voltage / float64(v.ActivePhases)
		}
		if veh, ok := m.vehicles[v.Vehicle]; ok && v.Connected {
			v.Finish = finish(veh.status, vs.CapacityWh, v.Plan, v.Charging, v.PowerW, in.Now)
		}
		v.Prices = m.unitPrices(lp, v, feedPrice, gridPrice, chargePrice)
		if in.Circuits != nil {
			next := own // observed or external: the charger keeps drawing what it draws
			if lp.ctl != nil && mode != ModeExternal {
				next = circuit.Phases{}
				if d.Enabled && v.Connected {
					next = circuit.Spread(d.CurrentA, d.Phases)
				}
			}
			in.Circuits.Commit(v.ID, own, next)
		}
		switch {
		case in.Alloc != nil: // what it draws from now on
			w := v.PowerW // external, observed: keeps drawing what it draws
			if lp.ctl != nil && mode != ModeExternal {
				w = 0
				if d.Enabled && v.Connected {
					// over the phases the car draws on: a 2-phase car at a
					// 3-phase wallbox takes two thirds
					ph := float64(DrawPhases(d.Phases, carPhases))
					w = d.CurrentA * Voltage * ph
					// a car that takes clearly less (its own limit) leaves the
					// rest to the next loads; one amp more to ramp up
					if v.Charging && v.PowerW > 0 && v.PowerW < 0.8*w {
						w = v.PowerW + Voltage*ph
					}
				}
			}
			in.Alloc.Take(v.ID, w, boosted)
		case mode != ModeExternal && d.Enabled:
			remaining = math.Max(0, surplus-d.CurrentA*Voltage*float64(DrawPhases(d.Phases, carPhases)))
		case mode != ModeExternal:
			remaining = math.Max(0, surplus)
		}
		m.apply(ctx, lp, mode, d)
	}
	return res
}

func nz(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

// apply writes the decision to the charger (owned chargers only).
func (m *Manager) apply(ctx context.Context, lp *lpRuntime, mode Mode, d Decision) {
	if lp.ctl == nil {
		msg := fmt.Sprintf("%v %.0fA %dp", d.Enabled, d.CurrentA, d.Phases)
		if mode != ModeExternal && lp.view.Connected && msg != lp.observedLg {
			slog.Info("loadpoint observed only, not controlling", "loadpoint", lp.view.ID, "would", d.Reason)
			lp.observedLg = msg
		}
		return
	}
	if mode == ModeExternal {
		if lp.wasLocked { // hand back our own lock once, then stay out
			if err := lp.ctl.EnableCharging(ctx, true); err != nil {
				slog.Error("releasing charger failed", "loadpoint", lp.view.ID, "err", err)
				return
			}
			lp.wasLocked = false
		}
		lp.applied = nil
		return
	}
	a := lp.applied
	if lp.phaseSw != nil && d.Phases != 0 && (a == nil || a.Phases != d.Phases) {
		if err := lp.phaseSw.SetPhases(ctx, d.Phases); err != nil {
			slog.Error("switching phases failed", "loadpoint", lp.view.ID, "err", err)
		}
	}
	if d.Enabled && (a == nil || math.Abs(a.CurrentA-d.CurrentA) >= 1) {
		if err := lp.ctl.SetCurrent(ctx, d.CurrentA); err != nil {
			slog.Error("setting current failed", "loadpoint", lp.view.ID, "err", err)
			return
		}
	}
	if a == nil || a.Enabled != d.Enabled {
		if err := lp.ctl.EnableCharging(ctx, d.Enabled); err != nil {
			slog.Error("enabling charger failed", "loadpoint", lp.view.ID, "err", err)
			return
		}
		lp.wasLocked = !d.Enabled
	}
	s := d.State
	lp.applied = &s
}

func (m *Manager) endSession(ctx context.Context, lp *lpRuntime, now time.Time) {
	s := lp.session
	lp.session, lp.lastTick, lp.state, lp.applied = nil, time.Time{}, State{}, nil
	lp.carPhases, lp.released, lp.releasedAt = 0, 0, time.Time{}
	if s == nil || m.sessions == nil {
		return
	}
	s.End = now
	if s.EnergyWh < MinSessionWh {
		return // plugged in without charging (or a plug bounce): nothing to report
	}
	if _, err := m.sessions.SaveRun(ctx, *s); err != nil {
		slog.Error("saving charging session failed", "err", err)
	}
}

// closeSessions keeps open sessions on shutdown (e.g. a configuration
// change restarts the site); they are continued or finished on the next
// start.
func (m *Manager) closeSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lp := range m.lps {
		if lp.session != nil && m.settings != nil {
			b, _ := json.Marshal(lp.session)
			_ = m.settings.Put("open:"+lp.view.ID, b)
		}
	}
}

// unitPrices are what a kWh costs the vehicle at the loadpoint now, per
// share, as its session is priced: behind a separate meter the charging
// tariff, guests the grid price for everything, own vehicles by their price
// rule. nil while no vehicle is chosen (or still being detected) or the
// tariff is unknown.
func (m *Manager) unitPrices(lp *lpRuntime, v *Loadpoint, feedCt, gridCt, chargeCt float64) *pricing.Unit {
	if v.Vehicle == "" || v.Detecting {
		return nil
	}
	all := func(ct float64) *pricing.Unit {
		if math.IsNaN(ct) {
			return nil
		}
		return &pricing.Unit{GridCt: ct, BatteryCt: ct, PVCt: ct}
	}
	switch {
	case lp.cfg.SeparateMeter:
		return all(chargeCt)
	case v.Vehicle == Guest:
		return all(gridCt)
	case math.IsNaN(gridCt) || math.IsNaN(feedCt):
		return nil
	}
	rule, _ := m.priceRule(v.Vehicle)
	u := rule.Unit(feedCt, gridCt)
	return &u
}

// openSession is the session left open by the previous manager (restart
// or configuration change), nil if none.
func (m *Manager) openSession(id string) *Session {
	if m.settings == nil {
		return nil
	}
	b, _, ok := m.settings.Get("open:" + id)
	if !ok || len(b) == 0 {
		return nil
	}
	var s Session
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// Close must be called when the site stops.
func (m *Manager) Close() { m.closeSessions() }

// Provider describes a vehicle account for the web page.
type Provider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`       // driver type
	NeedsLogin bool   `json:"needsLogin"` // OAuth login in the browser
	Authorized bool   `json:"authorized"`
	Vehicles   int    `json:"vehicles"`
}

// Providers lists the vehicle accounts.
func (m *Manager) Providers() []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Provider{}
	for _, dc := range m.cfg.Devices {
		p, ok := m.providers[dc.ID]
		if !ok {
			continue
		}
		pr := Provider{ID: dc.ID, Name: dc.Name, Type: dc.Type, Authorized: true}
		if pr.Name == "" {
			pr.Name = dc.ID
		}
		if a, ok := p.(sdk.OAuthAuthorizer); ok {
			pr.NeedsLogin, pr.Authorized = true, a.Authorized()
		}
		for _, v := range m.vehicles {
			if v.Provider == dc.ID {
				pr.Vehicles++
			}
		}
		out = append(out, pr)
	}
	return out
}

// Authorizer returns the OAuth login of a vehicle account.
func (m *Manager) Authorizer(id string) (sdk.OAuthAuthorizer, bool) {
	a, ok := m.providers[id].(sdk.OAuthAuthorizer)
	return a, ok
}

// Refresh reloads the vehicles now (e.g. right after a login).
func (m *Manager) Refresh(ctx context.Context) { m.refreshVehicles(ctx) }

// Prices returns the current grid and feed-in price (NaN if unknown).
func (m *Manager) Prices(ctx context.Context) (grid, feedin float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	return m.price(ctx, "grid", m.grid, now), m.price(ctx, "feedin", m.feedin, now)
}

// priceNote describes the price rule of a charging session.
func priceNote(separate, guest bool) string {
	switch {
	case separate:
		return "eigener Zähler: Tarif für Ladestrom"
	case guest:
		return "Gast: alles zum Netzpreis"
	}
	return "PV und Akku zur Einspeisevergütung, Netz zum Tarif"
}

func hasMode(ms []Mode, m Mode) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

// SetOff switches a loadpoint off (no charging for any vehicle) or on again.
func (m *Manager) SetOff(id string, off bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lp := range m.lps {
		if lp.view.ID == id {
			v := ""
			if off {
				v = "1"
			}
			lp.view.Off = off
			return m.settings.Put("off:"+id, []byte(v))
		}
	}
	return fmt.Errorf("unknown loadpoint %q", id)
}

// cardFor finds the setting of a charging card by its name.
func (m *Manager) cardFor(name string) (config.Card, bool) {
	for _, c := range m.cfg.Charging.Cards {
		if strings.EqualFold(c.Card, name) {
			c.Vehicle = m.resolve(c.Vehicle)
			return c, true
		}
	}
	return config.Card{}, false
}

// Cards lists the cards the chargers know (for the settings page).
func (m *Manager) Cards(ctx context.Context) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, lp := range m.lps {
		if cl, ok := lp.charger.(sdk.CardLister); ok {
			names, err := cl.Cards(ctx)
			if err != nil {
				continue
			}
			for _, n := range names {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// livePhases are the phases the car draws on as measured by the charger
// (0 = not charging, or the charger does not measure per phase). It is not
// guessed from the power: a 2-phase car at 16 A and a 3-phase car at 11 A
// draw the same.
func livePhases(st sdk.ChargerStatus, charging bool) int {
	if !charging || st.PowerW < 100 {
		return 0
	}
	return st.Phases
}

// learnAfter is how long released phases must hold before the measured
// phases count: right after switching, the car still draws on the old ones.
const learnAfter = 30 * time.Second

// carPhases are the phases the car draws on as far as known: measured in
// this session (fewer than released, or all of the wallbox's), else the
// phases set at the vehicle, else 0 (unknown).
func (m *Manager) carPhases(lp *lpRuntime, st sdk.ChargerStatus, vehicle string, now time.Time) int {
	installed := 3
	if lp.cfg.Phases == 1 {
		installed = 1
	}
	released := lp.released
	if released == 0 {
		released = installed
	}
	if n := livePhases(st, st.Charging); n > 0 && st.PowerW >= 500 && !lp.releasedAt.IsZero() && now.Sub(lp.releasedAt) >= learnAfter &&
		(n < released || released == installed) {
		lp.carPhases = min(n, installed)
	}
	if lp.carPhases > 0 {
		return lp.carPhases
	}
	if veh, ok := m.vehicles[vehicle]; ok && veh.Info.Phases > 0 {
		return veh.Info.Phases
	}
	return 0
}

// finish estimates the end of charging at the current power. The target is
// the plan, else the charge limit in the car, else full.
func finish(st sdk.VehicleStatus, capacityWh float64, plan *Schedule, charging bool, powerW float64, now time.Time) *Finish {
	if math.IsNaN(st.SoC) || capacityWh <= 0 {
		return nil
	}
	f := &Finish{TargetSoC: 1, Basis: "full"}
	switch {
	case plan != nil:
		f.TargetSoC, f.Basis = plan.SoC, "plan"
	case !math.IsNaN(st.LimitSoC) && st.LimitSoC > 0:
		f.TargetSoC, f.Basis = st.LimitSoC, "limit"
	}
	f.NeedWh = NeedWh(st.SoC, f.TargetSoC, capacityWh)
	switch {
	case f.NeedWh < 50:
		f.Reached = true
	case charging && powerW >= 300:
		f.At = now.Add(time.Duration(f.NeedWh / powerW * float64(time.Hour))).Truncate(time.Minute)
	}
	return f
}

// offReason explains why a loadpoint does not charge in mode "off".
func offReason(wallboxOff, sessionMode, guest, connected bool) string {
	switch {
	case wallboxOff:
		return "Wallbox ausgeschaltet – mit „Smart“ oder „Schnell“ wieder einschalten"
	case !connected:
		return "kein Fahrzeug angesteckt"
	case sessionMode:
		return "Laden aus für diesen Ladevorgang – bis zum Abstecken"
	case guest:
		return "Gäste laden nicht automatisch (Standard der Wallbox: Aus) – per Knopf oder Ladekarte starten"
	}
	return "Standardmodus des Fahrzeugs ist „Aus“ – per Knopf starten"
}

// priceRule returns the price rule of a configured vehicle (nil: default)
// and its configured name.
func (m *Manager) priceRule(key string) (*pricing.Rule, string) {
	for _, a := range m.own {
		if a.dev.ID == key {
			name := a.dev.Name
			if name == "" {
				name = a.dev.ID
			}
			return a.dev.Price, name
		}
	}
	return nil, ""
}

// PriceRule returns the price rule of a vehicle (by key or former name).
func (m *Manager) PriceRule(key string) *pricing.Rule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, _ := m.priceRule(m.resolve(key))
	return r
}

// AveragePrices returns the time-weighted feed-in and grid price over a
// period (ct/kWh, NaN if unknown) – to price runs without exact shares.
func (m *Manager) AveragePrices(ctx context.Context, from, to time.Time) (feedin, grid float64) {
	return tariff.Average(ctx, m.feedin, from, to), tariff.Average(ctx, m.grid, from, to)
}
