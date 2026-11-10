package calendar

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

//go:embed templates/calendar.html
var tplFS embed.FS

var tpl = template.Must(template.ParseFS(tplFS, "templates/calendar.html"))

type dayOpt struct {
	Value, Label string
	Checked      bool
}

// weekdayOrder is the order the day checkboxes appear in: Monday first.
var weekdayOrder = []struct {
	value, label string
	day          int
}{
	{"mon", "Mon", 1}, {"tue", "Tue", 2}, {"wed", "Wed", 3}, {"thu", "Thu", 4},
	{"fri", "Fri", 5}, {"sat", "Sat", 6}, {"sun", "Sun", 0},
}

func dayOptions(selected []int) []dayOpt {
	opts := make([]dayOpt, 0, len(weekdayOrder))
	for _, w := range weekdayOrder {
		on := false
		for _, d := range selected {
			on = on || d == w.day
		}
		opts = append(opts, dayOpt{Value: w.value, Label: w.label, Checked: on})
	}
	return opts
}

type formValues struct {
	Title, Date, Start, End, Tag string
	AllDay                       bool

	ShowRepeat   bool // show the repeat options at all
	RepeatLocked bool // editing a series: repeating can't be switched off
	Repeat       bool
	Every, Until string
	Days         []dayOpt
}

func blankForm(date string) formValues {
	return formValues{Date: date, ShowRepeat: true, Every: "1", Days: dayOptions(nil)}
}

type eventView struct {
	ID, SeriesID, Title, Tag, When string
	Repeats                        bool
}

type dayGroup struct {
	Label  string
	Events []eventView
}

type seriesView struct {
	ID, Title, Tag, Rule, Time string
}

type actionView struct {
	Kind, Text string
}

// pageData feeds every template in templates/calendar.html.
type pageData struct {
	Error, Notice string
	AIReady       bool
	SpeechReady   bool
	Groups        []dayGroup
	SeriesList    []seriesView
	Form          formValues
	ID            string // edit page

	Token    string // confirm page
	Say      string
	Original string
	Actions  []actionView
}

