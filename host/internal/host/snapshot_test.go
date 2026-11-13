package host

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSnapshotLocal is the rotation and the restore of a managed file's snapshots, on this machine's agent: an
// overwrite that changes the file shifts the snapshots, one that changes nothing leaves them, a restore puts
// the newest back and a path without a snapshot is removed.
func TestSnapshotLocal(t *testing.T) {
	if _, _, err := agentProgram(runtime.GOOS, runtime.GOARCH); err != nil {
		t.Skip(err)
	}
	h := &host{address: "127.0.0.1", local: true}
	h.c = &conn{h: h, history: 2}
	ctx := context.Background()
	defer h.c.close()

	path := filepath.Join(t.TempDir(), "f")
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return string(b)
	}
	write := func(s string) {
		if _, err := h.c.writeFile(ctx, path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("v1")
	write("v1") // no change: no rotation
	if _, err := os.Stat(path + ".bak.1"); err == nil {
		t.Fatal("a no-change write made a snapshot")
	}
	write("v2")
	write("v3")
	if got := read(path + ".bak.1"); got != "v2" {
		t.Errorf(".bak.1 = %q", got)
	}
	if got := read(path + ".bak.2"); got != "v1" {
		t.Errorf(".bak.2 = %q", got)
	}
	if ok, err := h.c.restore(ctx, path); err != nil || !ok {
		t.Fatalf("restore = %v, %v", ok, err)
	}
	if got := read(path); got != "v2" {
		t.Errorf("restored = %q", got)
	}
	if got := read(path + ".bak.1"); got != "v1" {
		t.Errorf(".bak.1 after restore = %q", got)
	}
	if ok, _ := h.c.restore(ctx, path); !ok {
		t.Fatal("the second restore found no snapshot")
	}
	if got := read(path); got != "v1" {
		t.Errorf("restored again = %q", got)
	}
	if ok, _ := h.c.restore(ctx, path); ok {
		t.Fatal("a third restore claims a snapshot")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a file without a snapshot stayed")
	}
}
