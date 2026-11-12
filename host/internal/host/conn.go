package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/horchestra/targets/host/internal/agentpb"
)

// The agent the driver carries for the hosts' platforms: `make agent` builds them. A host runs the one of its
// architecture; a local host runs its own, so the driver itself is a place the agent of this machine comes from.
//
//go:embed all:agentbin
var agentBin embed.FS

// conn is the driver side of one host's agent: the agent's binary brought to the host and started there (over
// SSH, or as a process of this machine for the local host), its loopback port reached through the connection,
// every call carrying the token it was started with. The agent lives as long as the session; a connection found
// dead is dialed again and the agent started anew — a step that was cut short is repeated by the saga.
type conn struct {
	h    *host    // the host's settings; local — this machine
	link *sshLink // nil for the local host

	mu   sync.Mutex
	sess *session
}

// session is a running agent: its gRPC client and what ends it.
type session struct {
	cli  agentpb.AgentClient
	gc   *grpc.ClientConn
	info *agentpb.InfoResponse
	stop func() // close the stdin, wait for the exit
}

// agentProgram is the embedded agent for GOOS/GOARCH and its checksum.
func agentProgram(goos, goarch string) ([]byte, string, error) {
	name := fmt.Sprintf("agentbin/pack-host-agent-%s-%s", goos, goarch)
	b, err := agentBin.ReadFile(name)
	if err != nil || len(b) == 0 {
		return nil, "", fmt.Errorf("this target has no agent for %s/%s (make agent)", goos, goarch)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// token is a fresh token for an agent run.
func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tokenCred carries the token as the metadata pack-agent-token on every call.
type tokenCred string

func (t tokenCred) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"pack-agent-token": string(t)}, nil
}

func (tokenCred) RequireTransportSecurity() bool { return false }

// client is the agent's client, the agent started when first needed; a session whose connection died is dropped
// and made again.
func (c *conn) client(ctx context.Context) (*session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		return c.sess, nil
	}
	s, err := c.start(ctx)
	if err != nil {
		return nil, err
	}
	c.sess = s
	return c.sess, nil
}

// reset drops the session (the agent died, or the connection did); the next client starts anew.
func (c *conn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		c.sess.stop()
		c.sess = nil
	}
}

// start brings the agent's binary to the host and runs it: over SSH (the sha256 of what is there decides the
// upload, sudo -n for a user that is not root), or as a process of this machine. The agent says its loopback
// address on stdout's first line; the gRPC calls go through the connection.
func (c *conn) start(ctx context.Context) (*session, error) {
	if c.h.local {
		return c.startLocal(ctx)
	}
	return c.startRemote(ctx)
}

// grpcClient dials the agent at addr with the token; dial is how one call's connection is made.
func grpcClient(addr string, tok string, dial func(ctx context.Context) (net.Conn, error)) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCred(tok)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dial(ctx) }))
}

// startRemote runs the agent on the host over SSH.
func (c *conn) startRemote(ctx context.Context) (*session, error) {
	// the agent's platform is the host's: asked cheaply before the upload decides
	cl, _, err := c.link.get()
	if err != nil {
		return nil, err
	}
	arch, err := remoteArch(ctx, cl)
	if err != nil {
		return nil, fmt.Errorf("%s%s", c.h.prefix(), err)
	}
	program, sum, err := agentProgram("linux", arch)
	if err != nil {
		return nil, err
	}
	path := ".pack/pack-host-agent-" + sum[:16]
	if err := c.upload(ctx, cl, path, program); err != nil {
		return nil, err
	}
	tok := token()
	cmd := path + " -token " + tok
	if c.h.sudo() {
		cmd = "sudo -n " + cmd
	}
	sess, err := cl.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	var stderr strings.Builder
	sess.Stderr = &stderr
	if err := sess.Start(cmd); err != nil {
		_ = sess.Close()
		return nil, err
	}
	addr, err := readListening(stdout)
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("%sthe agent did not start: %w%s", c.h.prefix(), err, stderr.String())
	}
	gc, err := grpcClient(addr, tok, func(ctx context.Context) (net.Conn, error) {
		cl, _, err := c.link.get()
		if err != nil {
			return nil, err
		}
		return cl.Dial("tcp", addr)
	})
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = sess.Wait(); close(done) }()
	s := &session{cli: agentpb.NewAgentClient(gc), gc: gc, stop: func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		_ = sess.Close()
		_ = gc.Close()
	}}
	info, err := s.cli.Info(ctx, &agentpb.InfoRequest{})
	if err != nil {
		s.stop()
		return nil, fmt.Errorf("%sthe agent does not answer: %w", c.h.prefix(), err)
	}
	s.info = info
	return s, nil
}

