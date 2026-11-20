package calendar

import (
	"testing"
	"time"
)

func TestEaster(t *testing.T) {
	for y, want := range map[int]string{2024: "2024-03-31", 2025: "2025-04-20", 2026: "2026-04-05", 2027: "2027-03-28"} {
		if got := easterSunday(y).Format("2006-01-02"); got != want {
			t.Errorf("easter %d = %s, want %s", y, got, want)
		}
	}
}

func TestDayType(t *testing.T) {
	c, err := New("2026-08-15")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Europe/Berlin")
	cases := map[string]DayType{
		"2026-11-18": Workday,         // Wednesday
		"2026-11-21": Saturday,        // Saturday
		"2026-11-22": SundayOrHoliday, // Sunday
		"2026-04-03": SundayOrHoliday, // Karfreitag
		"2026-05-14": SundayOrHoliday, // Christi Himmelfahrt
		"2026-05-25": SundayOrHoliday, // Pfingstmontag
		"2026-10-03": SundayOrHoliday, // Tag der Deutschen Einheit (Saturday!)
		"2026-08-15": SundayOrHoliday, // configured extra day
	}
	for s, want := range cases {
		d, _ := time.ParseInLocation("2006-01-02", s, loc)
		if got := c.DayType(d.Add(12 * time.Hour)); got != want {
			t.Errorf("%s: got %v, want %v", s, got, want)
		}
	}
}