// render fills a template and hands the result to the core, which wraps it
// in the dashboard layout.
func (c *Module) render(w http.ResponseWriter, r *http.Request, status int, name, title string, d pageData) {
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, d); err != nil {
		log.Printf("calendar: render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c.host.Page(w, r, status, "calendar", title, template.HTML(buf.String()))
}

func (c *Module) today() time.Time {
	y, m, d := c.now().In(c.loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, c.loc)
}

// baseData is what the main page needs: the upcoming events, the repeating
// series, and which AI features are switched on.
func (c *Module) baseData(ctx context.Context) pageData {
	d := pageData{
		AIReady:     c.host.AI().Available(),
		SpeechReady: c.host.Speech().Available(),
		Form:        blankForm(c.today().Format(dateLayout)),
	}
	groups, err := c.upcoming(ctx)
	if err != nil {
		log.Printf("calendar: upcoming: %v", err)
		d.Error = "Could not load events."
	}
	d.Groups = groups

	for _, s := range c.store.AllSeries() {
		d.SeriesList = append(d.SeriesList, seriesView{
			ID: s.ID, Title: s.Title, Tag: s.Tag, Rule: s.Rule(), Time: s.TimeText(),
		})
	}
	return d
}

// upcoming lists the next 14 days of events, grouped by the day they start
// (or today, for an event that started earlier and is still running).
func (c *Module) upcoming(ctx context.Context) ([]dayGroup, error) {
	from := c.today()
	events, err := c.store.Range(ctx, from, from.AddDate(0, 0, 14))
	if err != nil {
		return nil, err
	}

	var groups []dayGroup
	index := map[string]int{}
	for _, e := range events {
		day := e.Start.In(c.loc)
		if day.Before(from) {
			day = from
		}
		key := day.Format(dateLayout)
		i, ok := index[key]
		if !ok {
			groups = append(groups, dayGroup{Label: day.Format("Monday, Jan 2")})
			i = len(groups) - 1
			index[key] = i
		}
		seriesID, _, repeats := splitID(e.ID)
		groups[i].Events = append(groups[i].Events, eventView{
			ID: e.ID, SeriesID: seriesID, Title: e.Title, Tag: e.Tag, When: c.when(e), Repeats: repeats,
		})
	}
	return groups, nil
}

func (c *Module) when(e sdk.Event) string {
	if e.AllDay {
		return "All day"
	}
	return e.Start.In(c.loc).Format("3:04 PM") + "–" + e.End.In(c.loc).Format("3:04 PM")
}

// describe renders an event for the confirm page, for example
// "Thu Nov 12, 3:00 PM–4:00 PM".
func (c *Module) describe(e sdk.Event) string {
	s := e.Start.In(c.loc)
	if e.AllDay {
		return s.Format("Mon Jan 2") + " (all day)"
	}
	en := e.End.In(c.loc)
	if s.Format(dateLayout) == en.Format(dateLayout) {
		return s.Format("Mon Jan 2, 3:04 PM") + "–" + en.Format("3:04 PM")
	}
	return s.Format("Mon Jan 2, 3:04 PM") + " – " + en.Format("Mon Jan 2, 3:04 PM")
}

func (c *Module) page(w http.ResponseWriter, r *http.Request) {
	d := c.baseData(r.Context())
	if n, err := strconv.Atoi(r.URL.Query().Get("applied")); err == nil && n > 0 && n < 100 {
		d.Notice = fmt.Sprintf("Applied %d change(s).", n)
	}
	c.render(w, r, http.StatusOK, "list", "Calendar", d)
}

// parseEventForm reads the add and edit forms. On a problem it returns an
// error written for the person, along with the values so the form can be
// shown again filled in. When "Repeat" is ticked (and repeats are allowed) it
// also returns the series to create.
func (c *Module) parseEventForm(w http.ResponseWriter, r *http.Request, allowRepeat bool) (sdk.Event, *Series, formValues, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return sdk.Event{}, nil, formValues{}, errors.New("The form could not be read.")
	}

	fv := formValues{
		Title:      strings.TrimSpace(r.FormValue("title")),
		Date:       strings.TrimSpace(r.FormValue("date")),
		Start:      strings.TrimSpace(r.FormValue("start")),
		End:        strings.TrimSpace(r.FormValue("end")),
		Tag:        strings.TrimSpace(r.FormValue("tag")),
		AllDay:     r.FormValue("all_day") != "",
		ShowRepeat: allowRepeat,
		Repeat:     allowRepeat && r.FormValue("repeat") != "",
		Every:      r.FormValue("every"),
		Until:      strings.TrimSpace(r.FormValue("until")),
	}
	if fv.Every == "" {
		fv.Every = "1"
	}
	var chosen []int
	for _, v := range r.Form["days"] {
		for _, w := range weekdayOrder {
			if w.value == v {
				chosen = append(chosen, w.day)
			}
		}
	}
	fv.Days = dayOptions(chosen)

	switch {
	case fv.Title == "":
		return sdk.Event{}, nil, fv, errors.New("Give the event a title.")
	case len([]rune(fv.Title)) > maxTitleLen:
		return sdk.Event{}, nil, fv, fmt.Errorf("The title is too long (max %d characters).", maxTitleLen)
	case len([]rune(fv.Tag)) > maxTagLen:
		return sdk.Event{}, nil, fv, fmt.Errorf("The tag is too long (max %d characters).", maxTagLen)
	}

	date, err := time.ParseInLocation(dateLayout, fv.Date, c.loc)
	if err != nil {
		return sdk.Event{}, nil, fv, errors.New("Pick a valid date.")
	}

	e := sdk.Event{Title: fv.Title, Tag: fv.Tag, Calendar: "personal", AllDay: fv.AllDay, Start: date}
	if !fv.AllDay {
		start, err := atClock(date, fv.Start, c.loc)
		if err != nil {
			return sdk.Event{}, nil, fv, errors.New("Pick a start time, or tick All day.")
		}
		e.Start = start

		if fv.End != "" {
			end, err := atClock(date, fv.End, c.loc)
			if err != nil {
				return sdk.Event{}, nil, fv, errors.New("The end time is not valid.")
			}
			if !end.After(start) {
				return sdk.Event{}, nil, fv, errors.New("The end time must be after the start time.")
			}
			e.End = end
		}
	}
	if !fv.Repeat {
		return e, nil, fv, nil
	}

	every, err := strconv.Atoi(fv.Every)
	if err != nil || every < 1 || every > maxEveryWeeks {
		return sdk.Event{}, nil, fv, errors.New("Choose how often it repeats.")
	}
	if len(chosen) == 0 {
		return sdk.Event{}, nil, fv, errors.New("Pick at least one day for the repeat.")
	}
	sr := Series{
		Title: fv.Title, Tag: fv.Tag, Calendar: "personal", AllDay: fv.AllDay,
		Date: fv.Date, EveryWeeks: every, Weekdays: chosen,
	}
	if fv.Until != "" {
		until, err := time.ParseInLocation(dateLayout, fv.Until, c.loc)
		if err != nil {
			return sdk.Event{}, nil, fv, errors.New("The last day is not a valid date.")
		}
		if until.Before(date) {
			return sdk.Event{}, nil, fv, errors.New("The last day must not be before the first day.")
		}
		sr.Until = fv.Until
	}
	if !fv.AllDay {
		sr.Clock = fv.Start
		sr.Minutes = int(DefaultDuration / time.Minute)
		if fv.End != "" {
			sr.Minutes = int(e.End.Sub(e.Start) / time.Minute)
		}
	}
	return e, &sr, fv, nil
}

