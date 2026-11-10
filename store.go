package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

// DefaultDuration is applied when a timed event is saved without an end time.
const DefaultDuration = 60 * time.Minute

// ErrNotFound is returned when an event or series ID does not exist.
var ErrNotFound = errors.New("event not found")

// state is everything the store holds, and everything that is written to the
// file. Older files without series still load.
type state struct {
	Events       []sdk.Event `json:"events"`
	NextID       int         `json:"next_id"`
	Series       []Series    `json:"series,omitempty"`
	NextSeriesID int         `json:"next_series_id,omitempty"`
}

func (st state) clone() state {
	out := st
	out.Events = slices.Clone(st.Events)
	out.Series = make([]Series, len(st.Series))
	for i, s := range st.Series {
		out.Series[i] = s.clone()
	}
	return out
}

// Store keeps one-off events and repeating series in memory and, when opened
// with OpenStore, mirrors them to a JSON file after every change. It is safe
// for concurrent use, because the web server handles each request in its own
// goroutine.
type Store struct {
	mu   sync.Mutex
	st   state
	path string // empty means memory only
}

// NewStore returns a memory-only store (used by tests).
func NewStore() *Store {
	return &Store{st: state{NextID: 1, NextSeriesID: 1}}
}

// OpenStore loads the calendar from path, or starts empty if the file does
// not exist yet. Every later change is written back to the same file.
func OpenStore(path string) (*Store, error) {
	s := NewStore()
	s.path = path

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if st.NextID < 1 {
		st.NextID = 1
	}
	if st.NextSeriesID < 1 {
		st.NextSeriesID = 1
	}
	s.st = st
	return s, nil
}

