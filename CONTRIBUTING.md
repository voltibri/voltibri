# Contributing to voltibri

Thank you for helping! A few rules keep the project healthy.

## Contributor License Agreement (CLA)

voltibri is dual licensed: the core is available under the AGPL-3.0 and under commercial
licenses. To make this possible, every contributor must agree to the Contributor License
Agreement before a pull request can be merged. The CLA lets you keep the copyright of your
contribution while granting the maintainers the right to distribute it under other licenses
as well.

The CLA check runs automatically on your first pull request (CLA assistant).
<!-- TODO: add the CLA text (e.g. based on the Harmony CLA or the Apache ICLA) and set up
     https://github.com/cla-assistant/cla-assistant before accepting external contributions. -->

Contributions to `sdk/` are licensed under Apache-2.0.

## Development

```bash
go test ./...
go vet ./...
```

- Keep the core free of device specifics: devices are drivers implementing capabilities
  from `sdk/`.
- Drivers register with `sdk.RegisterDriver` and describe name, category, capabilities and
  every parameter (label, type, unit, default, secret) – the configuration UI is generated
  from it.
- Every driver that writes to hardware must honour `DryRun` and the control mode, and must
  make limits revert on the device side after a timeout.
- New planner behaviour needs a test and a simulation run (`voltibri simulate`) showing its
  effect.
- Source files carry an SPDX header (`AGPL-3.0-only` in the core, `Apache-2.0` in `sdk/`).
