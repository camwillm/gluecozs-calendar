package calendar

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

// DefaultDuration is applied when a timed event is saved without an end time.
const DefaultDuration = 60 * time.Minute

// Store keeps events in memory. It is safe for concurrent use, because the
// web server handles each request in its own goroutine.
type Store struct {
	mu     sync.Mutex
	events []sdk.Event
	nextID int
}

func NewStore() *Store {
	return &Store{nextID: 1}
}

// normalize fills in the end time so the rest of the app can rely on it.
//
//   - All-day events start at midnight and end at midnight the next day
//     (single-day only in v1).
//   - Timed events with no end, or an end that is not after the start,
//     get Start + DefaultDuration.
func normalize(e sdk.Event) sdk.Event {
	if e.AllDay {
		y, m, d := e.Start.Date()
		e.Start = time.Date(y, m, d, 0, 0, 0, 0, e.Start.Location())
		e.End = e.Start.AddDate(0, 0, 1)
		return e
	}
	if e.End.IsZero() || !e.End.After(e.Start) {
		e.End = e.Start.Add(DefaultDuration)
	}
	return e
}

// Add saves a new event, assigns its ID, and returns the saved copy.
func (s *Store) Add(e sdk.Event) sdk.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	e = normalize(e)
	e.ID = strconv.Itoa(s.nextID)
	s.nextID++
	if e.Source == "" {
		e.Source = "calendar"
	}
	s.events = append(s.events, e)
	return e
}

// Update replaces the event with the same ID. It reports whether one was found.
func (s *Store) Update(e sdk.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.events {
		if s.events[i].ID == e.ID {
			if e.Source == "" {
				e.Source = s.events[i].Source
			}
			s.events[i] = normalize(e)
			return true
		}
	}
	return false
}

// Delete removes the event with the given ID. It reports whether one was found.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.events {
		if s.events[i].ID == id {
			s.events = slices.Delete(s.events, i, i+1)
			return true
		}
	}
	return false
}

// EventsOn returns every event that overlaps the calendar day of `day`,
// in day's own time zone. All-day events come first, then by start time.
// An event that crosses midnight shows up on both days.
func (s *Store) EventsOn(ctx context.Context, day time.Time) ([]sdk.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	y, m, d := day.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, day.Location())
	end := start.AddDate(0, 0, 1)

	s.mu.Lock()
	var out []sdk.Event
	for _, e := range s.events {
		if e.Start.Before(end) && e.End.After(start) {
			out = append(out, e)
		}
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].AllDay != out[j].AllDay {
			return out[i].AllDay
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out, nil
}

// Compile-time check that Store satisfies the SDK interface.
var _ sdk.EventProvider = (*Store)(nil)
