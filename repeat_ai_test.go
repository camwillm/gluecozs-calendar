package calendar

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

func TestParseProposalAddRepeat(t *testing.T) {
	s := NewStore()

	tests := []struct {
		name  string
		reply string
		check func(t *testing.T, sr Series)
	}{
		{
			name:  "every Sunday 3 to 11:30 PM",
			reply: `{"say":"ok","actions":[{"op":"add","title":"Work","start":"2026-11-15T15:00","end":"2026-11-15T23:30","tag":"Work","repeat":{"every_weeks":1,"weekdays":["sun"]}}]}`,
			check: func(t *testing.T, sr Series) {
				if sr.Title != "Work" || sr.Date != "2026-11-15" || sr.Clock != "15:00" || sr.Minutes != 510 ||
					sr.EveryWeeks != 1 || !slices.Equal(sr.Weekdays, []int{0}) || sr.Until != "" {
					t.Errorf("got %+v", sr)
				}
			},
		},
		{
			name:  "every other Tuesday and Friday until a date, full weekday names",
			reply: `{"actions":[{"op":"add","title":"Pay-week shift","start":"2026-11-17T17:00","end":"21:00","repeat":{"every_weeks":2,"weekdays":["Tuesday","Friday"],"until":"2027-01-03"}}]}`,
			check: func(t *testing.T, sr Series) {
				if sr.EveryWeeks != 2 || !slices.Equal(sr.Weekdays, []int{2, 5}) || sr.Until != "2027-01-03" || sr.Minutes != 240 {
					t.Errorf("got %+v", sr)
				}
			},
		},
		{
			name:  "no weekdays means the weekday of the first day",
			reply: `{"actions":[{"op":"add","title":"Review","start":"2026-11-15T11:00","repeat":{}}]}`,
			check: func(t *testing.T, sr Series) {
				if !slices.Equal(sr.Weekdays, []int{0}) || sr.Minutes != 60 || sr.EveryWeeks != 1 {
					t.Errorf("got %+v", sr)
				}
			},
		},
		{
			name:  "all-day repeat",
			reply: `{"actions":[{"op":"add","title":"Weigh-in","start":"2026-11-16","all_day":true,"repeat":{"weekdays":["mon"]}}]}`,
			check: func(t *testing.T, sr Series) {
				if !sr.AllDay || sr.Clock != "" || sr.Minutes != 0 {
					t.Errorf("got %+v", sr)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parseProposal(tt.reply, s, ny, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Actions) != 1 || p.Actions[0].Op != "add_series" {
				t.Fatalf("actions = %+v", p.Actions)
			}
			tt.check(t, p.Actions[0].Series)
		})
	}
	if s.Len() != 0 {
		t.Error("parsing a proposal must not change the calendar")
	}
}

func TestParseProposalRepeatRejects(t *testing.T) {
	s := NewStore()
	tests := []struct {
		name  string
		reply string
		want  string
	}{
		{"unknown weekday", `{"actions":[{"op":"add","title":"x","start":"2026-11-15T15:00","repeat":{"weekdays":["funday"]}}]}`, "unknown day"},
		{"every 9 weeks", `{"actions":[{"op":"add","title":"x","start":"2026-11-15T15:00","repeat":{"every_weeks":9,"weekdays":["sun"]}}]}`, "1 to 4"},
		{"ends before it starts", `{"actions":[{"op":"add","title":"x","start":"2026-11-15T15:00","end":"2026-11-15T14:00","repeat":{"weekdays":["sun"]}}]}`, "end time"},
		{"runs past midnight", `{"actions":[{"op":"add","title":"x","start":"2026-11-15T22:00","end":"2026-11-16T02:00","repeat":{"weekdays":["sun"]}}]}`, "midnight"},
		{"until before start", `{"actions":[{"op":"add","title":"x","start":"2026-11-15T15:00","repeat":{"weekdays":["sun"],"until":"2026-10-01"}}]}`, "before the first day"},
		{"year slip in until", `{"actions":[{"op":"add","title":"x","start":"2026-11-15T15:00","repeat":{"weekdays":["sun"],"until":"2031-01-01"}}]}`, "outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseProposal(tt.reply, s, ny, testNow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestParseProposalChangesToSeries(t *testing.T) {
	s := NewStore()
	sr := mustAddSeries(t, s, sundayShift())

	t.Run("delete one day skips it", func(t *testing.T) {
		p, err := parseProposal(`{"actions":[{"op":"delete","id":"s1:2026-11-22"}]}`, s, ny, testNow)
		if err != nil {
			t.Fatal(err)
		}
		a := p.Actions[0]
		if a.Op != "skip" || a.Series.ID != "s1" || a.Date != "2026-11-22" || a.Event.Title != "Work" {
			t.Errorf("got %+v", a)
		}
	})

	t.Run("delete the series", func(t *testing.T) {
		p, err := parseProposal(`{"actions":[{"op":"delete","id":"s1"}]}`, s, ny, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if p.Actions[0].Op != "delete_series" || p.Actions[0].Series.Title != "Work" {
			t.Errorf("got %+v", p.Actions[0])
		}
	})

	t.Run("move one day becomes skip plus a one-off", func(t *testing.T) {
		p, err := parseProposal(`{"actions":[{"op":"update","id":"s1:2026-11-22","start":"2026-11-22T16:00"}]}`, s, ny, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Actions) != 2 || p.Actions[0].Op != "skip" || p.Actions[1].Op != "add" {
			t.Fatalf("actions = %+v", p.Actions)
		}
		added := p.Actions[1].Event
		if added.Title != "Work" || added.ID != "" || !added.Start.Equal(at(22, 16, 0)) || !added.End.Equal(at(23, 0, 30)) {
			t.Errorf("one-off = %+v (start %v, end %v)", added, added.Start, added.End)
		}
	})

	t.Run("change the time of the whole series", func(t *testing.T) {
		p, err := parseProposal(`{"actions":[{"op":"update","id":"s1","start":"16:00","end":"23:30"}]}`, s, ny, testNow)
		if err != nil {
			t.Fatal(err)
		}
		a := p.Actions[0]
		if a.Op != "update_series" || a.Series.Clock != "16:00" || a.Series.Minutes != 450 || a.BeforeSeries.Clock != "15:00" {
			t.Errorf("got %+v", a)
		}
	})

	t.Run("a full date and time is accepted as a time of day", func(t *testing.T) {
		p, err := parseProposal(`{"actions":[{"op":"update","id":"s1","start":"2026-11-15T14:00"}]}`, s, ny, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if p.Actions[0].Series.Clock != "14:00" || p.Actions[0].Series.Minutes != 510 {
			t.Errorf("got %+v", p.Actions[0].Series)
		}
	})

	t.Run("rename and change the days", func(t *testing.T) {
		p, err := parseProposal(`{"actions":[{"op":"update","id":"s1","title":"Shift","repeat":{"weekdays":["sat","sun"],"until":"2026-12-20"}}]}`, s, ny, testNow)
		if err != nil {
			t.Fatal(err)
		}
		a := p.Actions[0].Series
		if a.Title != "Shift" || !slices.Equal(a.Weekdays, []int{0, 6}) || a.Until != "2026-12-20" {
			t.Errorf("got %+v", a)
		}
	})

	if got, _ := s.GetSeries(sr.ID); got.Title != "Work" {
		t.Error("parsing a proposal must not change the series")
	}
}

func TestParseProposalSeriesRejects(t *testing.T) {
	s := NewStore()
	mustAddSeries(t, s, sundayShift())

	tests := []struct {
		name  string
		reply string
		want  string
	}{
		{"missing series", `{"actions":[{"op":"delete","id":"s9"}]}`, "no series"},
		{"day that is not a repeat", `{"actions":[{"op":"delete","id":"s1:2026-11-23"}]}`, "no event"},
		{"repeat on a single day", `{"actions":[{"op":"update","id":"s1:2026-11-22","repeat":{"weekdays":["mon"]}}]}`, "own repeat"},
		{"all-day toggle", `{"actions":[{"op":"update","id":"s1","all_day":true}]}`, "all-day"},
		{"series twice", `{"actions":[{"op":"delete","id":"s1"},{"op":"update","id":"s1","title":"x"}]}`, "twice"},
		{"series and a day of it", `{"actions":[{"op":"delete","id":"s1"},{"op":"delete","id":"s1:2026-11-22"}]}`, "as a whole"},
		{"same day twice", `{"actions":[{"op":"delete","id":"s1:2026-11-22"},{"op":"delete","id":"s1:2026-11-22"}]}`, "twice"},
		{"end before start", `{"actions":[{"op":"update","id":"s1","end":"14:00"}]}`, "end time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseProposal(tt.reply, s, ny, testNow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestPromptListsSeriesAndOccurrenceIDs(t *testing.T) {
	s := NewStore()
	sr := mustAddSeries(t, s, sundayShift())
	events, _ := s.Range(t.Context(), at(9, 0, 0), at(20, 0, 0))

	system, prompt := buildPrompt(testNow, ny, events, []Series{sr}, "skip this sunday")

	for _, want := range []string{
		"s1:2026-11-15 | 2026-11-15T15:00 | 2026-11-15T23:30 | Work | Work",
		"s1 | Every Sunday | 3:00 PM–11:30 PM | Work | Work",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q\n%s", want, prompt)
		}
	}
	if !strings.Contains(system, `"repeat"`) || !strings.Contains(system, "whole repeating series") {
		t.Error("the system prompt does not explain repeats")
	}
}

func TestAskRepeatingEventEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.ai.reply = `{"say":"Adding your Sunday shift.","actions":[{"op":"add","title":"Work","start":"2026-11-15T15:00","end":"23:30","tag":"Work","repeat":{"weekdays":["sun"]}}]}`

	status, body, _ := e.post(t, "/m/calendar/ask", url.Values{"text": {"add work every sunday 3 to 11:30 pm"}})
	if status != http.StatusOK || !strings.Contains(body, "Work: Every Sunday, 3:00 PM–11:30 PM, starting Sun Nov 15") || !strings.Contains(body, ">Repeat<") {
		t.Fatalf("confirm page: status %d\n%s", status, body)
	}
	if e.mod.store.Len() != 0 {
		t.Fatal("the calendar changed before confirming")
	}

	token := between(body, `name="token" value="`, `"`)
	if status, _, _ := e.post(t, "/m/calendar/apply", url.Values{"token": {token}}); status != http.StatusSeeOther {
		t.Fatalf("apply status = %d", status)
	}

	if len(e.mod.store.AllSeries()) != 1 {
		t.Fatal("the series was not created")
	}
	got, _ := e.mod.store.Range(t.Context(), at(1, 0, 0), at(30, 0, 0))
	if !slices.Equal(days(got), []string{"Nov 15", "Nov 22", "Nov 29"}) {
		t.Errorf("repeats = %v", days(got))
	}

	// Then skip one Sunday through the AI.
	e.ai.reply = `{"actions":[{"op":"delete","id":"s1:2026-11-22"}]}`
	_, body, _ = e.post(t, "/m/calendar/ask", url.Values{"text": {"skip this sunday the 22nd"}})
	if !strings.Contains(body, "this day only") || !strings.Contains(body, ">Skip<") {
		t.Fatalf("skip confirm page:\n%s", body)
	}
	token = between(body, `name="token" value="`, `"`)
	e.post(t, "/m/calendar/apply", url.Values{"token": {token}})
	got, _ = e.mod.store.Range(t.Context(), at(1, 0, 0), at(30, 0, 0))
	if !slices.Equal(days(got), []string{"Nov 15", "Nov 29"}) {
		t.Errorf("repeats after skipping = %v", days(got))
	}
}

func TestAddRepeatingEventFromTheForm(t *testing.T) {
	e := newEnv(t)

	status, _, hdr := e.post(t, "/m/calendar/events", url.Values{
		"title": {"Pay-week shift"}, "date": {"2026-11-17"}, "start": {"17:00"}, "end": {"21:00"}, "tag": {"Work"},
		"repeat": {"1"}, "every": {"2"}, "days": {"tue", "fri"}, "until": {"2026-12-18"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d", status)
	}
	_ = hdr

	sr, ok := e.mod.store.GetSeries("s1")
	if !ok || sr.EveryWeeks != 2 || !slices.Equal(sr.Weekdays, []int{2, 5}) || sr.Clock != "17:00" || sr.Minutes != 240 || sr.Until != "2026-12-18" {
		t.Fatalf("series = %+v, ok=%v", sr, ok)
	}

	got, _ := e.mod.store.Range(t.Context(), at(9, 0, 0), time.Date(2027, 1, 5, 0, 0, 0, 0, ny))
	want := []string{"Nov 17", "Nov 20", "Dec 1", "Dec 4", "Dec 15", "Dec 18"}
	if !slices.Equal(days(got), want) {
		t.Errorf("repeats = %v, want %v", days(got), want)
	}

	// The page labels them and offers the right buttons.
	_, page := e.get(t, "/m/calendar")
	for _, want := range []string{"repeats", "Skip this day", "Edit all", "Repeating events", "Every 2 weeks on Tuesday and Friday, until Dec 18"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestRepeatFormErrorsKeepTheValues(t *testing.T) {
	e := newEnv(t)
	status, body, _ := e.post(t, "/m/calendar/events", url.Values{
		"title": {"Shift"}, "date": {"2026-11-15"}, "start": {"15:00"}, "repeat": {"1"}, "every": {"1"},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "at least one day") {
		t.Fatalf("status %d\n%s", status, body)
	}
	if !strings.Contains(body, `value="Shift"`) || !strings.Contains(body, `name="repeat" value="1" checked`) {
		t.Error("the form lost what was typed")
	}
	if e.mod.store.Len() != 0 {
		t.Error("an invalid series was saved")
	}

	status, body, _ = e.post(t, "/m/calendar/events", url.Values{
		"title": {"Shift"}, "date": {"2026-11-15"}, "start": {"15:00"}, "repeat": {"1"}, "days": {"sun"}, "until": {"2026-01-01"},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "not be before the first day") {
		t.Errorf("status %d\n%s", status, body)
	}
}

func TestEditSkipAndDeleteSeriesFromThePage(t *testing.T) {
	e := newEnv(t)
	mustAddSeries(t, e.mod.store, sundayShift())

	// Both the series id and one day's id open the series editor.
	for _, id := range []string{"s1", "s1:2026-11-22"} {
		status, page := e.get(t, "/m/calendar/edit?id="+url.QueryEscape(id))
		if status != http.StatusOK || !strings.Contains(page, `value="Work"`) || !strings.Contains(page, `value="15:00"`) ||
			!strings.Contains(page, `value="23:30"`) || !strings.Contains(page, "changes here apply to every repeat") {
			t.Fatalf("edit page for %s: status %d\n%s", id, status, page)
		}
		if !strings.Contains(page, `value="sun" checked`) {
			t.Errorf("edit page for %s does not show Sunday ticked", id)
		}
	}

	status, _, _ := e.post(t, "/m/calendar/update", url.Values{
		"id": {"s1"}, "title": {"Work (late)"}, "date": {"2026-11-15"}, "start": {"16:00"}, "end": {"23:30"},
		"repeat": {"1"}, "every": {"1"}, "days": {"sun", "sat"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("update status = %d", status)
	}
	sr, _ := e.mod.store.GetSeries("s1")
	if sr.Title != "Work (late)" || sr.Clock != "16:00" || sr.Minutes != 450 || !slices.Equal(sr.Weekdays, []int{0, 6}) {
		t.Errorf("series = %+v", sr)
	}

	status, _, _ = e.post(t, "/m/calendar/delete", url.Values{"id": {"s1:2026-11-22"}})
	if status != http.StatusSeeOther {
		t.Fatalf("skip status = %d", status)
	}
	if sr, _ := e.mod.store.GetSeries("s1"); !slices.Equal(sr.Skip, []string{"2026-11-22"}) {
		t.Errorf("skip list = %v", sr.Skip)
	}

	if status, _, _ = e.post(t, "/m/calendar/delete", url.Values{"id": {"s1"}}); status != http.StatusSeeOther {
		t.Fatalf("delete series status = %d", status)
	}
	if len(e.mod.store.AllSeries()) != 0 {
		t.Error("the series was not deleted")
	}
	if status, _, _ = e.post(t, "/m/calendar/delete", url.Values{"id": {"s1"}}); status != http.StatusNotFound {
		t.Errorf("deleting twice: status %d, want 404", status)
	}
}

func TestOneOffEditPageHasNoRepeatOptions(t *testing.T) {
	e := newEnv(t)
	mustAdd(t, e.mod.store, sdk.Event{Title: "Dentist", Start: at(12, 15, 0)})

	_, page := e.get(t, "/m/calendar/edit?id=1")
	if strings.Contains(page, `name="every"`) || strings.Contains(page, `name="repeat"`) {
		t.Error("the edit page for a one-off event offers repeat options")
	}
}
