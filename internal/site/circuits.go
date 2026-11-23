// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"context"
	"sync"

	"github.com/voltibri/voltibri/internal/circuit"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/sdk"
)

// circuitNodes builds the installation tree from the configuration (nil if
// neither a main fuse nor sub-distributions are configured).
func circuitNodes(cfg *config.Config) []circuit.Node {
	if cfg.Grid.FuseA <= 0 && len(cfg.Circuits) == 0 {
		return nil
	}
	nodes := []circuit.Node{{ID: circuit.Root, Name: "Hausanschluss", MaxA: cfg.Grid.FuseA, Phases: cfg.Grid.Phases, Meter: true}}
	for _, c := range cfg.Circuits {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		nodes = append(nodes, circuit.Node{ID: c.ID, Name: name, Parent: c.Parent, MaxA: c.MaxCurrentA, Phases: c.Phases,
			Meter: c.Meter != "", Devices: c.Devices})
	}
	return nodes
}

// measureCircuits reads phase currents (in parallel, they are extra
// requests) and aggregates the circuits. powers holds the corrected power
// of every consumer; phases the phase count of chargers.
func (s *Site) measureCircuits(ctx context.Context, powers map[string]float64, phases map[string]int, note func(error)) *circuit.Tree {
	if s.circuits == nil {
		return nil
	}
	want := map[string]bool{} // devices whose currents we need
	for id := range powers {
		want[id] = true
	}
	meterOf := map[string]string{} // device -> circuit it meters
	meterOf[s.cfg.Roles.Grid] = circuit.Root
	for _, c := range s.cfg.Circuits {
		if c.Meter != "" {
			meterOf[c.Meter] = c.ID
		}
	}
	for id := range meterOf {
		want[id] = true
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		currents = map[string]circuit.Phases{}
	)
	for id := range want {
		pc, ok := s.devices[id].(sdk.PhaseCurrents)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(id string, pc sdk.PhaseCurrents) {
			defer wg.Done()
			p, err := pc.Currents(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				note(err)
				return
			}
			currents[id] = s.corr[id].Currents(p, powers[id])
		}(id, pc)
	}
	wg.Wait()

	readings := map[string]circuit.Reading{}
	for id, w := range powers {
		r := circuit.Reading{PowerW: w, Phases: phases[id]}
		if c, ok := currents[id]; ok {
			r.Currents, r.HasCurrents = c, true
		}
		readings[id] = r
	}
	meters := map[string]circuit.Phases{}
	for dev, c := range meterOf {
		if p, ok := currents[dev]; ok {
			meters[c] = p
		}
	}
	return circuit.Compute(s.circuits, readings, meters)
}
