package calendar

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/camwillm/gluecozs-sdk"
)

type fakeAI struct {
	ready  bool
	reply  string
	err    error
	system string
	prompt string
}

func (f *fakeAI) Available() bool { return f.ready }
func (f *fakeAI) Ask(_ context.Context, system, prompt string) (string, error) {
	f.system, f.prompt = system, prompt
	return f.reply, f.err
}

type fakeSpeech struct {
	ready       bool
	text        string
	err         error
	gotType     string
	gotAudioLen int
}

func (f *fakeSpeech) Available() bool { return f.ready }
func (f *fakeSpeech) Transcribe(_ context.Context, audio io.Reader, contentType string) (string, error) {
	data, err := io.ReadAll(audio)
	if err != nil {
		return "", err
	}
	f.gotType, f.gotAudioLen = contentType, len(data)
	return f.text, f.err
}

// fakeHost stands in for the core: it wraps the body in a bare page.
type fakeHost struct {
	ai     *fakeAI
	speech *fakeSpeech
}

func (h *fakeHost) Page(w http.ResponseWriter, _ *http.Request, status int, navID, title string, body template.HTML) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, "<html><title>"+title+"</title><body data-nav=\""+navID+"\">"+string(body)+"</body></html>")
}
func (h *fakeHost) AI() sdk.AI              { return h.ai }
func (h *fakeHost) Speech() sdk.Transcriber { return h.speech }

type env struct {
	mod    *Module
	srv    *httptest.Server
	ai     *fakeAI
	speech *fakeSpeech
	client *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		ai:     &fakeAI{ready: true},
		speech: &fakeSpeech{ready: true},
	}
	e.mod = New(NewStore(), ny)
	e.mod.now = func() time.Time { return testNow }

	mux := http.NewServeMux()
	e.mod.Mount(mux, &fakeHost{ai: e.ai, speech: e.speech})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	e.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *env) get(t *testing.T, path string) (int, string) {
	t.Helper()
	res, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return readBody(t, res)
}

