// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package charging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/circuit"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/tariff"
	"github.com/voltibri/voltibri/sdk"
)

// Guest is the vehicle key of an unknown vehicle.
const Guest = "guest"

// DetectTimeout: how long voltibri waits for the vehicle cloud to report a
// plugged-in vehicle before treating it as a guest.
var DetectTimeout = 10 * time.Minute

// VehicleSettings are the user's settings for a vehicle (stored in the
// database, changed from the web page without a config version).
type VehicleSettings struct {
	Name       string  `json:"name,omitempty"`       // display name override
	CapacityWh float64 `json:"capacityWh,omitempty"` // needed for SoC plans
	Mode       Mode    `json:"mode,omitempty"`       // default pv
	MinPowerW  float64 `json:"minPowerW,omitempty"`  // minpv minimum (0 = minimum current)
	MaxPowerW  float64 `json:"maxPowerW,omitempty"`  // onboard charger limit (0 = none)
	Plans      []Plan  `json:"plans,omitempty"`
}

// Vehicle is a known vehicle with its state.
type Vehicle struct {
	Key       string          `json:"key"` // provider/id
	Provider  string          `json:"provider"`
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

// Session is one charging session (plug-in to unplug).
type Session struct {
	ID          int64     `json:"id"`
	Loadpoint   string    `json:"loadpoint"`
	Vehicle     string    `json:"vehicle"` // key or "guest"
	VehicleName string    `json:"vehicleName"`
	Guest       bool      `json:"guest"`
	Mode        Mode      `json:"mode"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end,omitzero"`
	EnergyWh    float64   `json:"energyWh"`
	SolarWh     float64   `json:"solarWh"` // from PV or the home battery
	CostCt      float64   `json:"costCt"`
}

// SessionStore persists finished sessions.
type SessionStore interface {
	SaveSession(ctx context.Context, s Session) (int64, error)
	Sessions(ctx context.Context, limit int) ([]Session, error)
}

// Loadpoint is the public view of a charge point.
type Loadpoint struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Owned       bool      `json:"owned"` // voltibri may write to the charger
	Phases      int       `json:"phases"`
	MinCurrentA float64   `json:"minCurrent"`
	MaxCurrentA float64   `json:"maxCurrent"`
	Guest       string    `json:"guestPolicy"`
	Separate    bool      `json:"separateMeter"`
	Connected   bool      `json:"connected"`
	Charging    bool      `json:"charging"`
	PowerW      float64   `json:"powerW"`
	Vehicle     string    `json:"vehicle"` // key, "guest" or ""
	VehicleName string    `json:"vehicleName"`
	Manual      bool      `json:"manual"`    // vehicle chosen by the user
	Detecting   bool      `json:"detecting"` // still waiting for the vehicle cloud
	Mode        Mode      `json:"mode"`
	Target      State     `json:"target"` // what voltibri wants
	Reason      string    `json:"reason"`
	Plan        *Schedule `json:"plan,omitempty"`
	Session     *Session  `json:"session,omitempty"`
	PriceCt     *float64  `json:"priceCt,omitempty"` // current price at this loadpoint
	LimitA      *float64  `json:"limitA,omitempty"`  // load management: max current per phase right now
	LimitBy     string    `json:"limitBy,omitempty"` // circuit that limits
}

// SiteInput is the site state for one control tick.
type SiteInput struct {
	Now             time.Time
	GridW, BatteryW float64
	PVW, LoadW      float64
	Chargers        map[string]sdk.ChargerStatus // corrected power
	BatteryPriority bool                         // PV goes to the home battery first
	Circuits        *circuit.Tree                // load management (nil: no fuses configured)
}

// Result tells the site about guest charging.
type Result struct {
	GuestW        float64 // guest charging power (kept out of the load forecast)
	ForceBatteryW float64 // > 0: charge the battery with this power (forceBattery policy)
}

type lpRuntime struct {
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
	wasLocked  bool // voltibri disabled the charger
	observedLg string
	resume     *Session // open session from before a restart
	ticked     bool
}

// Manager runs all loadpoints.
type Manager struct {
	loc       *time.Location
	cfg       *config.Config
	providers map[string]sdk.VehicleProvider
	settings  sdk.Cache
	sessions  SessionStore
	grid      tariff.Tariff
	feedin    tariff.Tariff
	chargingT tariff.Tariff
	dynGrid   bool

	mu       sync.RWMutex
	lps      []*lpRuntime
	vehicles map[string]*Vehicle
	prices   map[string]priceCache
}

type priceCache struct {
	slot time.Time
	v    float64
}

