package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A host reached through an SSH tunnel to its Docker socket. `hosts.<name>.ssh: [user@]host[:port]` (and `socket`,
// the engine's socket on the host, /var/run/docker.sock by default) makes the provider connect to the host over SSH,
// in its own process, and listen on a socket of its own that it forwards to the host's: the docker CLI is told
// `--host unix://<that socket>`, as ever. The host needs sshd and the socket, no docker CLI (`docker --host ssh://…`
// runs `docker system dial-stdio` there), and one connection serves the whole run.
//
// The connection is kept as a long-lived client is: TCP keepalive, so that one that was dropped without a word is
// noticed in seconds, not minutes; its death is read from Wait (a request sent to a dead connection can hang); and
// the next connection through the tunnel dials again, once, however many ask at the same time. The host's key is
// checked against known_hosts (`knownhosts`, default ~/.ssh/known_hosts) — a host it does not know is refused with
// the line to add; `insecure: true` turns the check off. Keys: `identity` (a private key file), else the
// ssh agent, else ~/.ssh/id_ed25519, id_ecdsa, id_rsa; a key with a passphrase is read with $PACK_SSH_PASSPHRASE.
const (
	defaultSocket = "/var/run/docker.sock"
	dialTimeout   = 15 * time.Second
)

// sshTarget is a host to reach, as the environment says it.
type sshTarget struct {
	dest       string // [user@]host[:port]
	socket     string // the engine's socket on the host
	identity   string // a private key file
	knownHosts string
	insecure   bool
}

// sshLink is the SSH connection to a host: made when first asked for, made again when it is found dead.
type sshLink struct {
	t sshTarget

	mu     sync.Mutex
	client *ssh.Client
	dead   chan struct{} // closed when client's connection is gone
}

// get is the live client, dialing if there is none, or it died, and the channel that is closed when it dies.
// Concurrent callers wait on the one dial.
func (l *sshLink) get() (*ssh.Client, <-chan struct{}, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		select {
		case <-l.dead:
			_ = l.client.Close()
			l.client = nil
		default:
			return l.client, l.dead, nil
		}
	}
	c, err := dialSSH(l.t)
	if err != nil {
		return nil, nil, err
	}
	dead := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(dead)
	}()
	l.client, l.dead = c, dead
	return c, dead, nil
}

func (l *sshLink) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		_ = l.client.Close()
		l.client = nil
	}
}

// splitDest reads [user@]host[:port]; the user is the current one when none is said, the port 22.
func splitDest(dest string) (userName, host, port string, err error) {
	userName, rest, found := strings.Cut(dest, "@")
	if !found {
		userName, rest = "", dest
	}
	if len(userName) == 0 {
		if u, err := user.Current(); err == nil {
			userName = u.Username
		}
	}
	host, port = rest, "22"
	if h, p, err := net.SplitHostPort(rest); err == nil {
		host, port = h, p
	}
	host = strings.Trim(host, "[]")
	if len(host) == 0 || len(userName) == 0 {
		return "", "", "", fmt.Errorf("ssh %q: [user@]host[:port]", dest)
	}
	return userName, host, port, nil
}

func dialSSH(t sshTarget) (*ssh.Client, error) {
	userName, host, port, err := splitDest(t.dest)
	if err != nil {
		return nil, err
	}
	auth, closeAgent, err := authMethods(t)
	if err != nil {
		return nil, err
	}
	hostKey, err := hostKeyCallback(t)
	if err != nil {
		closeAgent()
		return nil, err
	}
	cfg := &ssh.ClientConfig{User: userName, Auth: auth, HostKeyCallback: hostKey, Timeout: dialTimeout}
	addr := net.JoinHostPort(host, port)
	dialer := net.Dialer{Timeout: dialTimeout, KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	// the handshake is bounded here (NewClientConn does not honor ClientConfig.Timeout); the connection that comes
	// out of it is governed by keepalive alone
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		closeAgent()
		return nil, fmt.Errorf("ssh %s@%s: %w", userName, addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	c := ssh.NewClient(sc, chans, reqs)
	go func() { _ = c.Wait(); closeAgent() }()
	return c, nil
}

// authMethods: the key file named, else the agent's keys, else the default key files.
func authMethods(t sshTarget) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	if len(t.identity) > 0 {
		s, err := signerFromFile(t.identity)
		if err != nil {
			return nil, noop, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(s)}, noop, nil
	}
	var methods []ssh.AuthMethod
	closeAgent := noop
	if sock := os.Getenv("SSH_AUTH_SOCK"); len(sock) > 0 {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
			closeAgent = func() { _ = conn.Close() }
		}
	}
	home, _ := os.UserHomeDir()
	var signers []ssh.Signer
	var skipped []string
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		s, err := signerFromFile(p)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		signers = append(signers, s)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if len(methods) == 0 {
		msg := "no ssh key: say identity, run an ssh agent, or have ~/.ssh/id_ed25519"
		if len(skipped) > 0 {
			msg += " (" + strings.Join(skipped, "; ") + ")"
		}
		return nil, closeAgent, errors.New(msg)
	}
	return methods, closeAgent, nil
}

