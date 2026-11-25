// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package shelly

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

// A heating rod whose phases are switched by Shelly channels, one channel
// per phase (e.g. two Shelly 2PM per phase pair: channel 0 = rod A,
// channel 1 = rod B). The rod runs in steps: each switched-on phase adds a
// third of its power. Its own thermostat and safety cut-out stay wired in.
//
// Which phase a step switches is set by "phaseOrder":
//   - energy (default): a step up switches on the phase whose element has
//     heated least so far, a step down switches off the one that heated
//     most. The elements age evenly. The counters live in the Shelly (they
//     survive restarts); a per-phase correction in kWh accounts for a used
//     Shelly (subtract its old count) or a used element (add its past).
//   - random: any phase that is off (on), evenly over time.
//   - fixed: always L1 first, L3 last; a step down takes the last one back.
// Phases already in the right state are never switched: one step is one
// relay switching, whatever the order.

func init() {
	phase := func(n int) sdk.Param {
		p := sdk.Param{Name: fmt.Sprintf("l%d", n), Label: fmt.Sprintf("Kanal für L%d", n), Type: sdk.TypeString,
			Description: "Shelly und Kanal, z. B. shellypro2pm-1.internal:0 (Kanal 0 oder 1)"}
		if n == 1 {
			p.Required = true
			p.Description += ". Nur L1 eintragen, wenn der Heizstab einphasig ist."
		}
		return p
	}
	offset := func(n int) sdk.Param {
		return sdk.Param{Name: fmt.Sprintf("l%dOffsetKWh", n), Label: fmt.Sprintf("Zählerkorrektur L%d", n), Type: sdk.TypeFloat, Unit: "kWh", Advanced: true,
			Description: "Wird zum Energiezähler des Kanals addiert (nur für Reihenfolge „energy“). Gebrauchter Shelly: seinen alten Zählerstand negativ eintragen. Gebrauchtes Heizelement: seine bisherige Energie positiv."}
	}
	sdk.RegisterDriver(sdk.Driver{
		Type:         "shelly-rod",
		Name:         "Heizstab über Shelly-Kanäle",
		Description:  "Ein Heizstab, dessen Phasen einzeln über Shelly-Kanäle geschaltet werden (z. B. Shelly 2PM). Misst Leistung und Strom je Phase und schaltet in Stufen: jede Phase ist eine Stufe.",
		Category:     sdk.CatSwitch,
		Capabilities: []sdk.Capability{sdk.CapSwitch, sdk.CapStages, sdk.CapConsumer, sdk.CapPhases, sdk.CapEnergy},
		Params: []sdk.Param{
			phase(1), phase(2), phase(3),
			{Name: "phaseOrder", Label: "Reihenfolge der Phasen", Type: sdk.TypeEnum, Enum: []string{OrderEnergy, OrderRandom, OrderFixed}, Default: OrderEnergy,
				Description: "energy: die Phase, deren Heizelement bisher am wenigsten geheizt hat, zuerst (gleichmäßige Alterung der Heizelemente). random: zufällig. fixed: immer L1 zuerst."},
			offset(1), offset(2), offset(3),
			{Name: "powerKW", Label: "Nennleistung", Type: sdk.TypeFloat, Unit: "kW", Required: true, Min: sdk.Ptr(0.1), Max: sdk.Ptr(30),
				Description: "Leistung bei allen Phasen, z. B. 4,5 oder 6"},
		},
		Factory: NewRod,
	})
}

// Phase orders (param phaseOrder).
const (
	OrderEnergy = "energy"
	OrderRandom = "random"
	OrderFixed  = "fixed"
)

// rodPhase is the channel switching one phase of the rod.
type rodPhase struct {
	phase    int // 0..2 = L1..L3
	dev      *Device
	offsetWh float64 // added to the channel's energy counter
}

// Rod is a heating rod switched phase by phase.
type Rod struct {
	cfg    sdk.DeviceConfig
	phases []rodPhase
	powerW float64
	order  string
	randN  func(n int) int // random choice (replaced in tests)

	mu     sync.Mutex
	last   []switchStatus // per phase, from the latest read
	lastAt time.Time
	lastE  error
}

