package calendar

import (
	"context"
	"net/http"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

// Module is what the core registers. It owns the store and the time zone.
type Module struct {
	store *Store
	loc   *time.Location
	host  sdk.Host
	pend  *pendingStore
	now   func() time.Time
}

// New builds the calendar module. loc is the single time zone used in v1,
// for example America/New_York.
func New(store *Store, loc *time.Location) *Module {
	return &Module{store: store, loc: loc, pend: newPendingStore(), now: time.Now}
}

func (c *Module) ID() string { return "calendar" }

// Store gives other code (seeding, tests) access to the events.
func (c *Module) Store() *Store { return c.store }

// Nav is the entry shown in the molecule nav.
func (c *Module) Nav() sdk.NavItem {
	return sdk.NavItem{
		ID:     "calendar",
		Label:  "Cal",
		Href:   "/m/calendar",
		Icon:   `<rect x="3" y="5" width="18" height="16" rx="2"/><path d="M3 10h18M8 3v4M16 3v4"/>`,
		Status: "ok",
	}
}

// EventsOn implements sdk.EventProvider. The caller's calendar date
// (year, month, day) is read as-is and interpreted in the module's time zone.
func (c *Module) EventsOn(ctx context.Context, day time.Time) ([]sdk.Event, error) {
	y, m, d := day.Date()
	return c.store.EventsOn(ctx, time.Date(y, m, d, 0, 0, 0, 0, c.loc))
}

// Mount registers the module's pages and endpoints.
func (c *Module) Mount(mux *http.ServeMux, host sdk.Host) {
	c.host = host

	mux.HandleFunc("GET /m/calendar", c.page)
	mux.HandleFunc("POST /m/calendar/events", c.addEvent)
	mux.HandleFunc("GET /m/calendar/edit", c.editPage)
	mux.HandleFunc("POST /m/calendar/update", c.updateEvent)
	mux.HandleFunc("POST /m/calendar/delete", c.removeEvent)
	mux.HandleFunc("POST /m/calendar/ask", c.ask)
	mux.HandleFunc("POST /m/calendar/apply", c.applyProposal)
	mux.HandleFunc("POST /m/calendar/discard", c.discardProposal)
	mux.HandleFunc("POST /m/calendar/voice", c.voice)
}

var (
	_ sdk.Module        = (*Module)(nil)
	_ sdk.EventProvider = (*Module)(nil)
	_ sdk.Mounter       = (*Module)(nil)
)
