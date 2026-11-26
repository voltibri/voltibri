// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package tariff turns the configured tariffs into prices per 15-minute
// slot (ct/kWh). Three kinds exist, as in evcc:
//
//   - fixed:   one price, optionally with time zones (time-variable tariff,
//     e.g. cheaper at night or a §14a module 3 tariff)
//   - dynamic: an exchange price source plus charges and tax:
//     (spot + charges) * (1 + tax)
//
// A tariff is used for grid import, feed-in and optionally for a charger
// behind its own meter (e.g. metering concept 40 with a cheaper heat-pump /
// e-mobility tariff).
package tariff

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

// Slot is the slot length all prices are expanded to.
const Slot = 15 * time.Minute

// Config describes one tariff.
type Config struct {
	Type    string  `yaml:"type" json:"type"`                           // fixed | dynamic
	Price   float64 `yaml:"price,omitempty" json:"price,omitempty"`     // fixed: ct/kWh
	Zones   []Zone  `yaml:"zones,omitempty" json:"zones,omitempty"`     // fixed: time-variable prices
	Source  string  `yaml:"source,omitempty" json:"source,omitempty"`   // dynamic: device id of a price source
	Charges float64 `yaml:"charges,omitempty" json:"charges,omitempty"` // dynamic: ct/kWh added to the exchange price
	Tax     float64 `yaml:"tax,omitempty" json:"tax,omitempty"`         // dynamic: e.g. 0.19
}

// Zone overrides the price of a fixed tariff at certain times.
type Zone struct {
	Days  string  `yaml:"days,omitempty" json:"days,omitempty"`   // e.g. "Mo-Fr", "Sa,So"; empty = every day
	Hours string  `yaml:"hours,omitempty" json:"hours,omitempty"` // e.g. "22-6", "0-24"; empty = all day
	Price float64 `yaml:"price" json:"price"`
}

// Set bundles the tariffs of a site.
type Set struct {
	Currency string  `yaml:"currency,omitempty" json:"currency,omitempty"`
	Grid     *Config `yaml:"grid,omitempty" json:"grid,omitempty"`         // import price
	FeedIn   *Config `yaml:"feedin,omitempty" json:"feedin,omitempty"`     // feed-in compensation
	Charging *Config `yaml:"charging,omitempty" json:"charging,omitempty"` // chargers behind a separate meter
}

// Tariff delivers prices per slot.
type Tariff interface {
	// Prices returns one price per 15-minute slot from from (rounded down)
	// to to; unknown prices are NaN.
	Prices(ctx context.Context, from, to time.Time) ([]float64, error)
}

// Validate checks a tariff configuration; sources are the ids of devices
// that offer prices.
func (c *Config) Validate(sources map[string]bool) error {
	if c == nil {
		return nil
	}
	switch c.Type {
	case "fixed":
		for i, z := range c.Zones {
			if _, err := parseDays(z.Days); err != nil {
				return fmt.Errorf("zone %d: %w", i+1, err)
			}
			if _, _, err := parseHours(z.Hours); err != nil {
				return fmt.Errorf("zone %d: %w", i+1, err)
			}
		}
	case "dynamic":
		if c.Source == "" {
			return fmt.Errorf("dynamic tariff needs a source (device id of a price source)")
		}
		if sources != nil && !sources[c.Source] {
			return fmt.Errorf("source %q is no price source", c.Source)
		}
		if c.Tax < 0 || c.Tax > 1 {
			return fmt.Errorf("tax %.2f must be a fraction, e.g. 0.19", c.Tax)
		}
	default:
		return fmt.Errorf("unknown tariff type %q (fixed, dynamic)", c.Type)
	}
	return nil
}

// New builds a tariff; devices resolves price sources of dynamic tariffs.
func New(c *Config, loc *time.Location, devices map[string]sdk.Device) (Tariff, error) {
	if c == nil {
		return nil, nil
	}
	if err := c.Validate(nil); err != nil {
		return nil, err
	}
	if c.Type == "fixed" {
		f := &fixed{price: c.Price, loc: loc}
		for _, z := range c.Zones {
			days, _ := parseDays(z.Days)
			a, b, _ := parseHours(z.Hours)
			f.zones = append(f.zones, zone{days: days, from: a, to: b, price: z.Price})
		}
		return f, nil
	}
	src, ok := devices[c.Source].(sdk.TariffSource)
	if !ok {
		return nil, fmt.Errorf("tariff source %q provides no prices", c.Source)
	}
	return &dynamic{src: src, charges: c.Charges, tax: c.Tax}, nil
}

// Dynamic reports whether prices vary over time (zones or exchange prices).
func Dynamic(c *Config) bool {
	return c != nil && (c.Type == "dynamic" || len(c.Zones) > 0)
}

