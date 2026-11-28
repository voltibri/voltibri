// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package pricing applies price rules to the energy of a run. A rule says
// per energy share (PV, home battery, grid) what a kWh costs:
//
//   - inherit: the default – PV and battery at the lost feed-in tariff,
//     grid at the grid tariff
//   - replace: an own base – a fixed price, the feed-in tariff, the grid
//     tariff or free
//   - adjust:  the inherited price
//
// replace and adjust may add a percentage and a fixed amount per kWh:
// price = base × (1 + percent/100) + addCt.
//
// For now rules exist on vehicles only (docs/datamodel.md, "Price rules").
package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Modes of a share.
const (
	Inherit = "inherit"
	Replace = "replace"
	Adjust  = "adjust"
)

// Bases of a replaced share.
const (
	BaseFixed  = "fixed"  // Ct per kWh
	BaseFeedIn = "feedin" // lost feed-in tariff
	BaseGrid   = "grid"   // grid tariff
	BaseFree   = "free"   // nothing
)

// Share is the rule for one energy share.
type Share struct {
	Mode    string  `yaml:"mode" json:"mode"`                           // inherit (default), replace, adjust
	Base    string  `yaml:"base,omitempty" json:"base,omitempty"`       // replace: fixed, feedin, grid, free
	Ct      float64 `yaml:"ct,omitempty" json:"ct,omitempty"`           // base fixed: ct/kWh
	Percent float64 `yaml:"percent,omitempty" json:"percent,omitempty"` // surcharge in % (negative: discount)
	AddCt   float64 `yaml:"addCt,omitempty" json:"addCt,omitempty"`     // added ct/kWh
}

// Rule is a price rule; a specific share wins over All.
type Rule struct {
	All     *Share `yaml:"all,omitempty" json:"all,omitempty"`
	PV      *Share `yaml:"pv,omitempty" json:"pv,omitempty"`
	Battery *Share `yaml:"battery,omitempty" json:"battery,omitempty"`
	Grid    *Share `yaml:"grid,omitempty" json:"grid,omitempty"`
}

// Amount is one energy share of a run with its value at both tariffs
// (time-weighted, so dynamic prices stay exact).
type Amount struct {
	Wh       float64 `json:"wh"`
	FeedInCt float64 `json:"feedinCt"` // cost of this energy at the feed-in tariff
	GridCt   float64 `json:"gridCt"`   // cost of this energy at the grid tariff
}

// Add books wh at the given prices (ct/kWh; NaN counts as 0).
func (a *Amount) Add(wh, feedinCt, gridCt float64) {
	a.Wh += wh
	a.FeedInCt += wh / 1000 * nz(feedinCt)
	a.GridCt += wh / 1000 * nz(gridCt)
}

// Shares are the energy shares of a run.
type Shares struct {
	PV      Amount `json:"pv"`
	Battery Amount `json:"battery"`
	Grid    Amount `json:"grid"`
}

// Uniform returns shares with the same average prices for every share
// (runs without exact shares, e.g. imported ones).
func Uniform(pvWh, batteryWh, gridWh, feedinCt, gridCt float64) Shares {
	var s Shares
	s.PV.Add(pvWh, feedinCt, gridCt)
	s.Battery.Add(batteryWh, feedinCt, gridCt)
	s.Grid.Add(gridWh, feedinCt, gridCt)
	return s
}

// AttrKey is the run attribute that keeps the shares (to recalculate).
const AttrKey = "shares"

// FromAttrs reads the shares from run attributes.
func FromAttrs(attrs map[string]any) (Shares, bool) {
	v, ok := attrs[AttrKey]
	if !ok {
		return Shares{}, false
	}
	var s Shares
	switch t := v.(type) {
	case Shares:
		return t, true
	case *Shares:
		return *t, t != nil
	default: // decoded JSON
		b, err := json.Marshal(t)
		if err != nil || json.Unmarshal(b, &s) != nil {
			return Shares{}, false
		}
	}
	return s, true
}

// Empty reports whether the rule changes nothing.
func (r *Rule) Empty() bool {
	if r == nil {
		return true
	}
	for _, s := range []*Share{r.All, r.PV, r.Battery, r.Grid} {
		if s != nil && s.Mode != "" && s.Mode != Inherit {
			return false
		}
	}
	return true
}

// Validate checks a rule.
func (r *Rule) Validate() error {
	if r == nil {
		return nil
	}
	for _, x := range []struct {
		name string
		s    *Share
	}{{"all", r.All}, {"pv", r.PV}, {"battery", r.Battery}, {"grid", r.Grid}} {
		if x.s == nil {
			continue
		}
		if err := x.s.validate(); err != nil {
			return fmt.Errorf("%s: %w", x.name, err)
		}
	}
	return nil
}

