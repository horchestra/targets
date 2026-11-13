// Package local registers the host target for in-process applies. With no
// hosts in the environment, the target executes on the current machine without
// starting pack-target-host or using SSH.
package local

import (
	"sync"

	"github.com/horchestra/pack/pkg/target"
	"github.com/horchestra/targets/host/internal/host"
)

var registerOnce sync.Once

func Register() {
	registerOnce.Do(func() { target.Register("host", host.Factory) })
}
