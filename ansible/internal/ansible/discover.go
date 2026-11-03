package ansible

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/target"
)

// Discover tells the facts of the hosts: ansible's setup (the OS, CPUs, memory, mounts, DNS, clock, the service
// manager), the ports listened on (ss), the docker engine where there is one. Read only.
func (in *Installation) Discover(ctx context.Context, hosts []string) (*target.Facts, error) {
	r, err := in.transport()
	if err != nil {
		return nil, err
	}
	f := &target.Facts{At: time.Now().UTC().Truncate(time.Second), Hosts: map[string]target.HostFacts{}}
	for _, h := range in.order {
		if len(hosts) > 0 && !slices.Contains(hosts, h.name) {
			continue
		}
		hf, err := hostFacts(ctx, r, h.name)
		if err != nil {
			// a host that does not answer tells nothing; the others still do
			if f.Unreachable == nil {
				f.Unreachable = map[string]string{}
			}
			f.Unreachable[h.name] = err.Error()
			continue
		}
		hf.Address = h.address
		f.Hosts[h.name] = hf
	}
	return f, nil
}

func hostFacts(ctx context.Context, r Runner, host string) (target.HostFacts, error) {
	var out []string
	if err := r.Run(ctx, host, "setup", "", func(l string) { out = append(out, l) }); err != nil {
		return target.HostFacts{}, err
	}
	h := setupFacts(adhocJSON(out))
	// what listens and the engine: a host without ss or docker tells none of it, and is no failure
	var ss []string
	if r.Run(ctx, host, "command", "ss -Htlnup", func(l string) { ss = append(ss, l) }) == nil {
		h.Ports = listening(ss)
	}
	// --format json: a {{…}} in a module's arguments is ansible's template, not docker's
	var dv []string
	if r.Run(ctx, host, "command", "docker version --format json", func(l string) { dv = append(dv, l) }) == nil {
		if v := engineVersion(commandOutput(dv)); len(v) > 0 {
			h.Docker = v
			h.Runtimes = append(h.Runtimes, "docker")
		}
	}
	return h, nil
}

// engineVersion is the engine's version of docker version --format json; "" — no engine answered.
func engineVersion(lines []string) string {
	var v struct {
		Server *struct{ Version string }
	}
	if json.Unmarshal([]byte(strings.Join(lines, "\n")), &v) != nil || v.Server == nil {
		return ""
	}
	return v.Server.Version
}

// adhocJSON is the JSON of an ad-hoc module's answer: host | SUCCESS => {…}.
func adhocJSON(lines []string) map[string]any {
	text := strings.Join(lines, "\n")
	i := strings.Index(text, "=> {")
	if i < 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(text[i+3:]), &m) != nil {
		return nil
	}
	return m
}

// commandOutput is what a command printed: the lines after host | CHANGED | rc=0 >>.
func commandOutput(lines []string) []string {
	for i, l := range lines {
		if strings.Contains(l, ">>") {
			var out []string
			for _, o := range lines[i+1:] {
				if o = strings.TrimSpace(o); len(o) > 0 {
					out = append(out, o)
				}
			}
			return out
		}
	}
	return nil
}

func setupFacts(answer map[string]any) target.HostFacts {
	facts, _ := answer["ansible_facts"].(map[string]any)
	str := func(k string) string { s, _ := facts[k].(string); return s }
	num := func(v any) int64 { f, _ := v.(float64); return int64(f) }
	h := target.HostFacts{
		OS:        strings.ToLower(str("ansible_distribution")),
		OSVersion: str("ansible_distribution_major_version"),
		Arch:      goArch(str("ansible_architecture")),
		CPUs:      int(num(facts["ansible_processor_vcpus"])),
		MemoryMB:  num(facts["ansible_memtotal_mb"]),
	}
	if mounts, ok := facts["ansible_mounts"].([]any); ok {
		for _, m := range mounts {
			mm, _ := m.(map[string]any)
			mount, _ := mm["mount"].(string)
			if len(mount) == 0 {
				continue
			}
			h.Disks = append(h.Disks, target.Disk{Mount: mount, SizeMB: num(mm["size_total"]) >> 20, FreeMB: num(mm["size_available"]) >> 20})
		}
	}
	if dns, ok := facts["ansible_dns"].(map[string]any); ok {
		for _, s := range asStrings(dns["nameservers"]) {
			h.DNS = append(h.DNS, s)
		}
	}
	if dt, ok := facts["ansible_date_time"].(map[string]any); ok {
		if s, _ := dt["iso8601"].(string); len(s) > 0 {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				h.Time = &t
			}
		}
	}
	if mgr := str("ansible_service_mgr"); len(mgr) > 0 {
		h.Runtimes = append(h.Runtimes, mgr)
	}
	return h
}

func asStrings(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

var ssLine = regexp.MustCompile(`^(tcp|udp)\s+\S+\s+\d+\s+\d+\s+\S*:(\d+)\s+\S+(?:\s+users:\(\("([^"]+)")?`)

// listening are the ports of ss -Htlnup's lines, each once.
func listening(lines []string) []target.Port {
	seen := map[string]bool{}
	var out []target.Port
	for _, l := range commandOutput(lines) {
		m := ssLine.FindStringSubmatch(l)
		if m == nil || seen[m[2]+"/"+m[1]] {
			continue
		}
		seen[m[2]+"/"+m[1]] = true
		p, _ := strconv.Atoi(m[2])
		out = append(out, target.Port{Port: p, Proto: m[1], Process: m[3]})
	}
	slices.SortFunc(out, func(a, b target.Port) int { return a.Port - b.Port })
	return out
}
