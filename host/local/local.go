// Package local registers the host target for in-process applies. With no
// hosts in the environment, the target executes on the current machine without
// starting pack-target-host or using SSH.
package local

import (
	"io/fs"
	"sync"

	"github.com/horchestra/pack/pkg/target"
	"github.com/horchestra/targets/host/internal/host"
)

var registerOnce sync.Once

func Register() {
	registerOnce.Do(func() { target.Register("host", host.Factory) })
}

// SetAgents gives an embedding program the agents for remote hosts:
// pack-host-agent-<goos>-<goarch> at the root of f. Only a program that
// deploys to remote hosts in-process needs it; the agents are the ones
// a host release publishes on their own (pack-host-agent-linux-{amd64,arm64})
// or `make agent` builds.
func SetAgents(f fs.FS) { host.SetAgents(f) }