func slots(from, to time.Time) []time.Time {
	var out []time.Time
	for t := from.Truncate(Slot); t.Before(to); t = t.Add(Slot) {
		out = append(out, t)
	}
	return out
}

type zone struct {
	days     [7]bool
	from, to int // minutes since midnight, to may be < from (over midnight)
	price    float64
}

type fixed struct {
	price float64
	zones []zone
	loc   *time.Location
}

func (f *fixed) at(t time.Time) float64 {
	t = t.In(f.loc)
	m := t.Hour()*60 + t.Minute()
	for _, z := range f.zones {
		day := int(t.Weekday())
		in := false
		if z.from <= z.to {
			in = z.days[day] && m >= z.from && m < z.to
		} else { // over midnight: the evening part belongs to the day it starts
			prev := (day + 6) % 7
			in = (z.days[day] && m >= z.from) || (z.days[prev] && m < z.to)
		}
		if in {
			return z.price
		}
	}
	return f.price
}

func (f *fixed) Prices(_ context.Context, from, to time.Time) ([]float64, error) {
	ts := slots(from, to)
	out := make([]float64, len(ts))
	for i, t := range ts {
		out[i] = f.at(t)
	}
	return out, nil
}

type dynamic struct {
	src          sdk.TariffSource
	charges, tax float64
}

func (d *dynamic) Prices(ctx context.Context, from, to time.Time) ([]float64, error) {
	ts := slots(from, to)
	out := make([]float64, len(ts))
	for i := range out {
		out[i] = math.NaN()
	}
	if len(ts) == 0 {
		return out, nil
	}
	ps, err := d.src.Prices(ctx, ts[0], to)
	byUnix := map[int64]float64{}
	for _, p := range ps {
		dur := p.Duration
		if dur <= 0 {
			dur = time.Hour
		}
		for t := p.Start; t.Before(p.Start.Add(dur)); t = t.Add(Slot) {
			byUnix[t.Unix()] = p.Value
		}
	}
	for i, t := range ts {
		if v, ok := byUnix[t.Unix()]; ok {
			out[i] = (v + d.charges) * (1 + d.tax)
		}
	}
	return out, err
}

var weekdays = map[string]int{"so": 0, "mo": 1, "di": 2, "mi": 3, "do": 4, "fr": 5, "sa": 6,
	"su": 0, "tu": 2, "we": 3, "th": 4}

// parseDays reads "Mo-Fr", "Sa,So" or "" (every day).
func parseDays(s string) ([7]bool, error) {
	var d [7]bool
	if strings.TrimSpace(s) == "" {
		return [7]bool{true, true, true, true, true, true, true}, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		a, b, rng := strings.Cut(part, "-")
		i, ok := weekdays[strings.TrimSpace(a)]
		if !ok {
			return d, fmt.Errorf("unknown weekday %q (Mo, Di, Mi, Do, Fr, Sa, So)", a)
		}
		j := i
		if rng {
			if j, ok = weekdays[strings.TrimSpace(b)]; !ok {
				return d, fmt.Errorf("unknown weekday %q", b)
			}
		}
		for k := i; ; k = (k + 1) % 7 {
			d[k] = true
			if k == j {
				break
			}
		}
	}
	return d, nil
}

// parseHours reads "22-6" or "6:30-8" into minutes since midnight.
func parseHours(s string) (int, int, error) {
	if strings.TrimSpace(s) == "" {
		return 0, 24 * 60, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("hours %q: expected from-to, e.g. 22-6", s)
	}
	pa, err := clock(a)
	if err != nil {
		return 0, 0, err
	}
	pb, err := clock(b)
	return pa, pb, err
}

func clock(s string) (int, error) {
	s = strings.TrimSpace(s)
	h, m, _ := strings.Cut(s, ":")
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 24 {
		return 0, fmt.Errorf("invalid hour %q", s)
	}
	mm := 0
	if m != "" {
		if mm, err = strconv.Atoi(m); err != nil || mm < 0 || mm > 59 {
			return 0, fmt.Errorf("invalid minute %q", s)
		}
	}
	return hh*60 + mm, nil
}

// Average returns the mean slot price of t over a period (ct/kWh; NaN if t
// is nil, the period empty or a price unknown).
func Average(ctx context.Context, t Tariff, from, to time.Time) float64 {
	if t == nil || !to.After(from) {
		return math.NaN()
	}
	ps, err := t.Prices(ctx, from, to)
	if err != nil || len(ps) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, p := range ps {
		if math.IsNaN(p) {
			return math.NaN()
		}
		sum += p
	}
	return sum / float64(len(ps))
}