// New creates the manager. devices are all configured devices; ids lists
// the devices with a site role (grid, pv, battery) which are never
// loadpoints.
func New(cfg *config.Config, loc *time.Location, devices map[string]sdk.Device, settings sdk.Cache, sessions SessionStore) (*Manager, error) {
	if settings == nil {
		settings = sdk.NewMemCache()
	}
	m := &Manager{loc: loc, cfg: cfg, providers: map[string]sdk.VehicleProvider{}, settings: settings, sessions: sessions,
		vehicles: map[string]*Vehicle{}, prices: map[string]priceCache{}}
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
			m.providers[dc.ID] = vp
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
		lp.view = Loadpoint{ID: dc.ID, Name: name, Owned: lp.ctl != nil, Phases: lc.Phases, MinCurrentA: lc.MinCurrentA,
			MaxCurrentA: lc.MaxCurrentA, Guest: lc.Guest, Separate: lc.SeparateMeter}
		lp.manual = m.loadString("lp:" + dc.ID)
		if b, _, ok := settings.Get("open:" + dc.ID); ok && len(b) > 0 {
			var s Session
			if json.Unmarshal(b, &s) == nil {
				lp.resume = &s
			}
		}
		m.lps = append(m.lps, lp)
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
		if b, _, ok := m.settings.Get("vehicle:" + key); ok {
			_ = json.Unmarshal(b, &s)
		}
	}
	if s.Mode == "" {
		s.Mode = ModePV
	}
	return s
}

// HasLoadpoints reports whether any charger exists.
func (m *Manager) HasLoadpoints() bool { return len(m.lps) > 0 }

