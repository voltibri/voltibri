# voltibri data model

voltibri is generic from the start: a wallbox is one kind of load among
others (heat pump, heating rod, dehumidifier, ...). This document describes
the target model and the order in which it is introduced.

## Problems of the first model

- *Device* mixes **how** we talk to something (driver, host, credentials)
  with **what** it is (battery, wallbox, heating rod). One Fronius GEN24 is
  grid meter, PV and battery at once – hence the `roles` workaround. One
  Tibber account brings several vehicles, one OpenDTU several inverters.
- *Charging session* is too specific: heating rods and heat pumps have runs
  with energy, PV share and cost as well.
- No life cycle: replaced devices (old wallbox, heating rods, previous cars)
  must keep their history.
- History exists only for the whole house (PV, load), not per device.
- Provenance (measured, estimated, imported) exists only for sessions.

## Building blocks

### 1. Integration (today: the connection part of a device)

A connection to a system: driver, parameters, secrets, poll interval.
Examples: `fronius-hybrid`, `opendtu`, `tibber`, `awattar`, `forecast.solar`.
An integration offers **channels** with capabilities (measure, phase
currents, control): `fronius-hybrid.grid`, `fronius-hybrid.pv`,
`fronius-hybrid.battery`, `tibber.vehicle:<vin>`.

### 2. Asset (Anlage; today shown as "Gerät")

The physical thing, with a stable ID that is never reused.

| Field | Meaning |
|---|---|
| `kind` | `pv`, `battery`, `inverter`, `grid-meter`, `ev-charger`, `vehicle`, `heat-pump`, `heating-rod`, `climate`, `appliance`, `forecast`, `tariff`, `vehicle-account` – decides the area in the UI and the kind of its runs |
| bindings | `measure` (channel), `control` (channel), `correction` (belongs to the measuring binding) |
| control | `voltibri`, `program:<id>` (e.g. Ostrom NeoGrid), `external` (evcc, Home Assistant) |
| life cycle | `commissioned`, `decommissioned` – decommissioned assets are not polled but keep their history; assets with history are retired, not deleted |
| control (from the driver) | `none` (only measured), `switchable` (on/off: smart plug, relay, SG-Ready contact), `stepped` (a few fixed levels: SG-Ready states), `adjustable` (power set continuously: wallbox, heating rod with power controller, battery), `remote start` (start a prepared program: Home Connect, Miele) |
| flexibility (user policy) | `none` (default), `shiftable` (start can be delayed, the program then runs to its end: washing machine), `interruptible` (needs X kWh by a deadline, pauses are fine: e-bike, car, hot water by 6 pm), `surplus` (no target, runs only on PV surplus: heating rod, dehumidifier, pool pump) |
| runs | detect runs from the power (threshold, pause by kind); off for devices that cycle all day (fridge) – their consumption is still in the load profile |
| battery (appliances) | the device charges a battery (e-bike, power bank): `capacityWh`, `defaultSoC`, `limitSoC`, `efficiency`. voltibri estimates the state of charge from the measured energy and switches the plug off at the limit |

**State of charge (all batteries).** Vehicles, the home battery and
appliances with a battery share one estimate (package `internal/soc`): a
base value plus the energy charged since. The base is a value entered by
hand, the device's reading or a typical value when plugging in (vehicle
param `defaultSoC`, appliance `battery.defaultSoC`). A value by hand holds
until unplugged (vehicles, appliances) or until the battery reports a
different value (home battery); a reading that changes replaces it.
voltibri's own **charge limit** is a state of charge or, when it is
unknown, `+x %` of the capacity (5 % steps); charge plans accept `+x %`
too.

Every controllable device gets the same runtime modes as the wallbox today:
off/external, PV only, plan (target and deadline, as much PV and as cheap
as possible) and **boost** (now, full power, for a limited time or until
done; the wallbox "fast" mode is a boost).

Household devices (fridge, washing machine, oven, e-bike charger) are all
`appliance` ("Haushaltsgerät"); what differs is their flexibility, not
their kind. A fridge stays protected because switching requires an explicit
flexibility.

`roles` disappears: the grid meter is whatever measures the grid
connection, PV is every asset of kind `pv`.

### 3. Topology

