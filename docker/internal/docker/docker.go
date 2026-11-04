// Package docker is Docker engines as a target: one, or the hosts the
// environment names, each its own engine. A key becomes a container per
// instance — <installation>-<key>, or <installation>-<key>-<i> for scale:
// replicas. On one engine the containers reach each other by name on the
// installation's network; across hosts the operator says which hosts a key
// runs on, instance i on the i-th, and a neighbour reaches an instance at its
// host's address and the port the component says, which the host publishes.
// A container's config files are kept in a volume of its own on its host,
// mounted file by file; its claims are named volumes, its libraries image
// mounts. The target's half of the platform is replicas, hosts and publish.
// It performs steps through the docker CLI.
package docker

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/secretstore"
	"github.com/horchestra/pack/pkg/target"
)

// Factory makes the Docker target of an installation. Of the environment's set it reads
//
//   - hosts_<name>_address, _host, _context: the hosts (hosts: {<name>:
//     {address, host, context}}) — how a neighbour reaches the host, an IP or
//     a DNS name, and docker's --host and --context for its engine; none —
//     the one engine of host and context;
//   - docker: the binary, default from PATH;
//   - network: the network on each engine, default the installation's name;
//   - state: a file that keeps the installation's record and the credentials
//     pack provisioned; default — the engine keeps them (the first host's).
//     Not with pack's `state: db`: then the record is pack's database's, the
//     credentials the store's, and a file is refused;
//   - secrets_<name>_<key>: the values ${secret:<name>/<key>} names;
//
// and of each key's entry under `keys:` (the placement) the part that is the target's: replicas, hosts, publish and
// the runtime properties (name, hostname, aliases, ip, dns, user, groupAdd, sysctls, securityOpt, mounts, restart).
func Factory(installation string, set map[string]string) (target.Installation, error) {
	settings := target.Settings(set)
	if err := settings.Check("docker", append([]string{"host", "context", "ssh", "socket", "identity", "knownhosts", "insecure", "docker", "network", "network_subnet", "network_gateway", "state", "external_address"}, secretstore.SettingKeys...),
		[]string{"hosts_", "secrets_"}); err != nil {
		return nil, err
	}
	in := &Installation{
		installation: installation,
		network:      settings.Get("network", installation),
		settings:     settings,
		hosts:        map[string]*engine{},
		platform:     map[string]map[string]any{},
	}
	bin := settings.Get("docker", "docker")
	for k, v := range settings {
		rest, ok := strings.CutPrefix(k, "hosts_")
		if !ok {
			continue
		}
		i := strings.LastIndexByte(rest, '_')
		if i <= 0 {
			return nil, fmt.Errorf("%s: a host is hosts: {<name>: {address, host, context, ssh, socket, identity, knownhosts, insecure}}", k)
		}
		name, field := rest[:i], rest[i+1:]
		e := in.hosts[name]
		if e == nil {
			e = &engine{name: name, bin: bin}
			in.hosts[name] = e
		}
		switch field {
		case "address":
			e.address = v
		case "host":
			e.host = v
		case "context":
			e.context = v
		case "ssh":
			e.ssh = v
		case "socket":
			e.socket = v
		case "identity":
			e.identity = v
		case "knownhosts":
			e.knownHosts = v
		case "insecure":
			e.insecure = v == "true"
		default:
			return nil, fmt.Errorf("%s: a host has address, host, context, ssh, socket, identity, knownhosts, insecure", k)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(in.hosts)) {
		e := in.hosts[name]
		if len(e.address) == 0 {
			return nil, fmt.Errorf("hosts_%s_address: host %s has no address", name, name)
		}
		in.engines = append(in.engines, e)
	}
	if len(in.engines) == 0 {
		in.engines = []*engine{{host: settings.Get("host", ""), context: settings.Get("context", ""),
			ssh: settings.Get("ssh", ""), socket: settings.Get("socket", ""), identity: settings.Get("identity", ""),
			knownHosts: settings.Get("knownhosts", ""), insecure: settings.Get("insecure", "") == "true", bin: bin}}
	}
	for _, e := range in.engines {
		if len(e.ssh) > 0 && (len(e.host) > 0 || len(e.context) > 0) {
			return nil, fmt.Errorf("%shost: ssh is a way to reach the engine of its own: say ssh, or host, or context", e.prefix())
		}
		if (len(e.socket) > 0 || len(e.identity) > 0 || len(e.knownHosts) > 0 || e.insecure) && len(e.ssh) == 0 {
			return nil, fmt.Errorf("%ssocket, identity, knownhosts and insecure are for an ssh tunnel: say ssh too", e.prefix())
		}
	}
	in.state = engineState{in}
	if path := settings.Get("state", ""); len(path) > 0 {
		if settings.Get(target.SettingPackState, "") == "db" {
			return nil, fmt.Errorf("state %s: pack keeps the record in its state database (state: db) — the target's file would not outlive the run; drop `state:`, the credentials are the store's", path)
		}
		file, err := newFileState(path, installation)
		if err != nil {
			return nil, err
		}
		in.state = file
	}
	if err := in.withStore(); err != nil {
		return nil, err
	}
	return in, nil
}

// Installation is the target bound to one installation on its engines.
type Installation struct {
	installation, network string
	settings              target.Settings
	state                 state
	// engines are the hosts by name, or the one engine; hosts by name.
	engines []*engine
	hosts   map[string]*engine

	// platform is the target's half of each key's placement as Resolve took it (replicas, hosts, publish, the runtime
	// properties): what the unit of the key is run with. Resolve is called for the installed version and the wanted
	// one before any step, in this process.
	mu       sync.Mutex
	platform map[string]map[string]any
}

// spread: the environment names hosts, and a neighbour reaches an instance at
// its host's address.
func (in *Installation) spread() bool { return len(in.hosts) > 0 }

// Namespace: an engine has none.
func (in *Installation) Namespace() string { return "" }

// service is a key's name: its container's, its replicas' prefix, and on one
// engine an alias of every replica.
func (in *Installation) service(key string) string { return in.installation + "-" + key }

// fqdn is a container's name on one engine: <container>.<network>.
func (in *Installation) fqdn(name string) string { return name + "." + in.network }

// Resolve the key. On one engine it is <installation>-<key> on the
// installation's network, replicas <service>-<i> each reached by its name.
// Across hosts it runs on the hosts its platform names, instance i on the
// i-th — replicas, when not said, are as many as the hosts — and is reached
// at the host's address. The target's half of a key's platform is what the
// values say, under what the environment says of the key
// (platform_<key>_replicas, …_hosts, …_publish_<port>); installed, what the
// record kept.
func (in *Installation) Resolve(key string, p render.Placement) (*render.Resolved, error) {
	own, err := in.platformOf(key, p)
	if err != nil {
		return nil, err
	}
	res, err := in.resolve(key, p.Replicas != nil, own)
	if err != nil {
		return nil, err
	}
	in.mu.Lock()
	in.platform[key] = own
	in.mu.Unlock()
	if res.Reach, err = in.reachOf(res, own, p.Reach); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return res, nil
}

// platformFor is the target's half of the key's placement, as Resolve took it.
func (in *Installation) platformFor(key string) map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.platform[key]
}

