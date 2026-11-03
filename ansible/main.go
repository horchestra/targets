// Command pack-target-ansible is the target of pack for classic hosts, VMs or bare metal that SSH reaches: a program of
// its own, which pack starts and speaks to over gRPC (proto/target.proto). Put it on PATH, beside pack, or in a
// directory PACK_TARGET_PATH names.
package main

import (
	"fmt"
	"os"

	"github.com/horchestra/pack/pkg/target/remote"
	"github.com/horchestra/targets/ansible/internal/ansible"
)

func main() {
	if err := remote.Serve("ansible", ansible.Factory, remote.CapRecord, remote.CapDiscover, remote.CapLogs, remote.CapHealth); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-target-ansible:", err)
		os.Exit(1)
	}
}
