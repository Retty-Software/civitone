// Command civitone is the Civitone controller: it runs snapserver, keeps the
// node registry, and serves the console and the control channel.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/paging"
	"github.com/Retty-Software/civitone/controller/internal/registry"
	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
	"github.com/Retty-Software/civitone/controller/internal/ws"
	"github.com/Retty-Software/civitone/controller/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "signin-link" {
		if err := signinLink(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "signin-link:", err)
			os.Exit(1)
		}
		return
	}
	var (
		listen       = flag.String("listen", ":8080", "console + control channel address")
		snapBin      = flag.String("snapserver", defaultSnapBinary(), "snapserver binary (empty = don't launch, connect to an existing one)")
		snapConf     = flag.String("snapserver-config", "snapserver.conf", "snapserver config file")
		snapRPC      = flag.String("snap-rpc", "127.0.0.1:1705", "snapserver JSON-RPC address")
		dbPath       = flag.String("db", "civitone.db", "registry SQLite file")
		ceilingDB    = flag.Float64("ceiling-db", -1, "limiter ceiling in dBFS for every source")
		sourceBase   = flag.Int("source-port-base", 4960, "first snapserver TCP port for source streams")
		snapHost     = flag.String("snap-host", "", "snapserver host sent to nodes at adoption (empty = the host they dialled)")
		snapBufferMs = flag.Int("snap-buffer-ms", 2000, "speaker buffer until one is set in the console (overrides snapserver.conf's buffer when the controller runs snapserver)")
		snapRuntime  = flag.String("snapserver-runtime-config", "", "where to write the config snapserver actually runs with (default: next to --db)")
		snapPort     = flag.Int("snap-port", envInt("CIVITONE_SNAP_PORT", 1704), "snapserver stream port sent to nodes at adoption")
		adminKey     = flag.String("admin-key", os.Getenv("CIVITONE_ADMIN_KEY"), "turns on the admin API and console sign-in (docs/admin-api.md); also CIVITONE_ADMIN_KEY")
		publicURL    = flag.String("public-url", os.Getenv("CIVITONE_PUBLIC_URL"), "the console's address, for sign-in links (e.g. https://main-street.example.com)")
	)
	flag.Parse()
	if *snapHost == "" {
		*snapHost = os.Getenv("CIVITONE_SNAP_HOST")
	}
	if *adminKey != "" && len(*adminKey) < 24 {
		log.Fatal("--admin-key: use at least 24 random characters")
	}
	if v := os.Getenv("CIVITONE_LISTEN"); v != "" && !flagSet("listen") {
		*listen = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg, err := registry.Open(*dbPath)
	if err != nil {
		log.Fatalf("registry: %v", err)
	}
	defer reg.Close()
	st8, err := newSite(ctx, reg, *adminKey, *publicURL)
	if err != nil {
		log.Fatalf("site: %v", err)
	}

	// Speaker buffer: snapserver's [stream] buffer, i.e. how far behind
	// real time every speaker plays. Set in the console; applying it
	// restarts snapserver, so only when the controller runs snapserver.
	speakerBuffer := func() int {
		v, err := strconv.Atoi(reg.Setting(ctx, "speaker_buffer_ms", strconv.Itoa(*snapBufferMs)))
		if err != nil {
			return *snapBufferMs
		}
		return v
	}
	var sup *snap.Supervisor
	if *snapBin != "" {
		runtime := *snapRuntime
		if runtime == "" {
			runtime = filepath.Join(filepath.Dir(*dbPath), "snapserver.run.conf")
		}
		sup = &snap.Supervisor{Binary: *snapBin, Base: *snapConf, Runtime: runtime, BufferMs: speakerBuffer}
		go func() {
			if err := sup.Run(ctx); err != nil {
				log.Printf("snapserver: %v", err)
				stop()
			}
		}()
	}

	sc := snap.NewLink(*snapRPC)
	go sc.Run(ctx)
	chime := &chimer{sc: sc, buffer: func() time.Duration { return time.Duration(speakerBuffer()) * time.Millisecond }}

	hub := ws.NewHub()
	if st8.sessions() {
		hub.Allow, hub.SameOrigin = st8.stillAllowed, true
	}
	nm := nodes.NewManager(regStore{reg})
	nm.BeforeAdopt = st8.checkPoleLimit
	nm.SnapHost, nm.SnapPort = *snapHost, *snapPort
	refresh := make(chan struct{}, 1)
	nm.OnEvent = func(e nodes.Event) {
		go reg.Log(context.Background(), e.DeviceID, e.Level, e.Text)
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	go nm.Run(ctx)

	build := strconv.FormatInt(time.Now().UnixNano(), 36)

	// Adopted sources: one snapserver stream and jitter buffer each.
	src := sources.New(ctx, sc, reg)
	src.Base, src.CeilingDB = *sourceBase, *ceilingDB
	src.OnEvent = func(id, level, text string) {
		go reg.Log(context.Background(), id, level, text)
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	if err := src.Load(ctx); err != nil {
		log.Fatalf("sources: %v", err)
	}
	spk := &speakers{reg: reg, nm: nm, chime: chime}
	names := namer{nm: nm, src: src}
	if err := spk.load(ctx); err != nil {
		log.Fatalf("speakers: %v", err)
	}
	nm.OnConnect = func(st nodes.State) {
		src.Connect(st)
		spk.connected(st)
	}
	nm.OnAudio = src.Audio

	// Emergency paging: the paging mic's push-to-talk takes every speaker.
	pager := paging.New(sc, src)
	pager.SpeakerBuffer = func() time.Duration { return time.Duration(speakerBuffer()) * time.Millisecond }
	pager.OnEvent = src.OnEvent
	nm.OnPTT = func(id string, down bool) {
		pager.PTT(id, down)
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	nm.OnDisconnect = func(st nodes.State) {
		src.Disconnect(st)
		pager.SourceGone(st.DeviceID)
	}

	activeCodes := func() []registry.AdoptCode {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		c, err := reg.ActiveCodes(cctx)
		if err != nil {
			return []registry.AdoptCode{}
		}
		return c
	}

	// Push full status to consoles on every snapserver event, and every 2 s regardless.
	go func() {
		push := func() {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			st, err := sc.GetStatus(cctx)
			if err != nil {
				if err != snap.ErrNotConnected {
					log.Printf("snap: status: %v", err)
				}
				st = &snap.Status{}
			}
			hub.Broadcast(consoleState{Status: st, Nodes: nm.States(), Codes: activeCodes(), Sources: src.Infos(), Speakers: spk.all(), Paging: pager.State(), Latency: latencyState{speakerBuffer(), sup != nil}, Build: build, ServerTime: time.Now(),
				Plan: st8.planView(ctx), Events: eventViews(st8), DefaultSource: st8.Doc().DefaultSource})
		}
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				push()
			case ev := <-sc.Events:
				log.Printf("snap: %s", ev.Method)
				if ev.Method == "Link.Connected" {
					go src.Sync() // runtime streams don't survive a snapserver restart
				}
				push()
			case <-refresh:
				push()
			}
		}
	}()

	// Fast meters: each source's peak about ten times a second, between the
	// full state pushes. Read every tick even with no console open, so a
	// console that connects never sees a stale peak.
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				peaks := src.Peaks()
				if hub.Clients() > 0 {
					hub.BroadcastLive(map[string]any{"t": "levels", "peaks": peaks})
				}
			}
		}
	}()

	changed := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	go st8.countUsage(ctx, sc, names)
	go st8.closeEvents(ctx, sc, src, names, changed)
	logOps := func(r *http.Request, who, text string) { _ = reg.Log(r.Context(), who, "ops", text+by(r)) }
	srcStream := streamOf(src)

	mux := http.NewServeMux()
	mux.HandleFunc("/signin", st8.signin)
	mux.HandleFunc("/signout", st8.signout)
	mux.HandleFunc("/api/me", st8.me(srcStream))
	mux.Handle("/admin/v1/", &admin{site: st8, reg: reg, nm: nm, src: src, sc: sc, names: names, started: time.Now(), changed: changed})
	mux.Handle("/ws", hub)
	fw := firmwareStore{dir: filepath.Join(filepath.Dir(*dbPath), "firmware")}
	mux.Handle("/firmware/", fw.serveNode(reg)) // nodes, with their device token; not behind the console login
	logConsole := func(level, text string) { _ = reg.Log(context.Background(), "console", level, text) }
	mux.Handle("/api/firmware", fw.serveConsole(logConsole))
	mux.Handle("/api/firmware/", fw.serveConsole(logConsole))
	mux.Handle("/source", nm) // sources speak the node protocol with role "source"
	mux.Handle("/node", nm)
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, nm.States())
	})
	// GET  /api/latency  -> {"speaker_buffer_ms", "managed"}
	// POST /api/latency  {"speaker_buffer_ms": 900}: saves and restarts snapserver
	mux.HandleFunc("/api/latency", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"speaker_buffer_ms": speakerBuffer(), "managed": sup != nil})
		case http.MethodPost:
			var body struct {
				SpeakerBufferMs int `json:"speaker_buffer_ms"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if sup == nil {
				http.Error(w, "this controller doesn't run snapserver (--snapserver \"\"), so it can't change the speaker buffer", http.StatusConflict)
				return
			}
			ms := clamp(body.SpeakerBufferMs, 150, 5000)
			if err := reg.SetSetting(r.Context(), "speaker_buffer_ms", strconv.Itoa(ms)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			logOps(r, "console", fmt.Sprintf("speaker buffer set to %d ms (snapserver restarting)", ms))
			sup.Restart()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	// POST /api/sources/{device_id}/buffer {"ms": 300}: live, no restart
	// POST /api/sources/{device_id}/gain   {"db": 6}:    live, no restart
	// POST /api/sources/{device_id}/paging {"on": true}: its push-to-talk pages every speaker
	mux.HandleFunc("/api/sources/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sources/"), "/")
		if r.Method != http.MethodPost || len(parts) != 2 || (parts[1] != "buffer" && parts[1] != "gain" && parts[1] != "paging") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Ms, DB int
			On     bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		var what string
		switch parts[1] {
		case "buffer":
			err = src.SetBuffer(r.Context(), parts[0], body.Ms)
			what = fmt.Sprintf("source buffer set to %d ms", body.Ms)
		case "gain":
			db := max(sources.MinGainDB, min(sources.MaxGainDB, body.DB))
			err = src.SetGain(r.Context(), parts[0], db)
			what = fmt.Sprintf("source gain set to %+d dB", db)
		case "paging":
			err = src.SetPaging(r.Context(), parts[0], body.On)
			what = "no longer the paging mic"
			if body.On {
				what = "set as the paging mic: its talk button pages every speaker"
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		logOps(r, parts[0], what)
		w.WriteHeader(http.StatusNoContent)
	})
	// POST /api/page/end: stop the current page now
	mux.HandleFunc("/api/page/end", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		pager.End()
		w.WriteHeader(http.StatusNoContent)
	})
	// POST   /api/adopt-codes  {"auto_approve": false, "minutes": 60} -> code
	// GET    /api/adopt-codes
	// DELETE /api/adopt-codes/{code}
	mux.HandleFunc("/api/adopt-codes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			codes, err := reg.ActiveCodes(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, codes)
		case http.MethodPost:
			var body struct {
				AutoApprove bool `json:"auto_approve"`
				Minutes     int  `json:"minutes"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Minutes == 0 {
				body.Minutes = 60
			}
			body.Minutes = clamp(body.Minutes, 5, 24*60)
			now := time.Now()
			c := registry.AdoptCode{Code: nodes.NewCode(), CreatedAt: now, ExpiresAt: now.Add(time.Duration(body.Minutes) * time.Minute), Auto: body.AutoApprove}
			if err := reg.CreateCode(r.Context(), c); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			how := "approval required"
			if c.Auto {
				how = "auto-approve"
			}
			logOps(r, "console", fmt.Sprintf("adopt code %s created (%d min, %s)", nodes.FormatCode(c.Code), body.Minutes, how))
			select {
			case refresh <- struct{}{}:
			default:
			}
			writeJSON(w, c)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/adopt-codes/", func(w http.ResponseWriter, r *http.Request) {
		code := nodes.NormalizeCode(strings.TrimPrefix(r.URL.Path, "/api/adopt-codes/"))
		if r.Method != http.MethodDelete || code == "" {
			http.NotFound(w, r)
			return
		}
		if err := reg.RevokeCode(r.Context(), code); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logOps(r, "console", "adopt code "+nodes.FormatCode(code)+" revoked")
		select {
		case refresh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// POST /api/nodes/{device_id}/cmd     {"cmd": "identify", ...args}
	// POST /api/nodes/{device_id}/approve {"name": "..."}
	// POST /api/nodes/{device_id}/reject
	// POST /api/nodes/{device_id}/forget  revoke token; node keeps Wi-Fi, opens setup
	// POST /api/nodes/{device_id}/reset   forget + wipe the node (Wi-Fi too)
	// POST /api/nodes/{device_id}/rename  {"name": "..."}
	mux.HandleFunc("/api/nodes/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/")
		if r.Method != http.MethodPost || len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		switch parts[1] {
		case "approve":
			var body struct{ Name string }
			json.NewDecoder(r.Body).Decode(&body)
			cctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := nm.Approve(cctx, parts[0], strings.TrimSpace(body.Name)); err != nil {
				code := http.StatusBadGateway
				if err == nodes.ErrNotPending || errors.Is(err, errPoleLimit) {
					code = http.StatusConflict
				}
				http.Error(w, err.Error(), code)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case "reject":
			if err := nm.Reject(parts[0]); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case "forget", "reset":
			mac := ""
			for _, n := range nm.States() {
				if n.DeviceID == parts[0] {
					mac = n.MAC
				}
			}
			cctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			drop := nm.Forget
			if parts[1] == "reset" {
				drop = nm.Reset
			}
			src.Remove(cctx, parts[0]) // no-op unless it is a source
			if err := drop(cctx, parts[0]); err != nil {
				code := http.StatusInternalServerError
				if err == nodes.ErrNotConnected {
					code = http.StatusConflict
				}
				http.Error(w, err.Error(), code)
				return
			}
			// The node no longer plays from here; once it has dropped off,
			// remove it from snapserver too so it doesn't linger offline.
			if mac != "" {
				go func() {
					time.Sleep(5 * time.Second)
					dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := sc.DeleteClient(dctx, mac); err != nil {
						log.Printf("%s: snapserver delete client: %v", parts[0], err)
					}
				}()
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case "rename":
			var body struct{ Name string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				http.Error(w, "name required", http.StatusBadRequest)
				return
			}
			name := strings.TrimSpace(body.Name)
			if err := reg.Rename(r.Context(), parts[0], name); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			nm.Rename(parts[0], name)
			src.Rename(parts[0], name)
			logOps(r, parts[0], "renamed to "+strconv.Quote(name))
			w.WriteHeader(http.StatusNoContent)
			return
		case "speakers":
			// {"a":{"trim_db":-6,"muted":false},"b":{...}}: either speaker, either field.
			var p speakerPatch
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			sp, err, notApplied := spk.set(cctx, parts[0], p)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			logOps(r, parts[0], fmt.Sprintf("speakers: A %s, B %s", speakerText(sp.A), speakerText(sp.B)))
			select {
			case refresh <- struct{}{}:
			default:
			}
			switch {
			case notApplied == nil:
				w.WriteHeader(http.StatusNoContent)
			case errors.Is(notApplied, nodes.ErrNotConnected):
				http.Error(w, "saved; it applies when the node reconnects", http.StatusAccepted)
			default:
				http.Error(w, "saved, but not applied: "+notApplied.Error(), http.StatusBadGateway)
			}
			return
		case "update":
			// {"sha256": "..."}: one of the uploaded images (/api/firmware).
			var body struct{ SHA256 string }
			json.NewDecoder(r.Body).Decode(&body)
			img, ok := fw.get(strings.ToLower(body.SHA256))
			if !ok {
				http.Error(w, "no such firmware image; upload it first", http.StatusBadRequest)
				return
			}
			if pager.State().Active {
				http.Error(w, "a page is on air; update when it's over", http.StatusConflict)
				return
			}
			var st *nodes.State
			for _, n := range nm.States() {
				if n.DeviceID == parts[0] {
					n := n
					st = &n
				}
			}
			if st == nil || !st.Connected {
				http.Error(w, nodes.ErrNotConnected.Error(), http.StatusNotFound)
				return
			}
			if st.Role == nodes.RoleSource {
				http.Error(w, "source boards have no OTA slot; flash them over USB", http.StatusBadRequest)
				return
			}
			cctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			err := nm.Send(cctx, parts[0], "update_firmware", map[string]any{
				"path": "/firmware/" + img.SHA256 + ".bin", "sha256": img.SHA256, "size": img.Size,
			})
			if err != nil {
				if strings.Contains(err.Error(), "unknown cmd") {
					err = errors.New("this node's firmware can't update over the control channel yet (needs 0.4.0)")
				}
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			logOps(r, parts[0], fmt.Sprintf("firmware update to %s started (was %s)", img.Version, st.FW))
			w.WriteHeader(http.StatusNoContent)
			return
		case "chirp":
			var body struct{ Speaker string }
			json.NewDecoder(r.Body).Decode(&body)
			mac := ""
			for _, n := range nm.States() {
				if n.DeviceID == parts[0] {
					mac = n.MAC
				}
			}
			if mac == "" {
				http.Error(w, nodes.ErrNotConnected.Error(), http.StatusNotFound)
				return
			}
			cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			if err := spk.chirpOne(cctx, parts[0], strings.ToLower(body.Speaker), mac); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case "cmd":
		default:
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cmd, _ := body["cmd"].(string)
		if cmd == "" {
			http.Error(w, "cmd required", http.StatusBadRequest)
			return
		}
		switch cmd {
		case "adopt", "forget", "reset":
			http.Error(w, "use /api/nodes/{id}/approve, /forget or /reset", http.StatusBadRequest)
			return
		case "update_firmware":
			// Only images uploaded here, served with the node's token: never a URL from the request.
			http.Error(w, "use /api/nodes/{id}/update with an uploaded image", http.StatusBadRequest)
			return
		case "set_speakers":
			http.Error(w, "use /api/nodes/{id}/speakers (it's saved and resent on connect)", http.StatusBadRequest)
			return
		}
		delete(body, "cmd")
		if cmd == "identify" {
			// The chime goes through snapserver, so it works even before
			// the node's own identify (LED) does anything.
			for _, n := range nm.States() {
				if n.DeviceID == parts[0] && n.MAC != "" {
					go func(mac string) {
						cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						if err := chime.chime(cctx, mac); err != nil {
							log.Printf("identify %s: chime: %v", parts[0], err)
						}
					}(n.MAC)
				}
			}
		}
		cctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := nm.Send(cctx, parts[0], cmd, body); err != nil {
			code := http.StatusBadGateway
			switch err {
			case nodes.ErrNotConnected:
				code = http.StatusNotFound
			case nodes.ErrNotAdopted:
				code = http.StatusForbidden
			}
			http.Error(w, err.Error(), code)
			return
		}
		logOps(r, parts[0], cmd+" (console)")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		st, err := sc.GetStatus(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, st)
	})
	// POST /api/clients/{id}/volume  {"percent": 0-100, "muted": bool}
	mux.HandleFunc("/api/clients/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/clients/"), "/")
		if len(parts) != 2 || parts[1] != "volume" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Percent int  `json:"percent"`
			Muted   bool `json:"muted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pct := clamp(body.Percent, 0, 100)
		before, _ := sc.GetStatus(r.Context())
		if p := who(r); !p.Admin {
			pole := ""
			if before != nil {
				if c, ok := findClient(before, parts[0]); ok {
					pole, _ = names.client(c)
				}
			}
			limit, ok := p.Poles[pole]
			if !ok {
				jsonError(w, http.StatusForbidden, "that pole isn't part of your event")
				return
			}
			pct = min(pct, limit)
		}
		if err := sc.SetVolume(r.Context(), parts[0], pct, body.Muted); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if before != nil {
			if c, ok := findClient(before, parts[0]); ok {
				who, name := names.client(c)
				was := c.Config.Volume
				switch {
				case was.Muted != body.Muted && body.Muted:
					logOps(r, who, name+" muted")
				case was.Muted != body.Muted:
					logOps(r, who, name+" unmuted")
				case was.Percent != pct:
					logOps(r, who, fmt.Sprintf("%s volume %d%% → %d%%", name, was.Percent, pct))
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		ev, err := reg.Events(r.Context(), 300)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, ev)
	})
	// POST /api/groups/{id}/stream  {"stream_id": "Music"}
	mux.HandleFunc("/api/groups/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/groups/"), "/")
		if r.Method != http.MethodPost || len(parts) != 2 || parts[1] != "stream" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			StreamID string `json:"stream_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.StreamID == "" {
			http.Error(w, "stream_id required", http.StatusBadRequest)
			return
		}
		before, _ := sc.GetStatus(r.Context())
		if p := who(r); !p.Admin {
			g, ok := snap.Group{}, false
			if before != nil {
				g, ok = findGroup(before, parts[0])
			}
			for _, c := range g.Clients {
				if pole, _ := names.client(c); p.Poles[pole] == 0 {
					ok = false
				}
			}
			if !ok || len(g.Clients) == 0 {
				jsonError(w, http.StatusForbidden, "that zone has poles outside your event")
				return
			}
			if !st8.guestMayPlay(p, body.StreamID, srcStream) {
				jsonError(w, http.StatusForbidden, "that source isn't part of your event")
				return
			}
		}
		if err := sc.SetGroupStream(r.Context(), parts[0], body.StreamID); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if before != nil {
			if g, ok := findGroup(before, parts[0]); ok {
				who, name := names.group(g)
				logOps(r, who, fmt.Sprintf("%s: %s → %s", name, names.stream(g.StreamID), names.stream(body.StreamID)))
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// POST /api/mute-all  {"muted": true}. The kill switch: every client, one call each.
	mux.HandleFunc("/api/mute-all", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Muted bool `json:"muted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		st, err := sc.GetStatus(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var failed []string
		p := who(r)
		for _, g := range st.Server.Groups {
			for _, c := range g.Clients {
				if pole, _ := names.client(c); !p.Admin && p.Poles[pole] == 0 {
					continue // a guest's mute-all is their event's poles
				}
				if err := sc.SetVolume(r.Context(), c.ID, c.Config.Volume.Percent, body.Muted); err != nil {
					failed = append(failed, c.ID)
				}
			}
		}
		verb := "unmuted"
		if body.Muted {
			verb = "MUTED"
		}
		logOps(r, "console", "all zones "+verb)
		if len(failed) > 0 {
			http.Error(w, "failed: "+strings.Join(failed, ", "), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", web.Handler())

	httpSrv := &http.Server{Addr: *listen, Handler: st8.guard(mux)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		httpSrv.Shutdown(sctx)
	}()
	log.Printf("console: http://localhost%s", *listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}

// defaultSnapBinary prefers a snapserver next to the executable (the Windows
// deployment), then whatever is on PATH.
func defaultSnapBinary() string {
	exe, err := os.Executable()
	if err == nil {
		local := filepath.Join(filepath.Dir(exe), "snapserver")
		for _, cand := range []string{local, local + ".exe"} {
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
	}
	return "snapserver"
}

// consoleState is what /ws pushes: snapserver status at the top level (the
// console reads state.server) plus the control-channel view of each node.
type consoleState struct {
	*snap.Status
	Nodes         []nodes.State                `json:"nodes"`
	Build         string                       `json:"build"` // changes on every controller start; open consoles reload
	Codes         []registry.AdoptCode         `json:"codes"`
	Sources       []sources.Info               `json:"sources"`  // with live levels; snapserver's "playing" only means bytes arrive
	Speakers      map[string]registry.Speakers `json:"speakers"` // device id -> saved trim and mute for A and B
	Paging        paging.State                 `json:"paging"`
	Latency       latencyState                 `json:"latency"`
	ServerTime    time.Time                    `json:"server_time"` // "ago" is measured against this, not the browser clock
	Plan          planView                     `json:"plan"`
	Events        []eventView                  `json:"events"` // for standby labels on sources; who may do what is /api/me
	DefaultSource string                       `json:"default_source"`
}

type eventView struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Sources []string  `json:"sources"`
	Open    bool      `json:"open"`
	Until   time.Time `json:"until,omitzero"`
	Next    time.Time `json:"next,omitzero"`
}

func eventViews(s *site) []eventView {
	doc, loc, now := s.Doc(), s.Loc(), s.now()
	out := []eventView{}
	for _, e := range doc.Events {
		st := e.At(now, loc)
		out = append(out, eventView{ID: e.ID, Name: e.Name, Sources: e.Sources, Open: st.Open, Until: st.Until, Next: st.Next})
	}
	return out
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func flagSet(name string) (set bool) {
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// regStore adapts the registry to nodes.Store.
type regStore struct{ r *registry.Registry }

func (s regStore) Adoption(ctx context.Context, id string) (string, string, bool, error) {
	n, ok, err := s.r.Node(ctx, id)
	if !ok || err != nil || n.Token == "" {
		return "", "", false, err
	}
	return n.Token, n.Name, true, nil
}

func (s regStore) SaveAdoption(ctx context.Context, id, role, name, hash string) error {
	return s.r.Adopt(ctx, id, role, name, hash)
}

func (s regStore) Forget(ctx context.Context, id string) error { return s.r.Forget(ctx, id) }

func (s regStore) UseCode(ctx context.Context, code string) (bool, bool, error) {
	return s.r.UseCode(ctx, code)
}

type latencyState struct {
	SpeakerBufferMs int  `json:"speaker_buffer_ms"`
	Managed         bool `json:"managed"` // the controller runs snapserver, so it can change the speaker buffer
}
