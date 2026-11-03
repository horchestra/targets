// Command pack-target-k8s is the Kubernetes target of pack, a program of its own: pack starts it and speaks to it over
// gRPC (proto/target.proto). Put it on PATH, beside pack, or in a directory PACK_TARGET_PATH names.
package main

import (
	"fmt"
	"os"

	"github.com/horchestra/pack/pkg/target/remote"
	"github.com/horchestra/targets/k8s/internal/k8s"
)

func main() {
	if err := remote.Serve("k8s", k8s.Factory, remote.CapRecord, remote.CapDiscover); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-target-k8s:", err)
		os.Exit(1)
	}
}
