package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/horchestra/pack/pkg/logging"
)

// engine is one Docker engine the installation runs on: a host of the
// environment's hosts, or the one engine when it names none.
type engine struct {
	// name is the host's name in the environment; "" — the one engine.
	name string
	// address is how a neighbour reaches the host: an IP or a DNS name.
	address       string
	host, context string // docker's --host, --context
	bin           string
	// ssh and socket: the engine is reached through a tunnel to its socket (see tunnel.go); then host is not used
	ssh, socket, identity, knownHosts string
	insecure                          bool
	mu                                sync.Mutex
	tun                               *tunnel
}

// command is the docker CLI against the engine; an engine behind an ssh tunnel is reached through it, and an
// error is the tunnel's.
func (e *engine) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	host, err := e.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	var base []string
	if len(host) > 0 {
		base = append(base, "--host", host)
	}
	if len(e.context) > 0 && len(e.ssh) == 0 {
		base = append(base, "--context", e.context)
	}
	return exec.CommandContext(ctx, e.bin, append(base, args...)...), nil
}

// docker runs the docker CLI against the engine; stdin, when set, is what -
// reads, env is added to the CLI's environment. Output goes to events line
// by line.
func (e *engine) docker(ctx context.Context, stdin io.Reader, env []string, events func(string), args ...string) ([]byte, error) {
	cmd, err := e.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = stdin
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err = cmd.Run()
	// the arguments: a secret reaches docker through its environment, never its argv (materialize) — env is not logged
	logging.For("docker").Debug().Str("engine", e.name).Strs("args", args).Dur("took", time.Since(start)).
		Int("stdout_bytes", stdout.Len()).Err(err).Msg("docker")
	if events != nil {
		sc := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); len(line) > 0 {
				events(e.prefix() + line)
			}
		}
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) == 0 {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%sdocker %s: %s", e.prefix(), args[0], msg)
	}
	return stdout.Bytes(), nil
}

// lines runs docker and returns its output's non-empty lines.
func (e *engine) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := e.docker(ctx, nil, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var ls []string
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); len(l) > 0 {
			ls = append(ls, l)
		}
	}
	return ls, nil
}

// prefix names the host in what is told of it, when there are hosts.
func (e *engine) prefix() string {
	if len(e.name) == 0 {
		return ""
	}
	return e.name + ": "
}
