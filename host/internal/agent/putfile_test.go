package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/horchestra/targets/host/internal/agentpb"
)

func put(t *testing.T, cli agentpb.AgentClient, path string, mode uint32, chunks ...[]byte) bool {
	t.Helper()
	stream, err := cli.PutFile(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentpb.PutFileRequest{Path: path, Mode: mode}); err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if err := stream.Send(&agentpb.PutFileRequest{Chunk: c}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	return res.Changed
}

func TestPutFile(t *testing.T) {
	cli := serve(t)
	path := filepath.Join(t.TempDir(), "sub", "bin")

	if !put(t, cli, path, 0o755, []byte("one "), []byte("two "), []byte("three")) {
		t.Error("first put: changed = false")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "one two three" {
		t.Errorf("content %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode().Perm())
	}

	if put(t, cli, path, 0o755, []byte("one "), []byte("two "), []byte("three")) {
		t.Error("the same file again: changed = true")
	}
	if !put(t, cli, path, 0o700, []byte("one "), []byte("two "), []byte("three")) {
		t.Error("the same bytes, another mode: changed = false")
	}
	if !put(t, cli, path, 0o700, []byte("other")) {
		t.Error("other bytes: changed = false")
	}

	if err := func() error {
		stream, err := cli.PutFile(ctx())
		if err != nil {
			return err
		}
		if err := stream.Send(&agentpb.PutFileRequest{Path: "relative/f", Chunk: []byte("x")}); err != nil {
			return err
		}
		_, err = stream.CloseAndRecv()
		return err
	}(); err == nil {
		t.Error("a relative path was taken")
	}
}
