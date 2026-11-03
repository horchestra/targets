package docker

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/horchestra/pack/pkg/target"
)

// Logs gives the tail of what each container of the key's workload wrote, stdout and stderr, by container.
func (in *Installation) Logs(ctx context.Context, key string, lines int) (map[string][]string, error) {
	out := map[string][]string{}
	for _, e := range in.engines {
		names, err := e.lines(ctx, append(append([]string{"ps", "-a"}, in.filters(labelKey+"="+key, labelPart+"=workload")...), "--format", "{{.Names}}")...)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			cmd, err := e.command(ctx, "logs", "--tail", itoa(lines), n)
			if err != nil {
				return nil, err
			}
			b, _ := cmd.CombinedOutput() // a container that says nothing, or is gone, is no failure of the logs
			var ls []string
			for l := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
				if len(l) > 0 {
					ls = append(ls, l)
				}
			}
			out[e.prefix()+n] = ls
		}
	}
	return out, nil
}

// Health says how each key's workload is: ready when every container of it runs, and is healthy where it has a
// healthcheck.
func (in *Installation) Health(ctx context.Context) (map[string]target.Health, error) {
	out := map[string]target.Health{}
	for _, e := range in.engines {
		rows, err := e.lines(ctx, append(append([]string{"ps", "-a"}, in.filters(labelPart+"=workload")...),
			"--format", `{{.Label "`+labelKey+`"}}	{{.Names}}	{{.State}}	{{.Status}}`)...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			f := strings.SplitN(row, "\t", 4)
			if len(f) < 4 {
				continue
			}
			ih := instanceHealth(e.prefix()+f[1], f[2], f[3])
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

// itoa is docker logs's --tail of n lines: all of them when none is said.
func itoa(n int) string {
	if n <= 0 {
		return "all"
	}
	return strconv.Itoa(n)
}
