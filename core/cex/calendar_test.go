package cex

import (
	"os"
	"regexp"
	"slices"
	"testing"
	"time"
)

// newCalendar builds a calendar with the given overrides.
func newCalendar(t *testing.T, overrides ...CalendarOverride) *Calendar {
	t.Helper()
	c, err := NewCalendar(overrides)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// wantSession fails unless the Session at `at` has the given id and times.
func wantSession(t *testing.T, c *Calendar, at time.Time, id string, start, open, end time.Time) {
	t.Helper()
	s := c.SessionAt(at)
	if s.ID != id || !s.Start.Equal(start) || !s.RegularOpen.Equal(open) || !s.End.Equal(end) {
		t.Errorf("SessionAt(%s) = %s [%s, open %s, %s]\n want %s [%s, open %s, %s]", showTime(at), s.ID, showTime(s.Start), showTime(s.RegularOpen), showTime(s.End),
			id, showTime(start), showTime(open), showTime(end))
	}
}

// bj is a UTC+8 wall-clock time, the zone people read.
func bj(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, utc8)
}

// A weekend belongs to the Session that Friday's close starts: it runs to
// Monday's close, with entries until Monday's open (SPEC 5.1's example).
func TestSessionOverAWeekend(t *testing.T) {
	c := newCalendar(t)
	start, open, end := bj(2026, 11, 21, 5, 0), bj(2026, 11, 23, 22, 30), bj(2026, 11, 24, 5, 0)
	for _, at := range []time.Time{start, bj(2026, 11, 22, 12, 0), bj(2026, 11, 23, 23, 0), end.Add(-time.Nanosecond)} {
		wantSession(t, c, at, "2026-11-20", start, open, end)
	}
	// The close itself already belongs to the next Session.
	wantSession(t, c, end, "2026-11-23", end, bj(2026, 11, 24, 22, 30), bj(2026, 11, 25, 5, 0))
	wantSession(t, c, start.Add(-time.Nanosecond), "2026-11-19", bj(2026, 11, 20, 5, 0), bj(2026, 11, 20, 22, 30), start)
}

// A holiday belongs to the Session before it, and an early-close day ends
// its Session at 13:00 New York: Wednesday's close starts a Session that
// spans Thanksgiving and ends at Friday's early close.
func TestSessionOverAHolidayAndAnEarlyClose(t *testing.T) {
	c := newCalendar(t)
	start, open, end := bj(2026, 11, 26, 5, 0), bj(2026, 11, 27, 22, 30), bj(2026, 11, 28, 2, 0)
	wantSession(t, c, ny(2026, 11, 26, 12, 0), "2026-11-25", start, open, end) // Thanksgiving noon
	wantSession(t, c, end.Add(-time.Minute), "2026-11-25", start, open, end)
	// The early close starts the next Session, which runs over the weekend.
	wantSession(t, c, end, "2026-11-27", end, bj(2026, 11, 30, 22, 30), bj(2026, 12, 1, 5, 0))
	// Christmas Eve 2026 closes early, Christmas Day is a holiday.
	wantSession(t, c, ny(2026, 12, 24, 12, 0), "2026-12-23", ny(2026, 12, 23, 16, 0), ny(2026, 12, 24, 9, 30), ny(2026, 12, 24, 13, 0))
	wantSession(t, c, ny(2026, 12, 25, 12, 0), "2026-12-24", ny(2026, 12, 24, 13, 0), ny(2026, 12, 28, 9, 30), ny(2026, 12, 28, 16, 0))
}

// The close is 16:00 New York on both sides of a daylight-saving switch, so
// in UTC it moves by an hour inside the weekend's Session.
func TestSessionAcrossDaylightSavingSwitches(t *testing.T) {
	c := newCalendar(t)
	utc := func(year int, month time.Month, day, hour, minute int) time.Time {
		return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	}
	// Autumn: clocks go back on Sunday 2026-11-01. Friday's close is 20:00
	// UTC, Monday's 21:00 UTC.
	wantSession(t, c, utc(2026, 11, 1, 12, 0), "2026-10-30", utc(2026, 10, 30, 20, 0), utc(2026, 11, 2, 14, 30), utc(2026, 11, 2, 21, 0))
	// Spring: clocks go forward on Sunday 2027-03-14. Friday's close is
	// 21:00 UTC, Monday's 20:00 UTC.
	wantSession(t, c, utc(2027, 3, 14, 12, 0), "2027-03-12", utc(2027, 3, 12, 21, 0), utc(2027, 3, 15, 13, 30), utc(2027, 3, 15, 20, 0))
}

