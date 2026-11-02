package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// A container that runs to completion — a run-to-completion key or a hook —
// publishes credentials for the keys after it: a file
// /var/run/pack/publish/<name>/<key> it leaves behind becomes
// ${secret:<name>/<key>}, kept in the installation's state when it exits 0,
// the way a Kubernetes job publishes a Secret. Names and keys starting with a
// dot are not published: a working directory of the job's own.
//
// The directory is a volume of the key's, <installation>-<key>-publish, that
// outlives the container: a rerun finds what it published before (a CA to
// sign with, a root token) and can leave it as it is. It is mounted at
// /var/run/pack, holding publish/ writable by any user (1777), so an image
// that runs unprivileged writes there as well.
const (
	publishMount = "/var/run/pack"
	publishDir   = publishMount + "/publish"
)

func (in *Installation) publishVolume(key string) string {
	return containerName(in.service(key) + "-publish")
}

// ensurePublish creates the key's publish volume on the container's engine,
// publish/ in it world-writable, unless it is there.
func (in *Installation) ensurePublish(ctx context.Context, c container) error {
	e, v := c.Engine, in.publishVolume(c.Key)
	if _, err := e.docker(ctx, nil, nil, nil, "volume", "inspect", v); err == nil {
		return nil
	}
	if _, err := e.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
		"--label", labelKey+"="+c.Key, "--label", labelPart+"=publish", v); err != nil {
		return err
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	if err := tw.WriteHeader(&tar.Header{Name: "publish/", Mode: 0o1777, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// filled through a container that is never started, as the files volume
	helper := containerName(v + "-init")
	_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
	if _, err := e.docker(ctx, nil, nil, nil, "create", "--quiet", "--name", helper, "--entrypoint", "/pack-publish",
		"--label", labelInstallation+"="+in.installation, "--label", labelKey+"="+c.Key, "--label", labelPart+"=publish",
		"--mount", "type=volume,source="+v+",target=/pack-publish", c.Image); err != nil {
		return err
	}
	_, err := e.docker(ctx, &tb, nil, nil, "cp", "-", helper+":/pack-publish")
	_, _ = e.docker(ctx, nil, nil, nil, "rm", "--force", "--volumes", helper)
	return err
}

// collect keeps what an exited container published in the installation's
// state.
func (in *Installation) collect(ctx context.Context, c container, events func(string)) error {
	out, err := c.Engine.docker(ctx, nil, nil, nil, "cp", c.Name+":"+publishDir, "-")
	if err != nil {
		return fmt.Errorf("%s: read %s: %w", c.Name, publishDir, err)
	}
	got, err := published(out)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", c.Name, publishDir, err)
	}
	keys := map[string]int{}
	for _, p := range got {
		if err := in.state.keep(ctx, p.name, p.key, p.value); err != nil {
			return err
		}
		keys[p.name]++
	}
	if events != nil {
		for _, name := range slices.Sorted(maps.Keys(keys)) {
			events(fmt.Sprintf("%s%s published %s (%d keys)", c.Engine.prefix(), c.Name, name, keys[name]))
		}
	}
	return nil
}

type publication struct{ name, key, value string }

// published reads what docker cp gives of the publish directory: a tar of
// publish/<name>/<key>; a deeper path, or a dotted name or key, is the
// job's own.
func published(archive []byte) ([]publication, error) {
	var out []publication
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		segs := strings.Split(strings.TrimPrefix(h.Name, "./"), "/")
		if len(segs) != 3 || slices.ContainsFunc(segs[1:], func(s string) bool { return len(s) == 0 || s[0] == '.' }) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out = append(out, publication{segs[1], segs[2], string(data)})
	}
}
