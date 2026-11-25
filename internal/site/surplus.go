// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/voltibri/voltibri/internal/charging"
	"github.com/voltibri/voltibri/internal/config"
)

// Surplus distribution: the wallboxes and the heating rods in steps share
// the PV surplus by priority (0-10, higher first; runtime settings, so
// that other systems can change them later). At equal priority wallboxes
// come before heating rods, hot water before heating support, then the
// order of the configuration. A load put first for today ("top", until
// midnight) comes before all priorities, a load boosted by hand before
// that; loads that do not follow the surplus (a wallbox charging fast,
// by plan, external or off) are booked first, they draw what they draw.
//
// Each tick starts from the surplus there would be without the surplus
// loads,
//
//	surplus = feed-in + what the surplus loads draw − battery share
//
// and hands it out in that order: each load gets what the ones before left
// (including what it draws itself) and books what it will draw. The
// battery share is its discharge, and its charging while the battery has
// priority (low SoC). Boosted loads get the boost power of the battery on
// top (boost.go).

const (
	defaultPriority = 5
	maxPriority     = 10
	loadsNS         = "loads" // cache namespace of the load settings
	loadsKey        = "settings"
)

// LoadSetting is the runtime setting of a surplus load.
type LoadSetting struct {
	Priority int        `json:"priority"`
	Mode     LoadMode   `json:"mode,omitempty"`     // heating rods in steps
	TopUntil *time.Time `json:"topUntil,omitempty"` // first before all others until then (end of the day)
}

// top reports whether the load comes first now.
func (ls LoadSetting) top(now time.Time) bool { return ls.TopUntil != nil && now.Before(*ls.TopUntil) }

// LoadInfo is a surplus load for the web page and other systems.
type LoadInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"` // ev-charger or heating-rod
	Priority int      `json:"priority"`
	Mode     LoadMode `json:"mode,omitempty"` // heating rods only (wallboxes: charge mode of the vehicle)
	Top      bool     `json:"top,omitempty"`  // first for today
	Boosting bool     `json:"boosting,omitempty"`
	PowerW   float64  `json:"powerW"`
}

// setting returns the setting of a load with defaults (caller holds no lock).
func (s *Site) setting(id string) LoadSetting {
	s.mu.RLock()
	ls, ok := s.loadSet[id]
	s.mu.RUnlock()
	if !ok {
		ls.Priority = defaultPriority
	}
	if ls.Mode == "" {
		ls.Mode = LoadSurplus
	}
	return ls
}

// isLoad reports whether id is a surplus load (wallbox or rod in steps).
func (s *Site) isLoad(id string) (rod, ok bool) {
	if s.flex.load(id) != nil {
		return true, true
	}
	for _, lp := range s.cfg.Charging.Loadpoints {
		if lp.Charger == id {
			return false, true
		}
	}
	return false, false
}

