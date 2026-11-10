package calendar

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

func mustAddSeries(t *testing.T, s *Store, sr Series) Series {
	t.Helper()
	saved, err := s.AddSeries(sr)
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	return saved
}

// sundayShift is "work every Sunday 3:00 PM to 11:30 PM", starting Nov 15, 2026.
func sundayShift() Series {
	return Series{Title: "Work", Tag: "Work", Date: "2026-11-15", Clock: "15:00", Minutes: 510, Weekdays: []int{int(time.Sunday)}}
}

func days(evs []sdk.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Start.In(ny).Format("Jan 2")
	}
	return out
}

func TestWeeklySeriesExpansion(t *testing.T) {
	s := NewStore()
	sr := mustAddSeries(t, s, sundayShift())

	if sr.ID != "s1" {
		t.Errorf("ID = %q, want s1", sr.ID)
	}

	got, err := s.Range(context.Background(), at(1, 0, 0), at(30, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Nov 15", "Nov 22", "Nov 29"}
	if !slices.Equal(days(got), want) {
		t.Errorf("days = %v, want %v (nothing before the first day)", days(got), want)
	}

	first := got[0]
	if first.ID != "s1:2026-11-15" || first.Title != "Work" || first.Tag != "Work" {
		t.Errorf("first = %+v", first)
	}
	if !first.Start.Equal(at(15, 15, 0)) || !first.End.Equal(at(15, 23, 30)) {
		t.Errorf("times = %v to %v, want 3:00 PM to 11:30 PM", first.Start, first.End)
	}
}

func TestEventsOnIncludesRepeats(t *testing.T) {
	s := NewStore()
	mustAddSeries(t, s, sundayShift())
	mustAdd(t, s, sdk.Event{Title: "Morning review", Start: at(22, 11, 0)})

	got, _ := s.EventsOn(context.Background(), at(22, 12, 0))
	if !slices.Equal(titles(got), []string{"Morning review", "Work"}) {
		t.Errorf("got %v", titles(got))
	}
	if none, _ := s.EventsOn(context.Background(), at(23, 12, 0)); len(none) != 0 {
		t.Errorf("Monday should be empty, got %v", titles(none))
	}
}

func TestEveryOtherWeekMatchesPayWeeks(t *testing.T) {
	s := NewStore()
	// Tue and Fri, 5 to 9 PM, every 2 weeks starting the week of Nov 16.
	mustAddSeries(t, s, Series{
		Title: "Work (pay week)", Date: "2026-11-17", Clock: "17:00", Minutes: 240,
		EveryWeeks: 2, Weekdays: []int{int(time.Tuesday), int(time.Friday)},
	})

	got, _ := s.Range(context.Background(), at(9, 0, 0), time.Date(2027, 1, 5, 0, 0, 0, 0, ny))
	want := []string{"Nov 17", "Nov 20", "Dec 1", "Dec 4", "Dec 15", "Dec 18", "Dec 29", "Jan 1"}
	if !slices.Equal(days(got), want) {
		t.Errorf("days = %v\nwant   %v", days(got), want)
	}
	// Dec 29, a Tuesday, is in the list: the 5 to 9 PM shift on a birthday.
}

func TestUntilIsInclusive(t *testing.T) {
	s := NewStore()
	sr := sundayShift()
	sr.Until = "2026-11-22"
	mustAddSeries(t, s, sr)

	got, _ := s.Range(context.Background(), at(1, 0, 0), time.Date(2027, 1, 1, 0, 0, 0, 0, ny))
	if !slices.Equal(days(got), []string{"Nov 15", "Nov 22"}) {
		t.Errorf("days = %v", days(got))
	}
}

func TestStartDateNotOnAWeekdayBeginsAtTheNextOne(t *testing.T) {
	s := NewStore()
	sr := sundayShift()
	sr.Date = "2026-11-10" // a Tuesday
	mustAddSeries(t, s, sr)

	got, _ := s.Range(context.Background(), at(1, 0, 0), at(20, 0, 0))
	if !slices.Equal(days(got), []string{"Nov 15"}) {
		t.Errorf("days = %v", days(got))
	}
}

func TestSkipOccurrence(t *testing.T) {
	s := NewStore()
	mustAddSeries(t, s, sundayShift())

	if err := s.SkipOccurrence("s1", "2026-11-22"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Range(context.Background(), at(1, 0, 0), at(30, 0, 0))
	if !slices.Equal(days(got), []string{"Nov 15", "Nov 29"}) {
		t.Errorf("days = %v", days(got))
	}

	if err := s.SkipOccurrence("s1", "2026-11-22"); err == nil {
		t.Error("skipping the same day twice should fail")
	}
	if err := s.SkipOccurrence("s1", "2026-11-23"); err == nil {
		t.Error("skipping a day that is not a repeat should fail")
	}
	if err := s.SkipOccurrence("s9", "2026-11-22"); err == nil {
		t.Error("skipping in a missing series should fail")
	}
	if err := s.SkipOccurrence("s1", "not-a-date"); err == nil {
		t.Error("a bad date should fail")
	}
}

func TestGetOccurrence(t *testing.T) {
	s := NewStore()
	mustAddSeries(t, s, sundayShift())

	e, ok := s.GetOccurrence("s1:2026-11-22", ny)
	if !ok || e.Title != "Work" || !e.Start.Equal(at(22, 15, 0)) {
		t.Fatalf("got %+v, ok=%v", e, ok)
	}
	for _, bad := range []string{"s1:2026-11-23", "s2:2026-11-22", "s1", "1", "s1:junk"} {
		if _, ok := s.GetOccurrence(bad, ny); ok {
			t.Errorf("GetOccurrence(%q) should not find anything", bad)
		}
	}
}

func TestWallClockStaysTheSameAcrossDaylightSaving(t *testing.T) {
	s := NewStore()
	// Clocks go back on Sunday Nov 1, 2026. A 3 PM Sunday shift must stay at
	// 3 PM on both sides of it.
	mustAddSeries(t, s, Series{
		Title: "Shift", Date: "2026-10-25", Clock: "15:00", Minutes: 60, Weekdays: []int{int(time.Sunday)},
	})
	got, _ := s.Range(context.Background(), time.Date(2026, 10, 24, 0, 0, 0, 0, ny), time.Date(2026, 11, 9, 0, 0, 0, 0, ny))
	if len(got) != 3 {
		t.Fatalf("got %d repeats, want 3 (Oct 25, Nov 1, Nov 8)", len(got))
	}
	for _, e := range got {
		if h := e.Start.In(ny).Hour(); h != 15 {
			t.Errorf("%s starts at hour %d, want 15", e.ID, h)
		}
	}
	_, offFirst := got[0].Start.Zone()
	_, offLast := got[2].Start.Zone()
	if offFirst == offLast {
		t.Error("the test should span a daylight saving change")
	}
}

func TestAllDaySeries(t *testing.T) {
	s := NewStore()
	mustAddSeries(t, s, Series{Title: "Weigh-in", AllDay: true, Date: "2026-11-09", Weekdays: []int{int(time.Monday)}})

	got, _ := s.EventsOn(context.Background(), at(16, 12, 0))
	if len(got) != 1 || !got[0].AllDay || !got[0].Start.Equal(at(16, 0, 0)) || !got[0].End.Equal(at(17, 0, 0)) {
		t.Errorf("got %+v", got)
	}
}

func TestUpdateAndDeleteSeries(t *testing.T) {
	s := NewStore()
	sr := mustAddSeries(t, s, sundayShift())
	if err := s.SkipOccurrence("s1", "2026-11-22"); err != nil {
		t.Fatal(err)
	}

	sr.Clock, sr.Minutes, sr.Title = "16:00", 450, "Work (late)"
	sr.Skip = nil // the store keeps the skipped days no matter what is passed
	if err := s.UpdateSeries(sr); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Range(context.Background(), at(1, 0, 0), at(30, 0, 0))
	if !slices.Equal(days(got), []string{"Nov 15", "Nov 29"}) || got[0].Title != "Work (late)" || !got[0].Start.Equal(at(15, 16, 0)) {
		t.Errorf("after update: %v, %+v", days(got), got[0])
	}

	if err := s.UpdateSeries(Series{ID: "s9"}); err != ErrNotFound {
		t.Errorf("update of a missing series: %v", err)
	}
	if err := s.DeleteSeries("s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSeries("s1"); err != ErrNotFound {
		t.Errorf("second delete: %v", err)
	}
	if got, _ := s.Range(context.Background(), at(1, 0, 0), at(30, 0, 0)); len(got) != 0 {
		t.Errorf("a deleted series still has repeats: %v", days(got))
	}
}

func TestSeriesValidation(t *testing.T) {
	good := sundayShift()
	tests := []struct {
		name   string
		change func(*Series)
		want   string
	}{
		{"no title", func(s *Series) { s.Title = " " }, "title"},
		{"no days", func(s *Series) { s.Weekdays = nil }, "day of the week"},
		{"bad day", func(s *Series) { s.Weekdays = []int{9} }, "days of the week"},
		{"every 9 weeks", func(s *Series) { s.EveryWeeks = 9 }, "1 to 4"},
		{"bad first day", func(s *Series) { s.Date = "soon" }, "first day"},
		{"until before first day", func(s *Series) { s.Until = "2026-01-01" }, "before the first day"},
		{"bad clock", func(s *Series) { s.Clock = "25:99" }, "start time"},
		{"too long", func(s *Series) { s.Minutes = 5000 }, "length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := good
			tt.change(&sr)
			_, err := NewStore().AddSeries(sr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestSeriesPersistenceAndOldFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calendar.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, sdk.Event{Title: "one-off", Start: at(10, 9, 0)})
	mustAddSeries(t, s, sundayShift())
	if err := s.SkipOccurrence("s1", "2026-11-22"); err != nil {
		t.Fatal(err)
	}

	again, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Range(context.Background(), at(1, 0, 0), at(30, 0, 0))
	if !slices.Equal(titles(got), []string{"one-off", "Work", "Work"}) {
		t.Errorf("reloaded: %v", titles(got))
	}
	next := mustAddSeries(t, again, sundayShift())
	if next.ID != "s2" {
		t.Errorf("series IDs must keep counting after a restart: got %s", next.ID)
	}

	// A calendar file from before series existed still opens.
	old := filepath.Join(t.TempDir(), "old.json")
	os.WriteFile(old, []byte(`{"next_id":3,"events":[{"ID":"2","Title":"Old","Start":"2026-11-10T09:00:00-05:00","End":"2026-11-10T10:00:00-05:00"}]}`), 0o600)
	o, err := OpenStore(old)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := o.Get("2"); !ok || e.Title != "Old" {
		t.Errorf("old file: %+v, %v", e, ok)
	}
	if saved := mustAddSeries(t, o, sundayShift()); saved.ID != "s1" {
		t.Errorf("first series in an old file got ID %s", saved.ID)
	}
}

func TestRuleText(t *testing.T) {
	tests := []struct {
		sr   Series
		rule string
		when string
	}{
		{sundayShift(), "Every Sunday", "3:00 PM–11:30 PM"},
		{Series{Weekdays: []int{2, 5}, EveryWeeks: 2, Clock: "17:00", Minutes: 240}, "Every 2 weeks on Tuesday and Friday", "5:00 PM–9:00 PM"},
		{Series{Weekdays: []int{1, 3, 5}, EveryWeeks: 1, AllDay: true, Until: "2027-01-03"}, "Every Monday, Wednesday and Friday, until Jan 3", "all day"},
	}
	for _, tt := range tests {
		if got := tt.sr.Rule(); got != tt.rule {
			t.Errorf("Rule() = %q, want %q", got, tt.rule)
		}
		if got := tt.sr.TimeText(); got != tt.when {
			t.Errorf("TimeText() = %q, want %q", got, tt.when)
		}
	}
}
