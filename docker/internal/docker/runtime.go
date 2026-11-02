package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"
)

// What an engine runs a container with beyond what the pack says — the
// target's half of a key's platform, the words of a compose file:
//
//   - name: the container's name instead of <installation>-<key> (a replica
//     appends -<i>); the key's address on one engine is <name>.<network>
//   - hostname: what the container calls itself; a dotted one is split as
//     compose does, the first label the hostname, the rest the domain name,
//     and is the key's address
//   - aliases: more names on the network
//   - ip: a fixed IPv4 address on the network (the environment's
//     network.subnet holds it)
//   - dns: name servers
//   - user, groupAdd, sysctls, securityOpt: docker run's
//   - mounts: "<source>:<target>[:<options>]" — a source starting with / is
//     a path of the host (options as docker -v takes them: ro, z);
//     otherwise a volume of the installation shared by the keys that name
//     it, "<volume>[/<subpath>]" (option ro), created on first use with the
//     subpaths the environment names
//   - restart: the engine's restart policy instead of the pack's; on-failure
//     for a key that runs to completion retries it, and the wait waits for
//     the run that exits 0
var runtimeProps = []string{"name", "hostname", "aliases", "ip", "dns", "user", "groupAdd", "sysctls", "securityOpt", "mounts", "restart"}

func strOf(pl map[string]any, k string) string {
	s, _ := pl[k].(string)
	return s
}

func listOf(pl map[string]any, k string) []string {
	var out []string
	switch x := pl[k].(type) {
	case []any:
		for _, v := range x {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = slices.Clone(x)
	case string:
		for s := range strings.SplitSeq(x, ",") {
			if s = strings.TrimSpace(s); len(s) > 0 {
				out = append(out, s)
			}
		}
	}
	return out
}

func mapOf(pl map[string]any, k string) map[string]string {
	out := map[string]string{}
	if m, ok := pl[k].(map[string]any); ok {
		for name, v := range m {
			out[name] = fmt.Sprint(v)
		}
	}
	return out
}

// serviceOf is a key's container name: the platform's name, or
// <installation>-<key>.
func (in *Installation) serviceOf(key string, pl map[string]any) string {
	if n := strOf(pl, "name"); len(n) > 0 {
		return n
	}
	return in.service(key)
}

// splitHost is a dotted hostname as compose splits it.
func splitHost(h string) (host, domain string) {
	host, domain, _ = strings.Cut(h, ".")
	return host, domain
}

// shared is a mount of a volume the installation's keys share.
type shared struct {
	Volume, Subpath, Target string
	ReadOnly                bool
}

// bind is a mount of a path of the host, as docker -v takes it.
type bind struct{ Source, Target, Options string }

// parseMounts splits the platform's mounts into the installation's volumes
// and the host's paths.
func (in *Installation) parseMounts(key string, specs []string) ([]shared, []bind, error) {
	var vols []shared
	var binds []bind
	for _, s := range specs {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) < 2 || len(parts[0]) == 0 || !strings.HasPrefix(parts[1], "/") {
			return nil, nil, fmt.Errorf("%s: mount %q is <source>:<target>[:<options>]", key, s)
		}
		opts := ""
		if len(parts) == 3 {
			opts = parts[2]
		}
		if strings.HasPrefix(parts[0], "/") {
			binds = append(binds, bind{parts[0], parts[1], opts})
			continue
		}
		vol, sub, _ := strings.Cut(parts[0], "/")
		ro := false
		for o := range strings.SplitSeq(opts, ",") {
			switch o {
			case "", "z", "Z", "rw":
			case "ro":
				ro = true
			default:
				return nil, nil, fmt.Errorf("%s: mount %q: a volume takes ro only", key, s)
			}
		}
		vols = append(vols, shared{Volume: containerName(in.installation + "-" + vol), Subpath: path.Clean("/" + sub)[1:], Target: parts[1], ReadOnly: ro})
	}
	return vols, binds, nil
}

// ensureShared creates the installation's shared volumes a container mounts,
// and the subpaths it mounts in them — a volume-subpath must exist before
// the container that mounts it does.
func (in *Installation) ensureShared(ctx context.Context, c container) error {
	subs := map[string][]string{}
	for _, v := range c.Shared {
		subs[v.Volume] = append(subs[v.Volume], v.Subpath)
	}
	e := c.Engine
	for _, v := range slices.Sorted(maps.Keys(subs)) {
		if _, err := e.docker(ctx, nil, nil, nil, "volume", "inspect", v); err != nil {
			if _, err := e.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
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
				p = path.Join(p, seg)
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
		_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if _, err := e.docker(ctx, nil, nil, nil, "create", "--quiet", "--name", helper, "--entrypoint", "/pack-shared",
			"--label", labelInstallation+"="+in.installation, "--label", labelPart+"=shared",
			"--mount", "type=volume,source="+v+",target=/pack-shared", c.Image); err != nil {
			return err
		}
		_, err := e.docker(ctx, &tb, nil, nil, "cp", "-", helper+":/pack-shared")
		_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
		if err != nil {
			return err
		}
	}
	return nil
}

// runtimeArgs are docker create's arguments for the container's runtime
// half.
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
