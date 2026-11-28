// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package site

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/sdk"
)

func TestBatterySoCFallbackAndByHand(t *testing.T) {
	cfg := config.Default()
	cfg.Battery.CapacityWh, cfg.Battery.ChargeEff, cfg.Battery.DischargeEff = 10000, 1, 1
	cache := sdk.NewMemCache()
	s := &Site{cfg: cfg, bat: socBat{}, batCache: cache}
	t0 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	fail := errors.New("modbus timeout")

	if v, src := s.batterySoC(math.NaN(), fail, 0, at(0)); !math.IsNaN(v) || src != "" {
		t.Fatalf("nothing known: %v %q", v, src)
	}
	if v, src := s.batterySoC(0.5, nil, 0, at(1)); v != 0.5 || src != BatSoCRead {
		t.Fatalf("read: %v %q", v, src)
	}
	// the reading fails: 60 min charging with 1 kW = +10 %
	var v float64
	var src string
	for m := 2; m <= 61; m++ {
		v, src = s.batterySoC(0, fail, -1000, at(m))
	}
	if math.Abs(v-0.6) > 1e-6 || src != BatSoCEstimated {
		t.Fatalf("estimated: %v %q", v, src)
	}
	// by hand; a reading that still says 50 % does not end it, a new one does
	s.state.SoC, s.state.SoCSource = 0.5, BatSoCRead
	if err := s.SetBatterySoC(ptrF(0.3)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBatterySoC(ptrF(-0.1)); err == nil {
		t.Fatal("negative accepted")
	}
	s.batterySoC(0.5, nil, 0, at(62))
	for m := 63; m <= 92; m++ { // 30 min discharging with 1 kW = −5 %
		v, src = s.batterySoC(0.5, nil, 1000, at(m))
	}
	if math.Abs(v-0.25) > 1e-6 || src != BatSoCManual {
		t.Fatalf("by hand: %v %q", v, src)
	}
	// restart: the value by hand is kept
	s2 := &Site{cfg: cfg, bat: socBat{}, batCache: cache}
	s2.loadBatSoC()
	if v, src := s2.batterySoC(0.5, nil, 0, at(93)); math.Abs(v-0.25) > 0.01 || src != BatSoCManual {
		t.Fatalf("after restart: %v %q", v, src)
	}
	if v, src := s.batterySoC(0.48, nil, 0, at(94)); v != 0.48 || src != BatSoCRead {
		t.Fatalf("read again: %v %q", v, src)
	}
}

func ptrF(f float64) *float64 { return &f }

func TestStateJSONUnknownSoC(t *testing.T) {
	b, err := json.Marshal(State{SoC: math.NaN(), GridW: 100})
	if err != nil || !strings.Contains(string(b), `"soc":null`) || !strings.Contains(string(b), `"gridW":100`) {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = json.Marshal(State{SoC: 0.5})
	if !strings.Contains(string(b), `"soc":0.5`) {
		t.Fatalf("%s", b)
	}
}
