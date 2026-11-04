// Package ansible is classic hosts — VMs or bare metal that SSH reaches — as
// a target. A key runs no container: the target installs the OS packages its
// platform names, delivers its rendered config files to the hosts it runs
// on, and starts and enables the systemd services it says. Ansible is only a
// transport: every step is ad-hoc commands (ansible -m …) of the built-in
// modules package, copy, file, service and command; custom playbooks are not
// supported.
package ansible

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/secretstore"
	"github.com/horchestra/pack/pkg/target"
)

// Factory makes the ansible target of an installation: legacy hosts — classic VMs or bare metal. Of the
// environment's set it reads
//
//   - inventory: an ansible inventory file; without one the hosts are
//     hosts_<name>_host (+ _user, _port, _become), and a temporary inventory
//     is generated from them;
//   - ansible: the binary, default from PATH;
//   - state: the file that keeps the installation's record and the
//     credentials pack provisioned, default ~/.pack/ansible-state.json.
//     Not with pack's `state: db`: then the record is pack's database's,
//     the credentials the store's, and a file is refused;
//   - become, become_method, ssh_private_key: ansible's --become,
//     --become-method and --private-key;
//   - secrets_<name>_<key>: the values ${secret:<name>/<key>} names;
//
// and of each key's entry under `keys:` (the placement) packages, hosts and services.
func Factory(installation string, set map[string]string) (target.Installation, error) {
	settings := target.Settings(set)
	if err := check(settings); err != nil {
		return nil, err
	}
	if b := settings.Get("become", ""); len(b) > 0 {
		if _, err := strconv.ParseBool(b); err != nil {
			return nil, fmt.Errorf("become: %w", err)
		}
	}
	m, err := mirrorOf(settings)
	if err != nil {
		return nil, err
	}
	in := &Installation{mirror: m, installation: installation, settings: settings, hosts: map[string]*host{}, platform: map[string]map[string]any{}}
	for k, v := range settings {
		rest, ok := strings.CutPrefix(k, "hosts_")
		if !ok {
			continue
		}
		i := strings.LastIndexByte(rest, '_')
		if i <= 0 {
			return nil, fmt.Errorf("%s: a host is hosts: {<name>: {host, user, port, become}}", k)
		}
		name, field := rest[:i], rest[i+1:]
		h := in.hosts[name]
		if h == nil {
			h = &host{name: name}
			in.hosts[name] = h
		}
		switch field {
		case "host":
			h.address = v
		case "user":
			h.user = v
		case "port":
			h.port = v
		case "become":
			h.become = v
		default:
			return nil, fmt.Errorf("%s: a host has host, user, port, become", k)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(in.hosts)) {
		h := in.hosts[name]
		if len(h.address) == 0 {
			return nil, fmt.Errorf("hosts_%s_host: host %s has no host", name, name)
		}
		in.order = append(in.order, h)
	}
	if inv := settings.Get("inventory", ""); len(inv) > 0 {
		st, err := os.Stat(inv)
		if err != nil {
			return nil, fmt.Errorf("inventory %s: %w", inv, err)
		}
		if st.IsDir() {
			return nil, fmt.Errorf("inventory %s: a file, not a directory", inv)
		}
	}
	// With pack's state in a database the file is no place for the record (it would not outlive the run) nor for
	// the credentials: a `state:` file is refused, and a store is required.
	packState := settings.Get(target.SettingPackState, "")
	if packState == "db" {
		if path, set := set["state"]; set {
			return nil, fmt.Errorf("state %s: pack keeps the record in its state database (state: db) — the target's file would not outlive the run; drop `state:`, the credentials are the store's", path)
		}
	} else {
		file, err := newFileState(settings.Get("state", "~/.pack/ansible-state.json"), installation)
		if err != nil {
			return nil, err
		}
		in.state = file
	}
	if err := in.withStore(); err != nil {
		return nil, err
	}
	if packState == "db" && !in.usesStore() {
		return nil, fmt.Errorf("state: db, and no store: the credentials a job makes would not outlive the run — name store: in env.yaml (provider, addr, path)")
	}
	return in, nil
}

func check(settings target.Settings) error {
	return settings.Check("ansible", append([]string{"mirror_registry", "mirror_plain_http", "mirror_port", "mirror_username_env", "mirror_password_env", "inventory", "ansible", "state", "become", "become_method", "ssh_private_key"}, secretstore.SettingKeys...),
		[]string{"hosts_", "secrets_"})
}

// host is one host of the environment: its inventory name, the address
// ansible connects to — which is how a neighbour reaches it too — and how.
type host struct {
	name    string
	address string // ansible_host
	user    string
	port    string
	become  string
}

// Installation is the target bound to one installation on its hosts.
type Installation struct {
	mirror       *mirror // oci-proxy on a host while it installs; nil — none
	installation string
	settings     target.Settings
	hosts        map[string]*host
	order        []*host // by name
	state        state

	// platform is the target's half of each key's placement as Resolve took it (packages, hosts, services): what the
	// unit of the key is run with. Resolve is called for the installed version and the wanted one before any step,
	// in this process.
	platMu   sync.Mutex
	platform map[string]map[string]any

	// the OS family of each host, asked once (packages given by family)
	famMu    sync.Mutex
	families map[string]string
	facts    map[string]osFacts // the OS of each host, asked once

	// the ansible runner, made on first use; a test sets a fake.
	once   sync.Once
	runner Runner
	runErr error
}

// Namespace: a host has none; the installation's name scopes its records.
func (in *Installation) Namespace() string { return in.installation }

// Resolve the key: it runs on the hosts its placement names — every host of the environment when it names none — and
// is reached at the first host's address, an instance per host when the environment gives it replicas. The target's
// half of a key's placement is the entries of its `keys:` it reads: packages, hosts, services.
func (in *Installation) Resolve(key string, p render.Placement) (*render.Resolved, error) {
	own, err := in.platformOf(key, p)
	if err != nil {
		return nil, err
	}
	hosts, err := in.keyHosts(key, own)
	if err != nil {
		return nil, err
	}
	in.platMu.Lock()
	in.platform[key] = own
	in.platMu.Unlock()
	svc := in.installation + "-" + key
	res := &render.Resolved{Service: svc, Address: in.addressOf(hosts[0]), Reach: p.Reach}
	if p.Replicas != nil {
		for i, h := range hosts {
			res.Instances = append(res.Instances, render.Instance{Name: fmt.Sprintf("%s-%d", svc, i), Address: in.addressOf(h)})
		}
	}
	return res, nil
}

// platformFor is the target's half of the key's placement, as Resolve took it.
func (in *Installation) platformFor(key string) map[string]any {
	in.platMu.Lock()
	defer in.platMu.Unlock()
	return in.platform[key]
}

func (in *Installation) platformOf(key string, p render.Placement) (map[string]any, error) {
	own := map[string]any{}
	for k, v := range p.Raw {
		switch k {
		case "packages", "hosts", "services":
			own[k] = v
		default:
			return nil, fmt.Errorf("%s: keys.%s.%s: the ansible target has packages, hosts, services", key, key, k)
		}
	}
	return own, nil
}

// keyHosts are the names of the hosts a key runs on: its platform's hosts,
// every host of the environment when it says none.
func (in *Installation) keyHosts(key string, platform map[string]any) ([]string, error) {
	hosts := stringsOf(platform["hosts"])
	if len(hosts) == 0 {
		if len(in.order) == 0 {
			return nil, fmt.Errorf("%s: say which hosts it runs on — keys: {%s: {hosts: [...]}}: "+
				"the inventory is the environment's own and its hosts are unknown here", key, key)
		}
		for _, h := range in.order {
			hosts = append(hosts, h.name)
		}
		return hosts, nil
	}
	if len(in.order) == 0 {
		return hosts, nil // an inventory of the environment's: the names are its hosts'
	}
	for _, h := range hosts {
		if in.hosts[h] == nil {
			return nil, fmt.Errorf("%s: hosts: %s is no host of the environment", key, h)
		}
	}
	return hosts, nil
}

// addressOf is how a neighbour reaches a host: its address, or, from an
// inventory of the environment's own, its name there.
func (in *Installation) addressOf(name string) string {
	if h := in.hosts[name]; h != nil {
		return h.address
	}
	return name
}

// stringsOf reads a platform property that is a list of strings.
func stringsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, s := range x {
			if s, ok := s.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = x
	}
	return out
}
