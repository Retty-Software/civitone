package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
)

// Firmware images for speaker nodes, kept beside the database as
// <sha256>.bin. The console uploads them; a node told to update_firmware
// downloads one from /firmware/ with its device token. Nothing else on the
// LAN or the internet can push firmware to a node.
const (
	maxImage    = 1984 * 1024 // the OTA slot (firmware/snapclient/partitions.csv)
	appDescAt   = 32          // esp_app_desc_t after the image and first segment headers
	appDescWord = 0xABCD5432
)

var shaName = regexp.MustCompile(`^[0-9a-f]{64}$`)

type firmwareStore struct{ dir string }

type firmwareImage struct {
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	Version string    `json:"version"` // the image's own (PROJECT_VER)
	Project string    `json:"project"`
	Built   string    `json:"built"`
	Added   time.Time `json:"added"`
}

// describe checks that b is an ESP32 app image and reads its app description.
func describe(b []byte) (firmwareImage, error) {
	var img firmwareImage
	if len(b) < appDescAt+128 || b[0] != 0xE9 {
		return img, errors.New("not an ESP32 firmware image (.bin from the build, not the .elf)")
	}
	if len(b) > maxImage {
		return img, fmt.Errorf("image is %d KB; the OTA slot holds %d KB", len(b)/1024, maxImage/1024)
	}
	d := b[appDescAt:]
	if binary.LittleEndian.Uint32(d) != appDescWord {
		return img, errors.New("image has no app description; is it an app image?")
	}
	str := func(off, n int) string { return string(bytes.TrimRight(d[off:off+n], "\x00")) }
	img.Version, img.Project = str(16, 32), str(48, 32)
	img.Built = strings.TrimSpace(str(96, 16) + " " + str(80, 16))
	sum := sha256.Sum256(b)
	img.SHA256, img.Size = hex.EncodeToString(sum[:]), int64(len(b))
	return img, nil
}

func (f firmwareStore) path(sha string) string { return filepath.Join(f.dir, sha+".bin") }

func (f firmwareStore) add(b []byte) (firmwareImage, error) {
	img, err := describe(b)
	if err != nil {
		return img, err
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return img, err
	}
	tmp := f.path(img.SHA256) + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return img, err
	}
	if err := os.Rename(tmp, f.path(img.SHA256)); err != nil {
		return img, err
	}
	img.Added = time.Now()
	return img, nil
}

// list: newest first.
func (f firmwareStore) list() []firmwareImage {
	out := []firmwareImage{}
	ents, _ := os.ReadDir(f.dir)
	for _, e := range ents {
		sha, ok := strings.CutSuffix(e.Name(), ".bin")
		if !ok || !shaName.MatchString(sha) {
			continue
		}
		b, err := os.ReadFile(f.path(sha))
		if err != nil {
			continue
		}
		img, err := describe(b)
		if err != nil || img.SHA256 != sha {
			continue
		}
		if info, err := e.Info(); err == nil {
			img.Added = info.ModTime()
		}
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.After(out[j].Added) })
	return out
}

func (f firmwareStore) get(sha string) (firmwareImage, bool) {
	if !shaName.MatchString(sha) {
		return firmwareImage{}, false
	}
	b, err := os.ReadFile(f.path(sha))
	if err != nil {
		return firmwareImage{}, false
	}
	img, err := describe(b)
	return img, err == nil && img.SHA256 == sha
}

// serveNode is GET /firmware/<sha256>.bin for nodes, outside the console's
// login: the node names itself and proves it with its device token.
func (f firmwareStore) serveNode(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/firmware/"), ".bin")
		if r.Method != http.MethodGet || !ok || !shaName.MatchString(sha) {
			http.NotFound(w, r)
			return
		}
		id := r.Header.Get("X-Civitone-Device")
		tok, hasTok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		n, found, err := reg.Node(r.Context(), id)
		if !hasTok || tok == "" || err != nil || !found || n.Token == "" ||
			subtle.ConstantTimeCompare([]byte(n.Token), []byte(nodes.HashToken(tok))) != 1 {
			http.Error(w, "device token required", http.StatusUnauthorized)
			return
		}
		file, err := os.Open(f.path(sha))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, sha+".bin", time.Time{}, file)
	}
}

// serveConsole is /api/firmware: GET lists, POST uploads (the raw .bin as the
// body), DELETE /api/firmware/<sha256> removes one.
func (f firmwareStore) serveConsole(logf func(level, text string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.list())
		case http.MethodPost:
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImage+1))
			if err != nil {
				http.Error(w, "image too large or upload broke off", http.StatusBadRequest)
				return
			}
			img, err := f.add(b)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			logf("ops", fmt.Sprintf("firmware %s uploaded (%s, %d KB)", img.Version, img.SHA256[:12], img.Size/1024))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(img)
		case http.MethodDelete:
			sha := strings.TrimPrefix(r.URL.Path, "/api/firmware/")
			if !shaName.MatchString(sha) {
				http.NotFound(w, r)
				return
			}
			if err := os.Remove(f.path(sha)); err != nil {
				http.NotFound(w, r)
				return
			}
			logf("ops", "firmware "+sha[:12]+" removed")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "GET, POST or DELETE", http.StatusMethodNotAllowed)
		}
	}
}
