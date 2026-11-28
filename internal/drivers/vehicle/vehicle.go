// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package vehicle is a vehicle asset: it can be chosen at the wallbox, keeps
// its own charge mode and plans, and appears in the reports. Its live data
// (state of charge) comes from a data source – a vehicle account such as
// Tibber, bound in the configuration (params source and sourceVehicle) and
// resolved by the charging manager. Without one the vehicle is manual.
package vehicle

import (
	"context"
	"math"

	"github.com/voltibri/voltibri/sdk"
)

func init() {
	sdk.RegisterDriver(sdk.Driver{
		Type:         "vehicle",
		Name:         "Fahrzeug",
		Description:  "Ein Fahrzeug als eigener Eintrag: an der Wallbox auswählbar, mit eigenem Lademodus und eigenen Auswertungen. Ladestand und Ladeziel kommen aus einer wählbaren Datenquelle (Fahrzeugkonto) – oder es bleibt manuell.",
		Category:     sdk.CatVehicle,
		Capabilities: []sdk.Capability{sdk.CapVehicles},
		Params: []sdk.Param{
			{Name: "source", Label: "Datenquelle", Type: sdk.TypeString,
				Description: "Fahrzeugkonto, das Ladestand und Ladeziel liefert (z. B. Tibber). Leer = manuell, Ladestand unbekannt."},
			{Name: "sourceVehicle", Label: "Fahrzeug im Konto", Type: sdk.TypeString},
			{Name: "capacityKWh", Label: "Akkukapazität", Type: sdk.TypeFloat, Unit: "kWh", Min: sdk.Ptr(1), Max: sdk.Ptr(300)},
			{Name: "defaultSoC", Label: "Typischer Ladestand beim Anstecken", Type: sdk.TypeFloat, Unit: "%", Min: sdk.Ptr(0), Max: sdk.Ptr(100),
				Description: "Gilt beim Einstecken, solange weder die Datenquelle noch eine Eingabe von Hand den Ladestand kennt; voltibri rechnet ihn während des Ladens hoch. Leer = unbekannt."},
			{Name: "phases", Label: "Ladephasen", Type: sdk.TypeInt, Default: 3.0, Min: sdk.Ptr(1), Max: sdk.Ptr(3),
				Description: "Mit wie vielen Phasen das Auto lädt (manche Autos nur mit 1 oder 2)."},
			{Name: "maxCurrent", Label: "Max. Ladestrom", Type: sdk.TypeFloat, Unit: "A", Default: 16.0, Min: sdk.Ptr(6), Max: sdk.Ptr(32)},
			{Name: "brand", Label: "Marke", Type: sdk.TypeString, Advanced: true},
			{Name: "model", Label: "Modell", Type: sdk.TypeString, Advanced: true},
			{Name: "aliases", Label: "Frühere Namen", Type: sdk.TypeString, Advanced: true,
				Description: "Namen in älteren Ladevorgängen (z. B. aus dem evcc-Import), durch Komma getrennt. Die Berichte zählen sie zu diesem Fahrzeug."},
		},
		Factory: New,
	})
}

// Vehicle is one manual vehicle (provider of exactly one vehicle).
type Vehicle struct {
	id   string
	info sdk.VehicleInfo
}

// New creates the vehicle.
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	name := cfg.Name
	if name == "" {
		name = cfg.ID
	}
	phases := cfg.FloatOr("phases", 3)
	maxA := cfg.FloatOr("maxCurrent", 16)
	return &Vehicle{id: cfg.ID, info: sdk.VehicleInfo{ID: cfg.ID, Name: name, Brand: cfg.StringOr("brand", ""), Model: cfg.StringOr("model", ""),
		CapacityWh: math.Round(cfg.FloatOr("capacityKWh", 0) * 1000), MaxPowerW: math.Round(phases * maxA * 230),
		DefaultSoC: math.Max(0, math.Min(1, cfg.FloatOr("defaultSoC", 0)/100))}}, nil
}

func (v *Vehicle) ID() string { return v.id }

// Vehicles implements sdk.VehicleProvider.
func (v *Vehicle) Vehicles(context.Context) ([]sdk.VehicleInfo, error) {
	return []sdk.VehicleInfo{v.info}, nil
}

// VehicleStatus implements sdk.VehicleProvider: nothing is known.
func (v *Vehicle) VehicleStatus(context.Context, string) (sdk.VehicleStatus, error) {
	return sdk.VehicleStatus{SoC: math.NaN(), LimitSoC: math.NaN(), RangeKm: math.NaN()}, nil
}
