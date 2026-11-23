package config_test

import (
	"strings"
	"testing"

	"github.com/voltibri/voltibri/internal/config"
	_ "github.com/voltibri/voltibri/internal/drivers/all"
)

func TestExamplesAreValid(t *testing.T) {
	for _, f := range []string{"../../examples/voltibri.yaml", "../../examples/demo.yaml"} {
		c, err := config.Load(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if c.Ready() != nil {
			t.Errorf("%s: not ready: %v", f, c.Ready())
		}
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := config.Parse([]byte(`
roles:
  grid: plug
  battery: plug
devices:
  - id: plug
    type: shelly
    mode: sometimes
    params: { hots: 1.2.3.4 }
    correction: { factor: -1 }
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		`unknown mode "sometimes"`,
		`hots: unknown parameter`,
		`correction factor must be positive`,
		`roles.grid: device "plug" (shelly) cannot act as grid`,
		`battery.capacityWh is required`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestUnknownYAMLKeysRejected(t *testing.T) {
	if _, err := config.Parse([]byte("sitee:\n  timezone: x\n")); err == nil {
		t.Error("typo in top-level key must be rejected")
	}
}

func TestEmptyConfigIsValidButNotReady(t *testing.T) {
	c, err := config.Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ready() == nil {
		t.Error("empty config must not be ready")
	}
}

func TestCircuits(t *testing.T) {
	ok := `
roles: { grid: demo, pv: [demo] }
grid: { fuseA: 63 }
devices:
  - { id: demo, type: demo }
  - { id: a, type: demo-consumer }
  - { id: b, type: demo-consumer }
circuits:
  - { id: wb, name: Wallboxen, maxCurrent: 32, devices: [a, b] }
`
	if _, err := config.Parse([]byte(ok)); err != nil {
		t.Fatalf("valid circuits rejected: %v", err)
	}
	bad := `
roles: { grid: demo, pv: [demo] }
devices:
  - { id: demo, type: demo }
  - { id: a, type: demo-consumer }
circuits:
  - { id: x, maxCurrent: 32, parent: y, devices: [a, ghost] }
  - { id: y, maxCurrent: 16, parent: x, devices: [a], meter: a }
  - { id: grid, maxCurrent: 1 }
`
	_, err := config.Parse([]byte(bad))
	if err == nil {
		t.Fatal("invalid circuits accepted")
	}
	for _, want := range []string{`unknown device "ghost"`, `device "a" is in circuits`, "loop", `must not be "grid"`, "cannot measure phase currents"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestLifecycleAndKinds(t *testing.T) {
	cfg, err := config.Parse([]byte(`
roles: { grid: demo, pv: [demo] }
devices:
  - { id: demo, type: demo }
  - { id: openwb, name: OpenWB, type: none, kind: ev-charger, commissioned: "2024-09-01", decommissioned: "2025-02-15" }
  - { id: rod, type: none, icon: water, decommissioned: "2025-10-20" }
`))
	if err != nil {
		t.Fatalf("retired devices rejected: %v", err)
	}
	if k := config.KindOf(cfg.Devices[1]); k != config.KindEVCharger || config.RunKind(k) != config.RunCharging {
		t.Errorf("openwb kind %q", k)
	}
	if k := config.KindOf(cfg.Devices[2]); k != config.KindHeatingRod || config.RunKind(k) != config.RunHeating {
		t.Errorf("rod kind %q", k)
	}
	if k := config.KindOf(cfg.Devices[0]); k != config.KindInverter {
		t.Errorf("demo kind %q", k)
	}
	_, err = config.Parse([]byte(`
roles: { grid: demo, pv: [demo] }
devices:
  - { id: demo, type: demo, decommissioned: "2020-01-01" }
  - { id: x, type: none, kind: toaster, commissioned: "2025-05-01", decommissioned: "2025-01-01" }
  - { id: y, type: none, commissioned: "1.5.2025" }
`))
	for _, want := range []string{"decommissioned but still used", `unknown kind "toaster"`, "decommissioned before commissioned", "YYYY-MM-DD"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