// Loads lists the surplus loads in their current order.
func (s *Site) Loads() []LoadInfo {
	st := s.State()
	var out []LoadInfo
	for _, lp := range st.Loadpoints {
		ls := s.setting(lp.ID)
		out = append(out, LoadInfo{ID: lp.ID, Name: lp.Name, Kind: config.KindEVCharger, Priority: ls.Priority,
			Top: ls.top(time.Now()), Boosting: lp.Boosting, PowerW: lp.PowerW})
	}
	if len(st.Loadpoints) == 0 { // before the first measurement
		for _, lp := range s.cfg.Charging.Loadpoints {
			out = append(out, LoadInfo{ID: lp.Charger, Name: deviceName(s.cfg, lp.Charger), Kind: config.KindEVCharger, Priority: s.setting(lp.Charger).Priority})
		}
	}
	if s.flex != nil {
		for _, l := range s.flex.loads {
			ls := s.setting(l.id)
			in := LoadInfo{ID: l.id, Name: deviceName(s.cfg, l.id), Kind: config.KindHeatingRod, Priority: ls.Priority, Mode: ls.Mode, Top: ls.top(time.Now())}
			for _, c := range st.Consumers {
				if c.ID == l.id {
					in.PowerW, in.Boosting = c.PowerW, c.Boosting
				}
			}
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

func deviceName(cfg *config.Config, id string) string {
	for _, d := range cfg.Devices {
		if d.ID == id && d.Name != "" {
			return d.Name
		}
	}
	return id
}

// SetLoadPriority sets the priority (0-10) of a surplus load.
func (s *Site) SetLoadPriority(id string, p int) error {
	if _, ok := s.isLoad(id); !ok {
		return errors.New("Kein Verbraucher mit Überschuss-Steuerung: " + id)
	}
	if p < 0 || p > maxPriority {
		return errors.New("Die Priorität muss zwischen 0 und 10 liegen.")
	}
	ls := s.setting(id)
	ls.Priority = p
	slog.Info("load priority", "device", id, "priority", p)
	return s.saveLoad(id, ls)
}

// SetLoadTop puts a load first before all others until midnight, or ends that.
func (s *Site) SetLoadTop(id string, on bool) error {
	if _, ok := s.isLoad(id); !ok {
		return errors.New("Kein Verbraucher mit Überschuss-Steuerung: " + id)
	}
	ls := s.setting(id)
	ls.TopUntil = nil
	if on {
		loc := s.loc
		if loc == nil {
			loc = time.Local
		}
		n := time.Now().In(loc)
		t := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, loc)
		ls.TopUntil = &t
	}
	slog.Info("load first for today", "device", id, "on", on)
	return s.saveLoad(id, ls)
}

// SetLoadMode sets the mode of a heating rod in steps.
func (s *Site) SetLoadMode(id string, m LoadMode) error {
	if rod, ok := s.isLoad(id); !ok || !rod {
		return errors.New("Kein Heizstab mit Überschuss-Steuerung: " + id + " (Wallboxen: Lademodus des Fahrzeugs)")
	}
	if !m.Valid() {
		return errors.New("Unbekannter Modus: " + string(m) + " (off, surplus, boost)")
	}
	ls := s.setting(id)
	ls.Mode = m
	slog.Info("load mode", "device", id, "mode", m)
	return s.saveLoad(id, ls)
}

func (s *Site) saveLoad(id string, ls LoadSetting) error {
	s.mu.Lock()
	if s.loadSet == nil {
		s.loadSet = map[string]LoadSetting{}
	}
	s.loadSet[id] = ls
	// shown right away, not only after the next measurement
	cs := append([]Reading(nil), s.state.Consumers...)
	for i := range cs {
		if cs[i].ID == id && cs[i].LoadMode != "" {
			p := ls.Priority
			cs[i].LoadMode, cs[i].Priority, cs[i].Top = ls.Mode, &p, ls.top(time.Now())
		}
	}
	s.state.Consumers = cs
	lps := append([]charging.Loadpoint(nil), s.state.Loadpoints...)
	for i := range lps {
		if lps[i].ID == id {
			lps[i].Priority, lps[i].Top = ls.Priority, ls.top(time.Now())
		}
	}
	s.state.Loadpoints = lps
	all := make(map[string]LoadSetting, len(s.loadSet))
	for k, v := range s.loadSet {
		all[k] = v
	}
	s.mu.Unlock()
	if s.loadCache == nil {
		return nil
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return s.loadCache.Put(loadsKey, b)
}

// loadLoads restores the load settings.
func (s *Site) loadLoads() {
	s.loadSet = map[string]LoadSetting{}
	if s.loadCache == nil {
		return
	}
	if b, _, ok := s.loadCache.Get(loadsKey); ok {
		var m map[string]LoadSetting
		if json.Unmarshal(b, &m) == nil {
			for k, v := range m {
				s.loadSet[k] = v
			}
		}
	}
}

// allocEntry is one surplus load in the order.
type allocEntry struct {
	id    string
	rod   *flexLoad // nil: a wallbox (controlled by the charging manager)
	boost bool      // a rod that gets the boost power
	key   [4]int    // sort key
}

// allocator hands out the surplus in priority order (charging.Allocator).
type allocator struct {
	ctx     context.Context
	now     time.Time
	flex    *flexState
	entries []allocEntry
	rank    map[string]int
	next    int
	pv      float64 // surplus left
	boost   float64 // boost power of the battery left
}

// Rank implements charging.Allocator.
func (a *allocator) Rank(id string) int {
	if r, ok := a.rank[id]; ok {
		return r
	}
	return len(a.entries)
}

// Avail implements charging.Allocator: the rods before the loadpoint
// take their share first.
func (a *allocator) Avail(id string, boost bool) float64 {
	a.advance(a.Rank(id))
	return a.avail(boost)
}

func (a *allocator) avail(boost bool) float64 {
	if boost {
		return a.pv + a.boost
	}
	return a.pv
}

// Take implements charging.Allocator: from the surplus first, the rest
// from the boost power.
func (a *allocator) Take(_ string, w float64, boost bool) {
	a.pv -= w
	if boost && a.pv < 0 {
		t := math.Min(-a.pv, a.boost)
		a.boost -= t
		a.pv += t
	}
}

// advance runs the rods ranked before upto.
func (a *allocator) advance(upto int) {
	for ; a.next < len(a.entries) && a.next < upto; a.next++ {
		e := a.entries[a.next]
		if e.rod == nil {
			continue
		}
		why := "PV-Überschuss"
		if e.boost {
			why = "PV-Überschuss + Boost"
		}
		w := a.flex.control(a.ctx, a.now, e.rod, a.avail(e.boost), why)
		a.Take(e.id, w, e.boost)
	}
}

// finish runs the rods after the last loadpoint.
func (a *allocator) finish() { a.advance(len(a.entries)) }

// surplusInput is what the allocator is built from.
type surplusInput struct {
	gridW, batteryW float64
	batteryFirst    bool    // the battery's charging power stays with it
	boostW          float64 // boost power of the battery now (0 = none)
	autoBoost       bool
	loadpoints      []charging.Loadpoint // previous tick: modes, plans
	chargers        map[string]float64   // power per charger now
	rodPowers       map[string]float64   // power per rod now
}

// newAllocator orders the surplus loads and computes the surplus without them.
func (s *Site) newAllocator(ctx context.Context, now time.Time, in surplusInput) *allocator {
	a := &allocator{ctx: ctx, now: now, flex: s.flex, rank: map[string]int{}}
	a.pv = -in.gridW
	if in.batteryFirst {
		a.pv -= math.Max(in.batteryW, 0)
	} else {
		a.pv -= in.batteryW
	}
	a.boost = in.boostW
	index := map[string]int{}
	for i, d := range s.cfg.Devices {
		index[d.ID] = i
	}
	for _, lp := range s.cfg.Charging.Loadpoints {
		id := lp.Charger
		a.pv += in.chargers[id]
		e := allocEntry{id: id}
		ls := s.setting(id)
		prio := ls.Priority
		if ls.top(now) {
			prio = maxPriority + 1
		}
		var view *charging.Loadpoint
		for i := range in.loadpoints {
			if in.loadpoints[i].ID == id {
				view = &in.loadpoints[i]
			}
		}
		switch {
		case view == nil || !view.Connected || !view.Mode.Surplus() || (view.Plan != nil && view.Plan.Active(now)):
			prio = 100 // draws what it draws: booked first
		case view.Mode == charging.ModeBoost:
			prio = maxPriority + 2
		}
		e.key = [4]int{-prio, 0, index[id], 0}
		a.entries = append(a.entries, e)
	}
	if s.flex != nil {
		for _, l := range s.flex.loads {
			ls := s.setting(l.id)
			l.mode = ls.Mode
			a.pv += in.rodPowers[l.id]
			prio := ls.Priority
			if ls.top(now) {
				prio = maxPriority + 1
			}
			boost := !l.noBat && in.boostW > 0 && (ls.Mode == LoadBoost || (in.autoBoost && ls.Mode == LoadSurplus))
			if ls.Mode == LoadBoost {
				prio = maxPriority + 2
			}
			a.entries = append(a.entries, allocEntry{id: l.id, rod: l, boost: boost, key: [4]int{-prio, 1 + l.tie, l.index, 0}})
		}
	}
	sort.SliceStable(a.entries, func(i, j int) bool {
		ki, kj := a.entries[i].key, a.entries[j].key
		for n := range ki {
			if ki[n] != kj[n] {
				return ki[n] < kj[n]
			}
		}
		return false
	})
	for i, e := range a.entries {
		a.rank[e.id] = i
	}
	s.flex.reconcile(ctx, now)
	return a
}

// rodBoosting reports which rods get the boost power now (for the state).
func (a *allocator) rodBoosting(id string) bool {
	for _, e := range a.entries {
		if e.id == id {
			return e.boost
		}
	}
	return false
}
