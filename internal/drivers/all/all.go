// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package all registers all built-in drivers.
package all

import (
	_ "github.com/voltibri/voltibri/internal/drivers/awattar"
	_ "github.com/voltibri/voltibri/internal/drivers/demo"
	_ "github.com/voltibri/voltibri/internal/drivers/esphome"
	_ "github.com/voltibri/voltibri/internal/drivers/forecastsolar"
	_ "github.com/voltibri/voltibri/internal/drivers/fronius"
	_ "github.com/voltibri/voltibri/internal/drivers/goe"
	_ "github.com/voltibri/voltibri/internal/drivers/none"
	_ "github.com/voltibri/voltibri/internal/drivers/opendtu"
	_ "github.com/voltibri/voltibri/internal/drivers/shelly"
	_ "github.com/voltibri/voltibri/internal/drivers/tibber"
	_ "github.com/voltibri/voltibri/internal/drivers/vehicle"
)
