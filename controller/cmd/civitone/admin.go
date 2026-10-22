package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/access"
	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
)

// admin serves /admin/v1/: the hosting side's (or a self-hoster's scripts')
// way to run this controller. See docs/admin-api.md.
type admin struct {
	site    *site
	reg     *registry.Registry
	nm      *nodes.Manager
	src     *sources.Manager
	sc      *snap.Link
	names   namer
	started time.Time
	changed func() // the console should push state now
}

func (a *admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.site.adminAuthorized(r) {
		jsonError(w, http.StatusUnauthorized, "admin key required")
		return
	}
	route := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/admin/v1")
	switch route {
	case "GET /health":
		writeJSON(w, a.health(r.Context()))
	case "GET /usage":
		a.usage(w, r)
	case "GET /inventory":
		writeJSON(w, a.inventory(r.Context()))
	case "GET /plan":
		writeJSON(w, a.site.Plan())
	case "PUT /plan":
		var p planPatch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		before := a.site.Plan()
		next, err := a.site.setPlan(r.Context(), p)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if before.Suspended != next.Suspended {
			text := "site suspended: the console is read-only"
			if !next.Suspended {
				text = "site resumed"
			}
			_ = a.reg.Log(r.Context(), "console", "ops", text)
		}
		if before.PoleLimit != next.PoleLimit || before.Plan != next.Plan {
			_ = a.reg.Log(r.Context(), "console", "ops", "plan: "+planText(next))
		}
		a.changed()
		writeJSON(w, next)
	case "GET /access":
		writeJSON(w, a.site.Doc())
	case "PUT /access":
		var d access.Doc
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.site.setDoc(r.Context(), d); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.changed()
		w.WriteHeader(http.StatusNoContent)
	case "POST /signin":
		a.signin(w, r)
	case "GET /backup":
		a.backup(w, r)
	default:
		jsonError(w, http.StatusNotFound, "no such admin call: "+route)
	}
}

func planText(p plan) string {
	name := p.Plan
	if name == "" {
		name = "no plan name"
	}
	if p.PoleLimit > 0 {
		return name + ", up to " + strconv.Itoa(p.PoleLimit) + " poles"
	}
	return name + ", no pole limit"
}

type countView struct {
	Adopted int `json:"adopted"`
	Online  int `json:"online"`
	Limit   int `json:"limit,omitempty"`
}

type healthView struct {
	OK         bool      `json:"ok"`
	Version    string    `json:"version"`
	UptimeS    int64     `json:"uptime_s"`
	Snapserver bool      `json:"snapserver"`
	Suspended  bool      `json:"suspended"`
	Poles      countView `json:"poles"`
	Sources    countView `json:"sources"`
	Playing    bool      `json:"playing"`
	Pending    int       `json:"pending"`
}

func (a *admin) health(ctx context.Context) healthView {
	h := healthView{Version: version, UptimeS: int64(time.Since(a.started).Seconds()), Suspended: a.site.Plan().Suspended}
	h.Poles.Limit = a.site.Plan().PoleLimit
	for _, n := range a.nm.States() {
		switch {
		case !n.Adopted && n.Connected:
			h.Pending++
		case n.Role == nodes.RoleSource:
			h.Sources.Adopted++
			if n.Connected {
				h.Sources.Online++
			}
		case n.Adopted:
			h.Poles.Adopted++
			if n.Connected {
				h.Poles.Online++
			}
		}
	}
	// Poles adopted but not seen since start still count as adopted.
	if poles, err := a.reg.ByRole(ctx, nodes.RoleNode); err == nil && len(poles) > h.Poles.Adopted {
		h.Poles.Adopted = len(poles)
	}
	if srcs, err := a.reg.ByRole(ctx, nodes.RoleSource); err == nil && len(srcs) > h.Sources.Adopted {
		h.Sources.Adopted = len(srcs)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if st, err := a.sc.GetStatus(cctx); err == nil {
		h.Snapserver, h.OK = true, true
		h.Playing = playingPoles(st, a.names) > 0
	}
	return h
}

// playingPoles counts the adopted poles that are online in a zone whose
// stream is playing.
func playingPoles(st *snap.Status, names namer) int {
	playing := map[string]bool{}
	for _, s := range st.Server.Streams {
		playing[s.ID] = s.Status == "playing"
	}
	n := 0
	for _, g := range st.Server.Groups {
		if g.Muted || !playing[g.StreamID] {
			continue
		}
		for _, c := range g.Clients {
			if who, _ := names.client(c); c.Connected && who != "console" && !c.Config.Volume.Muted {
				n++
			}
		}
	}
	return n
}

func (a *admin) usage(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 400 {
		days = 31
	}
	rows, err := a.reg.Usage(r.Context(), days)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type day struct {
		Day          string  `json:"day"`
		PlayingHours float64 `json:"playing_hours"`
		PoleHours    float64 `json:"pole_hours"`
		EstGB        float64 `json:"est_gb"`
	}
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	out := []day{}
	for _, u := range rows {
		ph := float64(u.PoleS) / 3600
		out = append(out, day{u.Day, round(float64(u.PlayingS) / 3600), round(ph), round(ph * 0.0675)})
	}
	writeJSON(w, map[string]any{"days": out})
}

type inventoryView struct {
	Zones   []zoneView   `json:"zones"`
	Poles   []poleView   `json:"poles"`
	Sources []sourceView `json:"sources"`
	Streams []streamView `json:"streams"`
}

type zoneView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Poles    []string `json:"poles"`
	StreamID string   `json:"stream_id"`
}