// atClock combines a date with an "HH:MM" clock time.
func atClock(date time.Time, clock string, loc *time.Location) (time.Time, error) {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(date.Year(), date.Month(), date.Day(), t.Hour(), t.Minute(), 0, 0, loc), nil
}

func (c *Module) addEvent(w http.ResponseWriter, r *http.Request) {
	e, sr, fv, err := c.parseEventForm(w, r, true)
	if err == nil {
		if sr != nil {
			_, err = c.store.AddSeries(*sr)
			if err != nil {
				err = errors.New(capitalize(err.Error()) + ".")
			}
		} else if _, serr := c.store.Add(e); serr != nil {
			log.Printf("calendar: add: %v", serr)
			http.Error(w, "could not save the event", http.StatusInternalServerError)
			return
		}
	}
	if err != nil {
		d := c.baseData(r.Context())
		d.Error, d.Form = err.Error(), fv
		c.render(w, r, http.StatusBadRequest, "list", "Calendar", d)
		return
	}
	http.Redirect(w, r, "/m/calendar", http.StatusSeeOther)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// formFromSeries fills the edit form for a whole series.
func formFromSeries(s Series) formValues {
	fv := formValues{
		Title: s.Title, Tag: s.Tag, AllDay: s.AllDay, Date: s.Date,
		ShowRepeat: true, RepeatLocked: true, Repeat: true,
		Every: strconv.Itoa(s.EveryWeeks), Until: s.Until, Days: dayOptions(s.Weekdays),
	}
	if !s.AllDay {
		fv.Start = s.Clock
		if t, err := time.Parse("15:04", s.Clock); err == nil {
			fv.End = t.Add(time.Duration(s.Minutes) * time.Minute).Format("15:04")
		}
	}
	return fv
}

// editPage shows the edit form for a one-off event or, for a repeating
// event, for the whole series.
func (c *Module) editPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	if seriesID, _, isOcc := splitID(id); isOcc || isSeriesID(id) {
		sr, ok := c.store.GetSeries(seriesID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		c.render(w, r, http.StatusOK, "edit", "Edit repeating event", pageData{ID: sr.ID, Form: formFromSeries(sr)})
		return
	}

	e, ok := c.store.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	start := e.Start.In(c.loc)
	fv := formValues{Title: e.Title, Tag: e.Tag, AllDay: e.AllDay, Date: start.Format(dateLayout), Every: "1", Days: dayOptions(nil)}
	if !e.AllDay {
		fv.Start = start.Format("15:04")
		fv.End = e.End.In(c.loc).Format("15:04")
	}
	c.render(w, r, http.StatusOK, "edit", "Edit event", pageData{ID: e.ID, Form: fv})
}

func (c *Module) updateEvent(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	if isSeriesID(id) {
		c.updateSeries(w, r, id)
		return
	}

	parsed, _, fv, err := c.parseEventForm(w, r, false)
	if err != nil {
		c.render(w, r, http.StatusBadRequest, "edit", "Edit event", pageData{ID: id, Form: fv, Error: err.Error()})
		return
	}

	existing, ok := c.store.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Keep the ID, source and calendar; take everything else from the form.
	existing.Title, existing.Tag = parsed.Title, parsed.Tag
	existing.AllDay, existing.Start, existing.End = parsed.AllDay, parsed.Start, parsed.End

	switch err := c.store.Update(existing); {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		log.Printf("calendar: update: %v", err)
		http.Error(w, "could not save the event", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, "/m/calendar", http.StatusSeeOther)
	}
}

func (c *Module) updateSeries(w http.ResponseWriter, r *http.Request, id string) {
	_, sr, fv, err := c.parseEventForm(w, r, true)
	fv.RepeatLocked, fv.Repeat = true, true
	if err == nil && sr == nil {
		err = errors.New("Repeating events must repeat on at least one day.")
	}
	if err != nil {
		c.render(w, r, http.StatusBadRequest, "edit", "Edit repeating event", pageData{ID: id, Form: fv, Error: err.Error()})
		return
	}

	sr.ID = id
	switch err := c.store.UpdateSeries(*sr); {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		c.render(w, r, http.StatusBadRequest, "edit", "Edit repeating event",
			pageData{ID: id, Form: fv, Error: capitalize(err.Error()) + "."})
	default:
		http.Redirect(w, r, "/m/calendar", http.StatusSeeOther)
	}
}

// removeEvent deletes a one-off event, skips one day of a series (an id like
// "s3:2026-11-15"), or deletes a whole series (an id like "s3").
func (c *Module) removeEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	id := r.FormValue("id")

	var err error
	if seriesID, date, isOcc := splitID(id); isOcc {
		err = c.store.SkipOccurrence(seriesID, date)
	} else if isSeriesID(id) {
		err = c.store.DeleteSeries(id)
	} else {
		err = c.store.Delete(id)
	}

	switch {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		log.Printf("calendar: delete: %v", err)
		http.Error(w, "could not delete the event", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, "/m/calendar", http.StatusSeeOther)
	}
}