// Run refreshes the vehicle list and states in the background (vehicle
// clouds are slow; the control loop only uses the latest known state).
func (m *Manager) Run(ctx context.Context) {
	if len(m.providers) == 0 {
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
	found := map[string]*Vehicle{}
	for pid, p := range m.providers {
		if a, ok := p.(sdk.OAuthAuthorizer); ok && !a.Authorized() {
			continue
		}
		infos, err := p.Vehicles(ctx)
		if err != nil {
			slog.Warn("vehicles unavailable", "provider", pid, "err", err)
			m.mu.RLock()
			for k, v := range m.vehicles { // keep the last known ones
				if v.Provider == pid {
					found[k] = v
				}
			}
			m.mu.RUnlock()
			continue
		}
		for _, info := range infos {
			key := pid + "/" + info.ID
			v := &Vehicle{Key: key, Provider: pid, Info: info}
			st, err := p.VehicleStatus(ctx, info.ID)
			if err != nil {
				v.Error = err.Error()
				m.mu.RLock()
				if old, ok := m.vehicles[key]; ok {
					st = old.status
				}
				m.mu.RUnlock()
			}
			v.status = st
			found[key] = v
		}
	}
	m.mu.Lock()
	m.vehicles = found
	m.mu.Unlock()
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
	m.mu.RLock()
	_, ok := m.vehicles[key]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown vehicle %q", key)
	}
	if s.Mode != "" && !s.Mode.Valid() {
		return fmt.Errorf("unknown mode %q", s.Mode)
	}
	if s.CapacityWh < 0 || s.MinPowerW < 0 || s.MaxPowerW < 0 {
		return errors.New("values must not be negative")
	}
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

// SetLoadpointVehicle assigns a vehicle manually ("" = automatic, "guest").
// The assignment ends when the vehicle is unplugged.
func (m *Manager) SetLoadpointVehicle(id, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key != "" && key != Guest {
		if _, ok := m.vehicles[key]; !ok {
			return fmt.Errorf("unknown vehicle %q", key)
		}
	}
	for _, lp := range m.lps {
		if lp.view.ID == id {
			lp.manual = key
			return m.settings.Put("lp:"+id, []byte(key))
		}
	}
	return fmt.Errorf("unknown loadpoint %q", id)
}

// SetGuestMode changes the charge mode used for guests at a loadpoint.
func (m *Manager) SetGuestMode(id string, mode Mode) error {
	if !mode.Valid() {
		return fmt.Errorf("unknown mode %q", mode)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lp := range m.lps {
		if lp.view.ID == id {
			return m.settings.Put("guestmode:"+id, []byte(mode))
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
	done, err := m.sessions.Sessions(ctx, limit)
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
	for _, lp := range m.lps {
		st := in.Chargers[lp.view.ID]
		v := &lp.view
		wasConnected := v.Connected
		v.Connected, v.Charging, v.PowerW = st.Connected, st.Charging, st.PowerW
		if !lp.ticked { // first tick: continue a session interrupted by a restart
			lp.ticked = true
			if r := lp.resume; r != nil {
				lp.resume = nil
				_ = m.settings.Put("open:"+v.ID, []byte{})
				if v.Connected && in.Now.Sub(r.Start) < 48*time.Hour {
					lp.session, lp.plugged, wasConnected = r, r.Start, true
					lp.lastTick = in.Now
				} else if m.sessions != nil {
					r.End = in.Now
					if _, err := m.sessions.SaveSession(ctx, *r); err != nil {
						slog.Error("saving charging session failed", "err", err)
					}
				}
			}
		}
		if v.Connected && !wasConnected {
			lp.plugged = in.Now
		}
		if !v.Connected && wasConnected {
			m.endSession(ctx, lp, in.Now)
			lp.manual = ""
			_ = m.settings.Put("lp:"+v.ID, []byte{})
		}

		// vehicle and mode
		delete(taken, v.Vehicle)
		v.Vehicle, v.Detecting, v.Manual = "", false, lp.manual != ""
		vs := VehicleSettings{}
		guestMode := Mode(m.loadString("guestmode:" + v.ID))
		if !guestMode.Valid() {
			guestMode = Mode(lp.cfg.GuestMode)
		}
		v.Mode = guestMode
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
		guest := v.Connected && v.Vehicle == Guest

		// session bookkeeping
		if v.Connected && lp.session == nil {
			lp.session = &Session{Loadpoint: v.ID, Start: in.Now}
		}
		if s := lp.session; s != nil {
			s.Vehicle, s.VehicleName, s.Guest, s.Mode = v.Vehicle, v.VehicleName, guest, v.Mode
			if !lp.lastTick.IsZero() {
				dt := math.Min(in.Now.Sub(lp.lastTick).Hours(), 1.0/60)
				e := v.PowerW * dt
				fromGrid := math.Min(v.PowerW, math.Max(0, in.GridW)) * dt
				s.EnergyWh += e
				s.SolarWh += e - fromGrid
				switch {
				case lp.cfg.SeparateMeter:
					s.CostCt += nz(chargePrice) * e / 1000
				case guest:
					s.CostCt += nz(gridPrice) * e / 1000 // guests pay grid price for everything
				default:
					s.CostCt += (nz(gridPrice)*fromGrid + nz(feedPrice)*(e-fromGrid)) / 1000
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
		if veh, ok := m.vehicles[v.Vehicle]; ok && v.Connected && (v.Mode == ModePV || v.Mode == ModeMinPV) {
			if p, target := nextPlan(vs.Plans, in.Now, m.loc); !target.IsZero() {
				powerW := lp.cfg.MaxCurrentA * Voltage * float64(lp.cfg.Phases)
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

		surplus := Surplus(in.GridW, in.BatteryW, v.PowerW, prio)
		surplus = math.Min(surplus, remaining)
		// load management: the circuits (sub-distribution, house fuse) cap the current
		maxA, own := lp.cfg.MaxCurrentA, circuit.Phases{}
		v.LimitA, v.LimitBy = nil, ""
		if in.Circuits != nil {
			var ok bool
			if own, ok = in.Circuits.Draw(v.ID); !ok {
				own = circuit.Spread(v.PowerW/Voltage/float64(lp.cfg.Phases), lp.cfg.Phases)
			}
			if a, by := in.Circuits.Headroom(v.ID, own); a < maxA {
				maxA, v.LimitBy = math.Floor(a), by
				v.LimitA = &maxA
			}
		}
		d := Decide(Input{Now: in.Now, Mode: mode, PlanSlot: planSlot, Connected: v.Connected, SurplusW: surplus,
			MinA: lp.cfg.MinCurrentA, MaxA: math.Max(maxA, lp.cfg.MinCurrentA), Phases: lp.cfg.Phases, CanSwitch: lp.phaseSw != nil,
			MinPowerW: vs.MinPowerW, MaxPowerW: vs.MaxPowerW, Prev: lp.state})
		if guest && lp.cfg.Guest == config.GuestBatteryFirst && v.Mode == ModeFast {
			d.Reason = "Gast: Batterie hat Vorrang – " + d.Reason
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
		lp.state = d.State
		v.Target, v.Reason = d.State, d.Reason
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
		if mode != ModeExternal && d.Enabled {
			remaining = math.Max(0, surplus-d.CurrentA*Voltage*float64(d.Phases))
		} else if mode != ModeExternal {
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
	if s == nil || m.sessions == nil {
		return
	}
	s.End = now
	if s.EnergyWh < 1 && now.Sub(s.Start) < time.Minute {
		return // plug bounce
	}
	if _, err := m.sessions.SaveSession(ctx, *s); err != nil {
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