// calendar_overrides close a day the table has open, give a day another
// close, and open a day the table has closed.
func TestSessionWithOverrides(t *testing.T) {
	c := newCalendar(t,
		CalendarOverride{Date: "2026-12-09", Close: CloseClosed}, // a Wednesday without a session
		CalendarOverride{Date: "2026-12-10", Close: "14:30"},     // an unscheduled early close
		CalendarOverride{Date: "2026-12-12", Close: "16:00"})     // a Saturday session
	wantSession(t, c, ny(2026, 12, 9, 12, 0), "2026-12-08", ny(2026, 12, 8, 16, 0), ny(2026, 12, 10, 9, 30), ny(2026, 12, 10, 14, 30))
	wantSession(t, c, ny(2026, 12, 10, 15, 0), "2026-12-10", ny(2026, 12, 10, 14, 30), ny(2026, 12, 11, 9, 30), ny(2026, 12, 11, 16, 0))
	wantSession(t, c, ny(2026, 12, 11, 17, 0), "2026-12-11", ny(2026, 12, 11, 16, 0), ny(2026, 12, 12, 9, 30), ny(2026, 12, 12, 16, 0))
	if _, err := NewCalendar([]CalendarOverride{{Date: "2026-12-09", Close: "noon"}}); err == nil {
		t.Error("an override with close \"noon\" was accepted")
	}
}

// The carrier day of an ex-date is the last trading day before it; its
// pre-market opens at 04:00 New York.
func TestCarrierDay(t *testing.T) {
	c := newCalendar(t)
	for _, tc := range []struct{ exDate, carrier string }{
		{"2026-12-22", "2026-12-21"}, // Tuesday: Monday carries it
		{"2026-12-21", "2026-12-18"}, // Monday: Friday carries it
		{"2026-12-28", "2026-12-24"}, // the Monday after Christmas: Friday is a holiday, Thursday carries it
	} {
		if got := c.CarrierDay(tc.exDate); got != tc.carrier {
			t.Errorf("CarrierDay(%s) = %s, want %s", tc.exDate, got, tc.carrier)
		}
	}
	if got := c.Carrier("2026-12-21"); !got.Equal(bj(2026, 12, 21, 17, 0)) {
		t.Errorf("Carrier(2026-12-21) = %s, want 12-21 17:00 UTC+8 (04:00 New York)", showTime(got))
	}
}

// The comparison with a venue's schedule names a day only one side trades
// and a close that differs, inside the days the schedule lists.
func TestCompareSchedule(t *testing.T) {
	c := newCalendar(t)
	day := func(date string, closeHour int) ScheduleDay {
		d := parseDay(date)
		return ScheduleDay{Date: date, RegularOpen: time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, NewYork), RegularClose: time.Date(d.Year(), d.Month(), d.Day(), closeHour, 0, 0, 0, NewYork)}
	}
	// 11-23 to 11-30 as the calendar has it: Thanksgiving closed, the 27th early.
	same := []ScheduleDay{day("2026-11-23", 16), day("2026-11-24", 16), day("2026-11-25", 16), day("2026-11-27", 13), day("2026-11-30", 16)}
	if got := c.CompareSchedule(same); len(got) != 0 {
		t.Errorf("an identical schedule differs: %v", got)
	}
	// The venue trades Thanksgiving, closes the 27th at 16:00 and skips the 24th.
	other := []ScheduleDay{day("2026-11-23", 16), day("2026-11-25", 16), day("2026-11-26", 16), day("2026-11-27", 16), day("2026-11-30", 16)}
	want := []string{
		"2026-11-24: a trading day here (close 11-25 05:00), not on the venue",
		"2026-11-26: a trading day on the venue (close 11-27 05:00), not here",
		"2026-11-27: close 11-28 02:00 here, 11-28 05:00 on the venue",
	}
	if got := c.CompareSchedule(other); !slices.Equal(got, want) {
		t.Errorf("differences =\n %q\nwant\n %q", got, want)
	}
	if got := c.CompareSchedule(nil); got != nil {
		t.Errorf("an empty schedule gave differences: %v", got)
	}
}

// The holiday and early-close tables hold the same dates as the board
// collector's copy (dashboard/internal/scan/calendar.go): both come from the
// NYSE page and must change together.
func TestCalendarTablesMatchTheBoardCollector(t *testing.T) {
	src, err := os.ReadFile("../../dashboard/internal/scan/calendar.go")
	if err != nil {
		t.Skipf("the board collector's calendar is not readable here: %v", err)
	}
	datesOf := func(name string) []string {
		block := regexp.MustCompile(`(?s)var ` + name + ` = map\[string\]bool\{(.*?)\n\}`).FindSubmatch(src)
		if block == nil {
			t.Fatalf("no %s table in the board collector's calendar", name)
		}
		var out []string
		for _, m := range regexp.MustCompile(`"(\d{4}-\d{2}-\d{2})": true`).FindAllSubmatch(block[1], -1) {
			out = append(out, string(m[1]))
		}
		slices.Sort(out)
		return out
	}
	keys := func(table map[string]bool) []string {
		var out []string
		for date := range table {
			out = append(out, date)
		}
		slices.Sort(out)
		return out
	}
	if theirs, ours := datesOf("earlyCloses"), keys(earlyCloses); !slices.Equal(theirs, ours) {
		t.Errorf("early closes differ:\n board %v\n core  %v", theirs, ours)
	}
	if theirs, ours := datesOf("holidays"), keys(holidays); !slices.Equal(theirs, ours) {
		t.Errorf("holidays differ:\n board %v\n core  %v", theirs, ours)
	}
}
