// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package circuit models the electrical installation as a tree: the house
// connection (root) and sub-distributions with their own fuse, e.g. two
// wallboxes sharing 32 A. It aggregates measured phase currents and gives
// controllable devices the current they may still draw (load management).
package circuit

import (
	"math"
	"sort"
)

// Voltage per phase used to estimate currents from power.
const Voltage = 230.0

// Root is the ID of the house connection.
const Root = "grid"

// Phases holds one value per phase (L1, L2, L3).
type Phases = [3]float64

// Node is one circuit.
type Node struct {
	ID      string
	Name    string
	Parent  string  // "" or Root: directly behind the house connection
	MaxA    float64 // fuse per phase; 0 = no limit
	Phases  int     // 1 or 3
	Meter   bool    // MeterCurrents holds a measurement of the whole circuit
	Devices []string
}

// Reading is what is known about one device.
type Reading struct {
	Currents    Phases
	HasCurrents bool
	PowerW      float64
	Phases      int // phases the device uses when only power is known (0 = circuit phases)
}

// Status is a circuit after aggregation.
type Status struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Parent      string   `json:"parent,omitempty"`
	MaxA        float64  `json:"maxA"`
	Phases      int      `json:"phases"`
	CurrentsA   Phases   `json:"currentsA"`
	PowerW      float64  `json:"powerW"`
	Estimated   bool     `json:"estimated"` // part of the load was estimated from power
	Devices     []string `json:"devices"`
	Utilization float64  `json:"utilization"` // highest phase / MaxA (0 without limit)
	Over        bool     `json:"over"`
}

// Tree is the aggregated installation for one measurement.
type Tree struct {
	st        map[string]*Status
	order     []string // root first, then by name
	parent    map[string]string
	circuitOf map[string]string // device -> circuit
	draw      map[string]Phases // device -> measured or estimated currents
}

// Compute aggregates the readings. meters holds the currents of circuits
// with their own meter (key: circuit ID, Root for the grid meter).
func Compute(nodes []Node, readings map[string]Reading, meters map[string]Phases) *Tree {
	t := &Tree{st: map[string]*Status{}, parent: map[string]string{}, circuitOf: map[string]string{}, draw: map[string]Phases{}}
	children := map[string][]string{}
	for _, n := range nodes {
		p := n.Parent
		if n.ID == Root {
			p = ""
		} else if p == "" {
			p = Root
		}
		ph := n.Phases
		if ph != 1 {
			ph = 3
		}
		t.st[n.ID] = &Status{ID: n.ID, Name: n.Name, Parent: p, MaxA: n.MaxA, Phases: ph, Devices: append([]string{}, n.Devices...)}
		t.parent[n.ID] = p
		if p != "" {
			children[p] = append(children[p], n.ID)
		}
		for _, d := range n.Devices {
			t.circuitOf[d] = n.ID
		}
	}
	if _, ok := t.st[Root]; !ok {
		t.st[Root] = &Status{ID: Root, Name: "Hausanschluss", Phases: 3}
	}
	var visit func(id string, depth int) (Phases, float64, bool)
	visit = func(id string, depth int) (Phases, float64, bool) {
		s := t.st[id]
		var sum Phases
		var power float64
		est := false
		if depth > 16 { // cycle guard (validated in config)
			return sum, 0, true
		}
		for _, d := range s.Devices {
			r, ok := readings[d]
			if !ok {
				continue
			}
			power += r.PowerW
			if r.HasCurrents {
				for i := range sum {
					sum[i] += r.Currents[i]
				}
				t.draw[d] = r.Currents
				continue
			}
			if math.Abs(r.PowerW) < 1 {
				continue
			}
			est = true
			n := r.Phases
			if n == 0 {
				n = s.Phases
			}
			for i := 0; i < n && i < 3; i++ {
				sum[i] += math.Abs(r.PowerW) / Voltage / float64(n)
			}
			t.draw[d] = Spread(math.Abs(r.PowerW)/Voltage/float64(n), n)
		}
		kids := children[id]
		sort.Strings(kids)
		for _, c := range kids {
			cs, cp, ce := visit(c, depth+1)
			for i := range sum {
				sum[i] += cs[i]
			}
			power += cp
			est = est || ce
		}
		if m, ok := meters[id]; ok {
			sum, est = m, false
		}
		s.CurrentsA, s.PowerW, s.Estimated = sum, power, est
		s.update()
		return sum, power, est
	}
	visit(Root, 0)
	t.order = []string{Root}
	var rest []string
	for id := range t.st {
		if id != Root {
			rest = append(rest, id)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return t.st[rest[i]].Name < t.st[rest[j]].Name })
	t.order = append(t.order, rest...)
	return t
}

func (s *Status) update() {
	s.Utilization, s.Over = 0, false
	if s.MaxA > 0 {
		s.Utilization = maxOf(s.CurrentsA) / s.MaxA
		s.Over = s.Utilization > 1.0001
	}
}

func maxOf(p Phases) float64 { return math.Max(p[0], math.Max(p[1], p[2])) }

// Statuses returns all circuits, the house connection first.
func (t *Tree) Statuses() []Status {
	out := make([]Status, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, *t.st[id])
	}
	return out
}

// CircuitOf returns the circuit a device belongs to (Root if none).
func (t *Tree) CircuitOf(device string) string {
	if c, ok := t.circuitOf[device]; ok {
		return c
	}
	return Root
}

// Headroom returns the highest current per phase the device may draw,
// given what it draws now (own), and the circuit that limits it. Circuits
// without a fuse do not limit (+Inf).
func (t *Tree) Headroom(device string, own Phases) (float64, string) {
	limit, by := math.Inf(1), ""
	for id, n := t.CircuitOf(device), 0; id != "" && n < 17; id, n = t.parent[id], n+1 {
		s := t.st[id]
		if s == nil || s.MaxA <= 0 {
			continue
		}
		var others float64
		for i := range own {
			others = math.Max(others, s.CurrentsA[i]-own[i])
		}
		if a := math.Max(0, s.MaxA-others); a < limit {
			limit, by = a, s.Name
		}
	}
	return limit, by
}

// Commit books a changed draw of the device (from own to next) on all its
// circuits, so the next device sees the reduced headroom.
func (t *Tree) Commit(device string, own, next Phases) {
	for id, n := t.CircuitOf(device), 0; id != "" && n < 17; id, n = t.parent[id], n+1 {
		s := t.st[id]
		if s == nil {
			continue
		}
		for i := range own {
			s.CurrentsA[i] += next[i] - own[i]
		}
		s.update()
	}
}

// Spread returns the per-phase currents of a charger drawing a amps on n phases.
func Spread(a float64, n int) Phases {
	var p Phases
	for i := 0; i < n && i < 3; i++ {
		p[i] = a
	}
	return p
}

// Draw returns the measured (or estimated) currents of a device that is
// part of a circuit; false if the device is not in any circuit.
func (t *Tree) Draw(device string) (Phases, bool) {
	p, ok := t.draw[device]
	return p, ok
}
