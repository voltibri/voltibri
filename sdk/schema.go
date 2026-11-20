// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// ParamType is the data type of a driver parameter.
type ParamType string

const (
	TypeString   ParamType = "string"
	TypeHost     ParamType = "host"     // hostname or IP address
	TypeURL      ParamType = "url"      // http(s) URL
	TypeInt      ParamType = "int"      // whole number
	TypeFloat    ParamType = "float"    // decimal number
	TypeBool     ParamType = "bool"     // true/false
	TypeDuration ParamType = "duration" // Go duration, e.g. "30s", "5m"
	TypeSecret   ParamType = "secret"   // password / token, stored encrypted, never shown again
	TypeEnum     ParamType = "enum"     // one of Enum
	TypeList     ParamType = "list"     // list of objects described by Items
)

// Param describes one driver parameter for validation and form generation.
type Param struct {
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Description string    `json:"description,omitempty"`
	Type        ParamType `json:"type"`
	Required    bool      `json:"required,omitempty"`
	Default     any       `json:"default,omitempty"`
	Enum        []string  `json:"enum,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	Min         *float64  `json:"min,omitempty"`
	Max         *float64  `json:"max,omitempty"`
	Advanced    bool      `json:"advanced,omitempty"` // hidden behind "advanced settings"
	Items       []Param   `json:"items,omitempty"`    // for TypeList
}

// Ptr is a helper for Min/Max.
func Ptr(f float64) *float64 { return &f }

// Validate checks params against the driver schema, converts values to
// their canonical Go types (string, float64, bool, []any of map[string]any)
// and applies defaults. Unknown parameters are rejected to catch typos.
// Drivers without schema accept anything unchanged.
func (d Driver) Validate(params map[string]any) (map[string]any, error) {
	if d.Params == nil {
		if params == nil {
			params = map[string]any{}
		}
		return params, nil
	}
	return validateParams(d.Params, params, "")
}

func validateParams(schema []Param, in map[string]any, prefix string) (map[string]any, error) {
	out := map[string]any{}
	known := map[string]bool{}
	var errs []error
	for _, p := range schema {
		known[p.Name] = true
		v, ok := in[p.Name]
		if !ok || v == nil || v == "" {
			if p.Default != nil {
				out[p.Name] = p.Default
			} else if p.Required {
				errs = append(errs, fmt.Errorf("%s%s: required", prefix, p.Name))
			}
			continue
		}
		cv, err := convert(p, v, prefix)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[p.Name] = cv
	}
	for k := range in {
		if !known[k] {
			errs = append(errs, fmt.Errorf("%s%s: unknown parameter", prefix, k))
		}
	}
	return out, errors.Join(errs...)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

func convert(p Param, v any, prefix string) (any, error) {
	name := prefix + p.Name
	switch p.Type {
	case TypeString, TypeSecret:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: must be text", name)
		}
		return s, nil
	case TypeHost:
		s, ok := v.(string)
		if !ok || strings.ContainsAny(s, "/ ") {
			return nil, fmt.Errorf("%s: must be a hostname or IP address", name)
		}
		return s, nil
	case TypeURL:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: must be a URL", name)
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("%s: must be an http(s) URL", name)
		}
		return s, nil
	case TypeInt, TypeFloat:
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("%s: must be a number", name)
		}
		if p.Type == TypeInt && f != math.Trunc(f) {
			return nil, fmt.Errorf("%s: must be a whole number", name)
		}
		if p.Min != nil && f < *p.Min {
			return nil, fmt.Errorf("%s: must be >= %v", name, *p.Min)
		}
		if p.Max != nil && f > *p.Max {
			return nil, fmt.Errorf("%s: must be <= %v", name, *p.Max)
		}
		return f, nil
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s: must be true or false", name)
		}
		return b, nil
	case TypeDuration:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: must be a duration like \"30s\"", name)
		}
		if _, err := time.ParseDuration(s); err != nil {
			return nil, fmt.Errorf("%s: must be a duration like \"30s\"", name)
		}
		return s, nil
	case TypeEnum:
		s, ok := v.(string)
		if ok {
			for _, e := range p.Enum {
				if s == e {
					return s, nil
				}
			}
		}
		return nil, fmt.Errorf("%s: must be one of %v", name, p.Enum)
	case TypeList:
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a list", name)
		}
		out := make([]any, 0, len(items))
		var errs []error
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				errs = append(errs, fmt.Errorf("%s[%d]: must be an object", name, i))
				continue
			}
			cm, err := validateParams(p.Items, m, fmt.Sprintf("%s[%d].", name, i))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, cm)
		}
		return out, errors.Join(errs...)
	}
	return nil, fmt.Errorf("%s: unsupported parameter type %q", name, p.Type)
}

// SecretParams returns the names of all secret parameters (top level).
func (d Driver) SecretParams() []string {
	var out []string
	for _, p := range d.Params {
		if p.Type == TypeSecret {
			out = append(out, p.Name)
		}
	}
	return out
}