// ask sends the request to the AI, validates what comes back, and shows it
// for confirmation. It never changes the calendar itself.
func (c *Module) ask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	text := strings.TrimSpace(r.FormValue("text"))

	fail := func(status int, msg string) {
		d := c.baseData(r.Context())
		d.Error = msg
		c.render(w, r, status, "list", "Calendar", d)
	}

	if text == "" || len([]rune(text)) > 1000 {
		fail(http.StatusBadRequest, "Type or say what you want changed (up to 1000 characters).")
		return
	}
	ai := c.host.AI()
	if !ai.Available() {
		fail(http.StatusServiceUnavailable, "Add your Claude API key in Profile first.")
		return
	}

	now := c.now().In(c.loc)
	from := c.today().AddDate(0, 0, -1)
	known, err := c.store.Range(r.Context(), from, from.AddDate(0, 0, 22))
	if err != nil {
		log.Printf("calendar: ask range: %v", err)
		fail(http.StatusInternalServerError, "Could not read your calendar.")
		return
	}
	if len(known) > 80 {
		known = known[:80]
	}

	system, prompt := buildPrompt(now, c.loc, known, c.store.AllSeries(), text)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	reply, err := ai.Ask(ctx, system, prompt)
	if err != nil {
		log.Printf("calendar: ask: %v", err)
		fail(http.StatusBadGateway, "The AI request failed: "+err.Error())
		return
	}

	prop, err := parseProposal(reply, c.store, c.loc, now)
	if err != nil {
		log.Printf("calendar: ask parse: %v (start of reply: %.300q)", err, reply)
		fail(http.StatusUnprocessableEntity, "I couldn't turn that into changes: "+err.Error())
		return
	}
	if len(prop.Actions) == 0 {
		d := c.baseData(r.Context())
		d.Notice = prop.Say
		if d.Notice == "" {
			d.Notice = "No changes to make."
		}
		c.render(w, r, http.StatusOK, "list", "Calendar", d)
		return
	}

	d := pageData{
		Token:    c.pend.put(prop, c.now()),
		Say:      prop.Say,
		Original: text,
	}
	for _, a := range prop.Actions {
		d.Actions = append(d.Actions, c.actionView(a))
	}
	c.render(w, r, http.StatusOK, "confirm", "Confirm changes", d)
}

// seriesSummary is "Every Sunday, 3:00 PM–11:30 PM".
func seriesSummary(s Series) string {
	return s.Rule() + ", " + s.TimeText()
}

