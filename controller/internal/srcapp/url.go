package srcapp

import (
	"fmt"
	"strings"
)

// EndpointURL turns what a person types as the server address into the
// WebSocket URL for path, the same way the node setup page does:
//
//	civitone.example.com        -> wss://civitone.example.com/source (TLS)
//	192.168.1.10:8080, pi:8080  -> ws://192.168.1.10:8080/source (LAN)
//	ws://, wss://, http://, https:// are taken as given.
func EndpointURL(server, path string) (string, error) {
	s := strings.TrimSpace(server)
	if s == "" {
		return "", fmt.Errorf("no server address")
	}
	scheme, rest := "", s
	switch {
	case strings.HasPrefix(s, "wss://"):
		scheme, rest = "wss", s[6:]
	case strings.HasPrefix(s, "ws://"):
		scheme, rest = "ws", s[5:]
	case strings.HasPrefix(s, "https://"):
		scheme, rest = "wss", s[8:]
	case strings.HasPrefix(s, "http://"):
		scheme, rest = "ws", s[7:]
	case strings.Contains(s, ":"):
		scheme = "ws"
	default:
		scheme = "wss"
	}
	host, _, _ := strings.Cut(rest, "/")
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("no host in %q", server)
	}
	return scheme + "://" + host + path, nil
}
