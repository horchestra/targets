package ansible

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// Runner is the transport: one ansible ad-hoc command. It is an interface so
// a test records the calls instead of running ansible.
type Runner interface {
	// Run is ansible <pattern> -m <module> -a <args>: the output's lines to
	// events, a non-zero exit an error.
	Run(ctx context.Context, pattern, module, args string, events func(string)) error
}

// transport is the installation's runner, made on first use: ansible with
// the inventory the environment names, or with a temporary one generated
// from its hosts.
func (in *Installation) transport() (Runner, error) {
	in.once.Do(func() {
		if in.runner != nil {
			return
		}
		inv := in.settings.Get("inventory", "")
		if len(inv) == 0 {
			if len(in.order) == 0 {
				in.runErr = fmt.Errorf("the environment names no hosts: it needs inventory or hosts: {<name>: {host: …}}")
				return
			}
			if inv, in.runErr = writeInventory(in.order); in.runErr != nil {
				return
			}
		}
		in.runner = &ansibleRunner{
			bin:          in.settings.Get("ansible", "ansible"),
			inventory:    inv,
			become:       in.settings.Get("become", "") == "true",
			becomeMethod: in.settings.Get("become_method", ""),
			privateKey:   in.settings.Get("ssh_private_key", ""),
		}
	})
	return in.runner, in.runErr
}

// ansibleRunner is the Runner that shells out to the ansible binary.
type ansibleRunner struct {
	bin          string
	inventory    string
	become       bool
	becomeMethod string
	privateKey   string
}

func (r *ansibleRunner) Run(ctx context.Context, pattern, module, args string, events func(string)) error {
	a := []string{pattern, "-i", r.inventory, "-m", module, "-a", args}
	if r.become {
		a = append(a, "--become")
	}
	if len(r.becomeMethod) > 0 {
		a = append(a, "--become-method", r.becomeMethod)
	}
	if len(r.privateKey) > 0 {
		a = append(a, "--private-key", r.privateKey)
	}
	cmd := exec.CommandContext(ctx, r.bin, a...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var tail []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if events != nil {
				events(l)
			}
			if tail = append(tail, l); len(tail) > 20 {
				tail = tail[1:]
			}
		}
	}()
	err := cmd.Run()
	_ = pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%s on %s: %w: %s", module, pattern, err, strings.Join(tail, "; "))
	}
	return nil
}

// writeInventory is the hosts as a temporary ansible inventory, INI, 0600:
// it can carry credentials. The temp directory reaps it.
func writeInventory(hosts []*host) (string, error) {
	var b strings.Builder
	for _, h := range hosts {
		_, _ = fmt.Fprintf(&b, "%s ansible_host=%s", h.name, h.address)
		if len(h.user) > 0 {
			_, _ = fmt.Fprintf(&b, " ansible_user=%s", h.user)
		}
		if len(h.port) > 0 {
			_, _ = fmt.Fprintf(&b, " ansible_port=%s", h.port)
		}
		if len(h.become) > 0 {
			_, _ = fmt.Fprintf(&b, " ansible_become=%s", h.become)
		}
		b.WriteByte('\n')
	}
	f, err := os.CreateTemp("", "pack-ansible-inventory-*.ini")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// pattern is hosts as an ansible host pattern: the union a:b:c.
func pattern(hosts []string) string { return strings.Join(hosts, ":") }

// pkgInstall installs packages on the hosts — module package, state=present.
func pkgInstall(ctx context.Context, r Runner, hosts, pkgs []string, events func(string)) error {
	return r.Run(ctx, pattern(hosts), "package", "name="+strings.Join(pkgs, ",")+" state=present", events)
}

// copyFile delivers content to path on one host — module copy.
func copyFile(ctx context.Context, r Runner, host, path string, content []byte, mode *fs.FileMode, events func(string)) error {
	// the directory first: copy does not make it, and a package need not (Fedora's moby-engine has no /etc/docker
	// until the daemon has run); file state=directory leaves one that is there as it is
	if dir := dirOf(path); len(dir) > 0 && dir != "/" {
		if err := r.Run(ctx, host, "file", "path="+quote(dir)+" state=directory", events); err != nil {
			return err
		}
	}
	args := "dest=" + quote(path) + " content=" + quote(string(content))
	if mode != nil {
		args += fmt.Sprintf(" mode=0%o", mode.Perm())
	}
	return r.Run(ctx, host, "copy", args, events)
}

// dirOf is the directory of a path on the host.
func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return ""
}

// removeFile deletes a path on one host — module file, state=absent.
func removeFile(ctx context.Context, r Runner, host, path string, events func(string)) error {
	return r.Run(ctx, host, "file", "path="+quote(path)+" state=absent", events)
}

// service drives a systemd unit on the hosts — module service.
func service(ctx context.Context, r Runner, hosts []string, name, state string, enabled bool, events func(string)) error {
	args := "name=" + quote(name) + " state=" + state
	if enabled {
		args += " enabled=yes"
	}
	return r.Run(ctx, pattern(hosts), "service", args, events)
}

// command runs argv on one host — module command, never a shell.
func command(ctx context.Context, r Runner, host string, argv []string, events func(string)) error {
	return r.Run(ctx, host, "command", quoteAll(argv), events)
}

// quote is s as one word of module arguments — ansible parses them with
// shlex.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteAll(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = quote(a)
	}
	return strings.Join(q, " ")
}
