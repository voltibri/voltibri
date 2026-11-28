// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/soc"
	"github.com/voltibri/voltibri/sdk"
)

// Household devices that charge a battery (config battery): an e-bike
// charger or a power bank at a smart plug.
//
// A charge starts when the device draws more than its run threshold and
// ends after chargePause below it ("unplugged"). Its state of charge starts
// from a value entered by hand (also before the charge), else the typical
// value of the device, and the measured energy is added. With a limit (a
// state of charge or +x %) and a plug voltibri may switch (mode owned,
// flexibility not none), the plug is switched off at the limit; the charge
// then waits until the limit is raised or the user switches it on again
// (which starts a new charge).

const chargePause = 5 * time.Minute

// ChargeView is the battery of a household device, shown with its reading.
type ChargeView struct {
	CapacityWh   float64              `json:"capacityWh"`
	SoC          *float64             `json:"soc,omitempty"`
	SoCSource    string               `json:"socSource,omitempty"` // manual, default
	SoCAt        time.Time            `json:"socAt,omitzero"`
	SoCEstimated bool                 `json:"socEstimated,omitempty"`
	Active       bool                 `json:"active"`             // a charge runs (or waits at the limit)
	Since        time.Time            `json:"since,omitzero"`     // start of the charge
	EnergyWh     float64              `json:"energyWh,omitempty"` // energy of this charge
	Limit        *charging.LimitState `json:"limit,omitempty"`
	Switchable   bool                 `json:"switchable"` // voltibri may switch the plug
	Off          bool                 `json:"off,omitempty"`
	Reason       string               `json:"reason,omitempty"`
}

// chargeState is stored per device.
type chargeState struct {
	Est      *soc.Estimate   `json:"est,omitempty"`
	Active   bool            `json:"active,omitempty"`
	Since    time.Time       `json:"since,omitzero"`
	EnergyWh float64         `json:"energyWh,omitempty"`
	Limit    *charging.Limit `json:"limit,omitempty"` // of this charge (nil: the device's)
	FromWh   float64         `json:"fromWh,omitempty"`
	Hit      bool            `json:"hit,omitempty"` // switched off at the limit
}

type chargeable struct {
	id        string
	bat       config.DeviceBattery
	sw        sdk.Switch // nil: only shown
	threshold float64
	st        chargeState
	below     time.Time
	tick      time.Time
	saved     time.Time
	view      ChargeView
}

type chargeables struct {
	mu     sync.Mutex
	list   map[string]*chargeable
	cache  sdk.Cache
	dryRun bool
}

func newChargeables(cfg *config.Config, devices map[string]sdk.Device, cache sdk.Cache) *chargeables {
	cs := &chargeables{list: map[string]*chargeable{}, cache: cache, dryRun: cfg.Site.DryRun}
	for _, d := range cfg.Devices {
		if d.Battery == nil || devices[d.ID] == nil {
			continue
		}
		_, threshold, _ := runSettings(d)
		if threshold <= 0 {
			threshold = 25
		}
		c := &chargeable{id: d.ID, bat: *d.Battery, threshold: threshold}
		if sw, ok := devices[d.ID].(sdk.Switch); ok && sdk.ControlMode(d.Mode) == sdk.ModeOwned &&
			d.Flexibility != "" && d.Flexibility != config.FlexNone {
			c.sw = sw
		}
		if cache != nil {
			if b, _, ok := cache.Get("bat:" + d.ID); ok && len(b) > 0 {
				_ = json.Unmarshal(b, &c.st)
			}
		}
		cs.list[d.ID] = c
	}
	return cs
}

func (cs *chargeables) save(c *chargeable, now time.Time) {
	c.saved = now
	if cs.cache == nil {
		return
	}
	b, _ := json.Marshal(c.st)
	_ = cs.cache.Put("bat:"+c.id, b)
}

