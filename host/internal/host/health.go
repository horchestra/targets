package host

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/horchestra/pack/pkg/target"
)

// Logs gives the tail of what a key's instances wrote, by instance: a container key's containers' logs, a package
// key's services' journal — or, when it names none, of the host's errors (journalctl -p err).
func (in *Installation) Logs(ctx context.Context, key string, lines int) (map[string][]string, error) {
	out := map[string][]string{}
	n := strconv.Itoa(max(lines, 1))
	platform := in.platformFor(key)
	cns, err := in.conns(key, platform)
	if err != nil {
		return nil, err
	}
	if packaged(platform) {
		units := stringsOf(platform["services"])
		for _, cn := range cns {
			args := []string{"journalctl", "--no-pager", "-n", n, "-p", "err"}
			if len(units) > 0 {
				args = []string{"journalctl", "--no-pager", "-n", n}
				for _, u := range units {
					args = append(args, "-u", u)
				}
			}
			if got, err := cn.exec(ctx, args, nil, nil, nil); err == nil {
				out[cn.h.name] = nonEmpty(got)
			}
		}
		return out, nil
	}
	for _, cn := range in.allConns() {
		names, err := in.connDocker(ctx, cn, append(append([]string{"ps", "-a"}, in.filters(labelKey+"="+key, labelPart+"=workload")...), "--format", "{{.Names}}")...)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			got, _ := cn.exec(ctx, []string{"docker", "logs", "--tail", n, name}, nil, nil, nil) // a container that says nothing, or is gone, is no failure of the logs
			out[cn.h.prefix()+name] = nonEmpty(got)
		}
	}
	return out, nil
}

// Health says how each key's workload is: a container key ready when every container of it runs and is healthy
// where it has a healthcheck, a package key ready when its services are active on each of its hosts.
func (in *Installation) Health(ctx context.Context) (map[string]target.Health, error) {
	out := map[string]target.Health{}
	in.platMu.Lock()
	keys := slices.Sorted(maps.Keys(in.platform))
	in.platMu.Unlock()
	for _, key := range keys {
		platform := in.platformFor(key)
		if !packaged(platform) {
			continue // the containers' health is below, from the engines
		}
		units := stringsOf(platform["services"])
		if len(units) == 0 {
			continue
		}
		cns, err := in.conns(key, platform)
		if err != nil {
			continue
		}
		h := target.Health{Ready: true}
		for _, cn := range cns {
			for _, u := range units {
				got, err := cn.exec(ctx, []string{"sh", "-c", "systemctl is-active " + shellQuote(u) + " || true"}, nil, nil, nil)
				state := strings.TrimSpace(string(got))
				ih := target.InstanceHealth{Name: cn.h.name + "/" + u, State: state, Ready: err == nil && state == "active"}
				if !ih.Ready {
					if len(state) == 0 {
						state = "unknown"
					}
					ih.Reason = "the service is " + state
					h.Ready = false
				}
				h.Instances = append(h.Instances, ih)
			}
		}
		out[key] = h
	}
	for _, cn := range in.allConns() {
		rows, err := in.connDocker(ctx, cn, append(append([]string{"ps", "-a"}, in.filters(labelPart+"=workload")...),
			"--format", `{{.Label "`+labelKey+`"}}	{{.Names}}	{{.State}}	{{.Status}}`)...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			f := strings.SplitN(row, "\t", 4)
			if len(f) < 4 {
				continue
			}
			ih := instanceHealth(cn.h.prefix()+f[1], f[2], f[3])
			h := out[f[0]]
			h.Instances = append(h.Instances, ih)
			out[f[0]] = h
		}
	}
	for k, h := range out {
		h.Ready = !slices.ContainsFunc(h.Instances, func(i target.InstanceHealth) bool { return !i.Ready })
		out[k] = h
	}
	return out, nil
}

// instanceHealth is a container's: running and, with a healthcheck, healthy.
func instanceHealth(name, state, status string) target.InstanceHealth {
	ih := target.InstanceHealth{Name: name, State: state}
	switch {
	case state != "running":
		ih.Reason = status
	case strings.Contains(status, "(unhealthy)"):
		ih.Reason = "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		ih.Reason = "its healthcheck has not passed yet"
	default:
		ih.Ready = true
	}
	return ih
}