func signerFromFile(path string) (ssh.Signer, error) {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the ssh key: %w", err)
	}
	s, err := ssh.ParsePrivateKey(b)
	if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
		pass := os.Getenv("PACK_SSH_PASSPHRASE")
		if len(pass) == 0 {
			return nil, fmt.Errorf("%s has a passphrase: set PACK_SSH_PASSPHRASE, or use the ssh agent", path)
		}
		if s, err = ssh.ParsePrivateKeyWithPassphrase(b, []byte(pass)); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func hostKeyCallback(t sshTarget) (ssh.HostKeyCallback, error) {
	if t.insecure {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // the operator asked for it: insecure: true
	}
	path := t.knownHosts
	if len(path) == 0 {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("known hosts %s: %w (ssh-keyscan -H <host> >> it, or insecure: true)", path, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			return fmt.Errorf("host %s is not in %s (ssh-keyscan -H %s >> %s, or insecure: true): %w", hostname, path, strings.Split(hostname, ":")[0], path, err)
		}
		return err
	}, nil
}

// tunnel listens on a socket of its own and forwards each connection to the host's Docker socket.
type tunnel struct {
	link   *sshLink
	remote string
	dir    string
	sock   string
	ln     net.Listener
	wg     sync.WaitGroup
}

func openTunnel(t sshTarget) (*tunnel, error) {
	link := &sshLink{t: t}
	// the first connection is made now, so that a host that cannot be reached is the error of the first command
	if _, _, err := link.get(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pack-docker-")
	if err != nil {
		link.close()
		return nil, err
	}
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		link.close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	tn := &tunnel{link: link, remote: orDefault(t.socket, defaultSocket), dir: dir, sock: sock, ln: ln}
	tn.wg.Add(1)
	go tn.serve()
	return tn, nil
}

func (tn *tunnel) serve() {
	defer tn.wg.Done()
	for {
		local, err := tn.ln.Accept()
		if err != nil {
			return
		}

		tn.wg.Go(func() {
			tn.forward(local)
		})
	}
}

// forward carries one connection both ways, and the half-close of either: `docker cp -`, `exec -i` and attach end
// their input and wait for the answer.
func (tn *tunnel) forward(local net.Conn) {
	defer func() { _ = local.Close() }()
	var remote net.Conn
	for attempt := 0; attempt < 2 && remote == nil; attempt++ {
		c, dead, err := tn.link.get()
		if err != nil {
			return
		}
		if remote, err = c.Dial("unix", tn.remote); err != nil {
			remote = nil
			// a connection that died between get and Dial is dialed again; one that works but has no socket is not
			select {
			case <-dead:
				continue
			default:
				return
			}
		}
	}
	if remote == nil {
		return
	}
	defer func() { _ = remote.Close() }()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(remote, local)
	go pipe(local, remote)
	<-done
	<-done
}

func (tn *tunnel) close() {
	_ = tn.ln.Close()
	tn.link.close()
	tn.wg.Wait()
	_ = os.RemoveAll(tn.dir)
}

// endpoint is what docker is told as --host for the engine: the tunnel's socket when it has one, made on first use,
// else the host as the environment gave it.
func (e *engine) endpoint(_ context.Context) (string, error) {
	if len(e.ssh) == 0 {
		return e.host, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tun == nil {
		tn, err := openTunnel(sshTarget{dest: e.ssh, socket: e.socket, identity: e.identity, knownHosts: e.knownHosts, insecure: e.insecure})
		if err != nil {
			return "", fmt.Errorf("%stunnel: %w", e.prefix(), err)
		}
		e.tun = tn
	}
	return "unix://" + e.tun.sock, nil
}

// closeTunnel ends the engine's tunnel, if it has one.
func (e *engine) closeTunnel() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tun != nil {
		e.tun.close()
		e.tun = nil
	}
}

func orDefault(s, d string) string {
	if len(s) == 0 {
		return d
	}
	return s
}

// Close ends the tunnels of the installation's engines; the provider calls it when pack is done with it.
func (in *Installation) Close() error {
	for _, e := range in.engines {
		e.closeTunnel()
	}
	return nil
}
