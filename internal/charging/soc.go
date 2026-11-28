// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package charging

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/soc"
)

// State of charge of the vehicles and voltibri's own charge limit.
//
// Every plugged-in vehicle has an estimate: its base is a value entered by
// hand (also before plugging in), the value of the data source or the
// vehicle's typical state of charge when plugging in, in this order; the
// energy charged since is added (at Efficiency). A data source that reports
// a value different from the one seen when the base was taken replaces the
// base. The estimate ends when the vehicle is unplugged.

// pendingTTL: a state of charge entered without plugging in is forgotten
// after this time.
const pendingTTL = 12 * time.Hour

// Limit is voltibri's own charge limit: stop at a state of charge (SoC) or,
// when the state of charge is unknown, after adding a share of the battery
// (Add, "+20 %"; needs the battery capacity).
type Limit struct {
	SoC float64 `json:"soc,omitempty"` // 0.2..1
	Add float64 `json:"add,omitempty"` // 0.05..1 in steps of 0.05
}

// Empty reports whether no limit is set.
func (l Limit) Empty() bool { return l.SoC == 0 && l.Add == 0 }

// Validate checks a limit.
func (l Limit) Validate() error {
	switch {
	case l.SoC != 0 && l.Add != 0:
		return fmt.Errorf("Ladelimit: entweder ein Ladestand oder +x %%, nicht beides")
	case l.SoC != 0 && (l.SoC < 0.2 || l.SoC > 1):
		return fmt.Errorf("Ladelimit muss zwischen 20 und 100 %% liegen")
	case l.Add != 0 && (l.Add < 0.05 || l.Add > 1 || !step5(l.Add)):
		return fmt.Errorf("+x %% geht in 5-%%-Schritten von 5 bis 100 %%")
	}
	return nil
}

func step5(x float64) bool { r := x * 20; return math.Abs(r-math.Round(r)) < 1e-6 }

// String is the limit for reasons ("80 %", "+20 %").
func (l Limit) String() string {
	if l.Add > 0 {
		return fmt.Sprintf("+%.0f %%", l.Add*100)
	}
	return fmt.Sprintf("%.0f %%", l.SoC*100)
}

// LimitState is the limit that applies at a loadpoint.
type LimitState struct {
	Limit
	Session  bool    `json:"session,omitempty"`  // set for this session (else the vehicle's default)
	Reached  bool    `json:"reached,omitempty"`  // charging stopped at the limit
	TargetWh float64 `json:"targetWh,omitempty"` // +x %: session energy at which it stops
	Note     string  `json:"note,omitempty"`     // why the limit cannot act
}

// sessionLimit is the limit of one session; FromWh is the session energy
// when it was set (+x % counts from there).
type sessionLimit struct {
	Limit
	FromWh float64 `json:"fromWh,omitempty"`
}

// estimate returns the stored estimate of a vehicle (nil: none).
func (m *Manager) estimate(key string) *soc.Estimate {
	if e, ok := m.est[key]; ok {
		return e
	}
	e := soc.Load(m.settings, "soc:"+key)
	m.est[key] = e
	return e
}

// setEstimate stores (nil: removes) the estimate of a vehicle.
func (m *Manager) setEstimate(key string, e *soc.Estimate) {
	m.est[key] = e
	m.estSaved[key] = m.now()
	soc.Save(m.settings, "soc:"+key, e)
}

// socOf is the effective state of charge of a vehicle (NaN: unknown) and
// its estimate (nil: the data source's value as it is).
func (m *Manager) socOf(key string) (float64, *soc.Estimate) {
	v, ok := m.vehicles[key]
	if !ok {
		return math.NaN(), nil
	}
	if e := m.estimate(key); e != nil {
		return e.Value(m.capacity(key)), e
	}
	return v.status.SoC, nil
}

// capacity is the battery capacity of a vehicle (0: unknown).
func (m *Manager) capacity(key string) float64 {
	if v, ok := m.vehicles[key]; ok {
		return v.Info.CapacityWh
	}
	return 0
}

// trackSoC keeps the estimate of a plugged-in vehicle: starts it and takes
// a new value of the data source.
func (m *Manager) trackSoC(key string, veh *Vehicle, now time.Time) {
	api := veh.status.SoC
	e := m.estimate(key)
	switch {
	case e == nil && !math.IsNaN(api):
		e = soc.New(api, now, soc.SourceAPI, api)
	case e == nil && veh.Info.DefaultSoC > 0:
		e = soc.New(veh.Info.DefaultSoC, now, soc.SourceDefault, math.NaN())
	case e == nil:
		return
	case e.APIChanged(api) && (e.Source != soc.SourceManual || veh.status.Updated.After(e.At)):
		slog.Info("state of charge from the data source", "vehicle", key, "soc", api, "was", e.Value(m.capacity(key)), "source", e.Source)
		e.Rebase(api, now, soc.SourceAPI, api)
	case !e.InUse:
	default:
		if now.Sub(m.estSaved[key]) >= 5*time.Minute { // the charged energy, now and then
			m.setEstimate(key, e)
		}
		return
	}
	e.InUse = true
	m.setEstimate(key, e)
}

