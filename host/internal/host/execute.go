package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

const defaultTimeout = 10 * time.Minute

func hostHook(u *render.Unit) bool {
	return u != nil && len(u.Image) == 0
}

// ReadInstalled reads the record from the installation's state file.
func (in *Installation) ReadInstalled() ([]byte, bool, error) {
	return in.state.read()
}

// WriteInstalled keeps the record in the installation's state file.
func (in *Installation) WriteInstalled(record []byte) error {
	return in.state.write(record)
}

// DeleteInstalled removes the installation's record — pack delete's last act, once the uninstall saga is done.
// Only the record: the packages and the delivered files' removal are the remove steps', the claims' data volumes
// stay, and so do the credentials' volumes on an engine.
func (in *Installation) DeleteInstalled() error {
	return in.state.remove()
}

// Execute performs one step of the saga on the hosts: a key with an image as containers, a key without one as
// packages, files and services.
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
		if len(st.Unit.Image) > 0 {
			cs, err := in.containers(*st.Unit)
			if err != nil {
				return err
			}
			completes := st.Unit.RestartPolicy == "Never"
			if err := in.waitContainers(ctx, cs, completes, timeout, events); err != nil || !completes {
				return err
			}
			for _, c := range cs {
				if err := in.collect(ctx, c, events); err != nil {
					return err
				}
			}
			return nil
		}
		return in.waitServices(ctx, *st.Unit, timeout, events)
	case "stop":
		u, err := unitOf(st)
		if err != nil {
			return err
		}
		if len(u.Image) > 0 {
			return in.stopContainers(ctx, st.Key, events)
		}
		return in.stopServices(ctx, *u, events)
	case "remove":
		u, err := unitOf(st)
		if err != nil {
			return err
		}
		if len(u.Image) > 0 {
			return in.removeContainers(ctx, st.Key, events)
		}
		return in.removeFiles(ctx, *u, events)
	case "hook":
		if hostHook(st.Unit) {
			return in.execCommand(ctx, st, timeout, events)
		}
		return in.hook(ctx, st, timeout, events)
	case "exec":
		u := st.Unit
		if u != nil && len(u.Image) > 0 {
			return in.execContainer(ctx, st, timeout, events)
		}
		return in.execCommand(ctx, st, timeout, events)
	}
	return fmt.Errorf("step %d: %q is no step kind", st.N, st.Kind)
}

// unitOf is the unit a stop or a remove acts on: the installed one, else the one being installed.
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

// apply brings the key's hosts to what the unit says.
func (in *Installation) apply(ctx context.Context, u render.Unit, creds []render.Credential, events func(string)) error {
	if err := in.keep(creds); err != nil {
		return err
	}
	if len(u.Image) > 0 {
		return in.applyContainers(ctx, u, creds, events)
	}
	cns, err := in.conns(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	resolve := in.resolver(creds)
	for i, cn := range cns {
		if len(u.Packages) > 0 {
			p, f, err := in.packagesOn(ctx, cn, u)
			if err != nil {
				return err
			}
			err = in.withMirror(ctx, cn, func() error {
				return in.applyPackages(ctx, cn, u, p, f, resolve, events)
			})
			if err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, cn.h.name, err)
			}
			continue
		}
		applyFiles := func() error { return in.applyFiles(ctx, cn, u, i, resolve, events) }
		if slices.ContainsFunc(u.Files, func(f render.File) bool { // a file:// one is streamed, the proxy serves it not
			return len(f.URL) > 0 && !strings.HasPrefix(f.URL, "file://")
		}) {
			err = in.withMirror(ctx, cn, applyFiles)
		} else {
			err = applyFiles()
		}
		if err != nil {
			return fmt.Errorf("%s: %s: %w", u.Key, cn.h.name, err)
		}
	}
	return nil
}

// execCommand runs the step's command — an action's of the pack (pack run) — on the key's first host, the agent's
// exec, step.Timeout its deadline.
func (in *Installation) execCommand(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("%s %s: no unit", st.Kind, st.Name)
	}
	if len(st.Command) == 0 {
		return fmt.Errorf("%s %s: no command", st.Kind, st.Name)
	}
	if err := in.keep(st.Credentials); err != nil {
		return err
	}
	cn, err := in.connOf(st.Unit.Key, in.platformFor(st.Unit.Key), 0)
	if err != nil {
		return err
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := cn.exec(hctx, st.Command, nil, nil, events); err != nil {
		if errors.Is(hctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s %s: did not finish within %s", st.Kind, st.Name, timeout)
		}
		return fmt.Errorf("%s %s: %w", st.Kind, st.Name, err)
	}
	return nil
}

var (
	_ target.Installation = (*Installation)(nil)
	_ target.Recorder     = (*Installation)(nil)
)