type poleView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	FW     string `json:"fw"`
}

type sourceView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StreamID string `json:"stream_id"`
	Online   bool   `json:"online"`
}

type streamView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (a *admin) inventory(ctx context.Context) inventoryView {
	v := inventoryView{Zones: []zoneView{}, Poles: []poleView{}, Sources: []sourceView{}, Streams: []streamView{}}
	live := map[string]nodes.State{}
	for _, n := range a.nm.States() {
		live[n.DeviceID] = n
	}
	if poles, err := a.reg.ByRole(ctx, nodes.RoleNode); err == nil {
		for _, p := range poles {
			st := live[p.DeviceID]
			v.Poles = append(v.Poles, poleView{p.DeviceID, p.Name, st.Connected, st.FW})
		}
	}
	for _, s := range a.src.Infos() {
		v.Sources = append(v.Sources, sourceView{s.DeviceID, s.Name, s.Stream, s.Connected})
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if st, err := a.sc.GetStatus(cctx); err == nil {
		for _, s := range st.Server.Streams {
			v.Streams = append(v.Streams, streamView{s.ID, a.names.stream(s.ID)})
		}
		for _, g := range st.Server.Groups {
			z := zoneView{ID: g.ID, Name: g.Name, StreamID: g.StreamID, Poles: []string{}}
			var names []string
			for _, c := range g.Clients {
				if who, name := a.names.client(c); who != "console" {
					z.Poles = append(z.Poles, who)
					names = append(names, name)
				}
			}
			if len(z.Poles) == 0 {
				continue // a zone of local test clients, not poles
			}
			if z.Name == "" {
				z.Name = strings.Join(names, ", ")
			}
			v.Zones = append(v.Zones, z)
		}
	}
	return v
}

func (a *admin) signin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Person   string `json:"person"`
		Redirect string `json:"redirect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Person == "" {
		jsonError(w, http.StatusBadRequest, "person required")
		return
	}
	if _, ok := a.site.Doc().GrantFor(body.Person, time.Now(), a.site.Loc()); !ok {
		jsonError(w, http.StatusNotFound, "no such person in the access document")
		return
	}
	if !strings.HasPrefix(body.Redirect, "/") || strings.HasPrefix(body.Redirect, "//") {
		body.Redirect = "/"
	}
	token := newSecret()
	exp := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	if err := a.reg.CreateSignin(r.Context(), nodes.HashToken(token), body.Person, body.Redirect, exp); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	base := a.site.publicURL
	if base == "" {
		scheme := "http"
		if a.site.secure(r) {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	writeJSON(w, map[string]any{"url": base + "/signin?token=" + url.QueryEscape(token), "expires_at": exp})
}

func (a *admin) backup(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "civitone-backup")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "civitone.db")
	if err := a.reg.Backup(r.Context(), path); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.Open(path)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="civitone-`+time.Now().UTC().Format("20060102-150405")+`.db"`)
	io.Copy(w, f)
}
