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
bin/voltibri serve -config examples/demo.yaml
```

For a real setup start from [`examples/voltibri.yaml`](examples/voltibri.yaml).

## Drivers

| Type | Capabilities | Notes |
|---|---|---|
| `fronius` | grid, PV, battery, battery control | Solar API (read) + Modbus SunSpec model 124 (charge limit with revert timeout) |
| `opendtu` | PV | REST API |
| `go-e` | charger, charger control | local HTTP API v2 |
| `shelly` | switch, consumer power | Gen2+ RPC |
| `forecast.solar` | PV forecast | free API, multiple planes |
| `awattar` | spot prices (DE-LU) | public API |
| `demo` | everything | simulated site |

## Roadmap

- MQTT (in/out, Home Assistant discovery)
- EV charging with PV surplus, charging sessions and reports
- Users, roles, OIDC
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
