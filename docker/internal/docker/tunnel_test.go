package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

// fakeHost is an sshd that forwards direct-streamlocal channels, as OpenSSH does for `ssh -L sock:sock`, and a
// "Docker socket" behind it: it reads to the end of what it is sent and answers PONG:<that>.
type fakeHost struct {
	addr       string // 127.0.0.1:port
	dockerSock string
	identity   string // the client's private key file
	known      string // a known_hosts that has the host's key
	hostKey    ssh.PublicKey

	mu    sync.Mutex
	conns []*ssh.ServerConn
	ln    net.Listener
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	// a unix socket's path is short on some systems (104 bytes on macOS): not the test's long directory
	sd, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sd) })
	h := &fakeHost{dockerSock: filepath.Join(sd, "docker.sock")}

	// the "engine"
	dl, err := net.Listen("unix", h.dockerSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dl.Close() })
	go func() {
		for {
			c, err := dl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				_, _ = c.Write([]byte("PONG:" + string(b)))
			}()
		}
	}()

	// keys
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	h.hostKey = hostSigner.PublicKey()
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	h.identity = filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(h.identity, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	clientSSHPub, _ := ssh.NewPublicKey(clientPub)

	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) == string(clientSSHPub.Marshal()) {
			return nil, nil
		}
		return nil, io.ErrUnexpectedEOF
	}}
	cfg.AddHostKey(hostSigner)

	h.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.ln.Close() })
	h.addr = h.ln.Addr().String()
	h.known = filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(h.addr)}, h.hostKey)
	if err := os.WriteFile(h.known, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			c, err := h.ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				h.mu.Lock()
				h.conns = append(h.conns, sc)
				h.mu.Unlock()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "direct-streamlocal@openssh.com" {
						_ = nc.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var req struct {
						SocketPath string
						Reserved0  string
						Reserved1  uint32
					}
					if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
						_ = nc.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					up, err := net.Dial("unix", req.SocketPath)
					if err != nil {
						_ = nc.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, chReqs, _ := nc.Accept()
					go ssh.DiscardRequests(chReqs)
					go func() {
						_, _ = io.Copy(up, ch)
						_ = up.(*net.UnixConn).CloseWrite()
					}()
					go func() {
						_, _ = io.Copy(ch, up)
						_ = ch.CloseWrite()
					}()
				}
			}()
		}
	}()
	return h
}

// dropAll closes every connection the host has: the network went away.
func (h *fakeHost) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		_ = c.Close()
	}
	h.conns = nil
}

func (h *fakeHost) target() sshTarget {
	return sshTarget{dest: "ops@" + h.addr, socket: h.dockerSock, identity: h.identity, knownHosts: h.known}
}

// roundTrip sends s through the tunnel's socket, ends its input and reads the answer.
func roundTrip(t *testing.T, sock, s string) string {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.UnixConn).CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The docker CLI talks to a socket of the provider's; what is written to it, and its end, reach the host's socket,
// and the answer comes back.
func TestTunnelForwards(t *testing.T) {
	h := newFakeHost(t)
	tn, err := openTunnel(h.target())
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, tn.sock, "ping"); got != "PONG:ping" {
		t.Errorf("answer %q", got)
	}
	dir := tn.dir
	tn.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the tunnel's directory is left: %v", err)
	}
}

// A connection that was dropped is made again by the next request, not found dead by it.
func TestTunnelReconnects(t *testing.T) {
	h := newFakeHost(t)
	tn, err := openTunnel(h.target())
	if err != nil {
		t.Fatal(err)
	}
	defer tn.close()
	if got := roundTrip(t, tn.sock, "one"); got != "PONG:one" {
		t.Fatalf("answer %q", got)
	}
	_, dead, _ := tn.link.get()
	h.dropAll()
	select {
	case <-dead:
	case <-time.After(5 * time.Second):
		t.Fatal("the dropped connection was not noticed")
	}
	if got := roundTrip(t, tn.sock, "two"); got != "PONG:two" {
		t.Errorf("after the drop: %q", got)
	}
}

