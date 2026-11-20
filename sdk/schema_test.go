package sdk

import (
	"strings"
	"testing"
)

func testDriver() Driver {
	return Driver{
		Type: "test",
		Params: []Param{
			{Name: "host", Type: TypeHost, Required: true},
			{Name: "port", Type: TypeInt, Default: 502.0, Min: Ptr(1), Max: Ptr(65535)},
			{Name: "password", Type: TypeSecret},
			{Name: "mode", Type: TypeEnum, Enum: []string{"a", "b"}, Default: "a"},
			{Name: "revert", Type: TypeDuration, Default: "5m"},
			{Name: "planes", Type: TypeList, Items: []Param{
				{Name: "kwp", Type: TypeFloat, Required: true},
				{Name: "az", Type: TypeFloat, Default: 0.0},
			}},
		},
		Factory: func(DeviceConfig) (Device, error) { return nil, nil },
	}
}

func TestValidateDefaultsAndConversion(t *testing.T) {
	out, err := testDriver().Validate(map[string]any{
		"host":   "192.168.1.5",
		"port":   1502, // int from YAML
		"planes": []any{map[string]any{"kwp": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["port"] != 1502.0 || out["mode"] != "a" || out["revert"] != "5m" {
		t.Errorf("unexpected %v", out)
	}
	plane := out["planes"].([]any)[0].(map[string]any)
	if plane["kwp"] != 5.0 || plane["az"] != 0.0 {
		t.Errorf("unexpected plane %v", plane)
	}
	if _, ok := out["password"]; ok {
		t.Error("absent optional param without default must stay absent")
	}
}

func TestValidateErrors(t *testing.T) {
	_, err := testDriver().Validate(map[string]any{
		"port":   70000,
		"mode":   "c",
		"revert": "soon",
		"typo":   1,
		"planes": []any{map[string]any{"az": 90}},
	})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"host: required", "port: must be <= 65535", "mode: must be one of", "revert: must be a duration", "typo: unknown", "planes[0].kwp: required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestSecretParams(t *testing.T) {
	if s := testDriver().SecretParams(); len(s) != 1 || s[0] != "password" {
		t.Errorf("got %v", s)
	}
}
