package calendar

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

var ny = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// at builds a time on the given day of November 2026, in New York.
func at(day, hour, min int) time.Time {
	return time.Date(2026, time.November, day, hour, min, 0, 0, ny)
}

func titles(evs []sdk.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Title
	}
	return out
}

func TestAddEndTimes(t *testing.T) {
	tests := []struct {
		name    string
		in      sdk.Event
		wantEnd time.Time
	}{
		{
			name:    "missing end gets default duration",
			in:      sdk.Event{Title: "a", Start: at(10, 9, 0)},
			wantEnd: at(10, 10, 0),
		},
		{
			name:    "explicit end is kept",
			in:      sdk.Event{Title: "b", Start: at(10, 9, 0), End: at(10, 11, 30)},
			wantEnd: at(10, 11, 30),
		},
		{
			name:    "end before start is replaced",
			in:      sdk.Event{Title: "c", Start: at(10, 9, 0), End: at(10, 8, 0)},
			wantEnd: at(10, 10, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore()
			got := s.Add(tt.in)
			if !got.End.Equal(tt.wantEnd) {
				t.Errorf("End = %v, want %v", got.End, tt.wantEnd)
			}
			if got.ID == "" {
				t.Error("expected an ID to be assigned")
			}
		})
	}
}

func TestAddAllDay(t *testing.T) {
	s := NewStore()
	got := s.Add(sdk.Event{Title: "Holiday", AllDay: true, Start: at(10, 15, 30)})

	if !got.Start.Equal(at(10, 0, 0)) {
		t.Errorf("Start = %v, want midnight", got.Start)
	}
	if !got.End.Equal(at(11, 0, 0)) {
		t.Errorf("End = %v, want midnight next day", got.End)
	}
}

func TestEventsOn(t *testing.T) {
	s := NewStore()
	s.Add(sdk.Event{Title: "Other day", Start: at(9, 9, 0)})
	s.Add(sdk.Event{Title: "Afternoon", Start: at(10, 14, 0)})
	s.Add(sdk.Event{Title: "Early", Start: at(10, 8, 0)})
	s.Add(sdk.Event{Title: "Holiday", Start: at(10, 0, 0), AllDay: true})
	s.Add(sdk.Event{Title: "Late night", Start: at(10, 23, 30), End: at(11, 0, 30)})
	s.Add(sdk.Event{Title: "Next day", Start: at(11, 9, 0)})

	tests := []struct {
		name string
		day  time.Time
		want []string
	}{
		{
			name: "all-day first, then by start time",
			day:  at(10, 12, 0),
			want: []string{"Holiday", "Early", "Afternoon", "Late night"},
		},
		{
			name: "event crossing midnight also shows next day",
			day:  at(11, 0, 0),
			want: []string{"Late night", "Next day"},
		},
		{
			name: "empty day",
			day:  at(20, 0, 0),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.EventsOn(context.Background(), tt.day)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(titles(got), tt.want) {
				t.Errorf("got %v, want %v", titles(got), tt.want)
			}
		})
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s := NewStore()
	saved := s.Add(sdk.Event{Title: "Old title", Start: at(10, 9, 0)})

	saved.Title = "New title"
	if !s.Update(saved) {
		t.Fatal("Update reported not found")
	}
	got, _ := s.EventsOn(context.Background(), at(10, 0, 0))
	if len(got) != 1 || got[0].Title != "New title" {
		t.Fatalf("after update got %v", titles(got))
	}

	if s.Update(sdk.Event{ID: "does-not-exist"}) {
		t.Error("Update of unknown ID should report false")
	}

	if !s.Delete(saved.ID) {
		t.Fatal("Delete reported not found")
	}
	if s.Delete(saved.ID) {
		t.Error("second Delete should report false")
	}
	got, _ = s.EventsOn(context.Background(), at(10, 0, 0))
	if len(got) != 0 {
		t.Errorf("expected no events after delete, got %v", titles(got))
	}
}

func TestEventsOnCancelledContext(t *testing.T) {
	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.EventsOn(ctx, at(10, 0, 0)); err == nil {
		t.Error("expected an error from a cancelled context")
	}
}

// Run with: go test -race ./...
func TestConcurrentAdd(t *testing.T) {
	s := NewStore()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Add(sdk.Event{Title: "x", Start: at(10, 9, 0)})
		}()
	}
	wg.Wait()

	got, err := s.EventsOn(context.Background(), at(10, 0, 0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d events, want 50", len(got))
	}

	seen := make(map[string]bool)
	for _, e := range got {
		if seen[e.ID] {
			t.Fatalf("duplicate ID %q", e.ID)
		}
		seen[e.ID] = true
	}
}
