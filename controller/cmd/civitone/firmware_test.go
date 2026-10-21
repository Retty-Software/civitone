package main

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
)

// fakeImage is the start of an ESP32 app image: header byte, then the app
// description at offset 32.
func fakeImage(version string) []byte {
	b := make([]byte, 4096)
	b[0] = 0xE9
	binary.LittleEndian.PutUint32(b[appDescAt:], appDescWord)
	copy(b[appDescAt+16:], version)
	copy(b[appDescAt+48:], "snapclient")
	copy(b[appDescAt+80:], "17:00:00")
	copy(b[appDescAt+96:], "Oct 21 2026")
	return b
}

func TestDescribeReadsAppDescAndRejectsJunk(t *testing.T) {
	img, err := describe(fakeImage("0.4.0"))
	if err != nil || img.Version != "0.4.0" || img.Project != "snapclient" || img.Built != "Oct 21 2026 17:00:00" || len(img.SHA256) != 64 {
		t.Fatalf("describe = %+v, %v", img, err)
	}
	if _, err := describe([]byte("hello, not firmware")); err == nil {
		t.Fatal("junk accepted")
	}
	big := fakeImage("x")
	big = append(big, make([]byte, maxImage)...)
	if _, err := describe(big); err == nil {
		t.Fatal("image larger than the OTA slot accepted")
	}
}

func TestNodeDownloadNeedsItsDeviceToken(t *testing.T) {
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	reg.Adopt(context.Background(), "civ-1a2b", "node", "Bench", nodes.HashToken("good-token"))
	fw := firmwareStore{dir: filepath.Join(dir, "firmware")}
	img, err := fw.add(fakeImage("0.4.0"))
	if err != nil {
		t.Fatal(err)
	}
	h := fw.serveNode(reg)
	get := func(id, tok string) int {
		req := httptest.NewRequest(http.MethodGet, "/firmware/"+img.SHA256+".bin", nil)
		if id != "" {
			req.Header.Set("X-Civitone-Device", id)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	for _, c := range []struct {
		id, tok string
		want    int
	}{
		{"civ-1a2b", "good-token", http.StatusOK},
		{"civ-1a2b", "wrong", http.StatusUnauthorized},
		{"civ-1a2b", "", http.StatusUnauthorized},
		{"", "good-token", http.StatusUnauthorized},
		{"civ-0000", "good-token", http.StatusUnauthorized},
	} {
		if got := get(c.id, c.tok); got != c.want {
			t.Errorf("id=%q tok=%q: %d, want %d", c.id, c.tok, got, c.want)
		}
	}
}
