# voltibri

**Smart self-consumption optimizer** for homes with PV, battery, EV charger and heat pump.

voltibri plans ahead: it forecasts your household load, combines it with the PV forecast and
exchange prices, and decides how much your battery should charge in every 15-minute slot.

> Status: early MVP (stage 0). Battery planning, simulation and read access to devices work.
> Device control is **untested against real hardware** – run with `dryRun: true` first.

## Why another energy manager?

| | voltibri |
|---|---|
| **Late charging** | On sunny days the battery charges only as much as the night needs, as late as possible – instead of sitting at 100 % from 10 a.m. on. |
| **Grid friendly** | The needed energy is taken preferably from slots with negative exchange prices; optionally the battery is filled completely there. |
| **Explainable** | Every decision comes with a plain-language reason ("Genug Sonne: 13.5 kWh Überschuss erwartet, …"). |
| **Modular** | Devices are plugins with capabilities (`GridMeter`, `Battery`, `BatteryController`, `PVForecaster`, …) and a control mode (`owned`, `delegated`, `observed`). |
| **Simulation first** | Replay your history (e.g. from Home Assistant) and see what a strategy would have changed before letting it touch your inverter. |

## How the planner works (stage 0)

1. **Next morning**: the first slot after the night from which PV covers the load again.
2. **Night consumption**: forecast from the last comparable days (workday / Saturday /
   Sunday+holiday, weighted by age, temperature corrected for heat pumps), upper quantile (P80).
3. **Evening target** = morning buffer (25 %) + night consumption + safety margin (5 %).
4. If the (pessimistic, 80 %) PV surplus comfortably exceeds the missing energy, the battery
   charges only what is needed – negative-price slots first, otherwise latest first.
   Otherwise it charges immediately.
5. Re-planned every 15 minutes, so a cloudy afternoon automatically pulls charging forward.

Simulated on a synthetic year (10 kWp, 13.8 kWh, heat pump):

| | charge immediately | voltibri |
|---|---|---|
| Grid import | 7730 kWh | 7730 kWh |
| Hours with SoC ≥ 90 % | 1485 h | 373 h |
| Hours with full battery | 1182 h | 123 h |
| Battery charged at negative prices | 16 kWh | 270 kWh |

Run it yourself: `voltibri simulate`.

## Quick start

```bash
go build -o bin/voltibri ./cmd/voltibri

# compare strategies on synthetic data
bin/voltibri simulate

# … or on your own Home Assistant history
bin/voltibri import-ha -in history.csv -out mydata.csv \
  -pv sensor.pv_power,sensor.opendtu_power -grid sensor.grid_power -battery sensor.battery_power -temp sensor.outdoor_temp
bin/voltibri simulate -input mydata.csv

# live demo without hardware, web UI on http://127.0.0.1:7070
bin/voltibri config import -data ./data examples/demo.yaml
bin/voltibri serve -data ./data
```

For a real setup start from [`examples/voltibri.yaml`](examples/voltibri.yaml).

## Configuration

The configuration lives in a SQLite database in the data directory (`-data`, env `VOLTIBRI_DATA`)
together with the measurement history. Every change creates a new version (author, comment) and
takes effect immediately, without restart.

| Task | Command / API |
|---|---|
| Import a YAML file as new version | `voltibri config import FILE.yaml` · `POST /api/config/import` |
| Export (secrets redacted) | `voltibri config export` · `GET /api/config/export` |
| List versions | `voltibri config versions` · `GET /api/config/versions` |
| Roll back | `voltibri config rollback ID` · `POST /api/config/versions/{id}/rollback` |
| Edit (JSON) | `GET` / `PUT /api/config` |
| Driver schemas for forms | `GET /api/drivers` |

**Read-only mode** for GitOps / Kubernetes ConfigMaps: `voltibri serve -config voltibri.yaml` –
the file wins, is reloaded when it changes, and the UI cannot edit the configuration.

**Secrets** (passwords, tokens) are encrypted with AES-256-GCM. The key is `data/secret.key`
(created on first start) or `VOLTIBRI_SECRET_KEY` – back it up together with the database.

**Measurement corrections** per device, e.g. a go-e charger reporting 200 W too little while charging:

```yaml
correction: { factor: 1.0, offsetW: 200, activeAboveW: 50 }
```

> Until users and roles exist, configuration changes via the API are only accepted from the
> local machine.

## Drivers

| Type | Capabilities | Notes |
|---|---|---|
| `fronius` | grid, PV, battery, battery control, phase currents | Solar API (read) + Modbus SunSpec model 124 (charge limit with revert timeout) |
| `opendtu` | PV | REST API |
| `go-e` | charger, charger control, phase currents, phase switching, energy | local HTTP API v2 |
| `shelly` | switch, consumer power, current, energy | Gen2+ RPC |
| `forecast.solar` | PV forecast | free API, multiple planes |
| `awattar` | spot prices (DE-LU) | public API |
| `demo`, `demo-producer`, `demo-consumer` | everything | simulated site |

Every driver describes its parameters (type, unit, default, required, secret) so the UI can
generate its settings form – including plugins.

## Roadmap

- Users, roles, OIDC – then configuration UI (Vue 3) with setup wizard and device discovery
- Circuits (sub-distributions) with per-phase load management, shown in the energy flow
- MQTT (in/out, Home Assistant discovery)
- EV charging with PV surplus, charging sessions and reports
- Price rules per user (e.g. neighbour pays generation cost, own use at lost feed-in tariff)
- Heat pump control (SG-Ready / Modbus)
- External plugins via gRPC
- Forecast stage 1 (gradient boosting) and learned departure times

## License

- Core: [AGPL-3.0](LICENSE)
- Plugin SDK ([`sdk/`](sdk)): [Apache-2.0](sdk/LICENSE) – write plugins under any license
- Commercial licenses for the core are available on request.

Contributions require agreeing to the Contributor License Agreement, see [CONTRIBUTING.md](CONTRIBUTING.md).
"voltibri" is the project's name; please do not use it for forks or derived products.