// reachOf is how a neighbour on another target reaches the key: what the operator says; else, across hosts, the
// address of each instance's host and every port as itself; else, on one engine, the engine's external_address
// and the ports it publishes.
func (in *Installation) reachOf(res *render.Resolved, own map[string]any, said *render.Reach) (*render.Reach, error) {
	if said != nil {
		return said, nil
	}
	if in.spread() {
		rc := &render.Reach{SamePorts: true}
		if len(res.Instances) == 0 {
			rc.Instances = []string{res.Address}
		}
		for _, inst := range res.Instances {
			rc.Instances = append(rc.Instances, inst.Address)
		}
		return rc, nil
	}
	addr := in.settings.Get("external_address", "")
	if len(addr) == 0 {
		return nil, nil
	}
	rc := &render.Reach{Instances: []string{addr}}
	if pub, ok := own["publish"].(map[string]any); ok {
		rc.Ports = map[string]int{}
		for name, v := range pub {
			if p, ok := toInt(v); ok {
				rc.Ports[name] = p
			}
		}
	}
	return rc, nil
}

func (in *Installation) resolve(key string, scales bool, own map[string]any) (*render.Resolved, error) {
	svc := in.serviceOf(key, own)
	hosts := hostsOf(own["hosts"])
	n, explicit := toInt(own["replicas"])
	pub, _ := own["publish"].(map[string]any)

	if !in.spread() {
		if len(hosts) > 0 {
			return nil, fmt.Errorf("%s: hosts: the environment names no hosts (hosts: {<name>: {address, host}})", key)
		}
		if !explicit {
			n, own["replicas"] = 1, int64(1)
		}
		// a neighbour reaches an instance at <container>.<network>: what the
		// engine's resolver answers a reverse query with, and what the
		// container calls itself (--domainname) — one canonical name, which
		// Kerberos turns into the principal on both ends
		res := &render.Resolved{Service: svc, Address: in.fqdn(svc)}
		if h := strOf(own, "hostname"); len(h) > 0 && strings.Contains(h, ".") {
			res.Address = h
		}
		if scales {
			if n > 1 && len(pub) > 0 {
				return nil, fmt.Errorf("%s: publish binds a host port, which one container of %d replicas can have", key, n)
			}
			for i := range n {
				name := fmt.Sprintf("%s-%d", svc, i)
				res.Instances = append(res.Instances, render.Instance{Name: name, Address: in.fqdn(name)})
			}
		}
		return res, nil
	}

	if len(pub) > 0 {
		return nil, fmt.Errorf("%s: publish: across hosts every port is published as itself", key)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s: say which hosts it runs on — platform: {%s: {hosts: [...]}}, of %s",
			key, key, strings.Join(slices.Sorted(maps.Keys(in.hosts)), ", "))
	}
	for i, h := range hosts {
		if in.hosts[h] == nil {
			return nil, fmt.Errorf("%s: hosts: %s is no host of the environment", key, h)
		}
		if slices.Contains(hosts[:i], h) {
			return nil, fmt.Errorf("%s: hosts: %s twice — two instances of one key on a host publish the same ports", key, h)
		}
	}
	if !scales {
		if len(hosts) != 1 {
			return nil, fmt.Errorf("%s: hosts: a key that does not scale runs on one host, not %d", key, len(hosts))
		}
		own["replicas"] = int64(1)
		return &render.Resolved{Service: svc, Address: in.hosts[hosts[0]].address}, nil
	}
	if explicit && n != len(hosts) {
		return nil, fmt.Errorf("%s: %d replicas on %d hosts — an instance per host", key, n, len(hosts))
	}
	own["replicas"] = int64(len(hosts))
	res := &render.Resolved{Service: svc, Address: in.hosts[hosts[0]].address}
	for i, h := range hosts {
		res.Instances = append(res.Instances, render.Instance{Name: fmt.Sprintf("%s-%d", svc, i), Address: in.hosts[h].address})
	}
	return res, nil
}

