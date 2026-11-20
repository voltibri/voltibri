// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"fmt"
	"time"
)

// Params helpers read typed values from DeviceConfig.Params.

// String returns a required string parameter.
func (c DeviceConfig) String(key string) (string, error) {
	v, ok := c.Params[key]
	if !ok {
		return "", fmt.Errorf("%s: missing parameter %q", c.ID, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: parameter %q must be a string", c.ID, key)
	}
	return s, nil
}

// StringOr returns a string parameter or a default.
func (c DeviceConfig) StringOr(key, def string) string {
	if s, err := c.String(key); err == nil {
		return s
	}
	return def
}

// Float returns a required numeric parameter.
func (c DeviceConfig) Float(key string) (float64, error) {
	v, ok := c.Params[key]
	if !ok {
		return 0, fmt.Errorf("%s: missing parameter %q", c.ID, key)
	}
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	}
	return 0, fmt.Errorf("%s: parameter %q must be a number", c.ID, key)
}

// FloatOr returns a numeric parameter or a default.
func (c DeviceConfig) FloatOr(key string, def float64) float64 {
	if f, err := c.Float(key); err == nil {
		return f
	}
	return def
}

// DurationOr returns a duration parameter (e.g. "30s") or a default.
func (c DeviceConfig) DurationOr(key string, def time.Duration) time.Duration {
	s, err := c.String(key)
	if err != nil {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// Slice returns a list parameter (e.g. a list of maps for PV planes).
func (c DeviceConfig) Slice(key string) []any {
	if v, ok := c.Params[key].([]any); ok {
		return v
	}
	return nil
}
