// Package access decides who may do what in the console, and when: people
// with roles, and events that open the console to guests for a few poles and
// sources during their windows. The document comes from outside (the admin
// API); this package only evaluates it.
package access

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	Owner    = "owner"
	Operator = "operator"
	Guest    = "guest"
)

// Doc is the whole access document, replaced on every PUT /admin/v1/access.
type Doc struct {
	DefaultSource string   `json:"default_source"`
	People        []Person `json:"people"`
	Events        []Event  `json:"events"`
}

type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type Event struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Members       []string `json:"members"`
	Poles         []string `json:"poles"`
	Sources       []string `json:"sources"`
	VolumeCap     int      `json:"volume_cap"` // percent; 0 = 100
	Paging        bool     `json:"paging"`
	BufferMinutes int      `json:"buffer_minutes"`
	Windows       []Window `json:"windows"`
}

// Window is one rule for when an event is open, in the site's time zone.
type Window struct {
	Kind    string   `json:"kind"`              // once | weekly | monthly
	Start   string   `json:"start"`             // once: YYYY-MM-DDTHH:MM, else HH:MM
	End     string   `json:"end"`               // as Start; at or before Start runs into the next day
	Days    []string `json:"days,omitempty"`    // weekly: mon..sun
	Nth     int      `json:"nth,omitempty"`     // monthly: 1-5, or -1 for the last
	Weekday string   `json:"weekday,omitempty"` // monthly
	From    string   `json:"from,omitempty"`    // weekly, monthly: first date, YYYY-MM-DD
	Until   string   `json:"until,omitempty"`   // last date
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// Validate rejects a document the controller couldn't evaluate, so a bad
// PUT fails loudly instead of locking guests out later.
func (d Doc) Validate() error {
	ids := map[string]bool{}
	for _, p := range d.People {
		if p.ID == "" {
			return errors.New("every person needs an id")
		}
		if ids[p.ID] {
			return fmt.Errorf("person %q listed twice", p.ID)
		}
		ids[p.ID] = true
		switch p.Role {
		case Owner, Operator, Guest:
		default:
			return fmt.Errorf("person %q: role must be owner, operator or guest", p.ID)
		}
	}
	for _, e := range d.Events {
		if e.ID == "" {
			return errors.New("every event needs an id")
		}
		if e.VolumeCap < 0 || e.VolumeCap > 100 {
			return fmt.Errorf("event %q: volume_cap is a percent", e.ID)
		}
		if e.BufferMinutes < 0 || e.BufferMinutes > 24*60 {
			return fmt.Errorf("event %q: buffer_minutes must be 0 to 1440", e.ID)
		}
		for _, m := range e.Members {
			if !ids[m] {
				return fmt.Errorf("event %q: member %q isn't in people", e.ID, m)
			}
		}
		for i, w := range e.Windows {
			if err := w.validate(); err != nil {
				return fmt.Errorf("event %q window %d: %w", e.ID, i+1, err)
			}
		}
	}
	return nil
}

func (w Window) validate() error {
	switch w.Kind {
	case "once":
		if _, err := time.Parse("2006-01-02T15:04", w.Start); err != nil {
			return errors.New("start must be YYYY-MM-DDTHH:MM")
		}
		if _, err := time.Parse("2006-01-02T15:04", w.End); err != nil {
			return errors.New("end must be YYYY-MM-DDTHH:MM")
		}
		return nil
	case "weekly":
		if len(w.Days) == 0 {
			return errors.New("weekly needs days")
		}
		for _, d := range w.Days {
			if _, ok := weekdays[strings.ToLower(d)]; !ok {
				return fmt.Errorf("unknown day %q", d)
			}
		}
	case "monthly":
		if _, ok := weekdays[strings.ToLower(w.Weekday)]; !ok {
			return fmt.Errorf("unknown weekday %q", w.Weekday)
		}
		if w.Nth == 0 || w.Nth < -1 || w.Nth > 5 {
			return errors.New("nth must be 1-5 or -1")
		}
	default:
		return fmt.Errorf("kind must be once, weekly or monthly, not %q", w.Kind)
	}
	if _, _, ok := clock(w.Start); !ok {
		return errors.New("start must be HH:MM")
	}
	if _, _, ok := clock(w.End); !ok {
		return errors.New("end must be HH:MM")
	}
	for _, d := range []string{w.From, w.Until} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				return errors.New("from and until must be YYYY-MM-DD")
			}
		}
	}
	return nil
}

func clock(s string) (h, m int, ok bool) {
	hs, ms, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	return h, m, err1 == nil && err2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60
}

// span is one occurrence of a window: open from Start (buffer included) to End.
type span struct{ Start, End time.Time }

// on returns a recurring window's occurrence that starts on the given day, if any.
func (w Window) on(day time.Time, loc *time.Location) (span, bool) {
	y, mo, d := day.Date()
	date := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	switch w.Kind {
	case "weekly":
		if !slices.ContainsFunc(w.Days, func(s string) bool { return weekdays[strings.ToLower(s)] == date.Weekday() }) {
			return span{}, false
		}
	case "monthly":
		if weekdays[strings.ToLower(w.Weekday)] != date.Weekday() {
			return span{}, false
		}
		if w.Nth == -1 {
			if date.AddDate(0, 0, 7).Month() == mo {
				return span{}, false
			}
		} else if (d-1)/7+1 != w.Nth {
			return span{}, false
		}
	default:
		return span{}, false
	}
	key := date.Format("2006-01-02")
	if (w.From != "" && key < w.From) || (w.Until != "" && key > w.Until) {
		return span{}, false
	}
	sh, sm, ok1 := clock(w.Start)
	eh, em, ok2 := clock(w.End)
	if !ok1 || !ok2 {
		return span{}, false
	}
	s := time.Date(y, mo, d, sh, sm, 0, 0, loc)
	e := time.Date(y, mo, d, eh, em, 0, 0, loc)
	if !e.After(s) {
		e = e.AddDate(0, 0, 1)
	}
	return span{s, e}, true
}

