package host

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// What a host runs a container with beyond what the pack says — the target's half of a key's placement, the words
// of a compose file:
//
//   - name: the container's name instead of <installation>-<key> (a replica appends -<i>); the key's address on
//     one host is <name>.<network>
//   - hostname: what the container calls itself; a dotted one is split as compose does, the first label the
//     hostname, the rest the domain name, and is the key's address
//   - aliases: more names on the network
//   - ip: a fixed IPv4 address on the network (the environment's network.subnet holds it)
//   - dns: name servers
//   - user, groupAdd, sysctls, securityOpt: docker run's
//   - mounts: "<source>:<target>[:<options>]" — a source starting with / is a path of the host (options as docker
//     -v takes them: ro, z); otherwise a volume of the installation shared by the keys that name it,
//     "<volume>[/<subpath>]" (option ro), created on first use with the subpaths the environment names
//   - restart: the engine's restart policy instead of the pack's; on-failure for a key that runs to completion
//     retries it, and the wait waits for the run that exits 0
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

// serviceOf is a key's container name: the platform's name, or <installation>-<key>.
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

// stringsOf reads a platform property that is a list of strings.
func stringsOf(v any) []string { return hostsOf(v) }

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

// quantity reads a Kubernetes-style quantity: 500m, 2, 512Mi, 1G.
func quantity(s string) (float64, error) {
	units := []struct {
		suffix string
		mul    float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"m", 1e-3},
	}
	mul := 1.0
	num := s
	for _, u := range units {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			num, mul = rest, u.mul
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is no quantity", s)
	}
	return v * mul, nil
}

// shellQuote is s as one word of sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