A tree: grid connection → sub-distributions → assets. Every node may have a
fuse and a measuring binding; a node with its own meter can have its own
tariff (e.g. metering concept 40). Producers are part of the tree too.

### 4. Load profile (measurement series)

15-minute values per asset: import, export, the import split into PV /
battery / grid, cost, provenance. Stored compactly (device number, 0.1 Wh
integers, no extra index, slots without energy omitted): about 8–17 MB per
year for 14 devices. House values (PV, load, grid) are derived from it.

Load profile and runs complement each other: the profile answers *when and
how much* (complete, exact over midnight and with dynamic prices), runs
answer *what happened* (vehicle, mode, program; learning for control).
Reports per period use the profile where it exists and runs for the time
before (e.g. imported from evcc); the number of runs always comes from runs.

### 5. Runs (Läufe; today: charging sessions)

A continuous activity of an asset.

| Field | Meaning |
|---|---|
| `asset`, `kind` | `charging`, `heating`, `run` – follows from the asset kind |
| `start`, `end` | |
| `energyWh` = `pvWh` + `batteryWh` + `gridWh` | where the energy came from |
| `costCt`, `priceNote` | result and how it was computed (which rule on which level) |
| participants | vehicle (an asset, possibly retired), guest |
| attributes | odometer, SoC, mode, raw data of an import |
| provenance | `measured` or `import:<n>` |

The charging list is a view: runs of kind `charging`.

### 6. Provenance and imports

Every series value and run has a provenance: measured, estimated or
`import:<n>`. An import remembers source, parameters and mapping. Undo
deletes everything with that import number; values overwritten by an
import are saved first, so every import can be undone.

## Price rules

No "parties": prices can be overridden on several levels; the most specific
level that changes something wins, adjustments stack along the chain.

1. default tariff (grid import, feed-in; fixed, time zones, dynamic)
2. meter node of the topology (own meter, own tariff)
3. asset (e.g. a wallbox)
4. vehicle (e.g. the neighbour's car – applies at every wallbox)
5. single run (manual correction afterwards)

Guests without a recognised vehicle use the rule of their wallbox.

A rule handles every energy share separately (PV, battery, grid) or all at
once. Per share a level can:

| Mode | Meaning |
|---|---|
| inherit | take the price of the level above (default) |
| replace | own base: fixed price, lost feed-in tariff, generation cost, tariff, free |
| adjust | price above × (1 + percent) + fixed amount, optional min / max |

Example – neighbour: PV replaced by generation cost 11 ct, adjusted +10 %;
grid inherited; battery 11 ct + 3 ct wear. Taxes stay with the tariff;
amounts on lower levels are end prices.

Every run stores its calculation, e.g. "grid: 28.4 ct (tariff) + 2 ct
(wallbox Zaun) × 1.10 (vehicle neighbour) = 33.4 ct". Changing a rule does
not change old runs; "recalculate" applies the current rule to a period.

**Implemented so far: level 4 (vehicle)**, `price` on a vehicle device
(package `internal/pricing`), per share `inherit` / `replace` (fixed,
feedin, grid, free) / `adjust`, with percent and addCt; no min / max yet.
Charging runs keep their shares at both tariffs (`attrs.shares`), so a
rule prices them exactly also with dynamic tariffs. `POST /api/runs/recalc`
applies the saved rule to a vehicle's runs in a period; runs without
shares (imports) use the average tariff of their time, and the first
recalculation keeps the former cost in `attrs.origCostCt` /
`origPriceNote`. At a wallbox with its own meter the meter tariff still
applies, guests use the default.

## Order of introduction

1. **Done:** asset kind and life cycle on devices (devices without a
   connection for retired assets), runs instead of charging sessions with
   the PV / battery / grid split and provenance, the evcc import into runs;
   load profile per device, automatic runs for all measured consumers,
   flexibility (stored, control follows).
2. Integrations and channels separated from assets, bindings instead of
   `roles`, topology referencing assets; the UI keeps "add device" as one
   step.
3. Provenance for the load profile (HA import undoable); control of
   flexible devices (shiftable start, interruptible with deadline).
4. Price rules on the remaining levels (vehicle level done, see above), control by programs
   (NeoGrid), users and roles.