func (e *env) post(t *testing.T, path string, form url.Values) (int, string, http.Header) {
	t.Helper()
	res, err := e.client.PostForm(e.srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	status, body := readBody(t, res)
	return status, body, res.Header
}

func readBody(t *testing.T, res *http.Response) (int, string) {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

func TestPageListsUpcomingEvents(t *testing.T) {
	e := newEnv(t)
	mustAdd(t, e.mod.store, sdk.Event{Title: "Lawn route", Tag: "Work", Start: at(9, 9, 0)})
	mustAdd(t, e.mod.store, sdk.Event{Title: "Pay day", AllDay: true, Start: at(11, 0, 0)})
	mustAdd(t, e.mod.store, sdk.Event{Title: "Far future", Start: at(30, 9, 0)})

	status, body := e.get(t, "/m/calendar")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{"Lawn route", "9:00 AM–10:00 AM", "Pay day", "All day", "Monday, Nov 9", `data-nav="calendar"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(body, "Far future") {
		t.Error("an event beyond 14 days was listed")
	}
}

func TestPageHidesAIBoxWithoutKey(t *testing.T) {
	e := newEnv(t)
	e.ai.ready = false

	_, body := e.get(t, "/m/calendar")
	if strings.Contains(body, `name="text"`) {
		t.Error("the ask box is shown without a key")
	}
	if !strings.Contains(body, "/profile") {
		t.Error("expected a pointer to Profile")
	}
}

func TestMicButtonNeedsSpeech(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, "/m/calendar")
	if !strings.Contains(body, `id="mic"`) {
		t.Error("mic button missing when speech is available")
	}

	e.speech.ready = false
	_, body = e.get(t, "/m/calendar")
	if strings.Contains(body, `id="mic"`) {
		t.Error("mic button shown without a speech service")
	}
}

func TestAddEditDeleteEvent(t *testing.T) {
	e := newEnv(t)

	status, _, hdr := e.post(t, "/m/calendar/events", url.Values{
		"title": {"Dentist"}, "date": {"2026-11-12"}, "start": {"15:00"}, "tag": {"Health"},
	})
	if status != http.StatusSeeOther || hdr.Get("Location") != "/m/calendar" {
		t.Fatalf("add: status %d, location %q", status, hdr.Get("Location"))
	}
	got, _ := e.mod.store.Get("1")
	if got.Title != "Dentist" || !got.Start.Equal(at(12, 15, 0)) || !got.End.Equal(at(12, 16, 0)) {
		t.Fatalf("saved event = %+v", got)
	}

	// The edit page is filled in.
	status, body := e.get(t, "/m/calendar/edit?id=1")
	if status != http.StatusOK || !strings.Contains(body, `value="Dentist"`) || !strings.Contains(body, `value="15:00"`) {
		t.Fatalf("edit page: status %d\n%s", status, body)
	}

	status, _, _ = e.post(t, "/m/calendar/update", url.Values{
		"id": {"1"}, "title": {"Dentist (moved)"}, "date": {"2026-11-13"}, "start": {"10:00"}, "end": {"11:15"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("update status = %d", status)
	}
	got, _ = e.mod.store.Get("1")
	if got.Title != "Dentist (moved)" || !got.Start.Equal(at(13, 10, 0)) || !got.End.Equal(at(13, 11, 15)) {
		t.Fatalf("updated event = %+v", got)
	}
	if got.Source != "calendar" || got.Calendar != "personal" {
		t.Errorf("source/calendar were lost: %+v", got)
	}

	status, _, _ = e.post(t, "/m/calendar/delete", url.Values{"id": {"1"}})
	if status != http.StatusSeeOther || e.mod.store.Len() != 0 {
		t.Fatalf("delete: status %d, len %d", status, e.mod.store.Len())
	}
	status, _, _ = e.post(t, "/m/calendar/delete", url.Values{"id": {"1"}})
	if status != http.StatusNotFound {
		t.Errorf("deleting twice: status %d, want 404", status)
	}
}

func TestAddEventValidation(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
		want string
	}{
		{"no title", url.Values{"date": {"2026-11-12"}, "start": {"15:00"}}, "title"},
		{"bad date", url.Values{"title": {"x"}, "date": {"soon"}, "start": {"15:00"}}, "valid date"},
		{"no start and not all day", url.Values{"title": {"x"}, "date": {"2026-11-12"}}, "start time"},
		{"end before start", url.Values{"title": {"x"}, "date": {"2026-11-12"}, "start": {"15:00"}, "end": {"14:00"}}, "after the start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			status, body, _ := e.post(t, "/m/calendar/events", tt.form)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if !strings.Contains(body, tt.want) {
				t.Errorf("error does not mention %q", tt.want)
			}
			if e.mod.store.Len() != 0 {
				t.Error("an invalid event was saved")
			}
		})
	}
}

func TestAddAllDayEvent(t *testing.T) {
	e := newEnv(t)
	status, _, _ := e.post(t, "/m/calendar/events", url.Values{
		"title": {"Trip"}, "date": {"2026-11-14"}, "all_day": {"1"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d", status)
	}
	got, _ := e.mod.store.Get("1")
	if !got.AllDay || !got.Start.Equal(at(14, 0, 0)) {
		t.Errorf("got %+v", got)
	}
}

func TestTitlesAreEscaped(t *testing.T) {
	e := newEnv(t)
	mustAdd(t, e.mod.store, sdk.Event{Title: `<script>alert(1)</script>`, Start: at(9, 9, 0)})

	_, body := e.get(t, "/m/calendar")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("an event title was written into the page unescaped")
	}
}

func TestAskConfirmApply(t *testing.T) {
	e := newEnv(t)
	gym := mustAdd(t, e.mod.store, sdk.Event{Title: "Gym", Start: at(10, 14, 0), End: at(10, 15, 0)})
	e.ai.reply = `{"say":"Moving gym and adding dentist.","actions":[
		{"op":"update","id":"` + gym.ID + `","start":"2026-11-10T17:00"},
		{"op":"add","title":"Dentist","start":"2026-11-12T15:00","tag":"Health"}]}`

	status, body, _ := e.post(t, "/m/calendar/ask", url.Values{"text": {"move gym to 5pm tomorrow and add dentist thursday at 3"}})
	if status != http.StatusOK {
		t.Fatalf("ask status = %d\n%s", status, body)
	}
	for _, want := range []string{"Confirm changes", "Gym: Tue Nov 10, 2:00 PM–3:00 PM → Tue Nov 10, 5:00 PM–6:00 PM", "Dentist, Thu Nov 12, 3:00 PM–4:00 PM"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page is missing %q\n%s", want, body)
		}
	}
	if !strings.Contains(e.ai.prompt, "Monday, 2026-11-09") || !strings.Contains(e.ai.prompt, gym.ID+" | 2026-11-10T14:00") {
		t.Errorf("prompt did not carry today's date and the event list:\n%s", e.ai.prompt)
	}
	if e.mod.store.Len() != 1 {
		t.Fatal("the calendar changed before the person confirmed")
	}

	token := between(body, `name="token" value="`, `"`)
	if token == "" {
		t.Fatal("no token on the confirm page")
	}

	status, _, hdr := e.post(t, "/m/calendar/apply", url.Values{"token": {token}})
	if status != http.StatusSeeOther || hdr.Get("Location") != "/m/calendar?applied=2" {
		t.Fatalf("apply: status %d, location %q", status, hdr.Get("Location"))
	}
	moved, _ := e.mod.store.Get(gym.ID)
	if !moved.Start.Equal(at(10, 17, 0)) || !moved.End.Equal(at(10, 18, 0)) {
		t.Errorf("gym = %v to %v", moved.Start, moved.End)
	}
	if e.mod.store.Len() != 2 {
		t.Errorf("Len = %d, want 2", e.mod.store.Len())
	}

	// A token works once.
	status, _, _ = e.post(t, "/m/calendar/apply", url.Values{"token": {token}})
	if status != http.StatusGone {
		t.Errorf("second apply: status %d, want 410", status)
	}
	if e.mod.store.Len() != 2 {
		t.Error("the second apply changed the calendar")
	}
}

func TestAskThenDeny(t *testing.T) {
	e := newEnv(t)
	e.ai.reply = `{"actions":[{"op":"add","title":"Nope","start":"2026-11-12T15:00"}]}`

	_, body, _ := e.post(t, "/m/calendar/ask", url.Values{"text": {"add nope"}})
	token := between(body, `name="token" value="`, `"`)

	status, _, _ := e.post(t, "/m/calendar/discard", url.Values{"token": {token}})
	if status != http.StatusSeeOther {
		t.Fatalf("discard status = %d", status)
	}
	status, _, _ = e.post(t, "/m/calendar/apply", url.Values{"token": {token}})
	if status != http.StatusGone || e.mod.store.Len() != 0 {
		t.Errorf("after deny: apply status %d, Len %d", status, e.mod.store.Len())
	}
}

func TestApplyCannotBeForged(t *testing.T) {
	e := newEnv(t)
	status, _, _ := e.post(t, "/m/calendar/apply", url.Values{
		"token": {"made-up"}, "op": {"add"}, "title": {"Injected"}, "start": {"2026-11-12T15:00"},
	})
	if status != http.StatusGone || e.mod.store.Len() != 0 {
		t.Errorf("status %d, Len %d", status, e.mod.store.Len())
	}
}

func TestAskFailures(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(e *env)
		text   string
		status int
		want   string
	}{
		{"empty text", func(e *env) {}, "  ", http.StatusBadRequest, "Type or say"},
		{"no key", func(e *env) { e.ai.ready = false }, "add x", http.StatusServiceUnavailable, "API key"},
		{"AI error", func(e *env) { e.ai.err = errors.New("claude API: invalid x-api-key (HTTP 401)") }, "add x", http.StatusBadGateway, "invalid x-api-key"},
		{"unparseable reply", func(e *env) { e.ai.reply = "I would love to help!" }, "add x", http.StatusUnprocessableEntity, "turn that into changes"},
		{"invented id", func(e *env) { e.ai.reply = `{"actions":[{"op":"delete","id":"77"}]}` }, "delete x", http.StatusUnprocessableEntity, "no event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.setup(e)
			status, body, _ := e.post(t, "/m/calendar/ask", url.Values{"text": {tt.text}})
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if !strings.Contains(body, tt.want) {
				t.Errorf("page does not mention %q", tt.want)
			}
			if e.mod.store.Len() != 0 {
				t.Error("the calendar changed")
			}
		})
	}
}

func TestAskWithNothingToDoShowsTheAnswer(t *testing.T) {
	e := newEnv(t)
	e.ai.reply = `{"say":"Which Thursday do you mean?","actions":[]}`

	status, body, _ := e.post(t, "/m/calendar/ask", url.Values{"text": {"thursday"}})
	if status != http.StatusOK || !strings.Contains(body, "Which Thursday do you mean?") {
		t.Errorf("status %d\n%s", status, body)
	}
	if strings.Contains(body, "Confirm changes") {
		t.Error("a confirm page was shown for zero actions")
	}
}

func TestVoice(t *testing.T) {
	e := newEnv(t)
	e.speech.text = "  add dentist thursday at three  "

	res, err := e.client.Post(e.srv.URL+"/m/calendar/voice", "audio/webm;codecs=opus", strings.NewReader("fake-audio-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	status, body := readBody(t, res)
	if status != http.StatusOK || !strings.Contains(body, `"text":"add dentist thursday at three"`) {
		t.Fatalf("status %d, body %s", status, body)
	}
	if e.speech.gotType != "audio/webm;codecs=opus" || e.speech.gotAudioLen != len("fake-audio-bytes") {
		t.Errorf("service got type %q, %d bytes", e.speech.gotType, e.speech.gotAudioLen)
	}
}

func TestVoiceFailures(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := newEnv(t)
		e.speech.ready = false
		res, _ := e.client.Post(e.srv.URL+"/m/calendar/voice", "audio/webm", strings.NewReader("x"))
		if status, _ := readBody(t, res); status != http.StatusServiceUnavailable {
			t.Errorf("status = %d", status)
		}
	})
	t.Run("service error does not leak details", func(t *testing.T) {
		e := newEnv(t)
		e.speech.err = errors.New("secret internal detail")
		res, _ := e.client.Post(e.srv.URL+"/m/calendar/voice", "audio/webm", strings.NewReader("x"))
		status, body := readBody(t, res)
		if status != http.StatusBadGateway || strings.Contains(body, "secret internal detail") {
			t.Errorf("status %d, body %s", status, body)
		}
	})
	t.Run("recording too large", func(t *testing.T) {
		e := newEnv(t)
		big := strings.NewReader(strings.Repeat("a", 11<<20))
		res, _ := e.client.Post(e.srv.URL+"/m/calendar/voice", "audio/webm", big)
		if status, _ := readBody(t, res); status != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d", status)
		}
	})
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}
