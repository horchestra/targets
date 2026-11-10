package host

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/target"
)

// Discover tells the facts of the hosts: what the agent says of the OS, and what nproc, free, df, ss,
// resolv.conf and the clock say — the docker engine where there is one. Read only. A host that does not answer
// tells nothing; the others still do.
func (in *Installation) Discover(ctx context.Context, hosts []string) (*target.Facts, error) {
	f := &target.Facts{At: time.Now().UTC().Truncate(time.Second), Hosts: map[string]target.HostFacts{}}
	for _, h := range in.order {
		name := h.name
		if len(name) == 0 {
			name = "local"
		}
		if len(hosts) > 0 && !slices.Contains(hosts, name) {
			continue
		}
		hf, err := h.facts(ctx)
		if err != nil {
			if f.Unreachable == nil {
				f.Unreachable = map[string]string{}
			}
			f.Unreachable[name] = err.Error()
			continue
		}
		hf.Address = h.address
		f.Hosts[name] = hf
	}
	return f, nil
}

// facts is one host's, asked of its agent: a fact the host cannot tell is left out.
func (h *host) facts(ctx context.Context) (target.HostFacts, error) {
	c := h.c
	info, err := c.info(ctx)
	if err != nil {
		return target.HostFacts{}, err
	}
	out := target.HostFacts{OS: info.Id, OSVersion: info.Version, Arch: info.Arch, Runtimes: []string{"systemd"}}
	if b, err := c.exec(ctx, []string{"nproc"}, nil, nil, nil); err == nil {
		out.CPUs, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if b, err := c.exec(ctx, []string{"free", "-m"}, nil, nil, nil); err == nil {
		for l := range strings.Lines(string(b)) {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "Mem:" {
				out.MemoryMB, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	if b, err := c.exec(ctx, []string{"df", "-m", "-x", "tmpfs", "-x", "devtmpfs", "--output=target,size,avail"}, nil, nil, nil); err == nil {
		for l := range strings.Lines(string(b)) {
			f := strings.Fields(l)
			if len(f) != 3 || !strings.HasPrefix(f[0], "/") {
				continue
			}
			size, _ := strconv.ParseInt(f[1], 10, 64)
			free, _ := strconv.ParseInt(f[2], 10, 64)
			out.Disks = append(out.Disks, target.Disk{Mount: f[0], SizeMB: size, FreeMB: free})
		}
	}
	if b, err := c.exec(ctx, []string{"ss", "-Htlnup"}, nil, nil, nil); err == nil {
		out.Ports = listening(string(b))
	}
	if b, err := c.exec(ctx, []string{"cat", "/etc/resolv.conf"}, nil, nil, nil); err == nil {
		for l := range strings.Lines(string(b)) {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "nameserver" {
				out.DNS = append(out.DNS, f[1])
			}
		}
	}
	if b, err := c.exec(ctx, []string{"date", "-u", "+%Y-%m-%dT%H:%M:%SZ"}, nil, nil, nil); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
			out.Time = &t
		}
	}
	if b, err := c.exec(ctx, []string{"docker", "version", "--format", "json"}, nil, nil, nil); err == nil {
		var v struct {
			Server *struct{ Version string }
		}
		if json.Unmarshal(b, &v) == nil && v.Server != nil {
			out.Docker = v.Server.Version
			out.Runtimes = append(out.Runtimes, "docker")
		}
	}
	return out, nil
}

// listening reads ss -Htlnup: the ports listened on, with what listens when ss says.
func listening(out string) []target.Port {
	var ports []target.Port
	for l := range strings.Lines(out) {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		proto := f[0]
		local := f[4]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		n, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		p := target.Port{Port: n, Proto: proto}
		if len(f) >= 6 {
			if s := strings.TrimPrefix(f[5], "users:(("); s != f[5] {
				name, _, _ := strings.Cut(strings.TrimPrefix(s, `"`), `"`)
				p.Process = name
			}
		}
		ports = append(ports, p)
	}
	return ports
}

var _ target.Discoverer = (*Installation)(nil)
