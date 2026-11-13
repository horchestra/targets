package host

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/targets/host/internal/agentpb"
)

// The packages' steps: a key without an image is the OS packages its platform names (the pack's `packages` by
// OS), its config files delivered and its systemd services started and enabled — done by the agent on the host
// itself: the package manager, the files, systemctl. The agent's Info is the host's OS, asked once a session.

// osFacts is what a host says of its OS: the agent's Info in the shape the packages' choice is made by.
type osFacts struct {
	id, version, family, codename, arch, kernel string
}

func factsOf(info *agentpb.InfoResponse) osFacts {
	return osFacts{id: info.Id, version: info.Version, family: info.Family, codename: info.Codename, arch: info.Arch, kernel: info.Kernel}
}

// resolve puts the host's facts in place of ${os:…}.
func (f osFacts) resolve(s string) string {
	return strings.NewReplacer(
		"${os:codename}", f.codename,
		"${os:version}", f.version,
		"${os:arch}", f.arch,
		"${os:id}", f.id,
		"${os:kernel}", f.kernel,
	).Replace(s)
}

// candidates are the names a host goes by, the most exact first.
func (f osFacts) candidates() []string {
	return []string{f.id + "-" + f.version, f.id, f.family}
}

// match is the entry of a map by OS for the host; the keys are the pack's or the operator's, any OS at all.
func match[T any](m map[string]T, f osFacts) (T, bool) {
	for _, name := range f.candidates() {
		for k, v := range m {
			if strings.EqualFold(k, name) {
				return v, true
			}
		}
	}
	var zero T
	return zero, false
}

// packagesOn is what the pack says of the host's OS, the environment's words over it; nil for a pack that says
// nothing of packages. `packages` of the key's entry are the names (a list, or a map by OS, which also adds an OS
// the pack does not know); `services` the units.
func (in *Installation) packagesOn(ctx context.Context, cn *conn, u render.Unit) (*render.Packages, osFacts, error) {
	if len(u.Packages) == 0 {
		return nil, osFacts{}, nil
	}
	info, err := cn.info(ctx)
	if err != nil {
		return nil, osFacts{}, err
	}
	f := factsOf(info)
	p, found := match(u.Packages, f)
	platform := in.platformFor(u.Key)
	all, byOS := packagesOf(platform["packages"])
	if names, ok := match(byOS, f); ok {
		p.Names, found = names, true
	} else if len(all) > 0 {
		p.Names, found = all, true
	}
	if !found {
		known := make([]string, 0, len(u.Packages))
		for k := range u.Packages {
			known = append(known, k)
		}
		slices.Sort(known)
		return nil, f, fmt.Errorf("%s: %s is %s, the pack has packages for %s: name them in keys.%s.packages of env.yaml",
			u.Key, cn.h.prefix()+info.Id, strings.Join(f.candidates(), ", "), strings.Join(known, ", "), u.Key)
	}
	if svcs := stringsOf(platform["services"]); len(svcs) > 0 {
		p.Services = svcs
	}
	return &p, f, nil
}

// packagesOf reads the platform's packages entry: a list of names, or a map by OS.
func packagesOf(v any) (all []string, byOS map[string][]string) {
	byOS = map[string][]string{}
	switch x := v.(type) {
	case []any:
		for _, s := range x {
			if s, ok := s.(string); ok {
				all = append(all, s)
			}
		}
	case []string:
		all = x
	case map[string]any:
		for osName, names := range x {
			byOS[osName] = stringsOf(names)
		}
	}
	return all, byOS
}

// aptEnv is apt made quiet and non-interactive.
var aptEnv = []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}

