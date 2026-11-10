package host

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

// The containers' steps: a key with an image is a container per instance on its host's engine, driven through the
// agent with the docker CLI of the host.
const (
	stopGrace      = "30"
	healthInterval = "5s"
	healthTimeout  = "3s"
	healthRetries  = 3
	publishMount   = "/var/run/pack"
	publishDir     = publishMount + "/publish"
)

// docker is the docker CLI on the container's host: stdin what - reads, env added to the CLI's environment (a
// secret reaches docker through it, never through argv).
func (c container) docker(ctx context.Context, stdin []byte, env []string, events func(string), args ...string) ([]byte, error) {
	return c.Conn.exec(ctx, append([]string{"docker"}, args...), env, stdin, events)
}

func (c container) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := c.docker(ctx, nil, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	return nonEmpty(out), nil
}

// connDocker is the docker CLI on the connection's host.
func (in *Installation) connDocker(ctx context.Context, cn *conn, args ...string) ([]string, error) {
	out, err := cn.exec(ctx, append([]string{"docker"}, args...), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return nonEmpty(out), nil
}

func nonEmpty(out []byte) []string {
	var ls []string
	for l := range strings.Lines(string(out)) {
		if l = strings.TrimSpace(l); len(l) > 0 {
			ls = append(ls, l)
		}
	}
	return ls
}

func (in *Installation) filters(labels ...string) []string {
	args := []string{"--filter", "label=" + labelInstallation + "=" + in.installation}
	for _, l := range labels {
		args = append(args, "--filter", "label="+l)
	}
	return args
}

// applyContainers brings the key's containers to what the unit says: a container whose hash is the unit's stays
// (started, if it was stopped), any other is recreated, and one the unit no longer has — a replica beyond its
// count, an instance moved to another host — is removed.
func (in *Installation) applyContainers(ctx context.Context, u render.Unit, creds []render.Credential, events func(string)) error {
	cs, err := in.containers(u)
	if err != nil {
		return err
	}
	resolve := in.resolver(creds)
	want := map[*conn][]string{}
	for _, c := range cs {
		cn := c.Conn
		want[cn] = append(want[cn], c.Name)
		m, err := in.materialize(c, resolve)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		rows, err := c.lines(ctx, "ps", "-a", "--filter", "name=^/"+c.Name+"$", "--format", `{{.Label "`+labelHash+`"}} {{.State}}`)
		if err != nil {
			return err
		}
		if len(rows) == 1 {
			hash, state, _ := strings.Cut(rows[0], " ")
			if hash == m.hash {
				if state != "running" && c.Restart != "no" {
					if _, err := c.docker(ctx, nil, nil, events, "start", c.Name); err != nil {
						return err
					}
				}
				continue
			}
			if _, err := c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", c.Name); err != nil {
				return err
			}
		}
		if err := in.run(ctx, m, events); err != nil {
			return err
		}
	}
	for _, cn := range in.allConns() {
		have, err := in.connDocker(ctx, cn, append([]string{"ps", "-a", "--format", "{{.Names}}"}, in.filters(labelKey+"="+u.Key, labelPart+"=workload")...)...)
		if err != nil {
			return err
		}
		for _, n := range have {
			if slices.Contains(want[cn], n) {
				continue
			}
			if _, err := cn.exec(ctx, []string{"docker", "rm", "--force", "--volumes", n}, nil, nil, events); err != nil {
				return err
			}
			if err := in.prune(ctx, cn, in.filters(labelContainer+"="+n, labelPart+"=files"), ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// allConns is every host's connection, in order.
func (in *Installation) allConns() []*conn {
	var out []*conn
	for _, h := range in.order {
		out = append(out, h.c)
	}
	return out
}

// stopContainers stops the key's containers on every host.
func (in *Installation) stopContainers(ctx context.Context, key string, events func(string)) error {
	for _, cn := range in.allConns() {
		names, err := in.connDocker(ctx, cn, append([]string{"ps", "--format", "{{.Names}}"}, in.filters(labelKey+"="+key, labelPart+"=workload")...)...)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			if _, err := cn.exec(ctx, append([]string{"docker", "stop", "--time", stopGrace}, names...), nil, nil, events); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeContainers removes the key's containers and their files, which hold its secrets; the claims stay
// (--volumes removes only the anonymous ones of an image's VOLUME).
func (in *Installation) removeContainers(ctx context.Context, key string, events func(string)) error {
	for _, cn := range in.allConns() {
		names, err := in.connDocker(ctx, cn, append([]string{"ps", "-a", "--format", "{{.Names}}"}, in.filters(labelKey+"="+key)...)...)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			if _, err := cn.exec(ctx, append([]string{"docker", "rm", "--force", "--volumes"}, names...), nil, nil, events); err != nil {
				return err
			}
		}
		if err := in.prune(ctx, cn, in.filters(labelKey+"="+key, labelPart+"=files"), ""); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installation) ensureNetwork(ctx context.Context, cn *conn) error {
	if _, err := in.connDocker(ctx, cn, "network", "inspect", in.network); err == nil {
		return nil
	}
	a := []string{"network", "create", "--label", labelInstallation + "=" + in.installation}
	if s := in.settings.Get("network_subnet", ""); len(s) > 0 {
		a = append(a, "--subnet", s)
	}
	if g := in.settings.Get("network_gateway", ""); len(g) > 0 {
		a = append(a, "--gateway", g)
	}
	if _, err := in.connDocker(ctx, cn, append(a, in.network)...); err != nil {
		// steps run side by side: another one made it first
		if _, ierr := in.connDocker(ctx, cn, "network", "inspect", in.network); ierr == nil {
			return nil
		}
		return err
	}
	return nil
}

// materialized is a container ready to create on its host: docker create's arguments, the variables its secret
// env takes from the CLI's environment, its files as a tar and the volume they are kept in, and a hash of all of
// it.
type materialized struct {
	c       container
	volumes []string // claims
	args    []string
	env     []string
	files   []byte
	filesV  string
	hash    string
	// publish: it runs to completion and gets the key's publish volume
	publish bool
}

func (in *Installation) materialize(c container, resolve func(string) (string, error)) (*materialized, error) {
	m := &materialized{c: c}
	sum := sha256.New()

	// the files first: their volume is named by their content
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	var mounts []string
	put := func(p string, mode *fs.FileMode, data []byte) error {
		perm := int64(0o644)
		if mode != nil {
			perm = int64(mode.Perm())
		}
		rel := strings.TrimPrefix(p, "/")
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: perm, Size: int64(len(data)),
			Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		mounts = append(mounts, rel)
		return err
	}
	for _, f := range c.Files {
		s, err := substitute(string(f.Data), resolve)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if err := put(f.Path, f.Mode, []byte(s)); err != nil {
			return nil, err
		}
	}
	for _, f := range c.SecretFiles {
		v, err := resolve(f.Ref.Name + "/" + f.Ref.Key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if err := put(f.Path, f.Mode, []byte(v)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if len(mounts) > 0 {
		m.files = tb.Bytes()
		fs := sha256.Sum256(m.files)
		m.filesV = c.Name + "-files-" + hex.EncodeToString(fs[:6])
	}

	a := []string{"--name", c.Name, "--hostname", c.Hostname, "--network", in.network, "--restart", c.Restart}
	if len(c.Domain) > 0 {
		// the container calls itself <hostname>.<domain>: on one host by default <container>.<network>, the name
		// the engine's resolver gives it to the others
		a = append(a, "--domainname", c.Domain)
	}
	a = append(a, in.runtimeArgs(c)...)
	for _, al := range c.Aliases {
		a = append(a, "--network-alias", al)
	}
	for _, e := range c.Env {
		if e.Secret == nil {
			a = append(a, "--env", e.Name+"="+e.Value)
			continue
		}
		v, err := resolve(e.Secret.Name + "/" + e.Secret.Key)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", e.Name, err)
		}
		// the value reaches docker through its environment, never its argv
		a = append(a, "--env", e.Name)
		m.env = append(m.env, e.Name+"="+v)
		_, _ = fmt.Fprintf(sum, "env %s=%s\n", e.Name, v)
	}
	for _, p := range publishes(c) {
		a = append(a, "--publish", p.(string))
	}
	for _, v := range c.Volumes {
		a = append(a, "--mount", "type=volume,source="+v.Source+",target="+v.Target)
		m.volumes = append(m.volumes, v.Source)
	}
	if c.Completes {
		a = append(a, "--mount", "type=volume,source="+in.publishVolume(c.Key)+",target="+publishMount)
		m.publish = true
	}
	for _, rel := range mounts {
		a = append(a, "--mount", "type=volume,source="+m.filesV+",target=/"+rel+",volume-subpath="+rel+",readonly")
	}
	for _, l := range c.Libraries {
		a = append(a, "--mount", "type=image,source="+l.Source+",target="+l.Target)
	}
	if len(c.CPUs) > 0 {
		a = append(a, "--cpus", c.CPUs)
	}
	if c.CPUShares > 0 {
		a = append(a, "--cpu-shares", strconv.FormatInt(c.CPUShares, 10))
	}
	if c.Memory > 0 {
		a = append(a, "--memory", strconv.FormatInt(c.Memory, 10))
	}
	if c.MemReservation > 0 {
		a = append(a, "--memory-reservation", strconv.FormatInt(c.MemReservation, 10))
	}
	if len(c.Health) > 0 {
		cmd := c.Health[1]
		if c.Health[0] == "CMD" {
			cmd = shellJoin(c.Health[1:])
		}
		a = append(a, "--health-cmd", cmd, "--health-interval", healthInterval,
			"--health-timeout", healthTimeout, "--health-retries", strconv.Itoa(healthRetries))
	}
	for _, k := range sortedKeys(c.Labels) {
		a = append(a, "--label", k+"="+c.Labels[k])
	}
	if len(c.Entrypoint) > 0 {
		a = append(a, "--entrypoint", c.Entrypoint[0])
	}
	tail := []string{c.Image}
	if len(c.Entrypoint) > 1 {
		tail = append(tail, c.Entrypoint[1:]...)
	}
	tail = append(tail, c.Cmd...)
	for _, s := range append(slices.Clone(a), tail...) {
		_, _ = fmt.Fprintf(sum, "arg %s\n", s)
	}
	m.hash = hex.EncodeToString(sum.Sum(nil))[:16]
	m.args = append(append(a, "--label", labelHash+"="+m.hash), tail...)
	return m, nil
}

// runtimeArgs are docker create's arguments for the container's runtime half.
func (in *Installation) runtimeArgs(c container) []string {
	var a []string
	if len(c.IP) > 0 {
		a = append(a, "--ip", c.IP)
	}
	for _, d := range c.DNS {
		a = append(a, "--dns", d)
	}
	if len(c.User) > 0 {
		a = append(a, "--user", c.User)
	}
	for _, g := range c.GroupAdd {
		a = append(a, "--group-add", g)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Sysctls)) {
		a = append(a, "--sysctl", k+"="+c.Sysctls[k])
	}
	for _, o := range c.SecurityOpt {
		a = append(a, "--security-opt", o)
	}
	for _, v := range c.Shared {
		m := "type=volume,source=" + v.Volume + ",target=" + v.Target
		if len(v.Subpath) > 0 {
			m += ",volume-subpath=" + v.Subpath
		}
		if v.ReadOnly {
			m += ",readonly"
		}
		a = append(a, "--mount", m)
	}
	for _, b := range c.Binds {
		s := b.Source + ":" + b.Target
		if len(b.Options) > 0 {
			s += ":" + b.Options
		}
		a = append(a, "--volume", s)
	}
	return a
}

// run creates the container on its host and starts it: its network, its claims — volumes that outlive it — and
// its files volume first, the files copied into that volume through a container of the same image that is never
// started; the files volumes it had before are removed.
func (in *Installation) run(ctx context.Context, m *materialized, events func(string)) error {
	c, cn := m.c, m.c.Conn
	if err := in.ensureNetwork(ctx, cn); err != nil {
		return err
	}
	for _, v := range m.volumes {
		if _, err := c.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
			"--label", labelKey+"="+c.Key, "--label", labelPart+"=data", v); err != nil {
			return err
		}
	}
	if m.publish {
		if err := in.ensurePublish(ctx, c); err != nil {
			return err
		}
	}
	if err := in.ensureShared(ctx, c); err != nil {
		return err
	}
	if m.files != nil {
		if _, err := c.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
			"--label", labelKey+"="+c.Key, "--label", labelPart+"=files", "--label", labelContainer+"="+c.Name, m.filesV); err != nil {
			return err
		}
		helper := c.Name + "-files"
		_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if _, err := c.docker(ctx, nil, nil, nil, "create", "--quiet", "--name", helper, "--entrypoint", "/pack-files",
			"--label", labelInstallation+"="+in.installation, "--label", labelKey+"="+c.Key, "--label", labelPart+"=files",
			"--mount", "type=volume,source="+m.filesV+",target=/pack-files", c.Image); err != nil {
			return err
		}
		_, err := c.docker(ctx, m.files, nil, nil, "cp", "-", helper+":/pack-files")
		_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if err != nil {
			return err
		}
	}
	if _, err := c.docker(ctx, nil, m.env, nil, append([]string{"create", "--quiet"}, m.args...)...); err != nil {
		return err
	}
	if _, err := c.docker(ctx, nil, nil, nil, "start", c.Name); err != nil {
		return err
	}
	if events != nil {
		events(c.Conn.h.prefix() + "started " + c.Name)
	}
	return in.prune(ctx, cn, in.filters(labelContainer+"="+c.Name, labelPart+"=files"), m.filesV)
}

// prune removes the volumes the filters select, but keep.
func (in *Installation) prune(ctx context.Context, cn *conn, filters []string, keep string) error {
	vols, err := in.connDocker(ctx, cn, append([]string{"volume", "ls", "-q"}, filters...)...)
	if err != nil {
		return err
	}
	vols = slices.DeleteFunc(vols, func(v string) bool { return v == keep })
	if len(vols) > 0 {
		_, err = cn.exec(ctx, append([]string{"docker", "volume", "rm", "--force"}, vols...), nil, nil, nil)
	}
	return err
}

// waitContainers waits until every container runs and, with a healthcheck, is healthy; one that runs to
// completion until it exits 0. Then, or when it gives up, it tells the containers' logs.
func (in *Installation) waitContainers(ctx context.Context, cs []container, completes bool, timeout time.Duration, events func(string)) error {
	deadline := time.Now().Add(timeout)
	var verdict error
	for {
		ready, failed := 0, ""
		for _, c := range cs {
			out, err := c.docker(ctx, nil, nil, nil, "inspect", "--format",
				`{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}} {{.State.ExitCode}}`, c.Name)
			f := strings.Fields(string(out))
			if err != nil || len(f) != 3 {
				continue
			}
			status, health, code := f[0], f[1], f[2]
			switch {
			case completes && status == "exited" && code == "0":
				ready++
			case completes && (status == "exited" || status == "dead") && c.Restart == "on-failure" && status != "dead":
				// the engine runs it again: the run that exits 0 is waited for
			case completes && (status == "exited" || status == "dead"):
				failed = fmt.Sprintf("%s%s exited %s", c.Conn.h.prefix(), c.Name, code)
			case !completes && status == "running" && (health == "-" || health == "healthy" || execProbe(ctx, c)):
				ready++
			}
		}
		if len(failed) > 0 {
			verdict = errors.New(failed)
			break
		}
		if ready == len(cs) {
			break
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err() // cut short: no logs, at once
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			var names []string
			for _, c := range cs {
				names = append(names, c.Name)
			}
			verdict = fmt.Errorf("%s not ready within %s", strings.Join(names, ", "), timeout)
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if verdict != nil || completes {
		in.logsOf(cs, events)
	}
	return verdict
}

// execProbe runs a readiness that is a command itself, with docker exec and no shell: the engine runs
// --health-cmd through /bin/sh, which a scratch image does not have, and the container stays unhealthy whatever
// its binary says.
func execProbe(ctx context.Context, c container) bool {
	if len(c.Health) < 2 || c.Health[0] != "CMD" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.docker(pctx, nil, nil, nil, append([]string{"exec", c.Name}, c.Health[1:]...)...)
	return err == nil
}

// logsOf tells the tail of each container's log, on a context of its own: a step that ran out of time still says
// what its containers did.
func (in *Installation) logsOf(cs []container, events func(string)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range cs {
		out, _ := c.Conn.exec(ctx, []string{"docker", "logs", "--tail", "50", c.Name}, nil, nil, nil)
		for l := range strings.Lines(strings.TrimSpace(string(out))) {
			if len(l) > 0 {
				events(c.Conn.h.prefix() + c.Name + ": " + l)
			}
		}
	}
}

// hook runs a hook as a container of its own on replica 0's host: replica 0 of the key — its files, env and
// libraries — with the hook's image and command; an exclusive hook gets replica 0's volumes, and so does a
// preInstall hook: it runs before the first apply with the component's still empty volumes (hdfs namenode
// -format), nothing else has them yet.
func (in *Installation) hook(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("hook %s: no unit", st.Name)
	}
	if err := in.keep(st.Credentials); err != nil {
		return err
	}
	c, err := in.container(*st.Unit, 0)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s-%d", in.service(st.Key), st.Name, st.N)
	if st.Rollback {
		name = fmt.Sprintf("%s-undo-%s-%d", in.service(st.Key), st.Name, st.N)
	}
	c.Name, c.Part, c.Image, c.Entrypoint, c.Cmd = containerName(name), "hook", st.Image, st.Command, nil
	c.Aliases, c.Publish, c.Health, c.Restart, c.Completes = nil, nil, nil, "no", true
	// a hook is not the key's instance: no fixed address, its own hostname
	c.IP, c.Hostname = "", c.Name
	c.Labels[labelPart] = "hook"
	c.PublishAs = st.Name
	if !st.Exclusive && st.Phase != "preInstall" {
		c.Volumes = nil
	}
	m, err := in.materialize(c, in.resolver(st.Credentials))
	if err != nil {
		return err
	}
	_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", c.Name)
	if err := in.run(ctx, m, nil); err != nil {
		return err
	}
	if err := in.waitContainers(ctx, []container{c}, true, timeout, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	if err := in.collect(ctx, c, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", c.Name)
	return in.prune(ctx, c.Conn, in.filters(labelContainer+"="+c.Name, labelPart+"=files"), "")
}

// execContainer runs the step's command in the key's first instance as it runs — docker exec — with the
// instance's files, data and network: an action of the pack (pack run).
func (in *Installation) execContainer(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("exec %s: no unit", st.Name)
	}
	if len(st.Command) == 0 {
		return fmt.Errorf("exec %s: no command", st.Name)
	}
	c, err := in.container(*st.Unit, 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := c.docker(ctx, nil, nil, events, append([]string{"exec", c.Name}, st.Command...)...); err != nil {
		return fmt.Errorf("exec %s in %s: %w", st.Name, c.Name, err)
	}
	return nil
}

func (in *Installation) publishVolume(key string) string {
	return containerName(in.service(key) + "-publish")
}

// ensurePublish creates the key's publish volume on the container's host, publish/ in it world-writable, unless
// it is there.
func (in *Installation) ensurePublish(ctx context.Context, c container) error {
	v := in.publishVolume(c.Key)
	if _, err := c.docker(ctx, nil, nil, nil, "volume", "inspect", v); err == nil {
		return nil
	}
	if _, err := c.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
		"--label", labelKey+"="+c.Key, "--label", labelPart+"=publish", v); err != nil {
		return err
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	if err := tw.WriteHeader(&tar.Header{Name: "publish/", Mode: 0o1777, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// filled through a container that is never started, as the files volume
	helper := containerName(v + "-init")
	_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
	if _, err := c.docker(ctx, nil, nil, nil, "create", "--quiet", "--name", helper, "--entrypoint", "/pack-publish",
		"--label", labelInstallation+"="+in.installation, "--label", labelKey+"="+c.Key, "--label", labelPart+"=publish",
		"--mount", "type=volume,source="+v+",target=/pack-publish", c.Image); err != nil {
		return err
	}
	_, err := c.docker(ctx, tb.Bytes(), nil, nil, "cp", "-", helper+":/pack-publish")
	_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
	return err
}

// ensureShared creates the installation's shared volumes a container mounts, and the subpaths it mounts in them —
// a volume-subpath must exist before the container that mounts it does.
func (in *Installation) ensureShared(ctx context.Context, c container) error {
	subs := map[string][]string{}
	for _, v := range c.Shared {
		subs[v.Volume] = append(subs[v.Volume], v.Subpath)
	}
	for _, v := range slices.Sorted(maps.Keys(subs)) {
		if _, err := c.docker(ctx, nil, nil, nil, "volume", "inspect", v); err != nil {
			if _, err := c.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
				"--label", labelPart+"=shared", v); err != nil {
				return err
			}
		}
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		dirs := 0
		for _, sub := range subs[v] {
			if len(sub) == 0 {
				continue
			}
			// every directory of the subpath, parents first
			p := ""
			for seg := range strings.SplitSeq(sub, "/") {
				p = pathJoin(p, seg)
				if err := tw.WriteHeader(&tar.Header{Name: p + "/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)}); err != nil {
					return err
				}
				dirs++
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		if dirs == 0 {
			continue
		}
		helper := containerName(v + "-init")
		_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if _, err := c.docker(ctx, nil, nil, nil, "create", "--quiet", "--name", helper, "--entrypoint", "/pack-shared",
			"--label", labelInstallation+"="+in.installation, "--label", labelPart+"=shared",
			"--mount", "type=volume,source="+v+",target=/pack-shared", c.Image); err != nil {
			return err
		}
		_, err := c.docker(ctx, tb.Bytes(), nil, nil, "cp", "-", helper+":/pack-shared")
		_, _ = c.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if err != nil {
			return err
		}
	}
	return nil
}

func pathJoin(elem ...string) string { return path.Join(elem...) }

// collect keeps what an exited container published in the installation's state.
func (in *Installation) collect(ctx context.Context, c container, events func(string)) error {
	out, err := c.docker(ctx, nil, nil, nil, "cp", c.Name+":"+publishDir, "-")
	if err != nil {
		return fmt.Errorf("%s: read %s: %w", c.Name, publishDir, err)
	}
	got, err := published(out)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", c.Name, publishDir, err)
	}
	keys := map[string]int{}
	for _, p := range got {
		if err := in.state.keep(p.name, p.key, p.value); err != nil {
			return err
		}
		keys[p.name]++
	}
	if events != nil {
		for _, name := range slices.Sorted(maps.Keys(keys)) {
			events(fmt.Sprintf("%s%s published %s (%d keys)", c.Conn.h.prefix(), c.Name, name, keys[name]))
		}
	}
	return nil
}

type publication struct{ name, key, value string }

// published reads what docker cp gives of the publish directory: a tar of publish/<name>/<key>; a deeper path,
// or a dotted name or key, is the job's own.
func published(archive []byte) ([]publication, error) {
	var out []publication
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		segs := strings.Split(strings.TrimPrefix(h.Name, "./"), "/")
		if len(segs) != 3 || slices.ContainsFunc(segs[1:], func(s string) bool { return len(s) == 0 || s[0] == '.' }) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out = append(out, publication{segs[1], segs[2], string(data)})
	}
}
