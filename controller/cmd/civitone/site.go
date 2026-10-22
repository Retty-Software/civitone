package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/access"
	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
)

// version is the controller's release, set at build time with
// -ldflags "-X main.version=...".
var version = "0.5.0-dev"

// plan is what the hosting side sets with PUT /admin/v1/plan.
type plan struct {
	Plan      string `json:"plan"`
	PoleLimit int    `json:"pole_limit"` // 0 = no limit
	Suspended bool   `json:"suspended"`
	Timezone  string `json:"timezone"`
}

// site holds the plan and the access document, and decides who the console
// is talking to. With no admin key it stays out of the way: everyone is an
// owner, as before sign-in existed.
type site struct {
	reg       *registry.Registry
	adminKey  string
	publicURL string
	now       func() time.Time

	mu   sync.RWMutex
	plan plan
	loc  *time.Location
	doc  access.Doc
}

var errPoleLimit = errors.New("pole limit reached")

func newSite(ctx context.Context, reg *registry.Registry, adminKey, publicURL string) (*site, error) {
	s := &site{reg: reg, adminKey: adminKey, publicURL: strings.TrimRight(publicURL, "/"), now: time.Now, loc: time.UTC}
	if raw := reg.Setting(ctx, "plan", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.plan); err != nil {
			return nil, fmt.Errorf("saved plan: %w", err)
		}
	}
	if s.plan.Timezone != "" {
		loc, err := time.LoadLocation(s.plan.Timezone)
		if err != nil {
			return nil, fmt.Errorf("saved plan: %w", err)
		}
		s.loc = loc
	}
	if raw := reg.Setting(ctx, "access", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.doc); err != nil {
			return nil, fmt.Errorf("saved access: %w", err)
		}
	}
	return s, nil
}

// sessions: the console needs a sign-in (an admin key is set).
func (s *site) sessions() bool { return s.adminKey != "" }

func (s *site) Plan() plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.plan
}

func (s *site) Loc() *time.Location {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loc
}

func (s *site) Doc() access.Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doc
}

// planPatch: only the fields sent change.
type planPatch struct {
	Plan      *string `json:"plan"`
	PoleLimit *int    `json:"pole_limit"`
	Suspended *bool   `json:"suspended"`
	Timezone  *string `json:"timezone"`
}

func (s *site) setPlan(ctx context.Context, p planPatch) (plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, loc := s.plan, s.loc
	if p.Plan != nil {
		next.Plan = *p.Plan
	}
	if p.PoleLimit != nil {
		if *p.PoleLimit < 0 {
			return plan{}, errors.New("pole_limit can't be negative")
		}
		next.PoleLimit = *p.PoleLimit
	}
	if p.Suspended != nil {
		next.Suspended = *p.Suspended
	}
	if p.Timezone != nil {
		l, err := time.LoadLocation(*p.Timezone)
		if err != nil || *p.Timezone == "" || *p.Timezone == "Local" {
			return plan{}, fmt.Errorf("unknown timezone %q", *p.Timezone)
		}
		next.Timezone, loc = *p.Timezone, l
	}
	b, _ := json.Marshal(next)
	if err := s.reg.SetSetting(ctx, "plan", string(b)); err != nil {
		return plan{}, err
	}
	s.plan, s.loc = next, loc
	return next, nil
}

func (s *site) setDoc(ctx context.Context, d access.Doc) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.People == nil {
		d.People = []access.Person{}
	}
	if d.Events == nil {
		d.Events = []access.Event{}
	}
	b, _ := json.Marshal(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reg.SetSetting(ctx, "access", string(b)); err != nil {
		return err
	}
	s.doc = d
	return nil
}

// checkPoleLimit refuses an approval that would pass the plan's limit.
// Sources don't count.
func (s *site) checkPoleLimit(ctx context.Context, role string) error {
	limit := s.Plan().PoleLimit
	if role != nodes.RoleNode || limit <= 0 {
		return nil
	}
	poles, err := s.reg.ByRole(ctx, nodes.RoleNode)
	if err != nil {
		return err
	}
	if len(poles) >= limit {
		return fmt.Errorf("%w: %d of %d poles. Remove a pole, or add poles to the plan", errPoleLimit, len(poles), limit)
	}
	return nil
}

// principal is who a console request comes from, and what they may do now.
type principal struct {
	access.Grant
	Session string // hash of the session cookie; "" in open mode
}

