package ansible

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/horchestra/pack/pkg/render"
)

// osFacts is what a host says of its OS.
type osFacts struct {
	id, version, family, codename, arch string
}

var factLine = regexp.MustCompile(`"ansible_(distribution|distribution_major_version|distribution_release|os_family|architecture)":\s*"([^"]*)"`)

// factsOf is the OS of a host, asked of the host once (module setup).
func (in *Installation) factsOf(ctx context.Context, r Runner, host string) (osFacts, error) {
	in.famMu.Lock()
	defer in.famMu.Unlock()
	if f, ok := in.facts[host]; ok {
		return f, nil
	}
	var f osFacts
	err := r.Run(ctx, host, "setup", "filter=ansible_distribution*,ansible_os_family,ansible_architecture", func(l string) {
		for _, m := range factLine.FindAllStringSubmatch(l, -1) {
			switch m[1] {
			case "distribution":
				f.id = strings.ToLower(m[2])
			case "distribution_major_version":
				f.version = m[2]
			case "distribution_release":
				f.codename = m[2]
			case "os_family":
				f.family = strings.ToLower(m[2])
			case "architecture":
				f.arch = goArch(m[2])
			}
		}
	})
	if err != nil {
		return f, err
	}
	if len(f.id) == 0 {
		return f, fmt.Errorf("%s: the host did not say its OS", host)
	}
	if in.facts == nil {
		in.facts = map[string]osFacts{}
	}
	in.facts[host] = f
	return f, nil
}

// goArch names an architecture the way release files and {arch} of a target's source do.
func goArch(machine string) string {
	switch machine {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return machine
}

// resolve puts the host's facts in place of ${os:…}.
func (f osFacts) resolve(s string) string {
	return strings.NewReplacer("${os:codename}", f.codename, "${os:version}", f.version, "${os:arch}", f.arch, "${os:id}", f.id).Replace(s)
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

// packagesOn is what the pack says of the host's OS, the environment's words over it; nil for a pack that says nothing of
// packages. `packages` of the key's entry are the names (a list, or a map by OS, which also adds an OS the pack does not
// know); `services` the units.
func (in *Installation) packagesOn(ctx context.Context, r Runner, u render.Unit, host string) (*render.Packages, osFacts, error) {
	if len(u.Packages) == 0 {
		return nil, osFacts{}, nil
	}
	f, err := in.factsOf(ctx, r, host)
	if err != nil {
		return nil, f, err
	}
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
			u.Key, host, strings.Join(f.candidates(), ", "), strings.Join(known, ", "), u.Key)
	}
	if svcs := stringsOf(platform["services"]); len(svcs) > 0 {
		p.Services = svcs
	}
	return &p, f, nil
}

// watching passes events on and says whether the module reported a change: what ansible's notify listens to.
func watching(events func(string)) (func(string), *bool) {
	changed := new(bool)
	return func(l string) {
		if strings.Contains(l, "| CHANGED") {
			*changed = true
		}
		if events != nil {
			events(l)
		}
	}, changed
}

// install installs names: a URL is a package file, which apt takes by another argument than the module package.
func install(ctx context.Context, r Runner, host string, f osFacts, names []string, events func(string)) error {
	var plain []string
	for _, n := range names {
		n = f.resolve(n)
		if !strings.Contains(n, "://") {
			plain = append(plain, n)
			continue
		}
		module, args := "package", "name="+quote(n)+" state=present"
		if f.family == "debian" {
			module, args = "apt", "deb="+quote(n)
		}
		if err := r.Run(ctx, host, module, args, events); err != nil {
			return err
		}
	}
	if len(plain) == 0 {
		return nil
	}
	return pkgInstall(ctx, r, []string{host}, plain, events)
}

// deliver puts a file on the host and says whether that changed it: its bytes, or a download.
func deliver(ctx context.Context, r Runner, host string, f osFacts, file render.File, resolve func(string) (string, error), events func(string)) (bool, error) {
	ev, changed := watching(events)
	if len(file.URL) > 0 {
		if dir := dirOf(file.Path); len(dir) > 0 && dir != "/" {
			if err := r.Run(ctx, host, "file", "path="+quote(dir)+" state=directory", events); err != nil {
				return false, err
			}
		}
		args := "url=" + quote(f.resolve(file.URL)) + " dest=" + quote(file.Path)
		if file.Mode != nil {
			args += fmt.Sprintf(" mode=0%o", file.Mode.Perm())
		}
		return *changed, r.Run(ctx, host, "get_url", args, ev)
	}
	s, err := substitute(string(file.Content), resolve)
	if err != nil {
		return false, err
	}
	err = copyFile(ctx, r, host, file.Path, []byte(f.resolve(s)), file.Mode, ev)
	return *changed, err
}

// applyPackages brings one host to its OS's entry, in the order packschema.OSPackages says. A unit is restarted only
// when a file of the unit changed: an apply that changes nothing touches nothing that runs.
func (in *Installation) applyPackages(ctx context.Context, r Runner, u render.Unit, host string, p *render.Packages, f osFacts, resolve func(string) (string, error), events func(string)) error {
	if len(p.Prepare) > 0 {
		if err := install(ctx, r, host, f, p.Prepare, events); err != nil {
			return err
		}
	}
	for _, file := range p.Repo {
		if _, err := deliver(ctx, r, host, f, file, resolve, events); err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
	}
	if len(p.Repo) > 0 && f.family == "debian" { // apt knows of a new repository when it is told; dnf looks by itself
		if err := r.Run(ctx, host, "apt", "update_cache=yes", events); err != nil {
			return err
		}
	}
	if err := install(ctx, r, host, f, p.Names, events); err != nil {
		return err
	}
	for _, c := range p.Commands {
		if err := command(ctx, r, host, c, events); err != nil {
			return err
		}
	}
	changed := false
	for _, file := range p.Files {
		ch, err := deliver(ctx, r, host, f, file, resolve, events)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		changed = changed || ch
	}
	return startServices(ctx, r, []string{host}, p.Services, changed, events)
}

// startServices starts and enables the units; changed is the notify: they are restarted, once they run.
func startServices(ctx context.Context, r Runner, hosts, svcs []string, changed bool, events func(string)) error {
	for _, svc := range svcs {
		if err := service(ctx, r, hosts, svc, "started", true, events); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	if !changed {
		return nil
	}
	for _, svc := range svcs {
		if err := service(ctx, r, hosts, svc, "restarted", false, events); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	return nil
}

// servicesOn are the units of the key on the host.
func (in *Installation) servicesOn(ctx context.Context, r Runner, u render.Unit, host string) ([]string, error) {
	p, _, err := in.packagesOn(ctx, r, u, host)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return p.Services, nil
	}
	return stringsOf(in.platformFor(u.Key)["services"]), nil
}
