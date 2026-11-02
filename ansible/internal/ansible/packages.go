package ansible

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// packagesOf reads the platform's packages: a list for every host, or a map from an OS family (ansible's
// ansible_os_family: RedHat, Debian, Suse …) to the list for the hosts of that family.
func packagesOf(v any) (all []string, byFamily map[string][]string) {
	m, ok := v.(map[string]any)
	if !ok {
		return stringsOf(v), nil
	}
	byFamily = map[string][]string{}
	for family, list := range m {
		byFamily[family] = stringsOf(list)
	}
	return nil, byFamily
}

var familyLine = regexp.MustCompile(`"ansible_os_family":\s*"([^"]+)"`)

// familyOf is the OS family of a host, asked of the host once (module setup, the one fact).
func (in *Installation) familyOf(ctx context.Context, r Runner, host string) (string, error) {
	in.famMu.Lock()
	defer in.famMu.Unlock()
	if f, ok := in.families[host]; ok {
		return f, nil
	}
	var family string
	err := r.Run(ctx, host, "setup", "filter=ansible_os_family", func(l string) {
		if m := familyLine.FindStringSubmatch(l); m != nil {
			family = m[1]
		}
	})
	if err != nil {
		return "", err
	}
	if len(family) == 0 {
		return "", fmt.Errorf("%s: the host did not say its OS family", host)
	}
	if in.families == nil {
		in.families = map[string]string{}
	}
	in.families[host] = family
	return family, nil
}

// installPackages installs a key's packages: the same list on every host, or, from a map by OS family, the list of
// each host's family — hosts of one family in one call.
func (in *Installation) installPackages(ctx context.Context, r Runner, key string, hosts []string, packages any, events func(string)) error {
	all, byFamily := packagesOf(packages)
	say := func(pkgs, on []string) {
		if events != nil {
			events(key + ": install " + strings.Join(pkgs, ", ") + " on " + strings.Join(on, ", "))
		}
	}
	if byFamily == nil {
		if len(all) == 0 {
			return nil
		}
		say(all, hosts)
		return pkgInstall(ctx, r, hosts, all, events)
	}
	groups := map[string][]string{}
	for _, h := range hosts {
		family, err := in.familyOf(ctx, r, h)
		if err != nil {
			return err
		}
		if _, ok := byFamily[family]; !ok {
			known := make([]string, 0, len(byFamily))
			for f := range byFamily {
				known = append(known, f)
			}
			slices.Sort(known)
			return fmt.Errorf("%s: the OS family of %s is %s, the packages are for %s", key, h, family, strings.Join(known, ", "))
		}
		groups[family] = append(groups[family], h)
	}
	families := make([]string, 0, len(groups))
	for f := range groups {
		families = append(families, f)
	}
	slices.Sort(families)
	for _, f := range families {
		if len(byFamily[f]) == 0 {
			continue
		}
		say(byFamily[f], groups[f])
		if err := pkgInstall(ctx, r, groups[f], byFamily[f], events); err != nil {
			return err
		}
	}
	return nil
}
