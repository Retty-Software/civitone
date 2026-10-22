package access

import (
	"testing"
	"time"
)

var ny, _ = time.LoadLocation("America/New_York")

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, ny)
	if err != nil {
		panic(err)
	}
	return t
}

func TestOnceWindowWithBuffer(t *testing.T) {
	e := Event{BufferMinutes: 120, Windows: []Window{{Kind: "once", Start: "2026-12-05T08:00", End: "2026-12-07T23:00"}}}
	if st := e.At(at("2026-12-05 05:59"), ny); st.Open || !st.Next.Equal(at("2026-12-05 06:00")) {
		t.Fatalf("before buffer: %+v", st)
	}
	if st := e.At(at("2026-12-05 06:00"), ny); !st.Open || !st.Until.Equal(at("2026-12-07 23:00")) {
		t.Fatalf("buffer opens it: %+v", st)
	}
	if st := e.At(at("2026-12-07 23:00"), ny); st.Open || !st.Next.IsZero() {
		t.Fatalf("closed for good at the end: %+v", st)
	}
}

func TestMonthlyNthWeekday(t *testing.T) {
	// Third Friday, 4-9 PM. October 2026: Fridays are 2, 9, 16, 23, 30.
	e := Event{Windows: []Window{{Kind: "monthly", Nth: 3, Weekday: "fri", Start: "16:00", End: "21:00"}}}
	if st := e.At(at("2026-10-16 17:00"), ny); !st.Open || !st.Until.Equal(at("2026-10-16 21:00")) {
		t.Fatalf("3rd Friday: %+v", st)
	}
	if st := e.At(at("2026-10-23 17:00"), ny); st.Open || !st.Next.Equal(at("2026-11-20 16:00")) {
		t.Fatalf("4th Friday is closed, next is Nov 20: %+v", st)
	}
	last := Event{Windows: []Window{{Kind: "monthly", Nth: -1, Weekday: "fri", Start: "16:00", End: "21:00"}}}
	if st := last.At(at("2026-10-23 17:00"), ny); st.Open || !st.Next.Equal(at("2026-10-30 16:00")) {
		t.Fatalf("last Friday is the 30th: %+v", st)
	}
}

func TestWeeklyOvernightAndUntil(t *testing.T) {
	e := Event{Windows: []Window{{Kind: "weekly", Days: []string{"sat"}, Start: "20:00", End: "01:00", Until: "2026-10-31"}}}
	if st := e.At(at("2026-10-25 00:30"), ny); !st.Open || !st.Until.Equal(at("2026-10-25 01:00")) {
		t.Fatalf("runs past midnight: %+v", st)
	}
	if st := e.At(at("2026-11-01 12:00"), ny); st.Open || !st.Next.IsZero() {
		t.Fatalf("nothing after until: %+v", st)
	}
}

func TestBackToBackWindowsJoin(t *testing.T) {
	e := Event{Windows: []Window{
		{Kind: "weekly", Days: []string{"fri"}, Start: "18:00", End: "00:00"},
		{Kind: "weekly", Days: []string{"sat"}, Start: "00:00", End: "02:00"},
	}}
	if st := e.At(at("2026-10-23 19:00"), ny); !st.Open || !st.Until.Equal(at("2026-10-24 02:00")) {
		t.Fatalf("joined: %+v", st)
	}
}

func TestDSTUsesWallClock(t *testing.T) {
	// US clocks fall back Nov 1 2026; a Sunday 9 AM window is still 9 AM local.
	e := Event{Windows: []Window{{Kind: "weekly", Days: []string{"sun"}, Start: "09:00", End: "10:00"}}}
	if st := e.At(at("2026-10-31 12:00"), ny); !st.Next.Equal(at("2026-11-01 09:00")) {
		t.Fatalf("next: %v", st.Next)
	}
}

func doc() Doc {
	return Doc{
		DefaultSource: "Music",
		People:        []Person{{ID: "u1", Role: Owner}, {ID: "u2", Role: Guest}, {ID: "u3", Role: Guest}},
		Events: []Event{
			{ID: "xmas", Members: []string{"u2"}, Poles: []string{"p1", "p2"}, Sources: []string{"radiodj"}, VolumeCap: 80,
				Windows: []Window{{Kind: "once", Start: "2026-12-05T08:00", End: "2026-12-07T23:00"}}},
			{ID: "trucks", Members: []string{"u2", "u3"}, Poles: []string{"p2"}, Sources: []string{"phone"}, VolumeCap: 65, Paging: true,
				Windows: []Window{{Kind: "monthly", Nth: 1, Weekday: "sat", Start: "10:00", End: "20:00"}}},
		},
	}
}

func TestGrants(t *testing.T) {
	d := doc()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if g, ok := d.GrantFor("u1", at("2026-01-01 00:00"), ny); !ok || !g.Admin || !g.Open {
		t.Fatalf("owner: %+v", g)
	}
	if _, ok := d.GrantFor("nobody", time.Now(), ny); ok {
		t.Fatal("stranger got a grant")
	}
	// Dec 5 2026 is the first Saturday: both of u2's events are open.
	g, _ := d.GrantFor("u2", at("2026-12-05 12:00"), ny)
	if !g.Open || g.Poles["p1"] != 80 || g.Poles["p2"] != 80 || !g.Sources["radiodj"] || !g.Sources["phone"] || !g.Paging {
		t.Fatalf("u2 both open: %+v", g)
	}
	// u3 only has trucks: never radiodj, never p1.
	g, _ = d.GrantFor("u3", at("2026-12-05 12:00"), ny)
	if !g.Open || g.Sources["radiodj"] || g.Poles["p1"] != 0 || g.Poles["p2"] != 65 {
		t.Fatalf("u3: %+v", g)
	}
	g, _ = d.GrantFor("u3", at("2026-12-06 12:00"), ny)
	if g.Open || !g.Next.Equal(at("2027-01-02 10:00")) || len(g.Poles) != 0 {
		t.Fatalf("u3 closed: %+v", g)
	}
}

func TestValidate(t *testing.T) {
	bad := []Doc{
		{People: []Person{{ID: "a", Role: "boss"}}},
		{People: []Person{{ID: "a", Role: Guest}, {ID: "a", Role: Guest}}},
		{Events: []Event{{ID: "e", Members: []string{"ghost"}}}},
		{Events: []Event{{ID: "e", Windows: []Window{{Kind: "monthly", Nth: 6, Weekday: "fri", Start: "10:00", End: "11:00"}}}}},
		{Events: []Event{{ID: "e", Windows: []Window{{Kind: "weekly", Days: []string{"funday"}, Start: "10:00", End: "11:00"}}}}},
		{Events: []Event{{ID: "e", Windows: []Window{{Kind: "once", Start: "tomorrow", End: "later"}}}}},
	}
	for i, d := range bad {
		if d.Validate() == nil {
			t.Errorf("doc %d validated", i)
		}
	}
}
