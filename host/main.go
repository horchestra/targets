// Command pack-target-host is the host target of pack, a program of its own: pack starts it and speaks to it
// over gRPC (proto/target.proto). Put it on PATH, beside pack, or in a directory PACK_TARGET_PATH names.
//
// The target covers the hosts SSH reaches — what the docker and the ansible targets were: a key with an image is
// containers on the host's engine, a key without one is OS packages, files and systemd services. The doing is the
// agent's (cmd/agent), a binary the driver brings to the host and speaks to over the SSH connection.
package main

import (
	"fmt"
	"os"

	"github.com/horchestra/pack/pkg/target/remote"
	"github.com/horchestra/targets/host/internal/host"
)

func main() {
	if err := remote.Serve("host", host.Factory, remote.CapRecord, remote.CapDiscover, remote.CapLogs, remote.CapHealth); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-target-host:", err)
		os.Exit(1)
	}
}
