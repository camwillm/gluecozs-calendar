package calendar

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/camwillm/gluecozs-sdk"
)

const maxEveryWeeks = 4

// Series is an event that repeats weekly: every week or every 2 to 4 weeks,
// on one or more weekdays, with an optional last day.
//
// It stores a date and a clock time, never an instant, so "3:00 PM" stays
// 3:00 PM across daylight saving changes. Each repeat is worked out on demand
// as an ordinary sdk.Event, so the rest of Gluecozs never knows the
// difference. A repeat's ID looks like "s3:2026-11-15": the series, a colon,
// and the day.
type Series struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Tag        string   `json:"tag,omitempty"`
	Calendar   string   `json:"calendar,omitempty"`
	AllDay     bool     `json:"all_day,omitempty"`
	Date       string   `json:"date"`            // first day, 2006-01-02
	Clock      string   `json:"clock,omitempty"` // start time of day, 15:04 (timed events)
	Minutes    int      `json:"minutes,omitempty"`
	EveryWeeks int      `json:"every_weeks"`
	Weekdays   []int    `json:"weekdays"`        // time.Weekday values, 0 = Sunday
	Until      string   `json:"until,omitempty"` // last day, inclusive
	Skip       []string `json:"skip,omitempty"`  // single days that are skipped
}

func (s Series) clone() Series {
	s.Weekdays = slices.Clone(s.Weekdays)
	s.Skip = slices.Clone(s.Skip)
	return s
}

// normalizeSeries fills in defaults and tidies the lists.
func normalizeSeries(s Series) Series {
	s = s.clone()
	s.Title = strings.TrimSpace(s.Title)
	s.Tag = strings.TrimSpace(s.Tag)
	if s.EveryWeeks == 0 {
		s.EveryWeeks = 1
	}
	if s.AllDay {
		s.Clock, s.Minutes = "", 0
	} else if s.Minutes == 0 {
		s.Minutes = int(DefaultDuration / time.Minute)
	}
	slices.Sort(s.Weekdays)
	s.Weekdays = slices.Compact(s.Weekdays)
	slices.Sort(s.Skip)
	s.Skip = slices.Compact(s.Skip)
	return s
}

// validateSeries checks a normalized series.
func validateSeries(s Series) error {
	switch {
	case s.Title == "":
		return errors.New("a repeating event needs a title")
	case utf8.RuneCountInString(s.Title) > maxTitleLen:
		return fmt.Errorf("the title is too long (max %d characters)", maxTitleLen)
	case utf8.RuneCountInString(s.Tag) > maxTagLen:
		return fmt.Errorf("the tag is too long (max %d characters)", maxTagLen)
	case s.EveryWeeks < 1 || s.EveryWeeks > maxEveryWeeks:
		return fmt.Errorf("repeat every 1 to %d weeks", maxEveryWeeks)
	case len(s.Weekdays) == 0:
		return errors.New("pick at least one day of the week")
	}
	for _, d := range s.Weekdays {
		if d < 0 || d > 6 {
			return errors.New("the days of the week are not valid")
		}
	}

	start, err := time.Parse(dateLayout, s.Date)
	if err != nil {
		return errors.New("the first day is not a valid date")
	}
	if s.Until != "" {
		until, err := time.Parse(dateLayout, s.Until)
		if err != nil {
			return errors.New("the last day is not a valid date")
		}
		if until.Before(start) {
			return errors.New("the last day is before the first day")
		}
	}

	if s.AllDay {
		return nil
	}
	if _, err := time.Parse("15:04", s.Clock); err != nil {
		return errors.New("the start time is not valid")
	}
	if s.Minutes < 1 || s.Minutes > 24*60 {
		return errors.New("the length must be between 1 minute and 24 hours")
	}
	return nil
}

