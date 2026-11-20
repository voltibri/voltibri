package series

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestReadCSVResample(t *testing.T) {
	in := `time,pv_w,load_w,spot_ct
2026-06-15 12:00,1000,200,-1
2026-06-15 12:05,2000,400,-1
2026-06-15 12:10,3000,600,
2026-06-15 13:00,500,300,5
2026-06-15 14:00,0,100,5
`
	s, err := ReadCSV(strings.NewReader(in), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// 12:00 averaged and held until 13:00, 13:00 held until 14:00, 14:00 single
	if len(s) != 9 {
		t.Fatalf("got %d records: %+v", len(s), s)
	}
	if s[0].PVW != 2000 || s[0].LoadW != 400 || s[0].SpotCt != -1 {
		t.Errorf("unexpected first record %+v", s[0])
	}
	if s[0].HasTemp() {
		t.Error("temperature must be missing")
	}
	if !s[5].Time.Equal(time.Date(2026, 6, 15, 13, 15, 0, 0, time.UTC)) || s[5].PVW != 500 {
		t.Errorf("hourly value not held: %+v", s[5])
	}

	var buf bytes.Buffer
	if err := WriteCSV(&buf, s); err != nil {
		t.Fatal(err)
	}
	s2, err := ReadCSV(&buf, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2) != len(s) || s2[8].LoadW != 100 {
		t.Errorf("roundtrip mismatch: %+v", s2)
	}
}
