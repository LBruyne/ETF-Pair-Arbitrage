package scan

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// newYork is the listing exchange's time zone; the zone database handles
// daylight saving.
var newYork = mustLoadLocation("America/New_York")

// earlyCloses and holidays are the 13:00 closes and the full-day closures of
// May 2026 to 2028 from the NYSE hours and calendars page.
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

// calendarValidThrough is the last day the table above covers.
const calendarValidThrough = "2028-12-31"

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Calendar is the US equity calendar: the scheduled holidays and early
// closes above and the configuration's overrides, an unscheduled closure or
// a close moved to another time (calendar_overrides).
type Calendar struct {
	overrides map[string]string // New York date -> "closed", or the close as HH:MM New York
}

// newCalendar binds the overrides; Validate has checked them.
func newCalendar(overrides []CalendarOverride) Calendar {
	c := Calendar{overrides: map[string]string{}}
	for _, o := range overrides {
		c.overrides[o.Date] = o.Close
	}
	return c
}

// closeOf is the regular close of a New York trading day: 16:00, 13:00 on
// an early-close day, or the time an override gives.
func (c Calendar) closeOf(sessionDate string) (time.Time, error) {
	if sessionDate > calendarValidThrough {
		return time.Time{}, fmt.Errorf("session %s is past the calendar table (%s)", sessionDate, calendarValidThrough)
	}
	day, err := time.ParseInLocation("2006-01-02", sessionDate, newYork)
	if err != nil {
		return time.Time{}, err
	}
	hour, minute := 16, 0
	if earlyCloses[sessionDate] {
		hour = 13
	}
	if at, ok := c.overrides[sessionDate]; ok && at != "closed" {
		hm, err := time.Parse("15:04", at)
		if err != nil {
			return time.Time{}, fmt.Errorf("calendar override %s: %w", sessionDate, err)
		}
		hour, minute = hm.Hour(), hm.Minute()
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, newYork), nil
}

// tradingDays lists the New York trading days from the day of from to the
// day of to, oldest first: weekdays that are not holidays, less the days an
// override closes, plus a day an override gives a close.
func (c Calendar) tradingDays(from, to time.Time) []string {
	var out []string
	last := to.In(newYork).Format("2006-01-02")
	for day := from.In(newYork); day.Format("2006-01-02") <= last; day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		at, overridden := c.overrides[date]
		if overridden && at == "closed" || !overridden && !scheduledTradingDay(day) {
			continue
		}
		out = append(out, date)
	}
	return out
}

// scheduledTradingDay reports whether a date is a trading day by the table
// above: a weekday that is not a holiday.
func scheduledTradingDay(day time.Time) bool {
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !holidays[day.Format("2006-01-02")]
}

// Phase is one stretch of the US equity day.
type Phase struct {
	Start, End time.Time
	Name       string
}

// phasesOf lists the phases of the given trading days, oldest first and
// without a hole between them: for each day the overnight session from 20:00
// the evening before to 04:00, pre-market to 09:30, the regular session to
// the close and after-hours for four hours after it; the time between one
// day's after-hours and the next day's overnight session, a weekend or a
// holiday, is Closed.
func (c Calendar) phasesOf(days []string) ([]Phase, error) {
	var out []Phase
	for _, date := range days {
		closeAt, err := c.closeOf(date)
		if err != nil {
			return nil, err
		}
		at := func(dayOffset, hour, minute int) time.Time {
			return time.Date(closeAt.Year(), closeAt.Month(), closeAt.Day()+dayOffset, hour, minute, 0, 0, newYork)
		}
		day := []Phase{
			{at(-1, 20, 0), at(0, 4, 0), "Overnight"},
			{at(0, 4, 0), at(0, 9, 30), "Pre-Market"},
			{at(0, 9, 30), closeAt, "Regular"},
			{closeAt, closeAt.Add(4 * time.Hour), "After-Hours"},
		}
		if len(out) > 0 && out[len(out)-1].End.Before(day[0].Start) {
			out = append(out, Phase{out[len(out)-1].End, day[0].Start, "Closed"})
		}
		out = append(out, day...)
	}
	return out, nil
}

// lastClosedSession is the newest trading day whose regular close has passed.
func (c Calendar) lastClosedSession(now time.Time) (string, error) {
	days := c.tradingDays(now.AddDate(0, 0, -10), now)
	for i := len(days) - 1; i >= 0; i-- {
		closeAt, err := c.closeOf(days[i])
		if err != nil {
			return "", err
		}
		if !now.Before(closeAt) {
			return days[i], nil
		}
	}
	return "", fmt.Errorf("no session within ten days before %s", now.Format(time.RFC3339))
}