// startLocal runs the agent as a process of this machine: the embedded binary of this platform, unpacked into
// the cache directory by its checksum.
func (c *conn) startLocal(ctx context.Context) (*session, error) {
	program, sum, err := agentProgram(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	path := filepath.Join(cache, "pack", "agent-"+sum[:16])
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-*")
		if err != nil {
			return nil, err
		}
		if _, err := tmp.Write(program); err != nil {
			_ = tmp.Close()
			return nil, err
		}
		_ = tmp.Chmod(0o755)
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return nil, err
		}
	}
	tok := token()
	argv := []string{path, "-token", tok}
	if c.h.sudo() {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("the agent: %w", err)
	}
	addr, err := readListening(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("the agent did not start: %w%s", err, stderr.String())
	}
	gc, err := grpcClient(addr, tok, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	s := &session{cli: agentpb.NewAgentClient(gc), gc: gc, stop: func() {
		_ = stdin.Close()
		_ = gc.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	}}
	info, err := s.cli.Info(ctx, &agentpb.InfoRequest{})
	if err != nil {
		s.stop()
		return nil, fmt.Errorf("the agent does not answer: %w", err)
	}
	s.info = info
	return s, nil
}

// readListening is the agent's first stdout line, "listening <addr>".
func readListening(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return "", err
	}
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "listening ")
	if !ok {
		return "", fmt.Errorf("said %q", strings.TrimSpace(line))
	}
	return addr, nil
}

// remoteArch is the host's GOARCH, asked of uname.
func remoteArch(ctx context.Context, cl *ssh.Client) (string, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()
	out, err := sess.Output("uname -m")
	if err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}
	switch m := strings.TrimSpace(string(out)); m {
	case "x86_64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("the host's architecture %q has no agent", m)
	}
}