func (c *Module) actionView(a Action) actionView {
	switch a.Op {
	case "add":
		return actionView{Kind: "Add", Text: fmt.Sprintf("%s, %s", a.Event.Title, c.describe(a.Event))}
	case "add_series":
		text := fmt.Sprintf("%s: %s", a.Series.Title, seriesSummary(a.Series))
		if first, err := time.Parse(dateLayout, a.Series.Date); err == nil {
			text += ", starting " + first.Format("Mon Jan 2")
		}
		return actionView{Kind: "Repeat", Text: text}
	case "delete":
		return actionView{Kind: "Delete", Text: fmt.Sprintf("%s, %s", a.Event.Title, c.describe(a.Event))}
	case "delete_series":
		return actionView{Kind: "Delete", Text: fmt.Sprintf("%s, the whole series (%s)", a.Series.Title, seriesSummary(a.Series))}
	case "skip":
		return actionView{Kind: "Skip", Text: fmt.Sprintf("%s on %s (this day only)", a.Event.Title, c.describe(a.Event))}
	case "update_series":
		text := fmt.Sprintf("%s, whole series: %s → %s", a.BeforeSeries.Title, seriesSummary(a.BeforeSeries), seriesSummary(a.Series))
		if a.BeforeSeries.Title != a.Series.Title {
			text += fmt.Sprintf(" (renamed to %s)", a.Series.Title)
		}
		return actionView{Kind: "Change", Text: text}
	}
	text := fmt.Sprintf("%s: %s → %s", a.Before.Title, c.describe(a.Before), c.describe(a.Event))
	if a.Before.Title != a.Event.Title {
		text += fmt.Sprintf(" (renamed to %s)", a.Event.Title)
	}
	return actionView{Kind: "Change", Text: text}
}

func (c *Module) applyProposal(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	prop, ok := c.pend.take(r.FormValue("token"), c.now())
	if !ok {
		d := c.baseData(r.Context())
		d.Error = "Those changes expired or were already handled. Ask again."
		c.render(w, r, http.StatusGone, "list", "Calendar", d)
		return
	}

	n := 0
	for _, a := range prop.Actions {
		var err error
		switch a.Op {
		case "add":
			_, err = c.store.Add(a.Event)
		case "add_series":
			_, err = c.store.AddSeries(a.Series)
		case "update":
			err = c.store.Update(a.Event)
		case "update_series":
			err = c.store.UpdateSeries(a.Series)
		case "delete":
			err = c.store.Delete(a.Event.ID)
		case "delete_series":
			err = c.store.DeleteSeries(a.Series.ID)
		case "skip":
			err = c.store.SkipOccurrence(a.Series.ID, a.Date)
		default:
			err = fmt.Errorf("unknown change %q", a.Op)
		}
		if err != nil {
			log.Printf("calendar: apply %s: %v", a.Op, err)
			d := c.baseData(r.Context())
			d.Error = fmt.Sprintf("Applied %d of %d changes, then stopped at %q: %v",
				n, len(prop.Actions), a.label(), err)
			c.render(w, r, http.StatusConflict, "list", "Calendar", d)
			return
		}
		n++
	}
	http.Redirect(w, r, fmt.Sprintf("/m/calendar?applied=%d", n), http.StatusSeeOther)
}

func (c *Module) discardProposal(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	c.pend.take(r.FormValue("token"), c.now())
	http.Redirect(w, r, "/m/calendar", http.StatusSeeOther)
}

// voice transcribes a recording and returns the text as JSON. The page puts
// the text into the request box so the person can check it before anything
// is sent to the AI.
func (c *Module) voice(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, body map[string]string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}

	speech := c.host.Speech()
	if !speech.Available() {
		reply(http.StatusServiceUnavailable, map[string]string{"error": "no transcription service is set up in Profile"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	text, err := speech.Transcribe(ctx, r.Body, r.Header.Get("Content-Type"))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		reply(http.StatusRequestEntityTooLarge, map[string]string{"error": "the recording is too long"})
	case err != nil:
		log.Printf("calendar: voice: %v", err)
		reply(http.StatusBadGateway, map[string]string{"error": "transcription failed"})
	default:
		reply(http.StatusOK, map[string]string{"text": strings.TrimSpace(text)})
	}
}