// civil turns the calendar date of t into midnight UTC, so day arithmetic is
// never thrown off by daylight saving.
func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// mondayOf returns the Monday of the week containing d.
func mondayOf(d time.Time) time.Time {
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// occursOn reports whether the series has a repeat on the calendar day of t.
func (s Series) occursOn(t time.Time) bool {
	d := civil(t)
	start, err := time.Parse(dateLayout, s.Date)
	if err != nil || d.Before(start) {
		return false
	}
	if s.Until != "" {
		if until, err := time.Parse(dateLayout, s.Until); err == nil && d.After(until) {
			return false
		}
	}
	if !slices.Contains(s.Weekdays, int(d.Weekday())) {
		return false
	}
	every := max(s.EveryWeeks, 1)
	weeks := int(mondayOf(d).Sub(mondayOf(start)).Hours()/24) / 7
	if weeks%every != 0 {
		return false
	}
	return !slices.Contains(s.Skip, d.Format(dateLayout))
}

// occurrence builds the event for one day, in the given time zone.
func (s Series) occurrence(y int, m time.Month, d int, loc *time.Location) sdk.Event {
	day := time.Date(y, m, d, 0, 0, 0, 0, loc)
	e := sdk.Event{
		ID:       s.ID + ":" + day.Format(dateLayout),
		Title:    s.Title,
		Tag:      s.Tag,
		Calendar: s.Calendar,
		Source:   "calendar",
		AllDay:   s.AllDay,
	}
	if s.AllDay {
		e.Start, e.End = day, day.AddDate(0, 0, 1)
		return e
	}
	clock, _ := time.Parse("15:04", s.Clock)
	e.Start = time.Date(y, m, d, clock.Hour(), clock.Minute(), 0, 0, loc)
	e.End = e.Start.Add(time.Duration(s.Minutes) * time.Minute)
	return e
}

// occurrencesBetween returns the repeats that overlap [from, to), using
// from's time zone.
func (s Series) occurrencesBetween(from, to time.Time) []sdk.Event {
	loc := from.Location()
	y, m, d := from.Date()
	cur := time.Date(y, m, d, 12, 0, 0, 0, loc).AddDate(0, 0, -2) // noon avoids DST edges

	var out []sdk.Event
	for i := 0; i < 4000 && cur.Before(to.AddDate(0, 0, 1)); i++ {
		if s.occursOn(cur) {
			cy, cm, cd := cur.Date()
			if e := s.occurrence(cy, cm, cd, loc); e.Start.Before(to) && e.End.After(from) {
				out = append(out, e)
			}
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return out
}

// splitID separates "s3:2026-11-15" into the series ID and the day.
func splitID(id string) (seriesID, date string, isOccurrence bool) {
	seriesID, date, isOccurrence = strings.Cut(id, ":")
	return seriesID, date, isOccurrence
}

// isSeriesID reports whether id names a whole series, like "s3". Plain event
// IDs are numbers.
func isSeriesID(id string) bool {
	return strings.HasPrefix(id, "s") && !strings.Contains(id, ":")
}

var weekdayNames = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// Rule describes how often the series repeats, for example
// "Every Tuesday and Friday" or "Every 2 weeks on Tuesday".
func (s Series) Rule() string {
	names := make([]string, 0, len(s.Weekdays))
	for _, d := range s.Weekdays {
		if d >= 0 && d <= 6 {
			names = append(names, weekdayNames[d])
		}
	}
	var days string
	switch len(names) {
	case 0:
		days = "(no days)"
	case 1:
		days = names[0]
	default:
		days = strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}

	rule := "Every " + days
	if s.EveryWeeks > 1 {
		rule = fmt.Sprintf("Every %d weeks on %s", s.EveryWeeks, days)
	}
	if s.Until != "" {
		if u, err := time.Parse(dateLayout, s.Until); err == nil {
			rule += ", until " + u.Format("Jan 2")
		}
	}
	return rule
}

// TimeText describes the time of day, for example "3:00 PM–11:30 PM".
func (s Series) TimeText() string {
	if s.AllDay {
		return "all day"
	}
	start, err := time.Parse("15:04", s.Clock)
	if err != nil {
		return ""
	}
	end := start.Add(time.Duration(s.Minutes) * time.Minute)
	return start.Format("3:04 PM") + "–" + end.Format("3:04 PM")
}
