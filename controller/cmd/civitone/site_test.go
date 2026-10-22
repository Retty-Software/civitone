package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/access"
	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
)

const testKey = "test-admin-key-0123456789abcdef"

type testSite struct {
	*testing.T
	srv  *httptest.Server
	site *site
	reg  *registry.Registry
}

func newTestSite(t *testing.T) *testSite {
	t.Helper()
	ctx := context.Background()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	s, err := newSite(ctx, reg, testKey, "")
	if err != nil {
		t.Fatal(err)
	}
	// Fixed clock: Saturday Dec 5 2026, noon in New York.
	ny, _ := time.LoadLocation("America/New_York")
	s.now = func() time.Time { return time.Date(2026, 12, 5, 12, 0, 0, 0, ny) }
	sc := snap.NewLink("127.0.0.1:1") // never connects: status calls fail fast
	nm := nodes.NewManager(regStore{reg})
	src := sources.New(ctx, sc, reg)
	mux := http.NewServeMux()
	mux.HandleFunc("/signin", s.signin)
	mux.HandleFunc("/api/me", s.me(streamOf(src)))
	mux.Handle("/admin/v1/", &admin{site: s, reg: reg, nm: nm, src: src, sc: sc, names: namer{nm, src}, started: time.Now(), changed: func() {}})
	ok := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok " + who(r).Person.ID)) }
	for _, p := range []string{"/api/status", "/api/latency", "/api/mute-all", "/api/page/end", "/api/nodes/", "/api/clients/", "/api/groups/"} {
		mux.HandleFunc(p, ok)
	}
	srv := httptest.NewServer(s.guard(mux))
	t.Cleanup(srv.Close)
	return &testSite{t, srv, s, reg}
}

