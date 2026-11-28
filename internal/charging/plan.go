// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package charging

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// SlotLen is the planning resolution.
const SlotLen = 15 * time.Minute

// Efficiency of AC charging (grid to battery), as in evcc.
const Efficiency = 0.85

// Plan is a charge target: reach SoC by a time, or, with an unknown state
// of charge, add a share of the battery (Add, "+30 % bis 07:00").
type Plan struct {
	ID   string    `json:"id"`
	SoC  float64   `json:"soc"`            // target 0..1
	Add  float64   `json:"add,omitempty"`  // instead of SoC: share to add 0.05..1 (needs the capacity)
	At   time.Time `json:"at,omitzero"`    // one-off target time
	Days []int     `json:"days,omitempty"` // repeating: weekdays 0=So..6=Sa
	Time string    `json:"time,omitempty"` // repeating: "07:30"
	Off  bool      `json:"off,omitempty"`  // disabled
}

// Next returns the next due target time after now (zero if none).
func (p Plan) Next(now time.Time, loc *time.Location) time.Time {
	if p.Off {
		return time.Time{}
	}
	if len(p.Days) == 0 {
		if p.At.After(now) {
			return p.At
		}
		return time.Time{}
	}
	var h, m int
	if _, err := fmt.Sscanf(p.Time, "%d:%d", &h, &m); err != nil {
		return time.Time{}
	}
	n := now.In(loc)
	for i := 0; i <= 7; i++ {
		d := n.AddDate(0, 0, i)
		t := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if !t.After(now) {
			continue
		}
		for _, wd := range p.Days {
			if int(t.Weekday()) == wd {
				return t
			}
		}
	}
	return time.Time{}
}

// Validate checks a plan.
func (p Plan) Validate() error {
	switch {
	case p.Add != 0 && p.SoC != 0:
		return fmt.Errorf("plan: either a target SoC or +x %%, not both")
	case p.Add != 0:
		if err := (Limit{Add: p.Add}).Validate(); err != nil {
			return err
		}
	case p.SoC <= 0 || p.SoC > 1:
		return fmt.Errorf("target SoC must be between 1 and 100 %%")
	}
	if len(p.Days) == 0 {
		if p.At.IsZero() {
			return fmt.Errorf("plan needs a time (at) or weekdays and time")
		}
		return nil
	}
	for _, d := range p.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekday %d out of range 0..6", d)
		}
	}
	var h, m int
	if _, err := fmt.Sscanf(p.Time, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf("time %q: expected HH:MM", p.Time)
	}
	return nil
}

// Schedule is the computed charging window of a plan.
type Schedule struct {
	Target   time.Time   `json:"target"`
	SoC      float64     `json:"soc"`
	Add      float64     `json:"add,omitempty"` // plan with +x %
	NeedWh   float64     `json:"needWh"`
	Slots    []time.Time `json:"slots"`            // slot starts, sorted
	Start    time.Time   `json:"start,omitzero"`   // first slot
	Cheapest bool        `json:"cheapest"`         // chosen by price (else: as late as possible)
	Late     bool        `json:"late,omitempty"`   // not enough time: charges from now on
	CostCt   float64     `json:"costCt,omitempty"` // expected cost (known prices only)
	Reason   string      `json:"reason"`
}

// Active reports whether t lies in a planned slot.
func (s *Schedule) Active(t time.Time) bool {
	if s == nil {
		return false
	}
	st := t.Truncate(SlotLen)
	for _, x := range s.Slots {
		if x.Equal(st) {
			return true
		}
	}
	return false
}

// MakeSchedule plans the slots needed to charge needWh with powerW before
// target. prices holds one value per slot starting at now's slot (NaN or
// missing = unknown). With prices for the whole window the cheapest slots
// are used (ties: the later one); otherwise charging starts as late as
// possible.
func MakeSchedule(now, target time.Time, needWh, powerW float64, prices []float64) Schedule {
	s := Schedule{Target: target, NeedWh: needWh}
	if needWh <= 0 {
		s.Reason = "Ziel bereits erreicht"
		return s
	}
	if powerW <= 0 {
		s.Reason = "keine Ladeleistung bekannt"
		return s
	}
	first := now.Truncate(SlotLen)
	var window []time.Time
	for t := first; t.Before(target); t = t.Add(SlotLen) {
		window = append(window, t)
	}
	n := int(math.Ceil(needWh / (powerW * SlotLen.Hours())))
	if n >= len(window) {
		s.Slots, s.Late = window, true
		s.Reason = "Zeit reicht knapp oder nicht: lädt ab sofort durch"
	} else {
		known := len(prices) >= len(window)
		for i := 0; known && i < len(window); i++ {
			known = !math.IsNaN(prices[i])
		}
		idx := make([]int, len(window))
		for i := range idx {
			idx[i] = i
		}
		if known {
			sort.SliceStable(idx, func(a, b int) bool {
				pa, pb := prices[idx[a]], prices[idx[b]]
				if pa != pb {
					return pa < pb
				}
				return idx[a] > idx[b] // later first
			})
			s.Cheapest = true
			s.Reason = "lädt in den günstigsten Zeitfenstern vor dem Ziel"
		} else {
			sort.Sort(sort.Reverse(sort.IntSlice(idx)))
			s.Reason = "lädt so spät wie möglich vor dem Ziel"
		}
		idx = idx[:n]
		sort.Ints(idx)
		for _, i := range idx {
			s.Slots = append(s.Slots, window[i])
		}
	}
	for _, t := range s.Slots {
		i := int(t.Sub(first) / SlotLen)
		if i < len(prices) && !math.IsNaN(prices[i]) {
			s.CostCt += prices[i] * powerW * SlotLen.Hours() / 1000
		}
	}
	if len(s.Slots) > 0 {
		s.Start = s.Slots[0]
	}
	return s
}

// NeedWh is the grid energy to bring a battery from soc to target.
func NeedWh(soc, target, capacityWh float64) float64 {
	if math.IsNaN(soc) || capacityWh <= 0 {
		return math.NaN()
	}
	return math.Max(0, target-soc) * capacityWh / Efficiency
}