// installNames installs the packages on the host: apt on debian, dnf on rhel. A name with :// is a package file
// the agent fetches first (the mirror serves it over the loopback). A package the host already has — at the
// version a `name=version` pin says — is not asked of the package manager again: apt would upgrade a present name
// to the repository's newest, a change no plan showed.
func (c *conn) installNames(ctx context.Context, f osFacts, names []string, events func(string)) error {
	var plain []string
	for _, n := range names {
		n = f.resolve(n)
		if strings.Contains(n, "://") {
			if _, err := c.fetch(ctx, n, "/tmp/pack-package", 0o644); err != nil {
				return err
			}
			switch f.family {
			case "debian":
				if _, err := c.exec(ctx, []string{"apt-get", "install", "-y", "/tmp/pack-package"}, aptEnv, nil, events); err != nil {
					return err
				}
			default:
				if _, err := c.exec(ctx, []string{"dnf", "install", "-y", "/tmp/pack-package"}, nil, nil, events); err != nil {
					return err
				}
			}
			_, _ = c.exec(ctx, []string{"rm", "-f", "/tmp/pack-package"}, nil, nil, nil)
			continue
		}
		if c.hasPackage(ctx, f, n) {
			continue
		}
		plain = append(plain, n)
	}
	if len(plain) == 0 {
		return nil
	}
	switch f.family {
	case "debian":
		_, err := c.exec(ctx, append([]string{"apt-get", "install", "-y"}, plain...), aptEnv, nil, events)
		return err
	case "rhel":
		_, err := c.exec(ctx, append([]string{"dnf", "install", "-y"}, plain...), nil, nil, events)
		return err
	}
	return fmt.Errorf("%sthe OS family %q is not known (debian, rhel)", c.h.prefix(), f.family)
}

// hasPackage says whether the host has the package installed: the name, at the version a `name=version` pin says.
// A name that is not the package manager's (an unresolvable one) counts as absent: the install that follows says why.
func (c *conn) hasPackage(ctx context.Context, f osFacts, name string) bool {
	switch f.family {
	case "debian":
		base, pin, pinned := strings.Cut(name, "=")
		out, err := c.exec(ctx, []string{"dpkg-query", "-W", "-f=${db:Status-Abbrev} ${Version}", base}, nil, nil, nil)
		if err != nil {
			return false
		}
		status, v, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		return status == "ii" && (!pinned || v == pin)
	case "rhel":
		_, err := c.exec(ctx, []string{"rpm", "-q", name}, nil, nil, nil)
		return err == nil
	}
	return false
}

// deliverURL puts a URL's content at the path and says whether that changed it: the agent's own download.
func (c *conn) deliverURL(ctx context.Context, f osFacts, path, url string, mode uint32) (bool, error) {
	if mode == 0 {
		mode = 0o644
	}
	return c.fetch(ctx, f.resolve(url), path, mode)
}

// deliverFetched puts a URL's file on the host and says whether that changed it: a file:// one is streamed from
// this machine's disk, any other the agent downloads — a file of a pack in a registry from the mirror's proxy on
// the host's loopback.
func (in *Installation) deliverFetched(ctx context.Context, cn *conn, f osFacts, path, url string, mode uint32) (bool, error) {
	if local, ok := strings.CutPrefix(url, "file://"); ok {
		return cn.putFile(ctx, local, path, mode)
	}
	u, err := in.ociFetchURL(url)
	if err != nil {
		return false, err
	}
	return cn.deliverURL(ctx, f, path, u, mode)
}

// deliver puts a file on the host and says whether that changed it: its bytes, or a download. A file of a pack in
// a registry comes from the mirror's proxy on the host's loopback.
func (in *Installation) deliver(ctx context.Context, cn *conn, f osFacts, file render.File, resolve func(string) (string, error), events func(string)) (bool, error) {
	var mode uint32 = 0o644
	if file.Mode != nil {
		mode = uint32(file.Mode.Perm())
	}
	if len(file.URL) > 0 {
		ch, err := in.deliverFetched(ctx, cn, f, file.Path, file.URL, mode)
		if err != nil {
			return false, fmt.Errorf("%s: %w", file.Path, err)
		}
		return ch, nil
	}
	s, err := substitute(string(file.Content), resolve)
	if err != nil {
		return false, err
	}
	return cn.writeFile(ctx, file.Path, []byte(f.resolve(s)), mode)
}

