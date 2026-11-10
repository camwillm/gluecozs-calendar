package calendar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func mustAdd(t *testing.T, s *Store, e sdk.Event) sdk.Event {
	t.Helper()
	saved, err := s.Add(e)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return saved
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
			got := mustAdd(t, NewStore(), tt.in)
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
	got := mustAdd(t, NewStore(), sdk.Event{Title: "Holiday", AllDay: true, Start: at(10, 15, 30)})

	if !got.Start.Equal(at(10, 0, 0)) {
		t.Errorf("Start = %v, want midnight", got.Start)
	}
	if !got.End.Equal(at(11, 0, 0)) {
		t.Errorf("End = %v, want midnight next day", got.End)
	}
}

func TestEventsOn(t *testing.T) {
	s := NewStore()
	mustAdd(t, s, sdk.Event{Title: "Other day", Start: at(9, 9, 0)})
	mustAdd(t, s, sdk.Event{Title: "Afternoon", Start: at(10, 14, 0)})
	mustAdd(t, s, sdk.Event{Title: "Early", Start: at(10, 8, 0)})
	mustAdd(t, s, sdk.Event{Title: "Holiday", Start: at(10, 0, 0), AllDay: true})
	mustAdd(t, s, sdk.Event{Title: "Late night", Start: at(10, 23, 30), End: at(11, 0, 30)})
	mustAdd(t, s, sdk.Event{Title: "Next day", Start: at(11, 9, 0)})

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

func TestRangeIsOrderedByStart(t *testing.T) {
	s := NewStore()
	mustAdd(t, s, sdk.Event{Title: "later", Start: at(12, 9, 0)})
	mustAdd(t, s, sdk.Event{Title: "all-day on 10th", Start: at(10, 0, 0), AllDay: true})
	mustAdd(t, s, sdk.Event{Title: "outside", Start: at(30, 9, 0)})

	got, err := s.Range(context.Background(), at(10, 0, 0), at(20, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"all-day on 10th", "later"}
	if !slices.Equal(titles(got), want) {
		t.Errorf("got %v, want %v", titles(got), want)
	}
}

func TestGetUpdateDelete(t *testing.T) {
	s := NewStore()
	saved := mustAdd(t, s, sdk.Event{Title: "Old title", Start: at(10, 9, 0)})

	if _, ok := s.Get(saved.ID); !ok {
		t.Fatal("Get did not find the event")
	}
	if _, ok := s.Get("nope"); ok {
		t.Error("Get found an event that does not exist")
	}

	saved.Title = "New title"
	if err := s.Update(saved); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get(saved.ID)
	if got.Title != "New title" || got.Source != "calendar" {
		t.Fatalf("after update got %+v", got)
	}

	if err := s.Update(sdk.Event{ID: "does-not-exist"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update of unknown ID: got %v, want ErrNotFound", err)
	}

	if err := s.Delete(saved.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(saved.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: got %v, want ErrNotFound", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d after delete, want 0", s.Len())
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

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "calendar.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := mustAdd(t, s, sdk.Event{Title: "keep me", Start: at(10, 9, 0)})
	second := mustAdd(t, s, sdk.Event{Title: "delete me", Start: at(11, 9, 0)})
	if err := s.Delete(second.ID); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %v, want 0600", perm)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get(first.ID)
	if !ok || got.Title != "keep me" || !got.Start.Equal(at(10, 9, 0)) || !got.End.Equal(at(10, 10, 0)) {
		t.Fatalf("reloaded event = %+v, ok=%v", got, ok)
	}
	if reopened.Len() != 1 {
		t.Errorf("Len = %d, want 1", reopened.Len())
	}

	// IDs keep counting after a restart, so a deleted ID is never reused.
	third := mustAdd(t, reopened, sdk.Event{Title: "new", Start: at(12, 9, 0)})
	if third.ID == first.ID || third.ID == second.ID {
		t.Errorf("ID %s was reused", third.ID)
	}
}

func TestOpenStoreRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calendar.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Error("expected an error for a corrupt file")
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
			if _, err := s.Add(sdk.Event{Title: "x", Start: at(10, 9, 0)}); err != nil {
				t.Error(err)
			}
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
