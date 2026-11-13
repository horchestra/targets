// Package host is classic hosts — VMs or bare metal that SSH reaches — as a target, containers and OS packages
// alike: the driver carries a small agent to each host and speaks to it over the SSH connection (proto/agent.proto).
// A key with an image is a container per instance on its host's engine, as the docker target did it; a key without
// one is OS packages, config files and systemd services on its hosts, as the ansible target did it. The agent does
// the doing: package manager, files, systemctl, sysctl, modprobe, the docker CLI — and serves the files of an OCI
// artifact over the loopback when a package repository is kept in a registry.
package host

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/arenadata/oci-packer/pkg/registry/reference"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/secretstore"
	"github.com/horchestra/pack/pkg/target"
)

// Factory makes the host target of an installation. Of the environment's set it reads
//
//   - hosts_<name>_host (the address, which is also how a neighbour reaches it), _user (root), _port (22),
//     _identity, _knownhosts, _insecure: the hosts. None — this machine, the agent a process of it;
//   - network, network_subnet, network_gateway: the containers' network on each host, default the installation's
//     name;
//   - state: the file that keeps the installation's record and the credentials pack provisioned, default
//     ~/.pack/host-state.json. Not with pack's `state: db` or `state: observe`: then the record is pack's
//     database's or is not kept at all, and a file is refused;
//   - history_count: how many snapshots of a managed file are kept (<path>.bak.1 the newest), rotated before an
//     overwrite that changes it; 0 (the default) — none. The snapshots are what undoes a file step where pack
//     keeps no record (state: observe);
//   - mirror_registry, mirror_plain_http, mirror_port, mirror_username_env, mirror_password_env: the package
//     repositories are artifacts of this registry, served to the host's package manager over the loopback while
//     it installs;
//   - secrets_<name>_<key>: the values ${secret:<name>/<key>} names;
//
// and of each key's entry under `keys:` (the placement) hosts, publish, packages, services, firewall and the
// runtime properties (name, hostname, aliases, ip, dns, user, groupAdd, sysctls, securityOpt, mounts, restart).
//
// The user the SSH connects as needs to be root, or to have passwordless sudo (the agent runs under sudo -n).
func Factory(installation string, set map[string]string) (target.Installation, error) {
	settings := target.Settings(set)
	names := append([]string{"network", "network_subnet", "network_gateway", "state", "history_count",
		"mirror_registry", "mirror_plain_http", "mirror_port", "mirror_username_env", "mirror_password_env"}, secretstore.SettingKeys...)
	if err := settings.Check("host", names, []string{"hosts_", "secrets_"}); err != nil {
		return nil, err
	}
	history, err := historyOf(settings)
	if err != nil {
		return nil, err
	}
	m, err := mirrorOf(settings)
	if err != nil {
		return nil, err
	}
	in := &Installation{
		installation: installation,
		network:      settings.Get("network", installation),
		settings:     settings,
		mirror:       m,
		history:      history,
		hosts:        map[string]*host{},
		platform:     map[string]map[string]any{},
	}
	for k, v := range settings {
		rest, ok := strings.CutPrefix(k, "hosts_")
		if !ok {
			continue
		}
		i := strings.LastIndexByte(rest, '_')
		if i <= 0 {
			return nil, fmt.Errorf("%s: a host is hosts: {<name>: {host, user, port, identity, knownhosts, insecure}}", k)
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
		case "identity":
			h.identity = v
		case "knownhosts":
			h.knownHosts = v
		case "insecure":
			h.insecure = v == "true"
		default:
			return nil, fmt.Errorf("%s: a host has host, user, port, identity, knownhosts, insecure", k)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(in.hosts)) {
		h := in.hosts[name]
		if len(h.address) == 0 {
			return nil, fmt.Errorf("hosts_%s_host: host %s has no host", name, name)
		}
		in.order = append(in.order, h)
	}
	if len(in.order) == 0 {
		// this machine is the one host: the agent in this process, a neighbour reaching a container by name, a
		// package key at the loopback
		h := &host{address: "127.0.0.1", local: true}
		in.order = []*host{h}
		in.local = h
	}
	for _, h := range in.order {
		h.c = &conn{h: h, history: history}
		if !h.local {
			h.c.link = &sshLink{h: h}
		}
	}
	if path := settings.Get("state", ""); len(path) > 0 && settings.Get(target.SettingPackState, "") == "db" {
		return nil, fmt.Errorf("state %s: pack keeps the record in its state database (state: db) — the target's file would not outlive the run; drop `state:`, the credentials are the store's", path)
	}
	switch settings.Get(target.SettingPackState, "") {
	case "db", "observe": // the record is the database's, or is not kept at all: no file of the target's
	default:
		file, err := newFileState(settings.Get("state", "~/.pack/host-state.json"), installation)
		if err != nil {
			return nil, err
		}
		in.state = file
	}
	if err := in.withStore(); err != nil {
		return nil, err
	}
	if settings.Get(target.SettingPackState, "") == "db" && !in.usesStore() {
		return nil, fmt.Errorf("state: db, and no store: the credentials a job makes would not outlive the run — name store: in env.yaml (provider, addr, path)")
	}
	return in, nil
}

// host is one host of the environment: its name, the address SSH connects to — which is how a neighbour reaches
// it too — and how.
type host struct {
	name    string
	address string
	user    string
	port    string
	// identity, knownHosts, insecure: the SSH keys and the host key's check
	identity, knownHosts string
	insecure             bool
	// local: this machine, the agent a process of it, no SSH
	local bool
	c     *conn
}

// prefix names the host in what is told of it, when it has a name.
func (h *host) prefix() string {
	if len(h.name) == 0 {
		return ""
	}
	return h.name + ": "
}

// sudo: the agent needs root — the SSH user that is not root runs it under sudo -n (passwordless sudo is the
// contract, as ansible's become is). The local agent runs as pack's own user: a local install that needs root is
// run with sudo itself.
func (h *host) sudo() bool {
	if h.local {
		return false
	}
	return len(h.user) > 0 && h.user != "root"
}

// Installation is the target bound to one installation on its hosts.
type Installation struct {
	installation, network string
	settings              target.Settings
	state                 state
	mirror                *mirror
	history               int // snapshots of a managed file kept (<path>.bak.N); 0 — none
	hosts                 map[string]*host
	order                 []*host // by name
	local                 *host   // set when the environment names no hosts

	// platform is the target's half of each key's placement as Resolve took it: what the unit of the key is run
	// with. Resolve is called for the installed version and the wanted one before any step, in this process.
	platMu   sync.Mutex
	platform map[string]map[string]any
}

// spread: the environment names hosts, and a neighbour reaches an instance at its host's address.
func (in *Installation) spread() bool { return in.local == nil }

// Namespace: a host has none; the installation's name scopes its records.
func (in *Installation) Namespace() string { return in.installation }

// service is a key's name: its container's, its replicas' prefix, its systemd units' prefix, and on one host an
// alias of every replica.
func (in *Installation) service(key string) string { return in.installation + "-" + key }

// fqdn is a container's name on one host's engine: <container>.<network>.
func (in *Installation) fqdn(name string) string { return name + "." + in.network }

// Resolve the key. A key that installs packages (its placement has packages or services) runs on the hosts it
// names — every host when it names none — and is reached at the first host's address. A key with an image is
// containers: on this machine alone <installation>-<key> on the installation's network, across hosts instance i
// on the i-th host it names, reached at the host's address and every port as itself.
func (in *Installation) Resolve(key string, p render.Placement) (*render.Resolved, error) {
	own, err := in.platformOf(key, p)
	if err != nil {
		return nil, err
	}
	var res *render.Resolved
	if packaged(own) {
		res, err = in.resolvePackages(key, p, own)
	} else {
		res, err = in.resolveContainers(key, p, own)
	}
	if err != nil {
		return nil, err
	}
	in.platMu.Lock()
	in.platform[key] = own
	in.platMu.Unlock()
	return res, nil
}

// platformFor is the target's half of the key's placement, as Resolve took it.
func (in *Installation) platformFor(key string) map[string]any {
	in.platMu.Lock()
	defer in.platMu.Unlock()
	return in.platform[key]
}

// packaged: the key installs OS packages and runs systemd services, not containers.
func packaged(own map[string]any) bool {
	_, p := own["packages"]
	_, s := own["services"]
	return p || s
}

// platformOf is the target's half of a key's placement: the entries of its `keys:` the host target reads.
func (in *Installation) platformOf(key string, p render.Placement) (map[string]any, error) {
	own := map[string]any{}
	if p.Replicas != nil {
		own["replicas"] = int64(*p.Replicas)
	}
	for _, k := range append([]string{"hosts", "publish", "packages", "services", "firewall"}, runtimeProps...) {
		if v, ok := p.Raw[k]; ok {
			own[k] = v
		}
	}
	for k := range p.Raw {
		if k == "reach" || slices.Contains(runtimeProps, k) || k == "hosts" || k == "publish" || k == "packages" || k == "services" || k == "firewall" {
			continue
		}
		return nil, fmt.Errorf("%s: keys.%s.%s: the host target has hosts, publish, packages, services, firewall, %s",
			key, key, k, strings.Join(runtimeProps, ", "))
	}
	if packaged(own) {
		for _, k := range runtimeProps {
			if _, ok := own[k]; ok {
				return nil, fmt.Errorf("%s: %s is a container's property; a key with packages or services runs none", key, k)
			}
		}
		if _, ok := own["publish"]; ok {
			return nil, fmt.Errorf("%s: publish is a container's; a key with packages or services runs none", key)
		}
	} else if _, ok := own["firewall"]; ok {
		return nil, fmt.Errorf("%s: firewall is a packages key's; a container's ports are published by the engine", key)
	}
	return own, nil
}

// resolvePackages is a packages key: the hosts its placement names — every host when none — the address the
// first host's, an instance per host when the environment gives it replicas.
func (in *Installation) resolvePackages(key string, p render.Placement, own map[string]any) (*render.Resolved, error) {
	hosts, err := in.keyHosts(key, own)
	if err != nil {
		return nil, err
	}
	svc := in.service(key)
	res := &render.Resolved{Service: svc, Address: in.order[0].address, Reach: p.Reach}
	if h := in.hosts[hosts[0]]; h != nil {
		res.Address = h.address
	}
	if p.Replicas != nil {
		for i, h := range hosts {
			res.Instances = append(res.Instances, render.Instance{Name: fmt.Sprintf("%s-%d", svc, i), Address: in.addressOf(h)})
		}
	}
	return res, nil
}

// resolveContainers is a containers key, as the docker target resolved it.
func (in *Installation) resolveContainers(key string, p render.Placement, own map[string]any) (*render.Resolved, error) {
	svc := in.serviceOf(key, own)
	hosts := hostsOf(own["hosts"])
	scales := p.Replicas != nil
	n, explicit := toInt(own["replicas"])
	pub, _ := own["publish"].(map[string]any)

	if !in.spread() {
		if len(hosts) > 0 {
			return nil, fmt.Errorf("%s: hosts: the environment names no hosts (hosts: {<name>: {host}})", key)
		}
		if !explicit {
			n, own["replicas"] = 1, int64(1)
		}
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
		r, err := in.reachOf(res, own, p.Reach)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		res.Reach = r
		return res, nil
	}

	if len(pub) > 0 {
		return nil, fmt.Errorf("%s: publish: across hosts every port is published as itself", key)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s: say which hosts it runs on — keys: {%s: {hosts: [...]}}, of %s",
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
	var res *render.Resolved
	if !scales {
		if len(hosts) != 1 {
			return nil, fmt.Errorf("%s: hosts: a key that does not scale runs on one host, not %d", key, len(hosts))
		}
		own["replicas"] = int64(1)
		res = &render.Resolved{Service: svc, Address: in.hosts[hosts[0]].address}
	} else {
		if explicit && n != len(hosts) {
			return nil, fmt.Errorf("%s: %d replicas on %d hosts — an instance per host", key, n, len(hosts))
		}
		own["replicas"] = int64(len(hosts))
		res = &render.Resolved{Service: svc, Address: in.hosts[hosts[0]].address}
		for i, h := range hosts {
			res.Instances = append(res.Instances, render.Instance{Name: fmt.Sprintf("%s-%d", svc, i), Address: in.hosts[h].address})
		}
	}
	if p.Reach != nil {
		res.Reach = p.Reach
	} else {
		rc := &render.Reach{SamePorts: true, Instances: []string{res.Address}}
		for _, inst := range res.Instances {
			rc.Instances = append(rc.Instances, inst.Address)
		}
		res.Reach = rc
	}
	return res, nil
}

// reachOf is how a neighbour on another target reaches the key on this machine: what the operator says; else the
// loopback and the ports it publishes.
func (in *Installation) reachOf(res *render.Resolved, own map[string]any, said *render.Reach) (*render.Reach, error) {
	if said != nil {
		return said, nil
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

// keyHosts are the names of the hosts a key runs on: its platform's hosts, every host of the environment when it
// says none.
func (in *Installation) keyHosts(key string, platform map[string]any) ([]string, error) {
	if in.local != nil {
		return []string{""}, nil
	}
	hosts := hostsOf(platform["hosts"])
	if len(hosts) == 0 {
		for _, h := range in.order {
			hosts = append(hosts, h.name)
		}
		return hosts, nil
	}
	for _, h := range hosts {
		if in.hosts[h] == nil {
			return nil, fmt.Errorf("%s: hosts: %s is no host of the environment", key, h)
		}
	}
	return hosts, nil
}

// conns are the key's connections, by host name ("" — the one local host).
func (in *Installation) conns(key string, platform map[string]any) ([]*conn, error) {
	hosts, err := in.keyHosts(key, platform)
	if err != nil {
		return nil, err
	}
	var out []*conn
	for _, name := range hosts {
		h := in.hosts[name]
		if h == nil {
			h = in.local
		}
		out = append(out, h.c)
	}
	return out, nil
}

// connOf is the connection instance i of a key runs on.
func (in *Installation) connOf(key string, platform map[string]any, i int) (*conn, error) {
	hosts, err := in.keyHosts(key, platform)
	if err != nil {
		return nil, err
	}
	if i >= len(hosts) {
		return nil, fmt.Errorf("%s: instance %d has no host", key, i)
	}
	h := in.hosts[hosts[i]]
	if h == nil {
		h = in.local
	}
	return h.c, nil
}

// addressOf is how a neighbour reaches a host: its address.
func (in *Installation) addressOf(name string) string {
	if h := in.hosts[name]; h != nil {
		return h.address
	}
	return name
}

// Close ends the agents and the connections.
func (in *Installation) Close() error {
	for _, h := range in.order {
		h.c.close()
	}
	return nil
}

// mirror is how the package repositories of a registry are served to a host's package manager while it installs:
// mirror_registry names the registry; nil when the environment names none.
type mirror struct {
	registry  string
	plainHTTP bool
	port      string
	user      string
	password  string
}

func mirrorOf(s target.Settings) (*mirror, error) {
	reg := s.Get("mirror_registry", "")
	if len(reg) == 0 {
		return nil, nil
	}
	m := &mirror{registry: reg, port: s.Get("mirror_port", "8081")}
	if v := s.Get("mirror_plain_http", ""); len(v) > 0 {
		var err error
		if m.plainHTTP, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("mirror_plain_http: %w", err)
		}
	}
	if _, err := parsePort(m.port); err != nil {
		return nil, err
	}
	userEnv, passEnv := s.Get("mirror_username_env", ""), s.Get("mirror_password_env", "")
	if (len(userEnv) == 0) != (len(passEnv) == 0) {
		return nil, fmt.Errorf("mirror_username_env and mirror_password_env go together")
	}
	if len(userEnv) > 0 {
		m.user, m.password = os.Getenv(userEnv), os.Getenv(passEnv)
		if len(m.user) == 0 || len(m.password) == 0 {
			return nil, fmt.Errorf("mirror: %s and %s are not set", userEnv, passEnv)
		}
	}
	return m, nil
}

// historyOf is the snapshots a managed file keeps: history_count of the environment, 0 when it says none.
func historyOf(s target.Settings) (int, error) {
	v := s.Get("history_count", "")
	if len(v) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("history_count %q: a number, 0 or more", v)
	}
	return n, nil
}

// parsePort is a port number.
func parsePort(s string) (uint32, error) {
	p, err := strconv.ParseUint(s, 10, 32)
	if err != nil || p == 0 {
		return 0, fmt.Errorf("mirror_port %s: a port number", s)
	}
	return uint32(p), nil
}

// withMirror runs what installs on a host with the registry's artifacts served on its loopback, and takes the
// proxy away after: the host keeps nothing of it and opens nothing to the outside.
func (in *Installation) withMirror(ctx context.Context, c *conn, install func() error) error {
	if in.mirror == nil {
		return install()
	}
	if err := c.proxyStart(ctx, in.mirror); err != nil {
		return err
	}
	defer c.proxyStop(context.WithoutCancel(ctx))
	return install()
}

// ociFetchURL rewrites a file's oci:// URL — the pack's reference, /templates/<name> after it — into the URL of
// the mirror's proxy on the host's loopback; any other URL is the author's and goes as it is.
func (in *Installation) ociFetchURL(url string) (string, error) {
	if !strings.HasPrefix(url, "oci://") {
		return url, nil
	}
	if in.mirror == nil {
		return "", fmt.Errorf("%s: a file of a pack in a registry, but mirror_registry is not set", url)
	}
	parsed, file, err := splitOCIRef(url)
	if err != nil {
		return "", err
	}
	sep := ":"
	if strings.Contains(parsed.Ref, ":") { // a digest: sha256:…
		sep = "@"
	}
	path := parsed.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://127.0.0.1:%s%s%s%s/%s", in.mirror.port, path, sep, parsed.Ref, file), nil
}

// splitOCIRef cuts oci://<reference>/<file> into the two: the reference is the longest prefix that parses with a
// tag or a digest (neither holds a slash, so the file's start is found exactly).
func splitOCIRef(url string) (reference.Reference, string, error) {
	parts := strings.Split(strings.TrimPrefix(url, "oci://"), "/")
	for i := len(parts) - 1; i >= 1; i-- {
		parsed, err := reference.Parse("oci://" + strings.Join(parts[:i], "/"))
		if err == nil && len(parsed.Ref) > 0 {
			return parsed, strings.Join(parts[i:], "/"), nil
		}
	}
	return reference.Reference{}, "", fmt.Errorf("%s: no reference the file follows", url)
}
