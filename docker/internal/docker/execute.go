package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

const (
	defaultTimeout = 10 * time.Minute
	stopGrace      = "30"
	healthInterval = "5s"
	healthTimeout  = "3s"
	healthRetries  = 3
)

func (in *Installation) filters(labels ...string) []string {
	args := []string{"--filter", "label=" + labelInstallation + "=" + in.installation}
	for _, l := range labels {
		args = append(args, "--filter", "label="+l)
	}
	return args
}

// ReadInstalled reads the record from the installation's state.
func (in *Installation) ReadInstalled() ([]byte, bool, error) {
	return in.state.read(context.Background())
}

// WriteInstalled keeps the record in the installation's state.
func (in *Installation) WriteInstalled(record []byte) error {
	return in.state.write(context.Background(), record)
}

// DeleteInstalled removes the installation's record — pack delete's last
// act, once the uninstall saga is done. Only the record: the claims' data
// volumes stay, and so do the credentials' volumes on an engine.
func (in *Installation) DeleteInstalled() error {
	return in.state.remove(context.Background())
}

// Execute performs one step of the saga on the engines.
func (in *Installation) Execute(ctx context.Context, st target.Step, events func(string)) error {
	timeout := defaultTimeout
	if len(st.Timeout) > 0 {
		if d, err := time.ParseDuration(st.Timeout); err == nil {
			timeout = d
		}
	}
	switch st.Kind {
	case "apply":
		if st.Unit == nil {
			return fmt.Errorf("apply %s: no unit", st.Key)
		}
		return in.apply(ctx, *st.Unit, st.Credentials, events)
	case "wait":
		if st.Unit == nil {
			return fmt.Errorf("wait %s: no unit", st.Key)
		}
		cs, err := in.containers(*st.Unit)
		if err != nil {
			return err
		}
		completes := st.Unit.RestartPolicy == "Never"
		if err := in.wait(ctx, cs, completes, timeout, events); err != nil || !completes {
			return err
		}
		for _, c := range cs {
			if err := in.collect(ctx, c, events); err != nil {
				return err
			}
		}
		return nil
	case "stop":
		for _, e := range in.engines {
			names, err := e.lines(ctx, append([]string{"ps", "--format", "{{.Names}}"}, in.filters(labelKey+"="+st.Key, labelPart+"=workload")...)...)
			if err != nil {
				return err
			}
			if len(names) > 0 {
				if _, err := e.docker(ctx, nil, nil, events, append([]string{"stop", "--time", stopGrace}, names...)...); err != nil {
					return err
				}
			}
		}
		return nil
	case "remove":
		// the key's containers and their files, which hold its secrets; the
		// claims stay (--volumes removes only the anonymous ones of an
		// image's VOLUME)
		for _, e := range in.engines {
			names, err := e.lines(ctx, append([]string{"ps", "-a", "--format", "{{.Names}}"}, in.filters(labelKey+"="+st.Key)...)...)
			if err != nil {
				return err
			}
			if len(names) > 0 {
				if _, err := e.docker(ctx, nil, nil, events, append([]string{"rm", "--force", "--volumes"}, names...)...); err != nil {
					return err
				}
			}
			if err := in.prune(ctx, e, in.filters(labelKey+"="+st.Key, labelPart+"=files"), ""); err != nil {
				return err
			}
		}
		return nil
	case "hook":
		return in.hook(ctx, st, timeout, events)
	}
	return fmt.Errorf("step %d: %q is no step kind", st.N, st.Kind)
}

