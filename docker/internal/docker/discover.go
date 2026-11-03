package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/horchestra/pack/pkg/target"
)

// Discover tells the facts of the engines: what docker info says of the machine each runs on, and the ports its
// containers publish there. Read only.
func (in *Installation) Discover(ctx context.Context, hosts []string) (*target.Facts, error) {
	f := &target.Facts{At: time.Now().UTC().Truncate(time.Second), Hosts: map[string]target.HostFacts{}}
	for _, e := range in.engines {
		name := e.name
		if len(name) == 0 {
			name = "engine"
		}
		if len(hosts) > 0 && !slices.Contains(hosts, name) {
			continue
		}
		h, err := e.facts(ctx)
		if err != nil {
			return nil, err
		}
		f.Hosts[name] = h
	}
	return f, nil
}

// engineInfo is what of docker info the facts need.
type engineInfo struct {
	OperatingSystem string
	OSVersion       string
	Architecture    string
	NCPU            int
	MemTotal        int64
	ServerVersion   string
	SystemTime      string
}

func (e *engine) facts(ctx context.Context) (target.HostFacts, error) {
	out, err := e.docker(ctx, nil, nil, nil, "info", "--format", "{{json .}}")
	if err != nil {
		return target.HostFacts{}, err
	}
	var info engineInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return target.HostFacts{}, fmt.Errorf("%sdocker info: %w", e.prefix(), err)
	}
	os, version := osOf(info.OperatingSystem, info.OSVersion)
	h := target.HostFacts{Address: e.address, OS: os, OSVersion: version, Arch: archOf(info.Architecture), CPUs: info.NCPU,
		MemoryMB: info.MemTotal >> 20, Runtimes: []string{"docker"}, Docker: info.ServerVersion}
	if t, err := time.Parse(time.RFC3339Nano, info.SystemTime); err == nil {
		t = t.UTC().Truncate(time.Second)
		h.Time = &t
	}
	ports, err := e.lines(ctx, "ps", "--format", "{{.Ports}}")
	if err != nil {
		return target.HostFacts{}, err
	}
	h.Ports = publishedPorts(ports)
	return h, nil
}

// osOf is the OS of docker info's words: "Debian GNU/Linux 13 (trixie)" is debian 13.
func osOf(operatingSystem, version string) (string, string) {
	name := strings.ToLower(strings.Fields(operatingSystem + " linux")[0])
	if len(version) == 0 {
		if m := regexp.MustCompile(`\b(\d+(\.\d+)*)\b`).FindString(operatingSystem); len(m) > 0 {
			version = m
		}
	}
	return name, version
}

// archOf is GOARCH's word for an engine's architecture.
func archOf(a string) string {
	switch a {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return a
}

var publishedPort = regexp.MustCompile(`(?:[\d.]+|\[[0-9a-fA-F:]*\]):(\d+)->\d+/(tcp|udp)`)

// publishedPorts are the ports of the machine the containers publish: docker ps's 0.0.0.0:8200->8200/tcp, each once.
func publishedPorts(lines []string) []target.Port {
	seen := map[string]bool{}
	var out []target.Port
	for _, l := range lines {
		for _, m := range publishedPort.FindAllStringSubmatch(l, -1) {
			if seen[m[1]+"/"+m[2]] {
				continue
			}
			seen[m[1]+"/"+m[2]] = true
			p, _ := strconv.Atoi(m[1])
			out = append(out, target.Port{Port: p, Proto: m[2], Process: "docker"})
		}
	}
	slices.SortFunc(out, func(a, b target.Port) int { return a.Port - b.Port })
	return out
}
