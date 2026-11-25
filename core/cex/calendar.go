package cex

import (
	"fmt"
	"time"
)

// NewYork is the zone of the US equity market; the zone database handles
// daylight saving.
var NewYork = mustLoadLocation("America/New_York")

// utc8 is the zone times are shown in to people.
var utc8 = time.FixedZone("UTC+8", 8*60*60)

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// earlyCloses and holidays are the 13:00 closes and the full-day closures of
// the NYSE from May 2026 through 2028, from its hours and calendars page.
// The board's collector keeps its own copy of the same dates.
var earlyCloses = map[string]bool{
	"2026-11-27": true, "2026-12-24": true,
	"2027-11-26": true,
	"2028-07-03": true, "2028-11-24": true,
}

var holidays = map[string]bool{
	"2026-05-25": true, "2026-06-19": true, "2026-07-03": true, "2026-09-07": true, "2026-11-26": true, "2026-12-25": true,
	"2027-01-01": true, "2027-01-18": true, "2027-02-15": true, "2027-03-26": true, "2027-05-31": true,
	"2027-06-18": true, "2027-07-05": true, "2027-09-06": true, "2027-11-25": true, "2027-12-24": true,
	"2028-01-17": true, "2028-02-21": true, "2028-04-14": true, "2028-05-29": true, "2028-06-19": true,
	"2028-07-04": true, "2028-09-04": true, "2028-11-23": true, "2028-12-25": true,
}

// Calendar is the US equity calendar: the scheduled holidays and early
// closes above, changed by the configuration's calendar_overrides. It is
// immutable and all its methods are pure.
type Calendar struct {
	overrides map[string]string // New York date -> CloseClosed, or the close as HH:MM New York
}

// NewCalendar binds the overrides.
func NewCalendar(overrides []CalendarOverride) (*Calendar, error) {
	c := &Calendar{overrides: map[string]string{}}
	for _, o := range overrides {
		if _, err := time.Parse(dateLayout, o.Date); err != nil {
			return nil, fmt.Errorf("calendar override date %q is not a day as %s", o.Date, dateLayout)
		}
		if _, err := time.Parse("15:04", o.Close); o.Close != CloseClosed && err != nil {
			return nil, fmt.Errorf("calendar override %s: close %q is neither %q nor HH:MM", o.Date, o.Close, CloseClosed)
		}
		c.overrides[o.Date] = o.Close
	}
	return c, nil
}

// SessionAt returns the Session that contains t: it starts at the last
// regular close at or before t and ends at the next one.
func (c *Calendar) SessionAt(t time.Time) Session {
	// The trading day whose close starts the Session: the newest one whose
	// close is not after t.
	first := dayOf(t)
	for !c.tradingDay(first) || t.Before(c.closeOf(first)) {
		first = first.AddDate(0, 0, -1)
	}
	// The trading day that ends it: the next one.
	last := first.AddDate(0, 0, 1)
	for !c.tradingDay(last) {
		last = last.AddDate(0, 0, 1)
	}
	return Session{ID: first.Format(dateLayout), Start: c.closeOf(first),
		RegularOpen: time.Date(last.Year(), last.Month(), last.Day(), 9, 30, 0, 0, NewYork), End: c.closeOf(last)}
}

// CarrierDay is the carrier day of a corporate action with the given
// ex-date: the last trading day before the ex-date. The Session that starts
// at its close holds the moment the action takes effect.
func (c *Calendar) CarrierDay(exDate string) string {
	day := parseDay(exDate).AddDate(0, 0, -1)
	for !c.tradingDay(day) {
		day = day.AddDate(0, 0, -1)
	}
	return day.Format(dateLayout)
}

// Carrier is 04:00 New York of the given day, the start of its pre-market:
// where the entry ban of a corporate action carried by that day begins.
func (c *Calendar) Carrier(day string) time.Time {
	d := parseDay(day)
	return time.Date(d.Year(), d.Month(), d.Day(), 4, 0, 0, 0, NewYork)
}

// CompareSchedule lists where a venue's schedule of regular sessions and the
// calendar disagree, between the first and the last day the schedule lists:
// a day that is a trading day on one side only, or a different close. Times
// are written in UTC+8, dates are New York dates. An empty result means they
// agree.
func (c *Calendar) CompareSchedule(days []ScheduleDay) []string {
	if len(days) == 0 {
		return nil
	}
	theirs := map[string]time.Time{}
	first, last := days[0].Date, days[0].Date
	for _, d := range days {
		theirs[d.Date] = d.RegularClose
		first, last = min(first, d.Date), max(last, d.Date)
	}
	var out []string
	for day := parseDay(first); day.Format(dateLayout) <= last; day = day.AddDate(0, 0, 1) {
		date := day.Format(dateLayout)
		their, there := theirs[date]
		switch here := c.tradingDay(day); {
		case here && !there:
			out = append(out, fmt.Sprintf("%s: a trading day here (close %s), not on the venue", date, showUTC8(c.closeOf(day))))
		case !here && there:
			out = append(out, fmt.Sprintf("%s: a trading day on the venue (close %s), not here", date, showUTC8(their)))
		case here && !their.Equal(c.closeOf(day)):
			out = append(out, fmt.Sprintf("%s: close %s here, %s on the venue", date, showUTC8(c.closeOf(day)), showUTC8(their)))
		}
	}
	return out
}

// tradingDay reports whether the New York day has a regular session: a
// weekday that is not a holiday, unless an override closes it or gives it a
// close.
func (c *Calendar) tradingDay(day time.Time) bool {
	date := day.Format(dateLayout)
	if at, overridden := c.overrides[date]; overridden {
		return at != CloseClosed
	}
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !holidays[date]
}

// closeOf is the regular close of a trading day: 16:00 New York, 13:00 on an
// early-close day, or the time an override gives.
func (c *Calendar) closeOf(day time.Time) time.Time {
	date := day.Format(dateLayout)
	hour, minute := 16, 0
	if earlyCloses[date] {
		hour = 13
	}
	if at, overridden := c.overrides[date]; overridden && at != CloseClosed {
		hm, _ := time.Parse("15:04", at) // checked by NewCalendar
		hour, minute = hm.Hour(), hm.Minute()
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, NewYork)
}

// dayOf is midnight New York of the day t falls on.
func dayOf(t time.Time) time.Time {
	t = t.In(NewYork)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, NewYork)
}

// parseDay is midnight New York of a date written as 2006-01-02. The dates
// it is given come from the validated configuration or from the calendar
// itself.
func parseDay(date string) time.Time {
	day, _ := time.ParseInLocation(dateLayout, date, NewYork)
	return day
}

// showUTC8 writes an instant the way logs and events show it to people.
func showUTC8(t time.Time) string { return t.In(utc8).Format("01-02 15:04") }

// showTime writes an instant in full for a log line, in UTC+8; a zero time
// is empty.
func showTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(utc8).Format(time.DateTime)
}
