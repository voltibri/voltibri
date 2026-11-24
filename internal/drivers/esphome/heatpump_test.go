package esphome

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

type msg struct{ topic, payload string }

func (m msg) Duplicate() bool   { return false }
func (m msg) Qos() byte         { return 0 }
func (m msg) Retained() bool    { return false }
func (m msg) Topic() string     { return m.topic }
func (m msg) MessageID() uint16 { return 0 }
func (m msg) Payload() []byte   { return []byte(m.payload) }
func (m msg) Ack()              {}

func TestReceive(t *testing.T) {
	h := &HeatPump{id: "wp", topics: map[string]string{}, st: sdk.HeatStatus{COP: math.NaN(), ThermalW: math.NaN()}}
	for _, v := range values {
		h.topics["waermepumpe-modbus/"+v.topic] = v.key
	}
	for _, m := range []msg{
		{"waermepumpe-modbus/sensor/thermische_leistung/state", "4.25"},
		{"waermepumpe-modbus/sensor/cop/state", "3.9"},
		{"waermepumpe-modbus/sensor/r__cklauftemperatur/state", "29.5"},
		{"waermepumpe-modbus/sensor/erzeugte_w__rmemenge/state", "1234.5"},
		{"waermepumpe-modbus/text_sensor/betriebszustand/state", "Heizen"},
		{"waermepumpe-modbus/sensor/vorlauftemperatur/state", "nan"},
		{"other/topic", "1"},
	} {
		h.receive(nil, m)
	}
	h.seen = time.Now()
	st := h.st
	if st.ThermalW != 4250 || st.COP != 3.9 || st.ReturnC != 29.5 || st.HeatWh != 1234500 || st.State != "Heizen" || !math.IsNaN(st.FlowC) {
		t.Fatalf("status %+v", st)
	}
	_ = context.Background()
}