// apply brings the key's containers to what the unit says: a container whose
// hash is the unit's stays (started, if it was stopped), any other is
// recreated, and one the unit no longer has — a replica beyond its count, an
// instance moved to another host — is removed.
func (in *Installation) apply(ctx context.Context, u render.Unit, creds []render.Credential, events func(string)) error {
	if err := in.keep(ctx, creds); err != nil {
		return err
	}
	cs, err := in.containers(u)
	if err != nil {
		return err
	}
	resolve := in.resolver(ctx, creds)
	want := map[*engine][]string{}
	for _, c := range cs {
		e := c.Engine
		want[e] = append(want[e], c.Name)
		m, err := in.materialize(c, resolve)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		rows, err := e.lines(ctx, "ps", "-a", "--filter", "name=^/"+c.Name+"$", "--format", `{{.Label "`+labelHash+`"}} {{.State}}`)
		if err != nil {
			return err
		}
		if len(rows) == 1 {
			hash, state, _ := strings.Cut(rows[0], " ")
			if hash == m.hash {
				if state != "running" && c.Restart != "no" {
					if _, err := e.docker(ctx, nil, nil, events, "start", c.Name); err != nil {
						return err
					}
				}
				continue
			}
			if _, err := e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", c.Name); err != nil {
				return err
			}
		}
		if err := in.run(ctx, m, events); err != nil {
			return err
		}
	}
	for _, e := range in.engines {
		have, err := e.lines(ctx, append([]string{"ps", "-a", "--format", "{{.Names}}"}, in.filters(labelKey+"="+u.Key, labelPart+"=workload")...)...)
		if err != nil {
			return err
		}
		for _, n := range have {
			if slices.Contains(want[e], n) {
				continue
			}
			if _, err := e.docker(ctx, nil, nil, events, "rm", "--force", "--volumes", n); err != nil {
				return err
			}
			if err := in.prune(ctx, e, in.filters(labelContainer+"="+n, labelPart+"=files"), ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func (in *Installation) ensureNetwork(ctx context.Context, e *engine) error {
	if _, err := e.docker(ctx, nil, nil, nil, "network", "inspect", in.network); err == nil {
		return nil
	}
	a := []string{"network", "create", "--label", labelInstallation + "=" + in.installation}
	if s := in.settings.Get("network_subnet", ""); len(s) > 0 {
		a = append(a, "--subnet", s)
	}
	if g := in.settings.Get("network_gateway", ""); len(g) > 0 {
		a = append(a, "--gateway", g)
	}
	_, err := e.docker(ctx, nil, nil, nil, append(a, in.network)...)
	if err != nil {
		// steps run side by side: another one made it first
		if _, ierr := e.docker(ctx, nil, nil, nil, "network", "inspect", in.network); ierr == nil {
			return nil
		}
	}
	return err
}

// materialized is a container ready to create on its engine: docker
// create's arguments, the variables its secret env takes from the CLI's
// environment, its files as a tar and the volume they are kept in, and a
// hash of all of it.
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
		// the container calls itself <hostname>.<domain>: on one engine by
		// default <container>.<network>, the name the engine's resolver gives
		// it to the others
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

// run creates the container on its engine and starts it: its network, its
// claims — volumes that outlive it — and its files volume first, the files
// copied into that volume through a container of the same image that is
// never started; the files volumes it had before are removed.
func (in *Installation) run(ctx context.Context, m *materialized, events func(string)) error {
	c, e := m.c, m.c.Engine
	if err := in.ensureNetwork(ctx, e); err != nil {
		return err
	}
	for _, v := range m.volumes {
		if _, err := e.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
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
		if _, err := e.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
			"--label", labelKey+"="+c.Key, "--label", labelPart+"=files", "--label", labelContainer+"="+c.Name, m.filesV); err != nil {
			return err
		}
		helper := c.Name + "-files"
		_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if _, err := e.docker(ctx, nil, nil, nil, "create", "--quiet", "--name", helper, "--entrypoint", "/pack-files",
			"--label", labelInstallation+"="+in.installation, "--label", labelKey+"="+c.Key, "--label", labelPart+"=files",
			"--mount", "type=volume,source="+m.filesV+",target=/pack-files", c.Image); err != nil {
			return err
		}
		_, err := e.docker(ctx, bytes.NewReader(m.files), nil, nil, "cp", "-", helper+":/pack-files")
		_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if err != nil {
			return err
		}
	}
	if _, err := e.docker(ctx, nil, m.env, nil, append([]string{"create", "--quiet"}, m.args...)...); err != nil {
		return err
	}
	if _, err := e.docker(ctx, nil, nil, nil, "start", c.Name); err != nil {
		return err
	}
	if events != nil {
		events(e.prefix() + "started " + c.Name)
	}
	return in.prune(ctx, e, in.filters(labelContainer+"="+c.Name, labelPart+"=files"), m.filesV)
}

// prune removes the volumes the filters select, but keep.
func (in *Installation) prune(ctx context.Context, e *engine, filters []string, keep string) error {
	vols, err := e.lines(ctx, append([]string{"volume", "ls", "-q"}, filters...)...)
	if err != nil {
		return err
	}
	vols = slices.DeleteFunc(vols, func(v string) bool { return v == keep })
	if len(vols) > 0 {
		_, err = e.docker(ctx, nil, nil, nil, append([]string{"volume", "rm", "--force"}, vols...)...)
	}
	return err
}

// wait waits until every container runs and, with a healthcheck, is healthy;
// one that runs to completion until it exits 0. Then, or when it gives up,
// it tells the containers' logs.
func (in *Installation) wait(ctx context.Context, cs []container, completes bool, timeout time.Duration, events func(string)) error {
	deadline := time.Now().Add(timeout)
	var verdict error
	for {
		ready, failed := 0, ""
		for _, c := range cs {
			out, err := c.Engine.docker(ctx, nil, nil, nil, "inspect", "--format",
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
				failed = fmt.Sprintf("%s%s exited %s", c.Engine.prefix(), c.Name, code)
			case !completes && status == "running" && (health == "-" || health == "healthy" || execProbe(ctx, c)):
				ready++
			}
		}
		if len(failed) > 0 {
			verdict = fmt.Errorf("%s", failed)
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
		in.logs(cs, events)
	}
	return verdict
}

// execProbe runs a readiness that is a command itself, with docker exec and no shell: the engine runs
// --health-cmd through /bin/sh, which a scratch image does not have, and the container stays unhealthy
// whatever its binary says.
func execProbe(ctx context.Context, c container) bool {
	if len(c.Health) < 2 || c.Health[0] != "CMD" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.Engine.docker(pctx, nil, nil, nil, append([]string{"exec", c.Name}, c.Health[1:]...)...)
	return err == nil
}

// logs tells the tail of each container's log, on a context of its own: a
// step that ran out of time still says what its containers did.
func (in *Installation) logs(cs []container, events func(string)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range cs {
		var b bytes.Buffer
		cmd, err := c.Engine.command(ctx, "logs", "--tail", "50", c.Name)
		if err != nil {
			continue
		}
		cmd.Stdout, cmd.Stderr = &b, &b
		_ = cmd.Run()
		for l := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
			if len(l) > 0 {
				events(c.Engine.prefix() + c.Name + ": " + l)
			}
		}
	}
}

// hook runs a hook as a container of its own on replica 0's host: replica 0
// of the key — its files, env and libraries — with the hook's image and
// command; an exclusive hook gets replica 0's volumes, and so does a
// preInstall hook: it runs before the first apply with the component's
// still empty volumes (hdfs namenode -format), nothing else has them yet.
func (in *Installation) hook(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("hook %s: no unit", st.Name)
	}
	if err := in.keep(ctx, st.Credentials); err != nil {
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
	m, err := in.materialize(c, in.resolver(ctx, st.Credentials))
	if err != nil {
		return err
	}
	e := c.Engine
	_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", c.Name)
	if err := in.run(ctx, m, nil); err != nil {
		return err
	}
	if err := in.wait(ctx, []container{c}, true, timeout, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	if err := in.collect(ctx, c, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", c.Name)
	return in.prune(ctx, e, in.filters(labelContainer+"="+c.Name, labelPart+"=files"), "")
}

// keep stores the step's credentials in the installation's state.
func (in *Installation) keep(ctx context.Context, creds []render.Credential) error {
	for _, c := range creds {
		if in.usesStore() && kept(c) {
			continue // the store holds it where the credential says
		}
		if err := in.state.keep(ctx, c.Name, c.Leaf, c.Value); err != nil {
			return err
		}
	}
	return nil
}

// resolver reads ${secret:<name>/<key>}: the step's credentials, else the
// ones kept in the state, else the environment's secrets_<name>_<key> (its
// file, its Vault KV).
func (in *Installation) resolver(ctx context.Context, creds []render.Credential) func(string) (string, error) {
	return func(ref string) (string, error) {
		name, key, ok := strings.Cut(ref, "/")
		if !ok {
			return "", fmt.Errorf("${secret:%s}: <name>/<key>", ref)
		}
		for _, c := range creds {
			if c.Name == name && c.Leaf == key {
				return c.Value, nil
			}
		}
		v, ok, err := in.state.secret(ctx, name, key)
		if err != nil || ok {
			return v, err
		}
		setting := strings.ToLower("secrets_" + name + "_" + key)
		if v, ok := in.settings[setting]; ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %s/%s: not kept in the state, and the environment has no %s", name, key, setting)
	}
}

// substitute replaces ${secret:<name>/<key>} with what resolve says.
func substitute(s string, resolve func(string) (string, error)) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${secret:")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		v, err := resolve(s[i+len("${secret:") : i+j])
		if err != nil {
			return "", err
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+j+1:]
	}
}

// containerName is s as a container or volume name: [a-zA-Z0-9][a-zA-Z0-9_.-]*.
func containerName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		}
		return '-'
	}, s)
	return strings.TrimLeft(s, "_.-")
}

// shellJoin quotes argv for sh -c.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

var (
	_ target.Installation = (*Installation)(nil)
	_ target.Recorder     = (*Installation)(nil)
)
