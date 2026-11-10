package host

import (
	"errors"
	"fmt"
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

// The SSH connection to a host, made when first asked for and made again when it is found dead: keepalive probes
// notice a silently dropped one in seconds (a request sent to a dead connection can hang, Wait reads its death),
// and concurrent callers wait on the one dial. The host's key is checked against known_hosts (knownhosts, default
// ~/.ssh/known_hosts) unless insecure: true. Keys: identity (a private key file), else the ssh agent, else
// ~/.ssh/id_ed25519, id_ecdsa, id_rsa; a key with a passphrase is read with $PACK_SSH_PASSPHRASE.
const (
	dialTimeout = 15 * time.Second
	defaultPort = "22"
)

// sshLink is the connection to a host: one client, redialed when it died.
type sshLink struct {
	h *host

	mu     sync.Mutex
	client *ssh.Client
	dead   chan struct{} // closed when client's connection is gone
}

// get is the live client, dialing if there is none or it died, and the channel closed when it dies. Concurrent
// callers wait on the one dial.
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
	c, err := l.h.dial()
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

// dial opens the connection: TCP keepalive notices a dropped one, the handshake is bounded (NewClientConn honors
// no timeout), keepalive alone governs what comes out of it.
func (h *host) dial() (*ssh.Client, error) {
	auth, closeAgent, err := h.authMethods()
	if err != nil {
		return nil, err
	}
	hostKey, err := h.hostKeyCallback()
	if err != nil {
		closeAgent()
		return nil, err
	}
	userName := h.user
	if len(userName) == 0 {
		userName = "root"
	}
	port := h.port
	if len(port) == 0 {
		port = defaultPort
	}
	cfg := &ssh.ClientConfig{User: userName, Auth: auth, HostKeyCallback: hostKey, Timeout: dialTimeout}
	addr := net.JoinHostPort(h.address, port)
	dialer := net.Dialer{Timeout: dialTimeout, KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("%sdial %s: %w", h.prefix(), addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		closeAgent()
		return nil, fmt.Errorf("%sssh %s@%s: %w", h.prefix(), userName, addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	c := ssh.NewClient(sc, chans, reqs)
	go func() { _ = c.Wait(); closeAgent() }()
	return c, nil
}

// authMethods: the key file named, else the agent's keys, else the default key files.
func (h *host) authMethods() ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	if len(h.identity) > 0 {
		s, err := signerFromFile(h.identity)
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

func (h *host) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if h.insecure {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // the operator asked for it: insecure: true
	}
	path := h.knownHosts
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
			return fmt.Errorf("host %s is not in %s (ssh-keyscan -H %s >> %s, or insecure: true): %w",
				hostname, path, strings.Split(hostname, ":")[0], path, err)
		}
		return err
	}, nil
}

// currentUser is the local user's name, for docs and defaults.
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