// readCache: power, currents and step are read in one control tick; the
// channels are fetched once for all of them.
const readCache = 2 * time.Second

// ParseChannel reads "host:channel" (channel 0 when omitted). A number
// above 3 after the colon is a port, not a channel: "host:8080:1" works too.
func ParseChannel(s string) (host string, channel int, err error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "http://"), "https://"))
	if s == "" {
		return "", 0, errors.New("empty")
	}
	host = s
	if i := strings.LastIndex(s, ":"); i > 0 {
		if n, e := strconv.Atoi(s[i+1:]); e == nil && n >= 0 && n <= 3 {
			host, channel = s[:i], n
		}
	}
	return host, channel, nil
}

// NewRod creates the rod from the channels l1..l3.
func NewRod(cfg sdk.DeviceConfig) (sdk.Device, error) {
	r := &Rod{cfg: cfg, powerW: cfg.FloatOr("powerKW", 0) * 1000, order: cfg.StringOr("phaseOrder", OrderEnergy), randN: rand.IntN}
	switch r.order {
	case OrderEnergy, OrderRandom, OrderFixed:
	default:
		return nil, fmt.Errorf("%s: phaseOrder %q unknown", cfg.ID, r.order)
	}
	if r.powerW <= 0 {
		return nil, fmt.Errorf("%s: powerKW missing", cfg.ID)
	}
	for i := 0; i < 3; i++ {
		v := cfg.StringOr(fmt.Sprintf("l%d", i+1), "")
		if v == "" {
			continue
		}
		host, ch, err := ParseChannel(v)
		if err != nil {
			return nil, fmt.Errorf("%s: l%d %q: %w", cfg.ID, i+1, v, err)
		}
		r.phases = append(r.phases, rodPhase{phase: i, dev: &Device{cfg: cfg, url: "http://" + host, channel: ch},
			offsetWh: cfg.FloatOr(fmt.Sprintf("l%dOffsetKWh", i+1), 0) * 1000})
	}
	if len(r.phases) == 0 {
		return nil, fmt.Errorf("%s: no channel (l1)", cfg.ID)
	}
	return r, nil
}

func (r *Rod) ID() string { return r.cfg.ID }

// read fetches all channels (one request each), at most every readCache.
func (r *Rod) read(ctx context.Context) ([]switchStatus, error) {
	r.mu.Lock()
	if r.last != nil && time.Since(r.lastAt) < readCache {
		defer r.mu.Unlock()
		return r.last, r.lastE
	}
	r.mu.Unlock()
	out := make([]switchStatus, len(r.phases))
	var errs []error
	for i, p := range r.phases {
		s, err := p.dev.status(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("L%d: %w", p.phase+1, err))
			continue
		}
		out[i] = s
	}
	err := errors.Join(errs...)
	r.mu.Lock()
	r.last, r.lastAt, r.lastE = out, time.Now(), err
	r.mu.Unlock()
	return out, err
}

// ConsumerPower implements sdk.PowerConsumer: the sum of all phases.
func (r *Rod) ConsumerPower(ctx context.Context) (float64, error) {
	ss, err := r.read(ctx)
	w := 0.0
	for _, s := range ss {
		w += s.APower
	}
	return w, err
}

// Currents implements sdk.PhaseCurrents: each channel on its phase.
func (r *Rod) Currents(ctx context.Context) (sdk.Phases, error) {
	ss, err := r.read(ctx)
	var p sdk.Phases
	for i, s := range ss {
		p[r.phases[i].phase] += s.Current
	}
	return p, err
}

// EnergyWh implements sdk.EnergyMeter: the sum of all channel counters.
func (r *Rod) EnergyWh(ctx context.Context) (float64, error) {
	ss, err := r.read(ctx)
	e := 0.0
	for _, s := range ss {
		e += s.AEnergy.Total
	}
	return e, err
}