// applyPackages brings one host to its OS's entry, in the order packschema.OSInstall says. A unit is restarted
// only when a file of the unit changed: an apply that changes nothing touches nothing that runs.
func (in *Installation) applyPackages(ctx context.Context, cn *conn, u render.Unit, p *render.Packages, f osFacts, resolve func(string) (string, error), events func(string)) error {
	if len(p.Prepare) > 0 {
		if err := cn.installNames(ctx, f, p.Prepare, events); err != nil {
			return err
		}
	}
	if err := cn.applyKernel(ctx, u.Key, p, events); err != nil {
		return err
	}
	repoChanged := false
	for _, file := range p.Repo {
		ch, err := in.deliver(ctx, cn, f, file, resolve, events)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		repoChanged = repoChanged || ch
	}
	if repoChanged && f.family == "debian" { // apt knows of a new repository when it is told; dnf looks by itself
		if _, err := cn.exec(ctx, []string{"apt-get", "update"}, aptEnv, nil, events); err != nil {
			return err
		}
	}
	if err := cn.installNames(ctx, f, p.Names, events); err != nil {
		return err
	}
	for _, cmd := range p.Commands {
		if _, err := cn.exec(ctx, cmd, nil, nil, events); err != nil {
			return err
		}
	}
	changed := false
	for _, file := range p.Files {
		ch, err := in.deliver(ctx, cn, f, file, resolve, events)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		changed = changed || ch
	}
	if err := cn.startServices(ctx, p.Services, changed, events); err != nil {
		return err
	}
	rules, err := in.firewallOf(u.Key)
	if err != nil {
		return err
	}
	return cn.applyFirewall(ctx, f, rules, events)
}

// kernelFiles are the drop-ins a key's Sysctls and Modules are kept in.
func kernelFiles(key string) (sysctl, modules string) {
	return "/etc/sysctl.d/90-pack-" + key + ".conf", "/etc/modules-load.d/pack-" + key + ".conf"
}

// applyKernel loads the key's kernel modules and sets its sysctls, each kept in a drop-in of the OS so a boot
// brings them back. sysctl is asked again only when the file changed; modprobe is idempotent by itself.
func (c *conn) applyKernel(ctx context.Context, key string, p *render.Packages, events func(string)) error {
	sysctlF, modulesF := kernelFiles(key)
	if len(p.Modules) > 0 {
		var b strings.Builder
		for _, m := range p.Modules {
			b.WriteString(m + "\n")
		}
		if _, err := c.writeFile(ctx, modulesF, []byte(b.String()), 0o644); err != nil {
			return err
		}
		for _, m := range p.Modules {
			if _, err := c.exec(ctx, []string{"modprobe", m}, nil, nil, events); err != nil {
				return fmt.Errorf("modprobe %s: %w", m, err)
			}
		}
	}
	if len(p.Sysctls) > 0 {
		var b strings.Builder
		for _, k := range slices.Sorted(maps.Keys(p.Sysctls)) {
			fmt.Fprintf(&b, "%s=%s\n", k, p.Sysctls[k])
		}
		ch, err := c.writeFile(ctx, sysctlF, []byte(b.String()), 0o644)
		if err != nil {
			return err
		}
		if ch {
			if _, err := c.exec(ctx, []string{"sysctl", "-p", sysctlF}, nil, nil, events); err != nil {
				return fmt.Errorf("sysctl: %w", err)
			}
		}
	}
	return nil
}