// upload brings the program to the path if what is there differs — streamed to install over the session's stdin,
// no SFTP needed.
func (c *conn) upload(ctx context.Context, cl *ssh.Client, path string, program []byte) error {
	sum := sha256.Sum256(program)
	sess, err := cl.NewSession()
	if err != nil {
		return err
	}
	out, _ := sess.Output("sha256sum " + path + " 2>/dev/null")
	_ = sess.Close()
	if got, _, _ := strings.Cut(strings.TrimSpace(string(out)), " "); got == hex.EncodeToString(sum[:]) {
		return nil // already there: same content
	}
	sess, err = cl.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	var stderr strings.Builder
	sess.Stderr = &stderr
	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	if err := sess.Start("install -D -m 0755 /dev/stdin " + path); err != nil {
		return err
	}
	if _, err := stdin.Write(program); err != nil {
		return fmt.Errorf("%supload the agent: %w", c.h.prefix(), err)
	}
	if err := stdin.Close(); err != nil {
		return fmt.Errorf("%supload the agent: %w", c.h.prefix(), err)
	}
	if err := sess.Wait(); err != nil {
		return fmt.Errorf("%supload the agent: %w: %s", c.h.prefix(), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// info is the host's OS and architecture, asked once a session.
func (c *conn) info(ctx context.Context) (*agentpb.InfoResponse, error) {
	s, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	return s.info, nil
}

// exec runs argv on the host through the agent: the output's lines to events, a non-zero exit an error with the
// output's tail. A call the connection dropped resets the session: the next one starts the agent anew, and the
// saga repeats the step.
func (c *conn) exec(ctx context.Context, argv []string, env []string, stdin []byte, events func(string)) ([]byte, error) {
	s, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := s.cli.Exec(ctx, &agentpb.ExecRequest{Argv: argv, Env: env, Stdin: stdin})
	if err != nil {
		c.reset()
		return nil, fmt.Errorf("%s%s: %w", c.h.prefix(), argv[0], err)
	}
	var out strings.Builder
	var tail []string
	code := 0
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.reset()
			return nil, fmt.Errorf("%s%s: %w", c.h.prefix(), argv[0], err)
		}
		if ev.Done {
			code = int(ev.Exit)
			break
		}
		out.WriteString(ev.Line)
		out.WriteByte('\n')
		if events != nil {
			events(c.h.prefix() + ev.Line)
		}
		if tail = append(tail, ev.Line); len(tail) > 20 {
			tail = tail[1:]
		}
	}
	if code != 0 {
		return nil, fmt.Errorf("%s%s: exit %d: %s", c.h.prefix(), argv[0], code, strings.Join(tail, "; "))
	}
	return []byte(out.String()), nil
}

// lines is exec's output as non-empty lines.
func (c *conn) lines(ctx context.Context, argv ...string) ([]string, error) {
	out, err := c.exec(ctx, argv, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var ls []string
	for l := range strings.Lines(string(out)) {
		if l = strings.TrimSpace(l); len(l) > 0 {
			ls = append(ls, l)
		}
	}
	return ls, nil
}

// writeFile delivers the content to the path on the host and says whether that changed it.
func (c *conn) writeFile(ctx context.Context, path string, content []byte, mode uint32) (bool, error) {
	s, err := c.client(ctx)
	if err != nil {
		return false, err
	}
	res, err := s.cli.WriteFile(ctx, &agentpb.WriteFileRequest{Path: path, Content: content, Mode: mode})
	if err != nil {
		return false, fmt.Errorf("%s%s: %w", c.h.prefix(), path, err)
	}
	return res.Changed, nil
}

// putFile streams a file of this machine — a pack's Source file read from the disk as it goes — to the path on
// the host and says whether that changed it.
func (c *conn) putFile(ctx context.Context, local, path string, mode uint32) (bool, error) {
	f, err := os.Open(local)
	if err != nil {
		return false, fmt.Errorf("%s%s: %w", c.h.prefix(), local, err)
	}
	defer func() { _ = f.Close() }()
	s, err := c.client(ctx)
	if err != nil {
		return false, err
	}
	stream, err := s.cli.PutFile(ctx)
	if err != nil {
		return false, fmt.Errorf("%s%s: %w", c.h.prefix(), path, err)
	}
	req := &agentpb.PutFileRequest{Path: path, Mode: mode}
	buf := make([]byte, 1<<20)
	for {
		n, rerr := f.Read(buf)
		req.Chunk = buf[:n]
		if n > 0 || req.Path != "" {
			if err := stream.Send(req); err != nil {
				return false, fmt.Errorf("%s%s: %w", c.h.prefix(), path, err)
			}
			req = &agentpb.PutFileRequest{}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return false, fmt.Errorf("%s%s: %w", c.h.prefix(), local, rerr)
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return false, fmt.Errorf("%s%s: %w", c.h.prefix(), path, err)
	}
	return res.Changed, nil
}

// removeFile deletes the path on the host.
func (c *conn) removeFile(ctx context.Context, path string) error {
	s, err := c.client(ctx)
	if err != nil {
		return err
	}
	if _, err := s.cli.RemoveFile(ctx, &agentpb.RemoveFileRequest{Path: path}); err != nil {
		return fmt.Errorf("%s%s: %w", c.h.prefix(), path, err)
	}
	return nil
}

// fetch downloads the URL to the path on the host — the agent's own HTTP client — and says whether that changed
// it.
func (c *conn) fetch(ctx context.Context, url, path string, mode uint32) (bool, error) {
	s, err := c.client(ctx)
	if err != nil {
		return false, err
	}
	res, err := s.cli.Fetch(ctx, &agentpb.FetchRequest{Url: url, Path: path, Mode: mode})
	if err != nil {
		return false, fmt.Errorf("%s%s: %w", c.h.prefix(), url, err)
	}
	return res.Changed, nil
}

// proxyStart serves the registry's artifacts on the host's loopback; proxyStop takes it away.
func (c *conn) proxyStart(ctx context.Context, m *mirror) error {
	s, err := c.client(ctx)
	if err != nil {
		return err
	}
	port, err := parsePort(m.port)
	if err != nil {
		return err
	}
	_, err = s.cli.ProxyStart(ctx, &agentpb.ProxyStartRequest{
		Registry: m.registry, PlainHttp: m.plainHTTP, Username: m.user, Password: m.password, Port: port})
	if err != nil {
		return fmt.Errorf("%soci-proxy: %w", c.h.prefix(), err)
	}
	return nil
}

func (c *conn) proxyStop(ctx context.Context) {
	s, err := c.client(ctx)
	if err != nil {
		return
	}
	_, _ = s.cli.ProxyStop(ctx, &agentpb.ProxyStopRequest{})
}

// close ends the session and the connection.
func (c *conn) close() {
	c.mu.Lock()
	if c.sess != nil {
		c.sess.stop()
		c.sess = nil
	}
	c.mu.Unlock()
	if c.link != nil {
		c.link.close()
	}
}
