package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/horchestra/targets/host/internal/agentpb"
)

// serve runs the agent on a loopback port and returns its client.
func serve(t *testing.T) agentpb.AgentClient {
	t.Helper()
	srv, err := NewServer("t")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(AuthUnary("tok")), grpc.StreamInterceptor(AuthStream("tok")))
	agentpb.RegisterAgentServer(s, srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	gc, err := grpc.NewClient("passthrough:///"+ln.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCred("tok")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gc.Close() })
	return agentpb.NewAgentClient(gc)
}

type tokenCred string

func (t tokenCred) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"pack-agent-token": string(t)}, nil
}

func (tokenCred) RequireTransportSecurity() bool { return false }

func ctx() context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("pack-agent-token", "tok"))
}

func TestInfo(t *testing.T) {
	cli := serve(t)
	info, err := cli.Info(ctx(), &agentpb.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Arch != runtime.GOARCH {
		t.Errorf("arch = %q, want %q", info.Arch, runtime.GOARCH)
	}
	if _, err := os.Stat("/etc/os-release"); err == nil && len(info.Id) == 0 {
		t.Error("os-release is there, but the agent said no ID")
	}
}

func TestExec(t *testing.T) {
	cli := serve(t)
	stream, err := cli.Exec(ctx(), &agentpb.ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; cat; exit 3"}, Stdin: []byte("in\n")})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	code := -1
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			code = int(ev.Exit)
			break
		}
		lines = append(lines, ev.Line)
	}
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	want := map[string]bool{"out": true, "err": true, "in": true}
	for _, l := range lines {
		delete(want, l)
	}
	if len(want) > 0 {
		t.Errorf("missing output lines: %v (got %v)", want, lines)
	}
}

func TestWriteAndRemoveFile(t *testing.T) {
	cli := serve(t)
	path := filepath.Join(t.TempDir(), "sub", "f.txt")
	res, err := cli.WriteFile(ctx(), &agentpb.WriteFileRequest{Path: path, Content: []byte("one"), Mode: 0o640})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("first write: changed = false")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "one" {
		t.Errorf("content = %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", st.Mode().Perm())
	}
	res, err = cli.WriteFile(ctx(), &agentpb.WriteFileRequest{Path: path, Content: []byte("one"), Mode: 0o640})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("same write: changed = true")
	}
	rm, err := cli.RemoveFile(ctx(), &agentpb.RemoveFileRequest{Path: path})
	if err != nil || !rm.Removed {
		t.Errorf("remove = %+v, %v", rm, err)
	}
	rm, err = cli.RemoveFile(ctx(), &agentpb.RemoveFileRequest{Path: path})
	if err != nil || rm.Removed {
		t.Errorf("remove of a missing file = %+v, %v", rm, err)
	}
	if _, err := cli.WriteFile(ctx(), &agentpb.WriteFileRequest{Path: "relative", Content: []byte("x")}); err == nil {
		t.Error("a relative path is accepted")
	}
}

func TestAuth(t *testing.T) {
	serve(t) // the server runs; the client below carries no token
	srv, _ := NewServer("t")
	s := grpc.NewServer(grpc.UnaryInterceptor(AuthUnary("tok")))
	agentpb.RegisterAgentServer(s, srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	gc, err := grpc.NewClient("passthrough:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gc.Close() })
	cli := agentpb.NewAgentClient(gc)
	if _, err := cli.Info(context.Background(), &agentpb.InfoRequest{}); err == nil {
		t.Error("a call without the token is served")
	}
}