// startServices starts and enables the units; changed is the notify: they are restarted, once they run.
func (c *conn) startServices(ctx context.Context, svcs []string, changed bool, events func(string)) error {
	for _, svc := range svcs {
		if _, err := c.exec(ctx, []string{"systemctl", "enable", "--now", svc}, nil, nil, events); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	if !changed {
		return nil
	}
	for _, svc := range svcs {
		if _, err := c.exec(ctx, []string{"systemctl", "restart", svc}, nil, nil, events); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	return nil
}

// servicesOn are the units of the key on the host.
func (in *Installation) servicesOn(ctx context.Context, cn *conn, u render.Unit) ([]string, error) {
	p, _, err := in.packagesOn(ctx, cn, u)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return p.Services, nil
	}
	return stringsOf(in.platformFor(u.Key)["services"]), nil
}

// applyFiles brings the key's hosts to what the unit says: its files delivered — ${secret:<name>/<key>} resolved,
// the file's mode kept — its services started and enabled. A unit with packages is brought by applyPackages
// instead.
func (in *Installation) applyFiles(ctx context.Context, cn *conn, u render.Unit, instance int, resolve func(string) (string, error), events func(string)) error {
	platform := in.platformFor(u.Key)
	info, err := cn.info(ctx)
	if err != nil {
		return err
	}
	f := factsOf(info)
	if pkgs := stringsOf(platform["packages"]); len(pkgs) > 0 {
		if err := cn.installNames(ctx, f, pkgs, events); err != nil {
			return err
		}
	}
	changed := false
	for _, file := range u.Files {
		if len(file.URL) > 0 { // fetched: the agent downloads it, or it is streamed from this machine's disk
			var mode uint32
			if file.Mode != nil {
				mode = uint32(file.Mode.Perm())
			}
			ch, err := in.deliverFetched(ctx, cn, f, file.Path, file.URL, mode)
			if err != nil {
				return fmt.Errorf("%s: %w", file.Path, err)
			}
			changed = changed || ch
			continue
		}
		data := file.Content
		if file.PerInstance() {
			if instance >= len(file.Instances) || file.Instances[instance] == nil {
				continue // the instance's section was left empty: no file
			}
			data = file.Instances[instance]
		}
		s, err := substitute(string(data), resolve)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		var mode uint32
		if file.Mode != nil {
			mode = uint32(file.Mode.Perm())
		}
		ch, err := cn.writeFile(ctx, file.Path, []byte(s), mode)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		changed = changed || ch
	}
	for _, file := range u.SecretFiles {
		v, err := resolve(secretRef(file.Secret, file.PerInstance, instance))
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		var mode uint32
		if file.Mode != nil {
			mode = uint32(file.Mode.Perm())
		}
		ch, err := cn.writeFile(ctx, file.Path, []byte(v), mode)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		changed = changed || ch
	}
	if err := cn.startServices(ctx, stringsOf(platform["services"]), changed, events); err != nil {
		return err
	}
	rules, err := in.firewallOf(u.Key)
	if err != nil {
		return err
	}
	return cn.applyFirewall(ctx, f, rules, events)
}

// secretRef is the reference a secret file resolves: <name>/<key>, and per instance <name>/<key>.<index>.
func secretRef(ref render.SecretRef, perInstance bool, i int) string {
	if perInstance {
		return fmt.Sprintf("%s/%s.%d", ref.Name, ref.Key, i)
	}
	return ref.Name + "/" + ref.Key
}

// waitServices is the services of the key active on its hosts, polled until they are or the step's timeout is
// out; a key without services needs no waiting — apply saw its hosts answer.
func (in *Installation) waitServices(ctx context.Context, u render.Unit, timeout time.Duration, events func(string)) error {
	cns, err := in.conns(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var all []string
	svcsByConn := map[*conn][]string{}
	for _, cn := range cns {
		svcs, err := in.servicesOn(ctx, cn, u)
		if err != nil {
			return err
		}
		svcsByConn[cn] = svcs
		all = append(all, svcs...)
	}
	for {
		active := true
	each:
		for cn, svcs := range svcsByConn {
			for _, svc := range svcs {
				if _, err := cn.exec(ctx, []string{"systemctl", "is-active", svc}, nil, nil, nil); err != nil {
					active = false
					break each
				}
			}
		}
		if active {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%s: services %s not active within %s", u.Key, strings.Join(all, ", "), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// stopServices stops the key's services; a key without services has nothing running.
func (in *Installation) stopServices(ctx context.Context, u render.Unit, events func(string)) error {
	cns, err := in.conns(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	for _, cn := range cns {
		svcs, err := in.servicesOn(ctx, cn, u)
		if err != nil {
			return err
		}
		for _, svc := range svcs {
			if _, err := cn.exec(ctx, []string{"systemctl", "stop", svc}, nil, nil, events); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, svc, err)
			}
		}
	}
	return nil
}

// removeFiles stops the key's services and deletes the files it delivered. Its packages stay: another key may
// need them, and removing one is no safe rollback — only pack delete --destroy takes them (destroy), by name and
// without autoremove.
func (in *Installation) removeFiles(ctx context.Context, u render.Unit, destroy bool, events func(string)) error {
	if err := in.stopServices(ctx, u, events); err != nil {
		return err
	}
	cns, err := in.conns(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	for _, cn := range cns {
		files := u.Files
		p, f, err := in.packagesOn(ctx, cn, u)
		if err != nil {
			return err
		}
		if p != nil {
			files = p.Files
		}
		for _, f := range files {
			if err := cn.removeFile(ctx, f.Path); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
			}
		}
		for _, f := range u.SecretFiles {
			if err := cn.removeFile(ctx, f.Path); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
			}
		}
		// the kernel's drop-ins go too; what the kernel already learned (a module loaded, a sysctl set) stays
		sysctlF, modulesF := kernelFiles(u.Key)
		for _, path := range []string{sysctlF, modulesF} {
			if err := cn.removeFile(ctx, path); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, path, err)
			}
		}
		if destroy {
			if err := in.destroyPackages(ctx, cn, u, p, f, events); err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, cn.h.name, err)
			}
		}
		rules, err := in.firewallOf(u.Key)
		if err != nil {
			return err
		}
		if err := cn.removeFirewall(ctx, f, rules, events); err != nil {
			return fmt.Errorf("%s: %w", u.Key, err)
		}
	}
	return nil
}

