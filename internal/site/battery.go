// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/sdk"
)

// Holding the home battery back for some loads: the battery may only
// supply what the other loads need, the excluded ones (e.g. an EV charging
// "fast", a heat pump set to "never") run from PV and grid:
//
//	discharge limit = house load - excluded loads - PV
//
// PV goes to the other loads first. A small margin keeps short load peaks of
// the house from being drawn from the grid.
const dischargeMarginW = 50

// limitState is what was last written to the battery.
type limitState struct {
	charge, discharge float64 // -1 = no limit
	at                time.Time
	valid             bool
	warned            bool
}

// updateDischargeLimit computes the discharge limit from the current
// measurement and writes it when it changed noticeably.
func (s *Site) updateDischargeLimit(ctx context.Context, st *State, chargerExcludedW float64) {
	excludedW := chargerExcludedW
	var names []string
	for _, c := range st.Consumers {
		if d, ok := s.device(c.ID); ok && d.BatteryUse == config.BatteryNever && config.KindOf(d) != config.KindEVCharger {
			excludedW += c.PowerW
			if c.PowerW > 10 {
				names = append(names, c.Name)
			}
		}
	}
	for _, lp := range st.Loadpoints {
		if lp.BatteryExcluded && lp.PowerW > 10 {
			names = append(names, lp.Name)
		}
	}
	st.ExcludedW, st.Excluded, st.DischargeLimitW = excludedW, names, -1
	if !st.HasBattery || excludedW < 50 {
		s.setDischarge(ctx, -1)
		return
	}
	limit := math.Max(0, st.LoadW-excludedW-st.PVW) + dischargeMarginW
	if max := s.cfg.Battery.MaxDischargeW; max > 0 && limit >= max {
		limit = -1 // the limit would not restrict anything
	} else {
		limit = math.Round(limit/50) * 50
	}
	st.DischargeLimitW = limit
	s.setDischarge(ctx, limit)
}

func (s *Site) setDischarge(ctx context.Context, w float64) {
	s.mu.Lock()
	s.discharge = w
	s.mu.Unlock()
	s.writeLimits(ctx, false)
}

// writeLimits writes charge limit (from the plan) and discharge limit
// together. force: refresh even if unchanged (keep-alive before the
// inverter's revert timeout).
func (s *Site) writeLimits(ctx context.Context, force bool) {
	s.mu.RLock()
	charge := s.plan.CurrentLimitW()
	discharge := s.discharge
	bs, soc := s.batSet, s.state.SoC
	s.mu.RUnlock()
	if s.batCtl == nil || s.forcing { // forced charging (guest, boost) overrides the plan
		return
	}
	// charge targets and max. discharge of the battery settings (the SoC is
	// that of the previous measurement – a few seconds old)
	charge, discharge = s.guard.apply(bs, soc, charge, discharge)
	l := &s.limits
	changed := !l.valid || charge != l.charge || (discharge < 0) != (l.discharge < 0) ||
		math.Abs(discharge-l.discharge) >= 100
	if !force && !changed {
		return
	}
	if !force && l.valid && time.Since(l.at) < 10*time.Second {
		return // do not flood the inverter with writes
	}
	var err error
	if lim, ok := s.batCtl.(sdk.BatteryLimiter); ok {
		err = lim.SetLimits(ctx, charge, discharge)
	} else {
		if discharge >= 0 && !l.warned {
			slog.Warn("the battery cannot limit discharging: excluded loads are still supplied by the battery")
			l.warned = true
		}
		if charge < 0 {
			err = s.batCtl.ResetLimits(ctx)
		} else {
			err = s.batCtl.SetChargeLimit(ctx, charge)
		}
	}
	if err != nil {
		slog.Error("applying battery limits failed", "err", err)
		return
	}
	if changed {
		slog.Info("battery limits", "chargeW", charge, "dischargeW", discharge)
	}
	l.charge, l.discharge, l.at, l.valid = charge, discharge, time.Now(), true
	s.mu.Lock()
	s.state.LimitW = charge
	s.mu.Unlock()
}

// closeDevices releases connections of devices (e.g. MQTT clients) when the
// site stops – a new configuration builds new devices.
func (s *Site) closeDevices() {
	for id, d := range s.devices {
		if c, ok := d.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				slog.Warn("closing device failed", "device", id, "err", err)
			}
		}
	}
}

// HeatReading is the heat output of a heat pump for the web page (nil =
// unknown).
type HeatReading struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ThermalW  *float64 `json:"thermalW,omitempty"`
	COP       *float64 `json:"cop,omitempty"`
	ElectricW *float64 `json:"electricW,omitempty"`
	FlowC     *float64 `json:"flowC,omitempty"`
	ReturnC   *float64 `json:"returnC,omitempty"`
	OutdoorC  *float64 `json:"outdoorC,omitempty"`
	HotWaterC *float64 `json:"hotWaterC,omitempty"`
	State     string   `json:"state,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func num(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// readHeat collects the heat sources (heat pumps reporting heat output).
func (s *Site) readHeat(ctx context.Context) []HeatReading {
	var out []HeatReading
	for _, dc := range s.cfg.Devices {
		hs, ok := s.devices[dc.ID].(sdk.HeatSource)
		if !ok {
			continue
		}
		name := dc.Name
		if name == "" {
			name = dc.ID
		}
		r := HeatReading{ID: dc.ID, Name: name}
		st, err := hs.HeatStatus(ctx)
		if err != nil {
			r.Error = err.Error()
		}
		r.ThermalW, r.COP, r.ElectricW = num(st.ThermalW), num(st.COP), num(st.ElectricW)
		r.FlowC, r.ReturnC, r.OutdoorC, r.HotWaterC, r.State = num(st.FlowC), num(st.ReturnC), num(st.OutdoorC), num(st.HotWaterC), st.State
		out = append(out, r)
	}
	return out
}