func (ts *testSite) admin(method, path, body string) (*http.Response, string) {
	ts.Helper()
	req, _ := http.NewRequest(method, ts.srv.URL+"/admin/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

// signIn gets a one-time link for person and follows it with a cookie jar.
func (ts *testSite) signIn(person string) *http.Client {
	ts.Helper()
	resp, body := ts.admin("POST", "/signin", `{"person":"`+person+`"}`)
	if resp.StatusCode != 200 {
		ts.Fatalf("signin %s: %d %s", person, resp.StatusCode, body)
	}
	var link struct{ URL string }
	json.Unmarshal([]byte(body), &link)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	r, err := c.Get(link.URL)
	if err != nil {
		ts.Fatal(err)
	}
	r.Body.Close()
	if r.Request.URL.Path != "/" {
		ts.Fatalf("after sign-in landed on %s", r.Request.URL)
	}
	return c
}

func status(t *testing.T, c *http.Client, method, url string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(`{}`))
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r.StatusCode
}

const testDoc = `{
  "default_source": "Music",
  "people": [
    {"id": "u1", "name": "Jordan", "role": "owner"},
    {"id": "u2", "name": "Sam", "role": "guest"},
    {"id": "u3", "name": "Alex", "role": "guest"}
  ],
  "events": [
    {"id": "xmas", "name": "Christmas", "members": ["u2"], "poles": ["civ-1"], "sources": ["src-radiodj"], "volume_cap": 80,
     "windows": [{"kind": "once", "start": "2026-12-05T08:00", "end": "2026-12-07T23:00"}]},
    {"id": "trucks", "name": "Food Trucks", "members": ["u3"], "poles": ["civ-1"], "sources": ["src-phone"],
     "windows": [{"kind": "monthly", "nth": 3, "weekday": "fri", "start": "16:00", "end": "21:00"}]}
  ]
}`

func TestAdminKeyRequired(t *testing.T) {
	ts := newTestSite(t)
	for _, auth := range []string{"", "Bearer wrong", "Basic " + testKey} {
		req, _ := http.NewRequest("GET", ts.srv.URL+"/admin/v1/health", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		r, _ := http.DefaultClient.Do(req)
		if r.StatusCode != 401 {
			t.Errorf("auth %q: %d", auth, r.StatusCode)
		}
	}
	if r, body := ts.admin("GET", "/health", ""); r.StatusCode != 200 || !strings.Contains(body, `"version"`) {
		t.Fatalf("health: %d %s", r.StatusCode, body)
	}
}

func TestConsoleNeedsSignIn(t *testing.T) {
	ts := newTestSite(t)
	if c := status(t, http.DefaultClient, "GET", ts.srv.URL+"/api/status"); c != 401 {
		t.Fatalf("no session: %d", c)
	}
	if c := status(t, http.DefaultClient, "GET", ts.srv.URL+"/api/me"); c != 401 {
		t.Fatalf("me without session: %d", c)
	}
	// The static console itself loads, to show the signed-out screen.
	if c := status(t, http.DefaultClient, "GET", ts.srv.URL+"/index.html"); c == 401 {
		t.Fatal("static files need no session")
	}
}

func TestPlanPatchAndPoleLimit(t *testing.T) {
	ts := newTestSite(t)
	if r, body := ts.admin("PUT", "/plan", `{"plan":"event","pole_limit":1,"timezone":"America/New_York"}`); r.StatusCode != 200 || !strings.Contains(body, `"pole_limit":1`) {
		t.Fatalf("plan: %d %s", r.StatusCode, body)
	}
	if r, _ := ts.admin("PUT", "/plan", `{"timezone":"Mars/Olympus"}`); r.StatusCode != 400 {
		t.Fatalf("bad timezone: %d", r.StatusCode)
	}
	if r, body := ts.admin("PUT", "/plan", `{"suspended":true}`); !strings.Contains(body, `"plan":"event"`) || r.StatusCode != 200 {
		t.Fatalf("partial patch lost fields: %s", body)
	}
	ctx := context.Background()
	if err := ts.site.checkPoleLimit(ctx, nodes.RoleNode); err != nil {
		t.Fatalf("first pole: %v", err)
	}
	ts.reg.Adopt(ctx, "civ-1", nodes.RoleNode, "Pole 1", "hash")
	if err := ts.site.checkPoleLimit(ctx, nodes.RoleNode); !errors.Is(err, errPoleLimit) {
		t.Fatalf("second pole: %v", err)
	}
	if err := ts.site.checkPoleLimit(ctx, nodes.RoleSource); err != nil {
		t.Fatalf("sources don't count: %v", err)
	}
	// Survives a restart.
	again, _ := newSite(ctx, ts.reg, testKey, "")
	if p := again.Plan(); p.PoleLimit != 1 || !p.Suspended || again.Loc().String() != "America/New_York" {
		t.Fatalf("reloaded plan: %+v %v", p, again.Loc())
	}
}

func TestAccessDocValidated(t *testing.T) {
	ts := newTestSite(t)
	if r, _ := ts.admin("PUT", "/access", `{"people":[{"id":"u1","role":"king"}]}`); r.StatusCode != 400 {
		t.Fatalf("bad role accepted: %d", r.StatusCode)
	}
	if r, _ := ts.admin("PUT", "/access", `{"people":[],"surprise":1}`); r.StatusCode != 400 {
		t.Fatalf("unknown field accepted: %d", r.StatusCode)
	}
	if r, body := ts.admin("PUT", "/access", testDoc); r.StatusCode != 204 {
		t.Fatalf("doc: %d %s", r.StatusCode, body)
	}
	if r, _ := ts.admin("POST", "/signin", `{"person":"stranger"}`); r.StatusCode != 404 {
		t.Fatalf("stranger signin: %d", r.StatusCode)
	}
}

func TestOwnerAndGuests(t *testing.T) {
	ts := newTestSite(t)
	ts.admin("PUT", "/plan", `{"timezone":"America/New_York"}`)
	ts.admin("PUT", "/access", testDoc)
	u := ts.srv.URL

	owner := ts.signIn("u1")
	if c := status(t, owner, "POST", u+"/api/latency"); c != 200 {
		t.Fatalf("owner settings: %d", c)
	}

	// Sam's Christmas event is open (buffer aside, it's noon on Dec 5).
	sam := ts.signIn("u2")
	if c := status(t, sam, "GET", u+"/api/status"); c != 200 {
		t.Fatalf("guest read: %d", c)
	}
	for _, p := range []string{"/api/clients/x/volume", "/api/groups/g/stream", "/api/mute-all"} {
		if c := status(t, sam, "POST", u+p); c != 200 {
			t.Errorf("guest %s: %d", p, c)
		}
	}
	for _, p := range []string{"/api/latency", "/api/nodes/civ-1/approve", "/api/nodes/civ-1/update", "/api/page/end"} {
		if c := status(t, sam, "POST", u+p); c != 403 {
			t.Errorf("guest %s: %d, want 403", p, c)
		}
	}
	var me meView
	r, _ := sam.Get(u + "/api/me")
	json.NewDecoder(r.Body).Decode(&me)
	r.Body.Close()
	if !me.Open || me.Can.Admin || me.Can.VolumeCap != 80 || len(me.Can.Poles) != 1 || me.Until == nil {
		t.Fatalf("sam's me: %+v", me)
	}

	// Alex's Food Trucks is the 3rd Friday: closed today.
	alex := ts.signIn("u3")
	if c := status(t, alex, "GET", u+"/api/status"); c != 403 {
		t.Fatalf("closed guest read: %d", c)
	}
	r, _ = alex.Get(u + "/api/me")
	json.NewDecoder(r.Body).Decode(&me)
	r.Body.Close()
	if me.Open || me.Next == nil || !me.Next.Equal(time.Date(2026, 12, 18, 16, 0, 0, 0, ts.site.Loc())) {
		t.Fatalf("alex's me: %+v", me)
	}

	// Suspended: read-only for everyone, owners included.
	ts.admin("PUT", "/plan", `{"suspended":true}`)
	if c := status(t, owner, "POST", u+"/api/mute-all"); c != 403 {
		t.Fatalf("suspended owner write: %d", c)
	}
	if c := status(t, owner, "GET", u+"/api/status"); c != 200 {
		t.Fatalf("suspended owner read: %d", c)
	}
	ts.admin("PUT", "/plan", `{"suspended":false}`)

	// Removing Sam from the document ends the session at once.
	doc := access.Doc{People: []access.Person{{ID: "u1", Role: access.Owner}}}
	b, _ := json.Marshal(doc)
	ts.admin("PUT", "/access", string(b))
	if c := status(t, sam, "GET", u+"/api/status"); c != 401 {
		t.Fatalf("removed guest: %d", c)
	}
}

func TestSigninLinkWorksOnce(t *testing.T) {
	ts := newTestSite(t)
	ts.admin("PUT", "/access", testDoc)
	_, body := ts.admin("POST", "/signin", `{"person":"u1","redirect":"//evil.example"}`)
	var link struct{ URL string }
	json.Unmarshal([]byte(body), &link)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ := c.Get(link.URL)
	if r.StatusCode != 302 || r.Header.Get("Location") != "/" {
		t.Fatalf("first use: %d → %q", r.StatusCode, r.Header.Get("Location"))
	}
	if ck := r.Cookies(); len(ck) != 1 || !ck[0].HttpOnly {
		t.Fatalf("cookie: %+v", ck)
	}
	r, _ = http.Get(link.URL)
	if r.StatusCode != 403 {
		t.Fatalf("second use: %d", r.StatusCode)
	}
}