// destroyPackages takes the key's packages away, by name, without autoremove: pack delete --destroy, a host the
// installation has to itself. What stood before the key came is not told apart — the flag is the operator saying
// the host is the installation's.
func (in *Installation) destroyPackages(ctx context.Context, cn *conn, u render.Unit, p *render.Packages, f osFacts, events func(string)) error {
	var names []string
	if p != nil {
		names = append(names, p.Names...)
	}
	platform := in.platformFor(u.Key)
	all, _ := packagesOf(platform["packages"])
	names = append(names, all...)
	if len(names) == 0 {
		return nil
	}
	if svcs := stringsOf(platform["services"]); len(svcs) > 0 {
		for _, svc := range svcs {
			if _, err := cn.exec(ctx, []string{"systemctl", "disable", svc}, nil, nil, events); err != nil {
				return fmt.Errorf("%s: %w", svc, err)
			}
		}
	}
	// pins and package-file URLs are not the remove's names: the package's own name is
	var plain []string
	for _, n := range names {
		n = f.resolve(n)
		if strings.Contains(n, "://") {
			continue
		}
		base, _, _ := strings.Cut(n, "=")
		plain = append(plain, base)
	}
	switch f.family {
	case "debian":
		_, err := cn.exec(ctx, append([]string{"apt-get", "remove", "-y"}, plain...), aptEnv, nil, events)
		return err
	case "rhel":
		_, err := cn.exec(ctx, append([]string{"dnf", "remove", "-y"}, plain...), nil, nil, events)
		return err
	}
	return fmt.Errorf("%sthe OS family %q is not known (debian, rhel)", cn.h.prefix(), f.family)
}

// restoreFiles undoes an apply: the snapshots of the key's files go back to their places (a file without one was
// the step's own and is deleted), the services run what was restored. Without a history there is nothing to
// restore from: the files go, as a remove says.
func (in *Installation) restoreFiles(ctx context.Context, u render.Unit, events func(string)) error {
	if err := in.stopServices(ctx, u, events); err != nil {
		return err
	}
	cns, err := in.conns(u.Key, in.platformFor(u.Key))
	if err != nil {
		return err
	}
	restored := false
	for _, cn := range cns {
		if cn.history <= 0 {
			files := u.Files
			if p, _, err := in.packagesOn(ctx, cn, u); err != nil {
				return err
			} else if p != nil {
				files = p.Files
			}
			for _, f := range files {
				if err := cn.removeFile(ctx, f.Path); err != nil {
					return fmt.Errorf("%s: %s: %w", u.Key, f.Path, err)
				}
			}
			continue
		}
		var paths []string
		for _, f := range u.Files {
			paths = append(paths, f.Path)
		}
		for _, f := range u.SecretFiles {
			paths = append(paths, f.Path)
		}
		if p, _, err := in.packagesOn(ctx, cn, u); err != nil {
			return err
		} else if p != nil {
			for _, f := range p.Files {
				paths = append(paths, f.Path)
			}
			for _, f := range p.Repo {
				paths = append(paths, f.Path)
			}
		}
		sysctlF, modulesF := kernelFiles(u.Key)
		paths = append(paths, sysctlF, modulesF)
		for _, path := range paths {
			r, err := cn.restore(ctx, path)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", u.Key, path, err)
			}
			restored = restored || r
		}
	}
	if !restored {
		return nil
	}
	// run again what was restored
	for _, cn := range cns {
		svcs, err := in.servicesOn(ctx, cn, u)
		if err != nil {
			return err
		}
		if err := cn.startServices(ctx, svcs, true, events); err != nil {
			return err
		}
	}
	return nil
}