// platformOf is the target's half of a key's placement: the replicas the environment gives it and the entries of its
// `keys:` the docker target reads.
func (in *Installation) platformOf(key string, p render.Placement) (map[string]any, error) {
	own := map[string]any{}
	if p.Replicas != nil {
		own["replicas"] = int64(*p.Replicas)
	}
	for _, k := range append([]string{"hosts", "publish"}, runtimeProps...) {
		if v, ok := p.Raw[k]; ok {
			own[k] = v
		}
	}
	for k := range p.Raw {
		if k == "reach" || slices.Contains(runtimeProps, k) || k == "hosts" || k == "publish" {
			continue
		}
		return nil, fmt.Errorf("%s: keys.%s.%s: the docker target has replicas, hosts, publish, %s", key, key, k, strings.Join(runtimeProps, ", "))
	}
	return own, nil
}

// engineOf is the engine instance i of a unit runs on.
func (in *Installation) engineOf(u render.Unit, i int) (*engine, error) {
	if !in.spread() {
		return in.engines[0], nil
	}
	hosts := hostsOf(in.platformFor(u.Key)["hosts"])
	if i >= len(hosts) || in.hosts[hosts[i]] == nil {
		return nil, fmt.Errorf("%s: instance %d has no host", u.Key, i)
	}
	return in.hosts[hosts[i]], nil
}

func hostsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, h := range x {
			if s, ok := h.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = x
	}
	return out
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
