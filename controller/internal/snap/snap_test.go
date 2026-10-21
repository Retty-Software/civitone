package snap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigSetsBuffer(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.conf")
	os.WriteFile(base, []byte("[stream]\nsource = tcp://x\n# Sync buffer\nbuffer = 1000\nchunk_ms = 50\n"), 0o644)
	s := &Supervisor{Base: base, Runtime: filepath.Join(dir, "run.conf"), BufferMs: func() int { return 850 }}
	if err := s.writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.Runtime)
	if !strings.Contains(string(b), "\nbuffer = 850\n") || strings.Contains(string(b), "1000") || !strings.Contains(string(b), "# Sync buffer") {
		t.Fatalf("runtime config:\n%s", b)
	}

	os.WriteFile(base, []byte("[stream]\nsource = tcp://x\n"), 0o644)
	s.writeConfig()
	b, _ = os.ReadFile(s.Runtime)
	if !strings.Contains(string(b), "[stream]\nbuffer = 850\n") {
		t.Fatalf("missing buffer not added:\n%s", b)
	}
}
