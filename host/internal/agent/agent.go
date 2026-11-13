// Package agent is the server side of pack.agent.v1: the primitives a host gives the driver — commands (a package
// manager, systemctl, sysctl, modprobe, the docker CLI), files delivered and removed, the host's OS, and the files
// of OCI artifacts over HTTP (the proxy, for package repositories kept in a registry).
package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/horchestra/targets/host/internal/agentpb"
)

// Server is the agent: one gRPC service on the host's loopback.
type Server struct {
	agentpb.UnimplementedAgentServer

	mu     sync.Mutex
	proxy  *artifactProxy
	closed bool
}

// NewServer is the agent. token is checked by the interceptors, not here.
func NewServer(_ string) (*Server, error) { return &Server{}, nil }

// AuthUnary refuses a call without the token, as metadata pack-agent-token.
func AuthUnary(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkToken(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// AuthStream is AuthUnary for a streaming call.
func AuthStream(token string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkToken(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkToken(ctx context.Context, token string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "no metadata")
	}
	for _, v := range md.Get("pack-agent-token") {
		if v == token {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "a call carries no pack-agent-token, or a wrong one")
}

// Info is the host's OS (of /etc/os-release), running kernel and the agent's architecture.
func (s *Server) Info(_ context.Context, _ *agentpb.InfoRequest) (*agentpb.InfoResponse, error) {
	osr := readOSRelease()
	kernel, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "uname -r: %v", err)
	}
	return &agentpb.InfoResponse{
		Id:       osr["ID"],
		Version:  major(osr["VERSION_ID"]),
		Codename: osr["VERSION_CODENAME"],
		Family:   family(osr),
		Arch:     runtime.GOARCH,
		Kernel:   strings.TrimSpace(string(kernel)),
	}, nil
}

// readOSRelease is /etc/os-release as a map.
func readOSRelease() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return out
	}
	for l := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// major is the version's first number: "9.1" is "9".
func major(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}

// family is the OS family packages are chosen by: debian or rhel.
func family(osr map[string]string) string {
	for _, id := range append(strings.Fields(osr["ID_LIKE"]), osr["ID"]) {
		switch id {
		case "debian":
			return "debian"
		case "rhel", "fedora", "centos":
			return "rhel"
		}
	}
	return ""
}

// Exec runs the request's argv — never a shell — streaming the output's lines as they come, the last event the
// exit code.
func (s *Server) Exec(req *agentpb.ExecRequest, stream agentpb.Agent_ExecServer) error {
	code, err := s.Run(stream.Context(), req, func(line string) error {
		return stream.Send(&agentpb.ExecEvent{Line: line})
	})
	if err != nil {
		return err
	}
	return stream.Send(&agentpb.ExecEvent{Done: true, Exit: int32(code)})
}

// Run executes a command directly and emits its output lines as they arrive.
func (s *Server) Run(ctx context.Context, req *agentpb.ExecRequest, emit func(string) error) (int, error) {
	if len(req.Argv) == 0 {
		return 0, status.Error(codes.InvalidArgument, "exec: no argv")
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Env = append(os.Environ(), req.Env...)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	scanDone := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			if err := emit(sc.Text()); err != nil {
				scanDone <- err
				return
			}
		}
		scanDone <- sc.Err()
	}()
	runErr := cmd.Run()
	_ = pw.Close()
	scanErr := <-scanDone
	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else {
			return 0, status.Errorf(codes.Internal, "exec %s: %v", req.Argv[0], runErr)
		}
	}
	if scanErr != nil {
		return 0, status.Errorf(codes.Internal, "exec %s: %v", req.Argv[0], scanErr)
	}
	return code, nil
}

// WriteFile writes the content to the path, its directory made first, atomically (a temporary file renamed), and
// says whether anything changed.
func (s *Server) WriteFile(_ context.Context, req *agentpb.WriteFileRequest) (*agentpb.WriteFileResponse, error) {
	if len(req.Path) == 0 || req.Path[0] != '/' {
		return nil, status.Error(codes.InvalidArgument, "write: an absolute path")
	}
	mode := fs.FileMode(req.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	if b, err := os.ReadFile(req.Path); err == nil {
		if st, _ := os.Stat(req.Path); st != nil && st.Mode().Perm() == mode && string(b) == string(req.Content) {
			return &agentpb.WriteFileResponse{}, nil
		}
	}
	dir := filepath.Dir(req.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", req.Path, err)
	}
	tmp, err := os.CreateTemp(dir, ".pack-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", req.Path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(req.Content); err != nil {
		_ = tmp.Close()
		return nil, status.Errorf(codes.Internal, "write %s: %v", req.Path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return nil, status.Errorf(codes.Internal, "write %s: %v", req.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", req.Path, err)
	}
	if err := os.Rename(tmp.Name(), req.Path); err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", req.Path, err)
	}
	return &agentpb.WriteFileResponse{Changed: true}, nil
}

// RemoveFile deletes the path; one that is not there is no error.
func (s *Server) RemoveFile(_ context.Context, req *agentpb.RemoveFileRequest) (*agentpb.RemoveFileResponse, error) {
	if len(req.Path) == 0 || req.Path[0] != '/' {
		return nil, status.Error(codes.InvalidArgument, "remove: an absolute path")
	}
	err := os.Remove(req.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return &agentpb.RemoveFileResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", req.Path, err)
	}
	return &agentpb.RemoveFileResponse{Removed: true}, nil
}

// ProxyStart serves the files of the registry's artifacts on the loopback, replacing the proxy that runs.
func (s *Server) ProxyStart(_ context.Context, req *agentpb.ProxyStartRequest) (*agentpb.ProxyStartResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxy != nil {
		s.proxy.stop()
		s.proxy = nil
	}
	p, err := startProxy(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "proxy: %v", err)
	}
	s.proxy = p
	return &agentpb.ProxyStartResponse{}, nil
}

// ProxyStop takes the proxy away; none running is no error.
func (s *Server) ProxyStop(_ context.Context, _ *agentpb.ProxyStopRequest) (*agentpb.ProxyStopResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxy != nil {
		s.proxy.stop()
		s.proxy = nil
	}
	return &agentpb.ProxyStopResponse{}, nil
}
