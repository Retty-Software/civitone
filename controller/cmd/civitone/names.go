package main

import (
	"strings"

	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
)

// namer turns snapserver ids into what an operator calls things, for the
// event log: a client or group into its node (device id, so the entry shows
// on Node detail, and name), a stream into its source's name.
type namer struct {
	nm  *nodes.Manager
	src *sources.Manager
}

// client: the node playing as this snapclient, matched by MAC (the host's,
// or the client id, which is the MAC unless snapclient ran with --hostID).
func (n namer) client(c snap.Client) (who, name string) {
	for _, st := range n.nm.States() {
		if st.Adopted && st.MAC != "" && (strings.EqualFold(st.MAC, c.Host.MAC) || strings.EqualFold(st.MAC, c.ID)) {
			if st.Name != "" {
				return st.DeviceID, st.Name
			}
			return st.DeviceID, st.DeviceID
		}
	}
	for _, s := range []string{c.Config.Name, c.Host.Name, c.ID} {
		if s != "" {
			return "console", s
		}
	}
	return "console", "a speaker"
}

// group: its one speaker, or every speaker's name for a shared group.
func (n namer) group(g snap.Group) (who, name string) {
	who = "console"
	var names []string
	for _, c := range g.Clients {
		w, nm := n.client(c)
		if len(g.Clients) == 1 {
			who = w
		}
		names = append(names, nm)
	}
	if len(names) == 0 {
		return who, "an empty group"
	}
	return who, strings.Join(names, ", ")
}

func (n namer) stream(id string) string {
	switch id {
	case "", "None":
		return "silence"
	}
	for _, s := range n.src.Infos() {
		if s.Stream == id {
			return s.Name
		}
	}
	return id
}

func findClient(st *snap.Status, id string) (snap.Client, bool) {
	for _, g := range st.Server.Groups {
		for _, c := range g.Clients {
			if c.ID == id {
				return c, true
			}
		}
	}
	return snap.Client{}, false
}

func findGroup(st *snap.Status, id string) (snap.Group, bool) {
	for _, g := range st.Server.Groups {
		if g.ID == id {
			return g, true
		}
	}
	return snap.Group{}, false
}