// Steps implements sdk.Stager: one step per switched phase.
func (r *Rod) Steps() int { return len(r.phases) }

// StepPowerW implements sdk.Stager.
func (r *Rod) StepPowerW() float64 { return r.powerW / float64(len(r.phases)) }

// Step implements sdk.Stager: the number of phases switched on.
func (r *Rod) Step(ctx context.Context) (int, error) {
	ss, err := r.read(ctx)
	n := 0
	for _, s := range ss {
		if s.Output {
			n++
		}
	}
	return n, err
}

// SetStep implements sdk.Stager: n phases on, chosen by phaseOrder (see
// above). If the channels cannot be read, the first n phases
// are switched on and the others off.
func (r *Rod) SetStep(ctx context.Context, n int) error {
	if r.cfg.Mode != sdk.ModeOwned {
		return fmt.Errorf("%s: control not allowed in mode %s", r.cfg.ID, r.cfg.Mode)
	}
	n = max(0, min(n, len(r.phases)))
	ss, readErr := r.read(ctx)
	want := r.plan(ss, n)
	if readErr != nil {
		want = make([]bool, len(r.phases))
		for i := range want {
			want[i] = i < n
		}
	}
	if r.cfg.DryRun {
		slog.Info("dry-run: would switch heating rod", "device", r.cfg.ID, "step", n, "of", len(r.phases), "phases", phaseList(r.phases, want))
		return nil
	}
	var errs []error
	for i, p := range r.phases {
		if readErr == nil && ss[i].Output == want[i] {
			continue
		}
		if err := p.dev.SetSwitch(ctx, want[i]); err != nil {
			errs = append(errs, fmt.Errorf("L%d: %w", p.phase+1, err))
		}
	}
	r.mu.Lock()
	r.last = nil // read the new state on the next tick
	r.mu.Unlock()
	return errors.Join(errs...)
}

// plan returns which channels to switch on for n phases: phases already in
// the wanted state stay; the ones to add or drop are chosen by the order.
func (r *Rod) plan(ss []switchStatus, n int) []bool {
	want := make([]bool, len(ss))
	on := 0
	for i, s := range ss {
		want[i] = s.Output
		if s.Output {
			on++
		}
	}
	for ; on < n; on++ {
		want[r.pick(ss, want, false, true)] = true
	}
	for ; on > n; on-- {
		want[r.pick(ss, want, true, false)] = false
	}
	return want
}

// pick chooses among the channels in state from: up = the one to switch on.
func (r *Rod) pick(ss []switchStatus, want []bool, from, up bool) int {
	var cands []int
	for i := range ss {
		if want[i] == from {
			cands = append(cands, i)
		}
	}
	switch r.order {
	case OrderRandom:
		return cands[r.randN(len(cands))]
	case OrderFixed: // L1 first on, last off
		if up {
			return cands[0]
		}
		return cands[len(cands)-1]
	}
	// energy: least on, most off; ties go to the lower phase
	e := func(i int) float64 { return ss[i].AEnergy.Total + r.phases[i].offsetWh }
	best := cands[0]
	for _, i := range cands[1:] {
		if (up && e(i) < e(best)) || (!up && e(i) > e(best)) {
			best = i
		}
	}
	return best
}

// phaseList names the phases switched on, e.g. "L2 L3".
func phaseList(ps []rodPhase, on []bool) string {
	var out []string
	for i, p := range ps {
		if on[i] {
			out = append(out, fmt.Sprintf("L%d", p.phase+1))
		}
	}
	return strings.Join(out, " ")
}

// SwitchState implements sdk.Switch: on when any phase is on.
func (r *Rod) SwitchState(ctx context.Context) (bool, error) {
	n, err := r.Step(ctx)
	return n > 0, err
}

// SetSwitch implements sdk.Switch: all phases on or off.
func (r *Rod) SetSwitch(ctx context.Context, on bool) error {
	if on {
		return r.SetStep(ctx, len(r.phases))
	}
	return r.SetStep(ctx, 0)
}