// update runs one tick for every device with a battery and returns the
// views per device.
func (cs *chargeables) update(ctx context.Context, power map[string]float64, now time.Time) map[string]*ChargeView {
	if cs == nil || len(cs.list) == 0 {
		return nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := map[string]*ChargeView{}
	for id, c := range cs.list {
		w, ok := power[id]
		if !ok {
			continue
		}
		cs.tickOne(ctx, c, w, now)
		v := c.view
		out[id] = &v
	}
	return out
}

func (cs *chargeables) tickOne(ctx context.Context, c *chargeable, w float64, now time.Time) {
	st := &c.st
	capWh, eff := c.bat.CapacityWh, c.bat.Eff()
	changed := false
	on := w > c.threshold
	if on && !st.Active { // a charge starts
		st.Active, st.Since, st.EnergyWh, st.Hit, st.FromWh = true, now, 0, false, 0
		switch {
		case st.Est != nil && st.Est.Source == soc.SourceManual && !st.Est.InUse:
			st.Est.InUse = true // entered before the charge
		case c.bat.DefaultSoC > 0:
			st.Est = soc.New(c.bat.DefaultSoC, now, soc.SourceDefault, math.NaN())
			st.Est.InUse = true
		default:
			st.Est = nil
		}
		changed = true
		slog.Info("battery charge started", "device", c.id)
	}
	if st.Active && !c.tick.IsZero() {
		if dt := math.Min(now.Sub(c.tick).Hours(), 1.0/60); dt > 0 && w > 0 {
			st.EnergyWh += w * dt
			if st.Est != nil {
				st.Est.Charge(w*dt, eff, capWh)
			}
		}
	}
	c.tick = now
	switch {
	case on:
		c.below = time.Time{}
	case st.Active && !st.Hit:
		if c.below.IsZero() {
			c.below = now
		}
		if now.Sub(c.below) >= chargePause { // charger done or unplugged
			slog.Info("battery charge ended", "device", c.id, "energyWh", math.Round(st.EnergyWh))
			*st = chargeState{}
			changed = true
		}
	case !st.Active && st.Est != nil && (st.Est.InUse || now.Sub(st.Est.At) > 12*time.Hour):
		st.Est = nil // a value by hand that waited too long
		changed = true
	}

	// limit
	socNow := math.NaN()
	if st.Est != nil {
		socNow = st.Est.Value(capWh)
	}
	ls := c.limit(socNow)
	reason := ""
	if ls != nil && ls.Reached && st.Active && !st.Hit {
		switch {
		case c.sw == nil:
			reason = "Ladelimit erreicht – voltibri darf die Steckdose nicht schalten (Modus „owned“ und eine Flexibilität einstellen)"
		case cs.dryRun:
			reason = "Probelauf: würde jetzt ausschalten (Ladelimit erreicht)"
		default:
			if err := c.sw.SetSwitch(ctx, false); err != nil {
				reason = "Ladelimit erreicht, Ausschalten fehlgeschlagen: " + err.Error()
			} else {
				st.Hit, changed = true, true
				slog.Info("battery limit reached, switched off", "device", c.id, "limit", ls.Limit.String())
			}
		}
	}
	if st.Hit && reason == "" && ls != nil {
		reason = "Ladelimit " + ls.Limit.String() + " erreicht – ausgeschaltet. Limit erhöhen oder einschalten für eine neue Ladung."
		if ls.Add > 0 {
			reason = "Ladelimit erreicht: " + ls.Limit.String() + " geladen – ausgeschaltet."
		}
	}
	if changed || (st.Active && now.Sub(c.saved) >= 5*time.Minute) {
		cs.save(c, now)
	}
	c.buildView(ls, reason)
}

// buildView updates the view from the state.
func (c *chargeable) buildView(ls *charging.LimitState, reason string) {
	st := &c.st
	capWh := c.bat.CapacityWh
	v := ChargeView{CapacityWh: capWh, Active: st.Active, Since: st.Since, EnergyWh: math.Round(st.EnergyWh), Limit: ls,
		Switchable: c.sw != nil, Off: st.Hit, Reason: reason}
	if st.Est != nil {
		x := st.Est.Value(capWh)
		v.SoC, v.SoCSource, v.SoCAt, v.SoCEstimated = &x, string(st.Est.Source), st.Est.At, st.Est.Interpolated()
	}
	c.view = v
}

// changed shows a change by the user at once (not only after the next
// measurement).
func (s *Site) chargeChanged(c *chargeable) {
	socNow := math.NaN()
	if c.st.Est != nil {
		socNow = c.st.Est.Value(c.bat.CapacityWh)
	}
	reason := c.view.Reason
	if !c.st.Hit {
		reason = ""
	}
	c.buildView(c.limit(socNow), reason)
	v := c.view
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Consumers {
		if s.state.Consumers[i].ID == c.id {
			s.state.Consumers[i].Battery = &v
		}
	}
}

// limit is the limit of the charge (or of the device) and whether it is
// reached.
func (c *chargeable) limit(socNow float64) *charging.LimitState {
	st := &c.st
	ls := &charging.LimitState{}
	switch {
	case st.Limit != nil && !st.Limit.Empty():
		ls.Limit, ls.Session = *st.Limit, true
	case c.bat.LimitSoC > 0:
		ls.Limit = charging.Limit{SoC: c.bat.LimitSoC}
	default:
		return nil
	}
	if ls.Add > 0 {
		ls.TargetWh = st.FromWh + ls.Add*c.bat.CapacityWh/c.bat.Eff()
		ls.Reached = st.Active && st.EnergyWh >= ls.TargetWh-1
	} else if math.IsNaN(socNow) {
		ls.Note = "das Ladelimit wirkt erst mit bekanntem Ladestand (eingeben oder +x % wählen)"
	} else {
		ls.Reached = socNow >= ls.SoC-0.001
	}
	if st.Hit {
		ls.Reached = true
	}
	return ls
}

var errNoChargeable = errors.New("Dieses Gerät hat keinen Akku (in der Konfiguration eintragen).")

func (cs *chargeables) get(id string) (*chargeable, error) {
	if cs == nil {
		return nil, errNoChargeable
	}
	c, ok := cs.list[id]
	if !ok {
		return nil, errNoChargeable
	}
	return c, nil
}

// SetLoadSoC sets the state of charge of a device's battery by hand (nil:
// back to the typical value). Entered before the charge, it applies to the
// next one.
func (s *Site) SetLoadSoC(id string, v *float64) error {
	cs := s.chargers
	c, err := cs.get(id)
	if err != nil {
		return err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now()
	if v == nil {
		c.st.Est = nil
	} else {
		if err := soc.Validate(*v); err != nil {
			return err
		}
		c.st.Est = soc.New(*v, now, soc.SourceManual, math.NaN())
		c.st.Est.InUse = c.st.Active
	}
	c.resume(context.Background(), cs.dryRun)
	cs.save(c, now)
	s.chargeChanged(c)
	return nil
}

// SetLoadLimit sets the limit of the current (or next) charge (nil: back
// to the device's). A higher limit after the plug was switched off at the
// old one switches it on again.
func (s *Site) SetLoadLimit(id string, l *charging.Limit) error {
	cs := s.chargers
	c, err := cs.get(id)
	if err != nil {
		return err
	}
	if l != nil && !l.Empty() {
		if err := l.Validate(); err != nil {
			return err
		}
	} else {
		l = nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c.st.Limit, c.st.FromWh = l, c.st.EnergyWh
	c.resume(context.Background(), cs.dryRun)
	cs.save(c, time.Now())
	s.chargeChanged(c)
	return nil
}

// resume switches a plug that is off at the limit on again when the limit
// is no longer reached.
func (c *chargeable) resume(ctx context.Context, dryRun bool) {
	if !c.st.Hit {
		return
	}
	socNow := math.NaN()
	if c.st.Est != nil {
		socNow = c.st.Est.Value(c.bat.CapacityWh)
	}
	c.st.Hit = false
	if ls := c.limit(socNow); ls != nil && ls.Reached {
		c.st.Hit = true
		return
	}
	if c.sw != nil && !dryRun {
		if err := c.sw.SetSwitch(ctx, true); err != nil {
			slog.Warn("switching on failed", "device", c.id, "err", err)
		}
	}
}

// SwitchLoadOn switches a plug that is off at the limit on again and starts
// a new charge (a value by hand entered for it stays).
func (s *Site) SwitchLoadOn(id string) error {
	cs := s.chargers
	c, err := cs.get(id)
	if err != nil {
		return err
	}
	if c.sw == nil {
		return fmt.Errorf("voltibri darf diese Steckdose nicht schalten")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	est := c.st.Est
	if est != nil && est.Source == soc.SourceManual && !est.Interpolated() {
		est.InUse = false // entered for the new charge
	} else {
		est = nil
	}
	c.st = chargeState{Est: est}
	c.below = time.Time{}
	cs.save(c, time.Now())
	s.chargeChanged(c)
	if cs.dryRun {
		return nil
	}
	return c.sw.SetSwitch(context.Background(), true)
}
