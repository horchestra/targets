// Command pack-target-docker is the Docker target of pack, a program of its own: pack starts it and speaks to it over
// gRPC (proto/target.proto). Put it on PATH, beside pack, or in a directory PACK_TARGET_PATH names.
package main

import (
	"fmt"
	"os"

	"github.com/horchestra/pack/pkg/target/remote"
	"github.com/horchestra/targets/docker/internal/docker"
)

func main() {
	if err := remote.Serve("docker", docker.Factory, remote.CapRecord); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-target-docker:", err)
		os.Exit(1)
	}
}
