// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"errors"
	"log/slog"
	"time"

	"github.com/voltibri/voltibri/internal/planner"
)

// Charge targets: automatic or by hand.
//
// Automatic (default): the planner derives the upper and the lower target
// from the forecasts (planner.AutoTargets). Before a sunny day the battery
// may be emptied down to a low floor in the night, so that the PV of the
// next day fits in, and it is charged only to what the night needs (or a
// fixed upper target); before a dull day it keeps a higher floor and
// charges fully.
//
// By hand: the user sets the targets, e.g. 100 % for a pizza evening. By
// default that holds until the next morning (when PV takes over again),
// then the automatic takes over again; "always" keeps it until changed.

const (
	LimitsAuto    = "auto"
	LimitsManual  = "manual"
	ManualMorning = "morning" // by hand until the next morning (default)
	ManualAlways  = "always"  // by hand until changed

	autoFloorSunnyDef  = 0.05
	autoFloorCloudyDef = 0.2
)

// limitMode is the mode of the charge targets ("" derived: settings from
// before the automatic stay by hand).
func (bs BatterySettings) limitMode() string {
	switch {
	case bs.LimitMode != "":
		return bs.LimitMode
	case bs.MaxSoC > 0 || bs.MinSoC > 0:
		return LimitsManual
	}
	return LimitsAuto
}

// autoConfig is the configuration of the automatic targets with defaults.
func (bs BatterySettings) autoConfig() planner.AutoConfig {
	ac := planner.AutoConfig{FloorSunny: bs.AutoFloorSunny, FloorCloudy: bs.AutoFloorCloudy, MaxSoC: bs.AutoMaxSoC}
	if ac.FloorSunny <= 0 {
		ac.FloorSunny = autoFloorSunnyDef
	}
	if ac.FloorCloudy <= 0 {
		ac.FloorCloudy = autoFloorCloudyDef
	}
	return ac
}

// effective returns the settings with the targets that apply now: the
// automatic ones in mode auto (only what the battery supports).
func (s *Site) effective(bs BatterySettings, auto planner.Targets) BatterySettings {
	if bs.limitMode() != LimitsAuto || auto.Reason == "" {
		return bs
	}
	f := s.batteryFeatures()
	bs.MaxSoC, bs.MinSoC = 0, 0
	if f.MaxSoC && auto.MaxSoC < 1 {
		bs.MaxSoC = auto.MaxSoC
	}
	if f.MinSoC {
		bs.MinSoC = auto.MinSoC
	}
	return bs
}

// checkLimits validates the mode and the automatic settings.
func checkLimits(bs BatterySettings) error {
	switch bs.LimitMode {
	case "", LimitsAuto, LimitsManual:
	default:
		return errors.New("Unbekannter Modus der Ladegrenzen: " + bs.LimitMode)
	}
	switch bs.ManualFor {
	case "", ManualMorning, ManualAlways:
	default:
		return errors.New("Unbekannte Dauer: " + bs.ManualFor)
	}
	ac := bs.autoConfig()
	switch {
	case ac.FloorSunny > 0.5 || ac.FloorCloudy > 0.9:
		return errors.New("Die automatischen unteren Ladeziele sind zu hoch.")
	case ac.FloorCloudy < ac.FloorSunny:
		return errors.New("Vor trüben Tagen muss das untere Ladeziel mindestens so hoch sein wie vor sonnigen.")
	case ac.MaxSoC != 0 && (ac.MaxSoC < 0.5 || ac.MaxSoC > 1):
		return errors.New("Das obere Ladeziel muss automatisch sein oder zwischen 50 und 100 % liegen.")
	}
	return nil
}

// nextMorning is when targets set by hand until the morning end: when PV
// covers the load again, at the earliest in an hour; else 07:00.
func (s *Site) nextMorning(now time.Time) time.Time {
	s.mu.RLock()
	m, a := s.plan.Morning, s.auto.Morning
	s.mu.RUnlock()
	for _, t := range []*time.Time{m, a} {
		if t != nil && t.After(now.Add(time.Hour)) && t.Before(now.Add(36*time.Hour)) {
			return *t
		}
	}
	loc := s.loc
	if loc == nil {
		loc = time.Local
	}
	l := now.In(loc)
	t := time.Date(l.Year(), l.Month(), l.Day(), 7, 0, 0, 0, loc)
	if !t.After(now.Add(time.Hour)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// manualUntil sets when targets by hand end (nil = never).
func (s *Site) manualUntil(bs *BatterySettings, prev BatterySettings, now time.Time) {
	if bs.limitMode() != LimitsManual || bs.ManualFor == ManualAlways {
		bs.ManualUntil = nil
		return
	}
	if prev.limitMode() == LimitsManual && prev.ManualFor != ManualAlways && prev.ManualUntil != nil && prev.ManualUntil.After(now) {
		bs.ManualUntil = prev.ManualUntil // a change by hand keeps the end
		return
	}
	t := s.nextMorning(now)
	bs.ManualUntil = &t
}

// expireManual returns to the automatic targets in the morning.
func (s *Site) expireManual(now time.Time) {
	s.mu.Lock()
	bs := s.batSet
	if bs.limitMode() != LimitsManual || bs.ManualUntil == nil || now.Before(*bs.ManualUntil) {
		s.mu.Unlock()
		return
	}
	bs.LimitMode, bs.ManualUntil = LimitsAuto, nil
	s.batSet = bs
	s.mu.Unlock()
	slog.Info("charge targets: back to automatic")
	if err := s.saveBattery(setKey, bs); err != nil {
		slog.Error("storing battery settings failed", "err", err)
	}
}