func (s *Share) validate() error {
	switch s.Mode {
	case "", Inherit:
		if s.Base != "" || s.Ct != 0 || s.Percent != 0 || s.AddCt != 0 {
			return fmt.Errorf("inherit takes no base, price or surcharge")
		}
		return nil
	case Replace:
		switch s.Base {
		case BaseFixed:
			if s.Ct < 0 {
				return fmt.Errorf("fixed price must not be negative")
			}
		case BaseFeedIn, BaseGrid, BaseFree:
			if s.Ct != 0 {
				return fmt.Errorf("ct only with base fixed")
			}
		default:
			return fmt.Errorf("replace needs a base (fixed, feedin, grid, free), got %q", s.Base)
		}
	case Adjust:
		if s.Base != "" || s.Ct != 0 {
			return fmt.Errorf("adjust keeps the inherited price: no base or ct (use replace)")
		}
		if s.Percent == 0 && s.AddCt == 0 {
			return fmt.Errorf("adjust needs percent or addCt")
		}
	default:
		return fmt.Errorf("unknown mode %q (inherit, replace, adjust)", s.Mode)
	}
	if s.Percent < -100 || s.Percent > 1000 {
		return fmt.Errorf("percent %.0f out of range (-100 … 1000)", s.Percent)
	}
	return nil
}

// share returns the rule of one share: the specific one, else All.
func (r *Rule) share(specific *Share) *Share {
	if specific != nil && specific.Mode != "" {
		return specific
	}
	if r != nil && r.All != nil {
		return r.All
	}
	return nil
}

// Cost prices the shares: the total in ct and how it was computed. who
// names the level of the rule (e.g. the vehicle) in the note.
func (r *Rule) Cost(s Shares, who string) (float64, string) {
	var pv, bat, grid *Share
	if r != nil {
		pv, bat, grid = r.share(r.PV), r.share(r.Battery), r.share(r.Grid)
	}
	type part struct {
		label string
		a     Amount
		rule  *Share
		feed  bool // default base: feed-in (PV, battery) or grid
	}
	parts := []part{{"PV", s.PV, pv, true}, {"Akku", s.Battery, bat, true}, {"Netz", s.Grid, grid, false}}
	total := 0.0
	var notes []string
	for _, p := range parts {
		c, n := p.rule.price(p.a, p.feed)
		total += c
		if p.a.Wh > 0 || (p.rule != nil && p.rule.Mode != "" && p.rule.Mode != Inherit) {
			notes = append(notes, p.label+" "+n)
		}
	}
	note := strings.Join(notes, " · ")
	if !r.Empty() && who != "" {
		note = who + ": " + note
	}
	return total, note
}

// Unit is what one kWh of each share costs (ct/kWh).
type Unit struct {
	GridCt    float64 `json:"gridCt"`
	BatteryCt float64 `json:"batteryCt"`
	PVCt      float64 `json:"pvCt"`
}

// Unit returns the price of one kWh per share at the given tariffs.
func (r *Rule) Unit(feedinCt, gridCt float64) Unit {
	var pv, bat, grid *Share
	if r != nil {
		pv, bat, grid = r.share(r.PV), r.share(r.Battery), r.share(r.Grid)
	}
	kwh := Amount{Wh: 1000, FeedInCt: nz(feedinCt), GridCt: nz(gridCt)}
	u := Unit{}
	u.PVCt, _ = pv.price(kwh, true)
	u.BatteryCt, _ = bat.price(kwh, true)
	u.GridCt, _ = grid.price(kwh, false)
	return u
}

// price returns the cost of one share and a short description.
func (s *Share) price(a Amount, feedDefault bool) (float64, string) {
	inherited, name := a.GridCt, "Tarif"
	if feedDefault {
		inherited, name = a.FeedInCt, "Einspeisevergütung"
	}
	if s == nil || s.Mode == "" || s.Mode == Inherit {
		return inherited, name
	}
	base := inherited
	if s.Mode == Replace {
		switch s.Base {
		case BaseFixed:
			base, name = a.Wh/1000*s.Ct, ct(s.Ct)+" fest"
		case BaseFeedIn:
			base, name = a.FeedInCt, "Einspeisevergütung"
		case BaseGrid:
			base, name = a.GridCt, "Tarif"
		case BaseFree:
			base, name = 0, "kostenlos"
		}
	}
	cost := base*(1+s.Percent/100) + a.Wh/1000*s.AddCt
	if s.Percent != 0 {
		name += fmt.Sprintf(" %+g %%", s.Percent)
	}
	if s.AddCt != 0 {
		name += " " + signed(s.AddCt)
	}
	return cost, name
}

func ct(v float64) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', -1, 64), ".", ",", 1) + " ct"
}

func signed(v float64) string {
	if v < 0 {
		return "− " + ct(-v)
	}
	return "+ " + ct(v)
}

func nz(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