// A host that is not in known_hosts, or whose key is not the one there, is refused; insecure says it is not checked.
func TestTunnelHostKey(t *testing.T) {
	h := newFakeHost(t)
	tgt := h.target()

	empty := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tgt.knownHosts = empty
	if _, err := openTunnel(tgt); err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Errorf("an unknown host: %v", err)
	}

	other, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _ := ssh.NewPublicKey(other)
	changed := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(changed, []byte(knownhosts.Line([]string{knownhosts.Normalize(h.addr)}, otherPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt.knownHosts = changed
	if _, err := openTunnel(tgt); err == nil {
		t.Error("a host with another key than known_hosts has was accepted")
	}

	tgt.insecure = true
	tn, err := openTunnel(tgt)
	if err != nil {
		t.Fatalf("insecure: %v", err)
	}
	tn.close()
}

func TestSplitDest(t *testing.T) {
	for dest, want := range map[string][3]string{
		"ops@10.0.0.11":     {"ops", "10.0.0.11", "22"},
		"ops@node-a:2222":   {"ops", "node-a", "2222"},
		"root@[fe80::1]:22": {"root", "fe80::1", "22"},
	} {
		u, h, p, err := splitDest(dest)
		if err != nil || u != want[0] || h != want[1] || p != want[2] {
			t.Errorf("%s: %q %q %q %v", dest, u, h, p, err)
		}
	}
	if _, _, _, err := splitDest("@"); err == nil {
		t.Error("an empty destination")
	}
}

// ssh is a way to reach the engine of its own; its settings are for it only; the engine's command goes through it.
func TestConfigureSSH(t *testing.T) {
	for name, set := range map[string]target.Settings{
		"ssh and host":     {"ssh": "ops@h", "host": "tcp://h:2376"},
		"socket w/o ssh":   {"socket": "/run/docker.sock"},
		"insecure w/o ssh": {"insecure": "true"},
		"host ssh+context": {"hosts_a_address": "10.0.0.1", "hosts_a_ssh": "ops@h", "hosts_a_context": "c"},
	} {
		if _, err := Factory("x", set); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	in, err := Factory("x", target.Settings{"hosts_a_address": "10.0.0.1", "hosts_a_ssh": "ops@h:2222", "hosts_a_socket": "/run/docker.sock", "hosts_a_insecure": "true"})
	if err != nil {
		t.Fatal(err)
	}
	e := in.(*Installation).engines[0]
	if e.ssh != "ops@h:2222" || e.socket != "/run/docker.sock" || !e.insecure {
		t.Errorf("engine: %+v", e)
	}
}

// The docker CLI is run against the tunnel's socket.
func TestEngineCommandThroughTunnel(t *testing.T) {
	h := newFakeHost(t)
	tgt := h.target()
	e := &engine{name: "a", bin: "docker", ssh: tgt.dest, socket: tgt.socket, identity: tgt.identity, knownHosts: tgt.knownHosts}
	defer e.closeTunnel()
	cmd, err := e.command(context.Background(), "ps")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Args) != 4 || cmd.Args[1] != "--host" || !strings.HasPrefix(cmd.Args[2], "unix://") || cmd.Args[3] != "ps" {
		t.Fatalf("args: %v", cmd.Args)
	}
	again, _ := e.command(context.Background(), "ps")
	if again.Args[2] != cmd.Args[2] {
		t.Error("each command opened a tunnel of its own")
	}
	// a host that cannot be reached is the error of the command, with the host's name
	bad := &engine{name: "b", bin: "docker", ssh: "ops@127.0.0.1:1", insecure: true, identity: tgt.identity}
	if _, err := bad.command(context.Background(), "ps"); err == nil || !strings.Contains(err.Error(), "b: tunnel") {
		t.Errorf("an unreachable host: %v", err)
	}
}

// A unit with no image is for hosts, not for containers.
func TestContainerNeedsAnImage(t *testing.T) {
	in := &Installation{installation: "x", hosts: map[string]*engine{}, engines: []*engine{{bin: "docker"}}}
	if _, err := in.container(render.Unit{Key: "docker", Component: "docker"}, 0); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("a unit with no image: %v", err)
	}
}