// save writes st to disk (if the store has a file) using a temp file and
// rename, so a crash never leaves a half-written calendar.
func (s *Store) save(st state) error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".calendar-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// mutate changes a copy of the state, writes it to disk, and only then swaps
// it in. If anything fails, the calendar in memory is exactly as it was.
func (s *Store) mutate(change func(st *state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.st.clone()
	if err := change(&next); err != nil {
		return err
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.st = next
	return nil
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

// Len reports how many one-off events and series the store holds.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.st.Events) + len(s.st.Series)
}

// Add saves a new one-off event, assigns its ID, and returns the saved copy.
func (s *Store) Add(e sdk.Event) (sdk.Event, error) {
	var saved sdk.Event
	err := s.mutate(func(st *state) error {
		e = normalize(e)
		e.ID = strconv.Itoa(st.NextID)
		st.NextID++
		if e.Source == "" {
			e.Source = "calendar"
		}
		st.Events = append(st.Events, e)
		saved = e
		return nil
	})
	return saved, err
}

// Get returns the one-off event with the given ID. Repeats of a series are
// found with GetOccurrence.
func (s *Store) Get(id string) (sdk.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.st.Events {
		if e.ID == id {
			return e, true
		}
	}
	return sdk.Event{}, false
}

// Update replaces the one-off event with the same ID. It returns ErrNotFound
// if there is no such event.
func (s *Store) Update(e sdk.Event) error {
	return s.mutate(func(st *state) error {
		i := slices.IndexFunc(st.Events, func(x sdk.Event) bool { return x.ID == e.ID })
		if i < 0 {
			return ErrNotFound
		}
		if e.Source == "" {
			e.Source = st.Events[i].Source
		}
		st.Events[i] = normalize(e)
		return nil
	})
}

// Delete removes the one-off event with the given ID. It returns ErrNotFound
// if there is no such event.
func (s *Store) Delete(id string) error {
	return s.mutate(func(st *state) error {
		i := slices.IndexFunc(st.Events, func(x sdk.Event) bool { return x.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		st.Events = slices.Delete(st.Events, i, i+1)
		return nil
	})
}

// AddSeries saves a new repeating series and returns it with its ID ("s1").
func (s *Store) AddSeries(sr Series) (Series, error) {
	var saved Series
	err := s.mutate(func(st *state) error {
		sr = normalizeSeries(sr)
		if err := validateSeries(sr); err != nil {
			return err
		}
		sr.ID = "s" + strconv.Itoa(st.NextSeriesID)
		st.NextSeriesID++
		if sr.Calendar == "" {
			sr.Calendar = "personal"
		}
		st.Series = append(st.Series, sr)
		saved = sr
		return nil
	})
	return saved, err
}

// GetSeries returns the series with the given ID.
func (s *Store) GetSeries(id string) (Series, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sr := range s.st.Series {
		if sr.ID == id {
			return sr.clone(), true
		}
	}
	return Series{}, false
}

// AllSeries returns every series, in the order they were created.
func (s *Store) AllSeries() []Series {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Series, len(s.st.Series))
	for i, sr := range s.st.Series {
		out[i] = sr.clone()
	}
	return out
}

// UpdateSeries replaces the series with the same ID. The days that were
// skipped are kept. It returns ErrNotFound if there is no such series.
func (s *Store) UpdateSeries(sr Series) error {
	return s.mutate(func(st *state) error {
		i := slices.IndexFunc(st.Series, func(x Series) bool { return x.ID == sr.ID })
		if i < 0 {
			return ErrNotFound
		}
		sr.Skip = st.Series[i].Skip
		if sr.Calendar == "" {
			sr.Calendar = st.Series[i].Calendar
		}
		sr = normalizeSeries(sr)
		if err := validateSeries(sr); err != nil {
			return err
		}
		st.Series[i] = sr
		return nil
	})
}

// DeleteSeries removes a series and every repeat of it.
func (s *Store) DeleteSeries(id string) error {
	return s.mutate(func(st *state) error {
		i := slices.IndexFunc(st.Series, func(x Series) bool { return x.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		st.Series = slices.Delete(st.Series, i, i+1)
		return nil
	})
}

// SkipOccurrence removes a single day from a series. It returns ErrNotFound
// if the series has no repeat on that day.
func (s *Store) SkipOccurrence(seriesID, date string) error {
	return s.mutate(func(st *state) error {
		i := slices.IndexFunc(st.Series, func(x Series) bool { return x.ID == seriesID })
		if i < 0 {
			return ErrNotFound
		}
		day, err := time.Parse(dateLayout, date)
		if err != nil || !st.Series[i].occursOn(day) {
			return ErrNotFound
		}
		st.Series[i].Skip = append(st.Series[i].Skip, day.Format(dateLayout))
		st.Series[i] = normalizeSeries(st.Series[i])
		return nil
	})
}

// GetOccurrence returns one repeat of a series, such as "s3:2026-11-15", as
// an event in the given time zone.
func (s *Store) GetOccurrence(id string, loc *time.Location) (sdk.Event, bool) {
	seriesID, date, ok := splitID(id)
	if !ok {
		return sdk.Event{}, false
	}
	sr, found := s.GetSeries(seriesID)
	if !found {
		return sdk.Event{}, false
	}
	day, err := time.Parse(dateLayout, date)
	if err != nil || !sr.occursOn(day) {
		return sdk.Event{}, false
	}
	return sr.occurrence(day.Year(), day.Month(), day.Day(), loc), true
}

// Range returns every event that overlaps [from, to), ordered by start time:
// one-off events and the repeats of every series. Repeats are worked out in
// from's time zone.
func (s *Store) Range(ctx context.Context, from, to time.Time) ([]sdk.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	var out []sdk.Event
	for _, e := range s.st.Events {
		if e.Start.Before(to) && e.End.After(from) {
			out = append(out, e)
		}
	}
	for _, sr := range s.st.Series {
		out = append(out, sr.occurrencesBetween(from, to)...)
	}
	s.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// EventsOn returns every event that overlaps the calendar day of `day`,
// in day's own time zone. All-day events come first, then by start time.
// An event that crosses midnight shows up on both days.
func (s *Store) EventsOn(ctx context.Context, day time.Time) ([]sdk.Event, error) {
	y, m, d := day.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, day.Location())

	out, err := s.Range(ctx, start, start.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AllDay != out[j].AllDay {
			return out[i].AllDay
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out, nil
}

// Compile-time check that Store satisfies the SDK interface.
var _ sdk.EventProvider = (*Store)(nil)
