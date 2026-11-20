// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"fmt"
	"sort"
	"sync"
)

// ControlMode defines who is allowed to control a device.
type ControlMode string

const (
	// ModeOwned: voltibri plans and controls the device.
	ModeOwned ControlMode = "owned"
	// ModeDelegated: an external program (e.g. a utility's VPP) controls the
	// device; voltibri only observes it and plans around it.
	ModeDelegated ControlMode = "delegated"
	// ModeObserved: measurement only, never written to.
	ModeObserved ControlMode = "observed"
)

// DeviceConfig is the configuration passed to a driver factory.
type DeviceConfig struct {
	ID     string
	Type   string
	Mode   ControlMode
	DryRun bool           // log writes instead of executing them
	Params map[string]any // driver specific settings
}

// Factory creates a device from its configuration.
type Factory func(cfg DeviceConfig) (Device, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a driver available under the given type name.
// It is typically called from a driver package's init function.
func Register(typ string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[typ]; dup {
		panic("sdk: driver registered twice: " + typ)
	}
	factories[typ] = f
}

// New creates a device using the registered driver for cfg.Type.
func New(cfg DeviceConfig) (Device, error) {
	mu.RLock()
	f, ok := factories[cfg.Type]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown device type %q (available: %v)", cfg.Type, Types())
	}
	return f(cfg)
}

// Types returns the names of all registered drivers.
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
