package ansible

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

const defaultTimeout = 10 * time.Minute

// ReadInstalled reads the record from the installation's state file.
func (in *Installation) ReadInstalled() ([]byte, bool, error) {
	return in.state.read()
}

// WriteInstalled keeps the record in the installation's state file.
func (in *Installation) WriteInstalled(record []byte) error {
	return in.state.write(record)
}

// DeleteInstalled removes the installation's record — pack delete's last
// act, once the uninstall saga is done. Only the record: the packages and
// the delivered files' removal are the remove steps'.
func (in *Installation) DeleteInstalled() error {
	return in.state.remove()
}

// Execute performs one step of the saga on the hosts.
func (in *Installation) Execute(ctx context.Context, st target.Step, events func(string)) error {
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
		return in.wait(ctx, *st.Unit, st.Timeout, events)
	case "stop":
		return in.stop(ctx, st, events)
	case "remove":
		return in.remove(ctx, st, events)
	case "hook":
		return in.hook(ctx, st, events)
	}
	return fmt.Errorf("step %d: %q is no step kind", st.N, st.Kind)
}

// unitOf is the unit a stop or a remove acts on: the installed one, else the
// one being installed.
func unitOf(st target.Step) (*render.Unit, error) {
	u := st.Previous
	if u == nil {
		u = st.Unit
	}
	if u == nil {
		return nil, fmt.Errorf("%s %s: no unit", st.Kind, st.Key)
	}
	return u, nil
}

// apply brings the key's hosts to what the unit says: its packages
// installed, its files delivered — ${secret:<name>/<key>} resolved, the
// file's mode kept — its services started and enabled.
func (in *Installation) apply(ctx context.Context, u render.Unit, creds []render.Credential, events func(string)) error {
	r, err := in.transport()
	if err != nil {
		return err
	}
	if err := in.keep(creds); err != nil {
		return err
	}
	hosts, err := in.keyHosts(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	if err := in.installPackages(ctx, r, u.Key, hosts, in.platformFor(u.Key)["packages"], events); err != nil {
		return fmt.Errorf("%s: %w", u.Key, err)
	}
	resolve := in.resolver(creds)
	for i, h := range hosts {
		for _, f := range u.Files {
			data := f.Content
			if f.PerInstance() {
				if i >= len(f.Instances) || f.Instances[i] == nil {
					continue // the instance's section was left empty: no file
				}
				data = f.Instances[i]
			}
			s, err := substitute(string(data), resolve)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
			}
			if err := copyFile(ctx, r, h, f.Path, []byte(s), f.Mode, events); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
			}
		}
		for _, f := range u.SecretFiles {
			v, err := resolve(secretRef(f.Secret, f.PerInstance, i))
			if err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
			}
			if err := copyFile(ctx, r, h, f.Path, []byte(v), f.Mode, events); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
			}
		}
	}
	for _, svc := range stringsOf(in.platformFor(u.Key)["services"]) {
		if err := service(ctx, r, hosts, svc, "started", true, events); err != nil {
			return fmt.Errorf("%s: %s: %w", u.Key, svc, err)
		}
	}
	return nil
}

// wait is the services of the key active on its hosts, polled until they are
// or the step's timeout is out; a key without services needs no waiting —
// apply saw its hosts answer.
func (in *Installation) wait(ctx context.Context, u render.Unit, timeout string, events func(string)) error {
	svcs := stringsOf(in.platformFor(u.Key)["services"])
	if len(svcs) == 0 {
		return nil
	}
	r, err := in.transport()
	if err != nil {
		return err
	}
	hosts, err := in.keyHosts(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(stepTimeout(timeout))
	for {
		active := true
		for _, svc := range svcs {
			if err := command(ctx, r, pattern(hosts), []string{"systemctl", "is-active", svc}, nil); err != nil {
				active = false
				break
			}
		}
		if active {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%s: services %s not active within %s", u.Key, strings.Join(svcs, ", "), stepTimeout(timeout))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// stop stops the key's services; a key without services has nothing running.
func (in *Installation) stop(ctx context.Context, st target.Step, events func(string)) error {
	u, err := unitOf(st)
	if err != nil {
		return err
	}
	svcs := stringsOf(in.platformFor(u.Key)["services"])
	if len(svcs) == 0 {
		return nil
	}
	r, err := in.transport()
	if err != nil {
		return err
	}
	hosts, err := in.keyHosts(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if err := service(ctx, r, hosts, svc, "stopped", false, events); err != nil {
			return fmt.Errorf("%s: %s: %w", st.Key, svc, err)
		}
	}
	return nil
}

// remove stops the key's services and deletes the files it delivered. Its
// packages stay: another key may need them, and removing one is no safe
// rollback — pack delete does not take them either.
func (in *Installation) remove(ctx context.Context, st target.Step, events func(string)) error {
	u, err := unitOf(st)
	if err != nil {
		return err
	}
	if err := in.stop(ctx, st, events); err != nil {
		return err
	}
	r, err := in.transport()
	if err != nil {
		return err
	}
	hosts, err := in.keyHosts(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	for _, h := range hosts {
		for _, f := range u.Files {
			if err := removeFile(ctx, r, h, f.Path, events); err != nil {
				return fmt.Errorf("%s: %s: %w", st.Key, f.Path, err)
			}
		}
		for _, f := range u.SecretFiles {
			if err := removeFile(ctx, r, h, f.Path, events); err != nil {
				return fmt.Errorf("%s: %s: %w", st.Key, f.Path, err)
			}
		}
	}
	return nil
}

// hook runs the step's command on the key's first host — module command,
// step.Timeout its deadline.
func (in *Installation) hook(ctx context.Context, st target.Step, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("hook %s: no unit", st.Name)
	}
	if len(st.Command) == 0 {
		return fmt.Errorf("hook %s: no command", st.Name)
	}
	r, err := in.transport()
	if err != nil {
		return err
	}
	if err := in.keep(st.Credentials); err != nil {
		return err
	}
	hosts, err := in.keyHosts(st.Unit.Key, in.platformFor(st.Unit.Key))
	if err != nil {
		return err
	}
	timeout := stepTimeout(st.Timeout)
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := command(hctx, r, hosts[0], st.Command, events); err != nil {
		if errors.Is(hctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("hook %s: did not finish within %s", st.Name, timeout)
		}
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	return nil
}

// keep stores the step's credentials in the state file.
func (in *Installation) keep(creds []render.Credential) error {
	for _, c := range creds {
		if err := in.state.keep(c.Name, c.Leaf, c.Value); err != nil {
			return err
		}
	}
	return nil
}

// resolver reads ${secret:<name>/<key>}: the step's credentials, else the
// ones kept in the state, else the environment's secrets_<name>_<key> (its
// file, its Vault KV).
func (in *Installation) resolver(creds []render.Credential) func(string) (string, error) {
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
		if v, ok, err := in.state.secret(name, key); err != nil || ok {
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

func stepTimeout(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && len(s) > 0 {
		return d
	}
	return defaultTimeout
}

var (
	_ target.Installation = (*Installation)(nil)
	_ target.Recorder     = (*Installation)(nil)
)

// secretRef is the reference a secret file resolves: <name>/<key>, and per instance <name>/<key>.<index>.
func secretRef(ref render.SecretRef, perInstance bool, i int) string {
	if perInstance {
		return fmt.Sprintf("%s/%s.%d", ref.Name, ref.Key, i)
	}
	return ref.Name + "/" + ref.Key
}