// Status is whether an event is open at a moment, until when, and if not,
// when it next opens (zero: never again).
type Status struct {
	Open  bool      `json:"open"`
	Until time.Time `json:"until,omitzero"`
	Next  time.Time `json:"next,omitzero"`
}

// horizon is how far ahead Next looks: a year covers every monthly rule.
const horizon = 400

// At evaluates the event at t. Back-to-back or overlapping occurrences join,
// so Until is when the console actually closes.
func (e Event) At(t time.Time, loc *time.Location) Status {
	buf := time.Duration(e.BufferMinutes) * time.Minute
	t = t.In(loc)
	var spans []span
	// A recurring occurrence open at t started at most a day (plus the
	// buffer) before it; a one-off one can be any length.
	first := t.Add(-buf).AddDate(0, 0, -2)
	for _, w := range e.Windows {
		if w.Kind == "once" {
			s, err1 := time.ParseInLocation("2006-01-02T15:04", w.Start, loc)
			en, err2 := time.ParseInLocation("2006-01-02T15:04", w.End, loc)
			if err1 != nil || err2 != nil {
				continue
			}
			if !en.After(s) {
				en = en.AddDate(0, 0, 1)
			}
			spans = append(spans, span{s.Add(-buf), en})
			continue
		}
		for i := 0; i < horizon; i++ {
			if sp, ok := w.on(first.AddDate(0, 0, i), loc); ok {
				spans = append(spans, span{sp.Start.Add(-buf), sp.End})
			}
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return a.Start.Compare(b.Start) })
	var merged []span
	for _, sp := range spans {
		if n := len(merged); n > 0 && !sp.Start.After(merged[n-1].End) {
			if sp.End.After(merged[n-1].End) {
				merged[n-1].End = sp.End
			}
			continue
		}
		merged = append(merged, sp)
	}
	for _, sp := range merged {
		if !t.Before(sp.Start) && t.Before(sp.End) {
			return Status{Open: true, Until: sp.End}
		}
		if sp.Start.After(t) {
			return Status{Next: sp.Start}
		}
	}
	return Status{}
}

// Grant is what one person may do at one moment.
type Grant struct {
	Person Person `json:"person"`
	// Admin: an owner or operator, who may do everything.
	Admin bool `json:"admin"`
	// Open: a guest has at least one event open now (always true for admins).
	Open   bool          `json:"open"`
	Until  time.Time     `json:"until,omitzero"`
	Next   time.Time     `json:"next,omitzero"`
	Events []EventStatus `json:"events"`
	// For guests while open: the poles and sources of their open events,
	// each pole's volume cap (the highest of the events that cover it), and
	// whether any of them may control paging.
	Poles   map[string]int  `json:"-"`
	Sources map[string]bool `json:"-"`
	Paging  bool            `json:"paging"`
}

type EventStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Status
}

// GrantFor works out a person's grant at t. ok is false for someone not in
// the document.
func (d Doc) GrantFor(personID string, t time.Time, loc *time.Location) (g Grant, ok bool) {
	for _, p := range d.People {
		if p.ID == personID {
			g.Person, ok = p, true
		}
	}
	if !ok {
		return g, false
	}
	g.Events = []EventStatus{}
	if g.Person.Role == Owner || g.Person.Role == Operator {
		g.Admin, g.Open = true, true
		return g, true
	}
	g.Poles, g.Sources = map[string]int{}, map[string]bool{}
	for _, e := range d.Events {
		if !slices.Contains(e.Members, personID) {
			continue
		}
		st := e.At(t, loc)
		g.Events = append(g.Events, EventStatus{e.ID, e.Name, st})
		if !st.Open {
			if !st.Next.IsZero() && (g.Next.IsZero() || st.Next.Before(g.Next)) {
				g.Next = st.Next
			}
			continue
		}
		g.Open = true
		if st.Until.After(g.Until) {
			g.Until = st.Until
		}
		cap := e.VolumeCap
		if cap == 0 {
			cap = 100
		}
		for _, p := range e.Poles {
			g.Poles[p] = max(g.Poles[p], cap)
		}
		for _, s := range e.Sources {
			g.Sources[s] = true
		}
		g.Paging = g.Paging || e.Paging
	}
	if g.Open {
		g.Next = time.Time{}
	}
	return g, true
}

// OpenEvents returns the ids of the events open at t.
func (d Doc) OpenEvents(t time.Time, loc *time.Location) map[string]bool {
	open := map[string]bool{}
	for _, e := range d.Events {
		if e.At(t, loc).Open {
			open[e.ID] = true
		}
	}
	return open
}

// Event finds an event by id.
func (d Doc) Event(id string) (Event, bool) {
	for _, e := range d.Events {
		if e.ID == id {
			return e, true
		}
	}
	return Event{}, false
}
