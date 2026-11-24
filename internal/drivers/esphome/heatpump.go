// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package esphome reads a heat pump that publishes its values through an
// ESPHome node on MQTT (e.g. a Modbus bridge). voltibri only listens; it
// never writes to the heat pump.
//
// ESPHome publishes every sensor as <prefix>/sensor/<object_id>/state; the
// object id is the sensor name in lower case with every other character
// replaced by "_" ("Rücklauftemperatur" -> "r__cklauftemperatur"). The
// topics can be overridden per value.
package esphome

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/voltibri/voltibri/sdk"
)

// value names and their default topic below the prefix
var values = []struct{ key, label, topic string }{
	{"thermal", "Thermische Leistung (kW)", "sensor/thermische_leistung/state"},
	{"cop", "COP", "sensor/cop/state"},
	{"electric", "Verdichterleistung (W)", "sensor/verdichterleistung/state"},
	{"flow", "Vorlauftemperatur", "sensor/vorlauftemperatur/state"},
	{"return", "Rücklauftemperatur", "sensor/r__cklauftemperatur/state"},
	{"outdoor", "Außentemperatur", "sensor/umgebungstemperatur/state"},
	{"hotwater", "Warmwassertemperatur", "sensor/warmwassertemperatur/state"},
	{"heatTotal", "Wärmezähler (kWh)", "sensor/erzeugte_w__rmemenge/state"},
	{"electricTotal", "Stromzähler (kWh)", "sensor/energieverbrauch/state"},
	{"state", "Betriebszustand (Text)", "text_sensor/betriebszustand/state"},
}

func init() {
	params := []sdk.Param{
		{Name: "broker", Label: "MQTT-Broker", Type: sdk.TypeString, Required: true, Description: "z. B. tcp://mqtt.local:1883"},
		{Name: "username", Label: "Benutzer", Type: sdk.TypeString},
		{Name: "password", Label: "Passwort", Type: sdk.TypeSecret},
		{Name: "prefix", Label: "Topic-Präfix", Type: sdk.TypeString, Required: true, Description: "Name des ESPHome-Knotens, z. B. waermepumpe-modbus"},
	}
	for _, v := range values {
		params = append(params, sdk.Param{Name: "topic_" + v.key, Label: "Topic " + v.label, Type: sdk.TypeString, Advanced: true,
			Description: "Standard: <Präfix>/" + v.topic})
	}
	sdk.RegisterDriver(sdk.Driver{
		Type:         "esphome-heatpump",
		Name:         "Wärmepumpe über ESPHome (MQTT)",
		Description:  "Liest thermische Leistung, COP, Temperaturen und Zähler einer Wärmepumpe, die ein ESPHome-Knoten per MQTT veröffentlicht. Nur lesend.",
		Category:     sdk.CatMeter,
		Capabilities: []sdk.Capability{sdk.CapHeat},
		Params:       params,
		Factory:      New,
	})
}

// HeatPump keeps the latest values received on MQTT.
type HeatPump struct {
	id     string
	client mqtt.Client
	topics map[string]string // topic -> value key

	mu   sync.Mutex
	st   sdk.HeatStatus
	seen time.Time
}

// New connects to the broker and subscribes to the topics.
func New(cfg sdk.DeviceConfig) (sdk.Device, error) {
	broker, err := cfg.String("broker")
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(cfg.StringOr("prefix", ""), "/")
	if prefix == "" {
		return nil, fmt.Errorf("%s: prefix missing", cfg.ID)
	}
	h := &HeatPump{id: cfg.ID, topics: map[string]string{}, st: sdk.HeatStatus{ThermalW: math.NaN(), COP: math.NaN(), ElectricW: math.NaN(),
		FlowC: math.NaN(), ReturnC: math.NaN(), OutdoorC: math.NaN(), HotWaterC: math.NaN(), HeatWh: math.NaN(), ElectricWh: math.NaN()}}
	for _, v := range values {
		t := cfg.StringOr("topic_"+v.key, "")
		if t == "" {
			t = prefix + "/" + v.topic
		}
		h.topics[t] = v.key
	}
	// unique client id: old and new configuration overlap briefly
	opts := mqtt.NewClientOptions().AddBroker(broker).SetClientID(fmt.Sprintf("voltibri-%s-%d", cfg.ID, time.Now().UnixNano()%1e6)).
		SetUsername(cfg.StringOr("username", "")).SetPassword(cfg.StringOr("password", "")).
		SetAutoReconnect(true).SetConnectRetry(true).SetConnectRetryInterval(10 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			for t := range h.topics {
				c.Subscribe(t, 0, h.receive)
			}
			slog.Info("mqtt connected", "device", cfg.ID, "broker", broker)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) { slog.Warn("mqtt connection lost", "device", cfg.ID, "err", err) })
	h.client = mqtt.NewClient(opts)
	h.client.Connect() // connects (and retries) in the background
	return h, nil
}

func (h *HeatPump) ID() string { return h.id }

func (h *HeatPump) receive(_ mqtt.Client, m mqtt.Message) {
	key, ok := h.topics[m.Topic()]
	if !ok {
		return
	}
	s := strings.TrimSpace(string(m.Payload()))
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = time.Now()
	h.st.Updated = h.seen
	if key == "state" {
		h.st.State = s
		return
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		f = math.NaN() // ESPHome publishes "nan" while a value is unknown
	}
	switch key {
	case "thermal":
		h.st.ThermalW = f * 1000
	case "cop":
		h.st.COP = f
	case "electric":
		h.st.ElectricW = f
	case "flow":
		h.st.FlowC = f
	case "return":
		h.st.ReturnC = f
	case "outdoor":
		h.st.OutdoorC = f
	case "hotwater":
		h.st.HotWaterC = f
	case "heatTotal":
		h.st.HeatWh = f * 1000
	case "electricTotal":
		h.st.ElectricWh = f * 1000
	}
}

// HeatStatus implements sdk.HeatSource.
func (h *HeatPump) HeatStatus(context.Context) (sdk.HeatStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.client.IsConnected() && h.seen.IsZero() {
		return h.st, fmt.Errorf("%s: no connection to the MQTT broker", h.id)
	}
	if !h.seen.IsZero() && time.Since(h.seen) > 5*time.Minute {
		return h.st, fmt.Errorf("%s: no values for %s", h.id, time.Since(h.seen).Round(time.Minute))
	}
	return h.st, nil
}

// Close disconnects (called when the configuration changes).
func (h *HeatPump) Close() error {
	h.client.Disconnect(250)
	return nil
}
