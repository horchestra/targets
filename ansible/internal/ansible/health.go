package ansible

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/horchestra/pack/pkg/target"
)

// Logs gives, for each host of the key, the tail of its services' journal — or, when the key names none, of the
// host's errors (journalctl -p err): what the failed step left behind on the machine.
func (in *Installation) Logs(ctx context.Context, key string, lines int) (map[string][]string, error) {
	r, err := in.transport()
	if err != nil {
		return nil, err
	}
	platform := in.platformFor(key)
	hosts, err := in.keyHosts(key, platform)
	if err != nil {
		return nil, err
	}
	n := strconv.Itoa(max(lines, 1))
	units := stringsOf(platform["services"])
	out := map[string][]string{}
	for _, h := range hosts {
		args := "journalctl --no-pager -n " + n + " -p err"
		if len(units) > 0 {
			args = "journalctl --no-pager -n " + n
			for _, u := range units {
				args += " -u " + u
			}
		}
		var got []string
		if r.Run(ctx, h, "command", args, func(l string) { got = append(got, l) }) == nil {
			out[h] = commandOutput(got)
		}
	}
	return out, nil
}

// Health says, of every key whose services the environment names, whether they are active on each of its hosts.
func (in *Installation) Health(ctx context.Context) (map[string]target.Health, error) {
	r, err := in.transport()
	if err != nil {
		return nil, err
	}
	in.platMu.Lock()
	keys := slices.Sorted(maps.Keys(in.platform))
	in.platMu.Unlock()
	out := map[string]target.Health{}
	for _, key := range keys {
		platform := in.platformFor(key)
		units := stringsOf(platform["services"])
		if len(units) == 0 {
			continue
		}
		hosts, err := in.keyHosts(key, platform)
		if err != nil {
			continue
		}
		h := target.Health{Ready: true}
		for _, host := range hosts {
			for _, u := range units {
				var got []string
				err := r.Run(ctx, host, "command", "systemctl is-active "+u, func(l string) { got = append(got, l) })
				state := strings.Join(commandOutput(got), " ")
				ih := target.InstanceHealth{Name: host + "/" + u, State: state, Ready: err == nil && state == "active"}
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
	return out, nil
}
