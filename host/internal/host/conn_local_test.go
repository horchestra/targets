package host

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLocalConn is the agent as a process of this machine, end to end: unpacked from the embedded binary,
// started, spoken to over gRPC. It skips when `make agent` built no agent of this platform (the checkout keeps
// placeholders).
func TestLocalConn(t *testing.T) {
	if _, _, err := agentProgram(runtime.GOOS, runtime.GOARCH); err != nil {
		t.Skip(err)
	}
	h := &host{address: "127.0.0.1", local: true}
	h.c = &conn{h: h}
	ctx := context.Background()
	defer h.c.close()

	out, err := h.c.exec(ctx, []string{"echo", "hello"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("echo = %q", out)
	}
	info, err := h.c.info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Arch != runtime.GOARCH {
		t.Errorf("arch = %q", info.Arch)
	}
	path := filepath.Join(t.TempDir(), "f")
	changed, err := h.c.writeFile(ctx, path, []byte("x"), 0o600)
	if err != nil || !changed {
		t.Fatalf("writeFile = %v, %v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "x" {
		t.Errorf("content = %q", b)
	}
	if _, err := h.c.exec(ctx, []string{"false"}, nil, nil, nil); err == nil {
		t.Error("a failing command is no error")
	}
	facts, err := h.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && (facts.CPUs < 1 || facts.MemoryMB < 1) {
		// nproc and free are a Linux host's; elsewhere the facts they tell are left out
		t.Errorf("facts = %+v", facts)
	}
}