// dropStaleEstimates removes the estimates of vehicles that are not plugged
// in: one that was in use (unplugged during a restart) or a value entered
// by hand that waited too long.
func (m *Manager) dropStaleEstimates(connected map[string]bool, now time.Time) {
	for k, e := range m.est {
		if e == nil || connected[k] {
			continue
		}
		if e.InUse || e.Source != soc.SourceManual || now.Sub(e.At) > pendingTTL {
			m.setEstimate(k, nil)
		}
	}
}

// SetVehicleSoC sets the state of charge of a vehicle by hand (nil: back to
// the data source or the typical value). It holds until the vehicle is
// unplugged; entered before plugging in, it applies to the next session.
func (m *Manager) SetVehicleSoC(key string, v *float64) error {
	key = m.resolve(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	veh, ok := m.vehicles[key]
	if !ok {
		return fmt.Errorf("unknown vehicle %q", key)
	}
	if v == nil {
		m.setEstimate(key, nil)
		return nil
	}
	if err := soc.Validate(*v); err != nil {
		return err
	}
	e := soc.New(*v, m.now(), soc.SourceManual, veh.status.SoC)
	for _, lp := range m.lps {
		e.InUse = e.InUse || (lp.view.Connected && lp.view.Vehicle == key)
	}
	m.setEstimate(key, e)
	slog.Info("state of charge by hand", "vehicle", key, "soc", *v)
	return nil
}

// SetLoadpointLimit sets the charge limit of the current session at a
// loadpoint (nil: back to the vehicle's default). It ends when the vehicle
// is unplugged.
func (m *Manager) SetLoadpointLimit(id string, l *Limit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lp := range m.lps {
		if lp.view.ID != id {
			continue
		}
		lp.limitHit = ""
		if l == nil || l.Empty() {
			return m.settings.Put("limit:"+id, nil)
		}
		if err := l.Validate(); err != nil {
			return err
		}
		if l.Add > 0 && m.capacity(lp.view.Vehicle) <= 0 {
			return fmt.Errorf("+x %% braucht die Akkukapazität des Fahrzeugs (in der Konfiguration eintragen)")
		}
		sl := sessionLimit{Limit: *l}
		if lp.session != nil {
			sl.FromWh = lp.session.EnergyWh
		}
		b, _ := json.Marshal(sl)
		return m.settings.Put("limit:"+id, b)
	}
	return fmt.Errorf("unknown loadpoint %q", id)
}

// limitOf is the limit at a loadpoint: the session's, else the vehicle's.
func (m *Manager) limitOf(lp *lpRuntime, vs VehicleSettings, socNow, sessionWh float64) *LimitState {
	var ls LimitState
	var from float64
	if b, _, ok := m.settings.Get("limit:" + lp.view.ID); ok && len(b) > 0 {
		var sl sessionLimit
		if json.Unmarshal(b, &sl) == nil && !sl.Empty() {
			ls.Limit, ls.Session, from = sl.Limit, true, sl.FromWh
		}
	}
	if !ls.Session {
		if vs.Limit == nil || vs.Limit.Empty() {
			return nil
		}
		ls.Limit = *vs.Limit
	}
	switch {
	case ls.Add > 0 && vs.CapacityWh <= 0:
		ls.Note = "+x % wirkt erst mit bekannter Akkukapazität"
	case ls.Add > 0:
		ls.TargetWh = from + ls.Add*vs.CapacityWh/Efficiency
		ls.Reached = sessionWh >= ls.TargetWh-1
	case math.IsNaN(socNow):
		ls.Note = "das Ladelimit wirkt erst mit bekanntem Ladestand"
	default:
		ls.Reached = socNow >= ls.SoC-0.001
	}
	return &ls
}

// applyLimit stops charging at the limit; once reached it stays stopped
// until the limit changes or the vehicle is unplugged.
func (lp *lpRuntime) applyLimit(ls *LimitState, mode Mode, d *Decision) {
	if ls == nil {
		lp.limitHit = ""
		return
	}
	key := fmt.Sprintf("%v/%v/%v", ls.Session, ls.SoC, ls.TargetWh)
	if ls.Reached {
		lp.limitHit = key
	} else if lp.limitHit != key {
		lp.limitHit = ""
	}
	ls.Reached = lp.limitHit == key
	if !ls.Reached || mode == ModeExternal || mode == ModeOff {
		return
	}
	d.Enabled, d.CurrentA = false, 0
	d.SurplusSince, d.DeficitSince = time.Time{}, time.Time{}
	if ls.Add > 0 {
		d.Reason = fmt.Sprintf("Ladelimit erreicht: %s geladen", ls.String())
	} else {
		d.Reason = fmt.Sprintf("Ladelimit %s erreicht", ls.String())
	}
}

// planNeed is the grid energy a plan still needs (NaN: unknown).
func planNeed(p Plan, socNow, capacityWh, sessionWh float64) float64 {
	if p.Add > 0 {
		if capacityWh <= 0 {
			return math.NaN()
		}
		return math.Max(0, p.Add*capacityWh/Efficiency-sessionWh)
	}
	return NeedWh(socNow, p.SoC, capacityWh)
}