type ctxKey struct{}

func who(r *http.Request) *principal {
	p, _ := r.Context().Value(ctxKey{}).(*principal)
	if p == nil {
		return &principal{Grant: access.Grant{Admin: true, Open: true, Person: access.Person{Role: access.Owner}}}
	}
	return p
}

// by names the person for the activity log: " (Sam Ortiz)", or nothing
// when nobody signs in.
func by(r *http.Request) string {
	if p := who(r); p.Session != "" && p.Person.Name != "" {
		return " (" + p.Person.Name + ")"
	}
	return ""
}

const sessionCookie = "civitone_session"

func newSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// principalFor resolves a request's session, if it has a live one.
func (s *site) principalFor(r *http.Request) (*principal, bool) {
	if !s.sessions() {
		return &principal{Grant: access.Grant{Admin: true, Open: true, Person: access.Person{ID: "local", Role: access.Owner}}}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	hash := nodes.HashToken(c.Value)
	person, ok := s.reg.Session(r.Context(), hash)
	if !ok {
		return nil, false
	}
	g, ok := s.Doc().GrantFor(person, s.now(), s.Loc())
	if !ok {
		return nil, false // removed from the document since signing in
	}
	return &principal{Grant: g, Session: hash}, true
}

// guestGET and guestPOST are what a guest may reach while an event is open.
// The handlers then check the particular poles and sources.
var guestGET = map[string]bool{"/ws": true, "/api/events": true, "/api/status": true, "/api/nodes": true}

func guestPOST(path string) bool {
	switch {
	case path == "/api/mute-all", path == "/api/page/end":
		return true
	case strings.HasPrefix(path, "/api/clients/") && strings.HasSuffix(path, "/volume"):
		return true
	case strings.HasPrefix(path, "/api/groups/") && strings.HasSuffix(path, "/stream"):
		return true
	}
	return false
}

// public paths: devices with their own tokens, sign-in, the admin API (its
// own key), and the console's static files (no data in them).
func public(path string) bool {
	switch {
	case path == "/node", path == "/source", path == "/signin", path == "/signout", path == "/api/me":
		return true
	case strings.HasPrefix(path, "/firmware/"), strings.HasPrefix(path, "/admin/"):
		return true
	case path == "/ws", strings.HasPrefix(path, "/api/"):
		return false
	}
	return true
}

// guard is the console's front door: who's asking, and may they.
func (s *site) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		p, ok := s.principalFor(r)
		if !ok {
			jsonError(w, http.StatusUnauthorized, "sign in again: open the console from your account")
			return
		}
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		if !read && s.Plan().Suspended {
			jsonError(w, http.StatusForbidden, "this site is suspended, so the console is read-only. The account owner can fix this in billing")
			return
		}
		if !p.Admin {
			switch {
			case !p.Open:
				jsonError(w, http.StatusForbidden, "your event isn't open right now")
				return
			case read && !guestGET[r.URL.Path]:
				jsonError(w, http.StatusForbidden, "guests can't open that")
				return
			case !read && !guestPOST(r.URL.Path):
				jsonError(w, http.StatusForbidden, "only the site's owner and operators can do that")
				return
			case r.URL.Path == "/api/page/end" && !p.Paging:
				jsonError(w, http.StatusForbidden, "your event doesn't include paging")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// stillAllowed is checked for each open console WebSocket on every state
// push, so a removed guest or a closed event drops the live view too.
func (s *site) stillAllowed(r *http.Request) bool {
	p, ok := s.principalFor(r)
	return ok && p.Open
}

// signin turns a one-time link into a console session.
func (s *site) signin(w http.ResponseWriter, r *http.Request) {
	if !s.sessions() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	tok := r.URL.Query().Get("token")
	person, redirect, ok, err := "", "", false, error(nil)
	if tok != "" {
		person, redirect, ok, err = s.reg.UseSignin(r.Context(), nodes.HashToken(tok))
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, known := s.Doc().GrantFor(person, s.now(), s.Loc()); !ok || !known {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, signedOutPage("That sign-in link has expired or was already used. Open the console again from your account."))
		return
	}
	secret := newSecret()
	if err := s.reg.CreateSession(r.Context(), nodes.HashToken(secret), person, s.now().Add(12*time.Hour)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: secret, Path: "/", MaxAge: 12 * 3600, HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (s *site) signout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.reg.EndSession(r.Context(), nodes.HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *site) secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasPrefix(s.publicURL, "https://")
}

func signedOutPage(msg string) string {
	return `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Civitone</title>
<body style="margin:0;min-height:100vh;display:grid;place-items:center;background:#0a1020;color:#eef0f3;font:17px/1.5 system-ui,sans-serif">
<main style="max-width:420px;padding:24px"><h1 style="font-size:26px;margin:0 0 8px">Signed out</h1><p style="color:#c5ccd8;margin:0">` + html.EscapeString(msg) + `</p></main>`
}

// me is GET /api/me: who the console is signed in as, and what they may do.
type meView struct {
	Auth      string               `json:"auth"` // open | session
	SignedIn  bool                 `json:"signed_in"`
	Person    *access.Person       `json:"person,omitempty"`
	Suspended bool                 `json:"suspended"`
	Plan      planView             `json:"plan"`
	Open      bool                 `json:"open"`
	Until     *time.Time           `json:"until"`
	Next      *time.Time           `json:"next"`
	Events    []access.EventStatus `json:"events"`
	Can       canView              `json:"can"`
}

type planView struct {
	Plan      string `json:"plan"`
	PoleLimit int    `json:"pole_limit"`
	PolesUsed int    `json:"poles_used"`
	Suspended bool   `json:"suspended"`
	Timezone  string `json:"timezone"`
}

type canView struct {
	Admin     bool           `json:"admin"`
	Poles     []string       `json:"poles"` // null: all
	Caps      map[string]int `json:"caps,omitempty"`
	Sources   []string       `json:"sources"` // device ids and stream ids; null: all
	VolumeCap int            `json:"volume_cap"`
	Paging    bool           `json:"paging"`
}

func (s *site) planView(ctx context.Context) planView {
	p := s.Plan()
	used := 0
	if poles, err := s.reg.ByRole(ctx, nodes.RoleNode); err == nil {
		used = len(poles)
	}
	tz := p.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return planView{Plan: p.Plan, PoleLimit: p.PoleLimit, PolesUsed: used, Suspended: p.Suspended, Timezone: tz}
}

func (s *site) me(streamOf func(deviceID string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := meView{Auth: "open", Plan: s.planView(r.Context()), Suspended: s.Plan().Suspended, Events: []access.EventStatus{}}
		if s.sessions() {
			v.Auth = "session"
		}
		p, ok := s.principalFor(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(v)
			return
		}
		v.SignedIn, v.Open, v.Events = true, p.Open, p.Events
		if v.Events == nil {
			v.Events = []access.EventStatus{}
		}
		if s.sessions() {
			person := p.Person
			v.Person = &person
		}
		if !p.Until.IsZero() {
			v.Until = &p.Until
		}
		if !p.Next.IsZero() {
			v.Next = &p.Next
		}
		v.Can = canView{Admin: p.Admin, VolumeCap: 100, Paging: p.Admin || p.Paging}
		if !p.Admin {
			v.Can.Poles, v.Can.Sources, v.Can.Caps, v.Can.VolumeCap = []string{}, []string{}, p.Poles, 0
			for id, c := range p.Poles {
				v.Can.Poles = append(v.Can.Poles, id)
				v.Can.VolumeCap = max(v.Can.VolumeCap, c)
			}
			for id := range p.Sources {
				v.Can.Sources = append(v.Can.Sources, id)
				if st := streamOf(id); st != "" && st != id {
					v.Can.Sources = append(v.Can.Sources, st)
				}
			}
			if d := s.Doc().DefaultSource; d != "" && len(p.Poles) > 0 {
				v.Can.Sources = append(v.Can.Sources, d)
			}
		}
		writeJSON(w, v)
	}
}

// guestMayPlay: a guest may put a stream on their zones if it belongs to a
// source of one of their open events, or it's the site's default source.
func (s *site) guestMayPlay(p *principal, streamID string, streamOf func(string) string) bool {
	if p.Admin {
		return true
	}
	if streamID == s.Doc().DefaultSource && streamID != "" {
		return true
	}
	for id := range p.Sources {
		if id == streamID || streamOf(id) == streamID {
			return true
		}
	}
	return false
}

func (s *site) adminAuthorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.adminKey != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminKey)) == 1
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
