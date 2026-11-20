// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package calendar classifies days into day types (workday, Saturday,
// Sunday/holiday) which drive the load profiles of the forecaster.
package calendar

import "time"

// DayType groups days with similar consumption behaviour.
type DayType int

const (
	Workday DayType = iota
	Saturday
	SundayOrHoliday
)

func (d DayType) String() string {
	switch d {
	case Workday:
		return "workday"
	case Saturday:
		return "saturday"
	default:
		return "sunday/holiday"
	}
}

// Calendar knows the German federal holidays plus user supplied extra days
// (regional holidays, vacation days, ...).
type Calendar struct {
	extra map[civilDate]bool
}

type civilDate struct {
	y int
	m time.Month
	d int
}

func dateOf(t time.Time) civilDate {
	y, m, d := t.Date()
	return civilDate{y, m, d}
}

// New creates a calendar. extra are additional holidays in format YYYY-MM-DD.
func New(extra ...string) (*Calendar, error) {
	c := &Calendar{extra: map[civilDate]bool{}}
	for _, s := range extra {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, err
		}
		c.extra[dateOf(t)] = true
	}
	return c, nil
}

// DayType returns the day type of t (evaluated in t's location).
func (c *Calendar) DayType(t time.Time) DayType {
	if c.IsHoliday(t) || t.Weekday() == time.Sunday {
		return SundayOrHoliday
	}
	if t.Weekday() == time.Saturday {
		return Saturday
	}
	return Workday
}

// IsHoliday reports whether t falls on a federal or configured holiday.
func (c *Calendar) IsHoliday(t time.Time) bool {
	d := dateOf(t)
	if c != nil && c.extra[d] {
		return true
	}
	return federalHoliday(d)
}

func federalHoliday(d civilDate) bool {
	switch {
	case d.m == time.January && d.d == 1,
		d.m == time.May && d.d == 1,
		d.m == time.October && d.d == 3,
		d.m == time.December && (d.d == 25 || d.d == 26):
		return true
	}
	easter := easterSunday(d.y)
	day := time.Date(d.y, d.m, d.d, 0, 0, 0, 0, time.UTC)
	diff := int(day.Sub(easter).Hours() / 24)
	switch diff {
	case -2, 1, 39, 50: // Karfreitag, Ostermontag, Christi Himmelfahrt, Pfingstmontag
		return true
	}
	return false
}

// easterSunday computes Easter Sunday (Gregorian, anonymous algorithm).
func easterSunday(y int) time.Time {
	a := y % 19
	b := y / 100
	c := y % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(y, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}
