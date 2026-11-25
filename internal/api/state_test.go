// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/site"
)

func TestVisibleState(t *testing.T) {
	cop := 4.2
	cfg := &config.Config{Devices: []config.Device{{ID: "wp", Type: "none", Kind: config.KindHeatPump}, {ID: "rod", Type: "none", Kind: config.KindHeatingRod},
		{ID: "rod2", Type: "none", Kind: config.KindHeatingRod}, {ID: "fridge", Type: "none", Kind: config.KindAppliance}}}
	st := site.State{Heat: []site.HeatReading{{ID: "wp", COP: &cop}}, Errors: []string{"10.0.0.5: timeout"},
		Consumers: []site.Reading{{ID: "wp", Name: "WP", Group: "waermepumpe", PowerW: 900}, {ID: "rod", Group: "warmwasser", PowerW: 100},
			{ID: "rod2", Group: "warmwasser", PowerW: 50}, {ID: "fridge", Group: "haushalt", PowerW: 80}}}
	perms := func(g auth.Group) *auth.Principal {
		g.ID = "g"
		return auth.NewPrincipal(auth.User{Role: auth.RoleUser, Groups: []string{"g"}}, auth.Session{}, []auth.Group{g})
	}

	// flow without heat details: heat pump and rods as one total, no COP
	v := visibleState(perms(auth.Group{Flow: true}), st, cfg)
	if v.Heat != nil || v.Errors != nil || len(v.Consumers) != 2 {
		t.Fatalf("flow only: %+v", v)
	}
	if c := v.Consumers[0]; c.ID != "heat:heizung" || c.PowerW != 1050 || c.Group != "" {
		t.Fatalf("heating not combined: %+v", v.Consumers)
	}
	// with heat details: unchanged
	if v := visibleState(perms(auth.Group{Flow: true, Heat: true}), st, cfg); len(v.Consumers) != 4 || v.Heat == nil {
		t.Fatalf("flow+heat: %+v", v)
	}
	// heat card only: no energy flow
	if v := visibleState(perms(auth.Group{Heat: true}), st, cfg); len(v.Consumers) != 0 || v.Heat == nil {
		t.Fatalf("heat only: %+v", v)
	}
}