// nextBell is the first moment after now at which a session's bell anchor
// is read: bellDelay after a regular close.
func (c Calendar) nextBell(now time.Time) (time.Time, error) {
	for _, date := range c.tradingDays(now.AddDate(0, 0, -1), now.AddDate(0, 0, 10)) {
		closeAt, err := c.closeOf(date)
		if err != nil {
			return time.Time{}, err
		}
		if at := closeAt.Add(bellDelay); at.After(now) {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("no close within ten days after %s", now.Format(time.RFC3339))
}

// carrierDay is the last trading day before an ex-date, whose session holds
// the instant a dividend of that ex-date takes effect, and the end of the
// ex-date (midnight New York).
func (c Calendar) carrierDay(exDate string) (carrier string, exEnd time.Time, err error) {
	ex, err := time.ParseInLocation("2006-01-02", exDate, newYork)
	if err != nil {
		return "", time.Time{}, err
	}
	days := c.tradingDays(ex.AddDate(0, 0, -10), ex.AddDate(0, 0, -1))
	if len(days) == 0 {
		return "", time.Time{}, fmt.Errorf("no trading day within ten days before %s", exDate)
	}
	return days[len(days)-1], ex.AddDate(0, 0, 1), nil
}

// carrierWindow is when the sources of an action with the given ex-date are
// read at every anchor poll until its amount is known: from the pre-market
// of its carrier day, 04:00 New York, to the end of the ex-date (a None after
// its close included).
func (c Calendar) carrierWindow(exDate string) (from, to time.Time, err error) {
	carrier, to, err := c.carrierDay(exDate)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	day, _ := time.ParseInLocation("2006-01-02", carrier, newYork)
	return day.Add(4 * time.Hour), to, nil
}

// declarationWindow is when Direxion's distributions of a fund are read at
// every anchor poll until its amount of the given ex-date is known, as the
// owner authorised: from the regular close of the carrier day, after which
// Direxion declares, at the latest to the end of the first trading day after
// the ex-date (midnight New York): the API's data trail the declaration by
// about a day.
func (c Calendar) declarationWindow(exDate string) (from, to time.Time, err error) {
	carrier, exEnd, err := c.carrierDay(exDate)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from, err = c.closeOf(carrier); err != nil {
		return time.Time{}, time.Time{}, err
	}
	after := c.tradingDays(exEnd, exEnd.AddDate(0, 0, 10))
	if len(after) == 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("no trading day within ten days after %s", exDate)
	}
	next, err := time.ParseInLocation("2006-01-02", after[0], newYork)
	return from, next.AddDate(0, 0, 1), err
}

// ScheduleSession is one interval of Binance's schedule of the US equity
// market: OVERNIGHT, PRE_MARKET, REGULAR, AFTER_MARKET or NO_TRADING.
type ScheduleSession struct {
	StartMs int64  `json:"startTime"`
	EndMs   int64  `json:"endTime"`
	Type    string `json:"type"`
}

// binancePhases names Binance's session types as the calendar's phases.
var binancePhases = map[string]string{"OVERNIGHT": "Overnight", "PRE_MARKET": "Pre-Market", "REGULAR": "Regular", "AFTER_MARKET": "After-Hours",
	"NO_TRADING": "Closed"}

// compareSchedule compares Binance's schedule with the calendar inside the
// window the schedule covers, [from, to): the trading days and each day's
// close, the end of its REGULAR session, and the other phases. differ lists
// a trading day or a close that differs on a date without an override;
// info the same on an overridden date, and every difference of the other
// phases. Times are UTC+8, dates New York trading days.
func (c Calendar) compareSchedule(sessions []ScheduleSession) (from, to int64, differ, info []string, err error) {
	if len(sessions) == 0 {
		return 0, 0, nil, nil, fmt.Errorf("the schedule lists no session")
	}
	from, to = sessions[0].StartMs, sessions[0].EndMs
	theirs := map[string]int64{} // trading day -> close
	var theirPhases []string
	for _, s := range sessions {
		from, to = min(from, s.StartMs), max(to, s.EndMs)
		name, known := binancePhases[s.Type]
		if !known {
			return 0, 0, nil, nil, fmt.Errorf("unknown session type %q", s.Type)
		}
		if name == "Regular" {
			theirs[time.UnixMilli(s.StartMs).In(newYork).Format("2006-01-02")] = s.EndMs
			continue
		}
		theirPhases = append(theirPhases, phaseText(s.StartMs, s.EndMs, name))
	}
	days := c.tradingDays(time.UnixMilli(from).AddDate(0, 0, -1), time.UnixMilli(to).AddDate(0, 0, 1))
	phases, err := c.phasesOf(days)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	ours := map[string]int64{}
	var ourPhases []string
	for _, p := range phases {
		start, end := max(p.Start.UnixMilli(), from), min(p.End.UnixMilli(), to)
		if start >= end {
			continue
		}
		if p.Name == "Regular" {
			ours[p.Start.In(newYork).Format("2006-01-02")] = p.End.UnixMilli()
			continue
		}
		ourPhases = append(ourPhases, phaseText(start, end, p.Name))
	}
	var dates []string
	for date := range ours {
		dates = append(dates, date)
	}
	for date := range theirs {
		if _, ok := ours[date]; !ok {
			dates = append(dates, date)
		}
	}
	slices.Sort(dates)
	for _, date := range dates {
		mine, here := ours[date]
		binance, there := theirs[date]
		var d string
		switch {
		case here && !there:
			d = fmt.Sprintf("%s: a trading day here (close %s), not on Binance", date, utc8(mine))
		case there && !here:
			d = fmt.Sprintf("%s: a trading day on Binance (close %s), not here", date, utc8(binance))
		case mine != binance:
			d = fmt.Sprintf("%s: close %s here, %s on Binance", date, utc8(mine), utc8(binance))
		default:
			continue
		}
		if _, overridden := c.overrides[date]; overridden {
			info = append(info, d+" (overridden here)")
		} else {
			differ = append(differ, d)
		}
	}
	for _, p := range ourPhases {
		if !slices.Contains(theirPhases, p) {
			info = append(info, "here only: "+p)
		}
	}
	for _, p := range theirPhases {
		if !slices.Contains(ourPhases, p) {
			info = append(info, "Binance only: "+p)
		}
	}
	return from, to, differ, info, nil
}

// utc8 shows an instant as the board does.
func utc8(ms int64) string {
	return time.UnixMilli(ms).In(time.FixedZone("UTC+8", 8*3600)).Format("01-02 15:04")
}

func phaseText(startMs, endMs int64, name string) string {
	return strings.Join([]string{name, utc8(startMs), "-", utc8(endMs)}, " ")
}
