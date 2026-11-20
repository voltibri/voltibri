// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package series holds the 15-minute time series voltibri works on and
// reads/writes them as CSV.
//
// CSV format (header required, column order free):
//
//	time,pv_w,load_w,spot_ct,temp_c
//	2026-06-15T12:00:00+02:00,5230,640,-1.2,24.5
//
// time is RFC 3339 or "2006-01-02 15:04[:05]" (interpreted in the given
// location). pv_w and load_w are mean powers in W, spot_ct the exchange
// (day-ahead) price in ct/kWh, temp_c the outdoor temperature. spot_ct and
// temp_c are optional; empty cells are allowed.
package series

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Slot is the planning and storage resolution.
const Slot = 15 * time.Minute

// SlotHours is the length of one slot in hours (for W -> Wh).
const SlotHours = 0.25

// Record holds the mean values of one 15-minute slot.
// Missing optional values are NaN.
type Record struct {
	Time   time.Time // start of slot
	PVW    float64
	LoadW  float64
	SpotCt float64 // NaN if unknown
	TempC  float64 // NaN if unknown
}

// HasSpot reports whether a spot price is known.
func (r Record) HasSpot() bool { return !math.IsNaN(r.SpotCt) }

// HasTemp reports whether a temperature is known.
func (r Record) HasTemp() bool { return !math.IsNaN(r.TempC) }

// Series is a time ordered list of slot records (not necessarily gapless).
type Series []Record

// SlotStart truncates t to the start of its 15-minute slot.
func SlotStart(t time.Time) time.Time { return t.Truncate(Slot) }

var timeLayouts = []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"}

func parseTime(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t.In(loc), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable time %q", s)
}

func parseOptFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "nan") {
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// ReadCSV reads a CSV file and resamples it to 15-minute slots. Finer data
// is averaged, coarser data (e.g. hourly) is held for all slots it covers.
func ReadCSV(r io.Reader, loc *time.Location) (Series, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, req := range []string{"time", "pv_w", "load_w"} {
		if _, ok := col[req]; !ok {
			return nil, fmt.Errorf("missing column %q", req)
		}
	}
	get := func(row []string, name string) string {
		if i, ok := col[name]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}

	type acc struct {
		pv, load, spot, temp float64
		n, nSpot, nTemp      int
	}
	buckets := map[time.Time]*acc{}
	var raw []time.Time
	line := 1
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		t, err := parseTime(get(row, "time"), loc)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		pv, err1 := parseOptFloat(get(row, "pv_w"))
		load, err2 := parseOptFloat(get(row, "load_w"))
		spot, err3 := parseOptFloat(get(row, "spot_ct"))
		temp, err4 := parseOptFloat(get(row, "temp_c"))
		for _, e := range []error{err1, err2, err3, err4} {
			if e != nil {
				return nil, fmt.Errorf("line %d: %w", line, e)
			}
		}
		if math.IsNaN(pv) || math.IsNaN(load) {
			continue // incomplete measurement
		}
		k := SlotStart(t)
		a := buckets[k]
		if a == nil {
			a = &acc{}
			buckets[k] = a
			raw = append(raw, k)
		}
		a.pv += pv
		a.load += load
		a.n++
		if !math.IsNaN(spot) {
			a.spot += spot
			a.nSpot++
		}
		if !math.IsNaN(temp) {
			a.temp += temp
			a.nTemp++
		}
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Before(raw[j]) })

	var out Series
	for i, k := range raw {
		a := buckets[k]
		rec := Record{Time: k, PVW: a.pv / float64(a.n), LoadW: a.load / float64(a.n), SpotCt: math.NaN(), TempC: math.NaN()}
		if a.nSpot > 0 {
			rec.SpotCt = a.spot / float64(a.nSpot)
		}
		if a.nTemp > 0 {
			rec.TempC = a.temp / float64(a.nTemp)
		}
		out = append(out, rec)
		// hold coarse data (up to 1h) for the following empty slots
		if i+1 < len(raw) {
			gap := raw[i+1].Sub(k)
			if gap > Slot && gap <= time.Hour {
				for t := k.Add(Slot); t.Before(raw[i+1]); t = t.Add(Slot) {
					r := rec
					r.Time = t
					out = append(out, r)
				}
			}
		}
	}
	return out, nil
}

func fmtOpt(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// WriteHeader writes the CSV header.
func WriteHeader(w io.Writer) error {
	_, err := io.WriteString(w, "time,pv_w,load_w,spot_ct,temp_c\n")
	return err
}

// WriteRecord appends one record as CSV line.
func WriteRecord(w io.Writer, r Record) error {
	_, err := fmt.Fprintf(w, "%s,%.1f,%.1f,%s,%s\n", r.Time.Format(time.RFC3339), r.PVW, r.LoadW, fmtOpt(r.SpotCt), fmtOpt(r.TempC))
	return err
}

// WriteCSV writes a whole series including header.
func WriteCSV(w io.Writer, s Series) error {
	if err := WriteHeader(w); err != nil {
		return err
	}
	for _, r := range s {
		if err := WriteRecord(w, r); err != nil {
			return err
		}
	}
	return nil
}
