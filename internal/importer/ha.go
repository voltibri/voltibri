// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package importer converts Home Assistant history exports into the
// voltibri CSV format.
//
// Export in Home Assistant: History -> select the power sensors -> download
// (CSV with columns entity_id,state,last_changed). Power sensors must be in
// W (or kW with Scale = 1000).
package importer

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/voltibri/voltibri/internal/series"
)

// Mapping assigns HA entities to voltibri quantities. Load is either given
// directly or computed as grid + pv + battery (battery: + discharge).
type Mapping struct {
	PV      []string
	Load    string
	Grid    string
	Battery string
	Temp    string
	Scale   float64 // multiply power values (1 = W, 1000 = kW)
}

type sample struct {
	t time.Time
	v float64
}

// ConvertHA reads an HA history CSV and returns 15-minute records using
// time-weighted averages of the step-wise sensor values.
func ConvertHA(r io.Reader, m Mapping, loc *time.Location) (series.Series, error) {
	if m.Scale == 0 {
		m.Scale = 1
	}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	for _, c := range []string{"entity_id", "state", "last_changed"} {
		if _, ok := idx[c]; !ok {
			return nil, fmt.Errorf("missing column %q (expected Home Assistant history export)", c)
		}
	}
	data := map[string][]sample{}
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		v, err := strconv.ParseFloat(row[idx["state"]], 64)
		if err != nil {
			continue // "unavailable", "unknown", ...
		}
		t, err := time.Parse(time.RFC3339Nano, row[idx["last_changed"]])
		if err != nil {
			return nil, fmt.Errorf("bad timestamp %q", row[idx["last_changed"]])
		}
		id := row[idx["entity_id"]]
		data[id] = append(data[id], sample{t.In(loc), v})
	}
	for id := range data {
		sort.Slice(data[id], func(i, j int) bool { return data[id][i].t.Before(data[id][j].t) })
	}
	need := append([]string{}, m.PV...)
	if m.Load != "" {
		need = append(need, m.Load)
	} else {
		if m.Grid == "" {
			return nil, fmt.Errorf("either a load entity or a grid entity is required")
		}
		need = append(need, m.Grid)
		if m.Battery != "" {
			need = append(need, m.Battery)
		}
	}
	var from, to time.Time
	for _, id := range need {
		s := data[id]
		if len(s) == 0 {
			return nil, fmt.Errorf("entity %q not found in export", id)
		}
		if from.IsZero() || s[0].t.After(from) {
			from = s[0].t
		}
		if to.IsZero() || s[len(s)-1].t.Before(to) {
			to = s[len(s)-1].t
		}
	}
	from = series.SlotStart(from).Add(series.Slot)
	to = series.SlotStart(to)

	avg := func(id string, a, b time.Time) float64 {
		s := data[id]
		if len(s) == 0 {
			return math.NaN()
		}
		i := sort.Search(len(s), func(i int) bool { return s[i].t.After(a) }) - 1
		if i < 0 {
			return math.NaN()
		}
		var sum float64
		cur := a
		for ; i < len(s) && cur.Before(b); i++ {
			end := b
			if i+1 < len(s) && s[i+1].t.Before(b) {
				end = s[i+1].t
			}
			sum += s[i].v * end.Sub(cur).Seconds()
			cur = end
		}
		return sum / b.Sub(a).Seconds()
	}

	var out series.Series
	for t := from; t.Before(to); t = t.Add(series.Slot) {
		end := t.Add(series.Slot)
		var pv float64
		for _, id := range m.PV {
			pv += math.Max(0, avg(id, t, end)) * m.Scale
		}
		var load float64
		if m.Load != "" {
			load = avg(m.Load, t, end) * m.Scale
		} else {
			load = avg(m.Grid, t, end)*m.Scale + pv
			if m.Battery != "" {
				load += avg(m.Battery, t, end) * m.Scale
			}
		}
		temp := math.NaN()
		if m.Temp != "" {
			temp = avg(m.Temp, t, end)
		}
		if math.IsNaN(pv) || math.IsNaN(load) {
			continue
		}
		out = append(out, series.Record{Time: t, PVW: pv, LoadW: math.Max(0, load), SpotCt: math.NaN(), TempC: temp})
	}
	return out, nil
}
