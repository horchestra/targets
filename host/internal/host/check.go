package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

// Inspect says whether the key's hosts already hold exactly what the unit says — the files' bytes and modes, the
// packages at their pins, the services enabled, a container's image — so that an installation that keeps no
// record (state: observe) plans from what is there: absent — nothing of the key; differs — there, not as the
// unit says; matches — as it says.
func (in *Installation) Inspect(ctx context.Context, u render.Unit, creds []render.Credential) (target.Hold, error) {
	if len(u.Image) > 0 {
		return in.inspectContainers(ctx, u)
	}
	cns, err := in.conns(u.Key, in.platformFor(u.Key))
	if err != nil {
		return "", err
	}
	resolve := in.resolver(creds)
	found, differed := false, false
	for i, cn := range cns {
		st, err := in.inspectHost(ctx, cn, u, i, resolve)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", u.Key, cn.h.name, err)
		}
		found = found || st.found
		differed = differed || st.differed
	}
	switch {
	case !found:
		return target.HoldAbsent, nil
	case differed:
		return target.HoldDiffers, nil
	}
	return target.HoldMatches, nil
}

// verdict is what one host's inspection found: something of the key is there (found), and something is not as
// the unit says (differed). A missing piece both differs and is not found.
type verdict struct{ found, differed bool }

func (v *verdict) see(ok bool, exists bool) {
	v.found = v.found || exists
	v.differed = v.differed || !ok
}

// inspectHost is one host against the unit: the packages' entry of its OS (or the bare files and services), the
// kernel's drop-ins, the firewall left aside — its rules are the OS's own once saved.
func (in *Installation) inspectHost(ctx context.Context, cn *conn, u render.Unit, instance int, resolve func(string) (string, error)) (verdict, error) {
	var v verdict
	info, err := cn.info(ctx)
	if err != nil {
		return v, err
	}
	f := factsOf(info)
	platform := in.platformFor(u.Key)
	p, _, err := in.packagesOn(ctx, cn, u)
	if err != nil {
		return v, err
	}
	var files []render.File
	var svcs []string
	if p != nil {
		files = append(slices.Clone(p.Repo), p.Files...)
		svcs = p.Services
		for _, n := range p.Names {
			n = f.resolve(n)
			if strings.Contains(n, "://") { // a package file: its presence is the files' below
				continue
			}
			ok := cn.hasPackage(ctx, f, n)
			v.see(ok, ok)
		}
		if len(p.Sysctls) > 0 || len(p.Modules) > 0 {
			kv, err := in.kernelVerdict(ctx, cn, u.Key, p)
			if err != nil {
				return v, err
			}
			v.found = v.found || kv.found
			v.differed = v.differed || kv.differed
		}
	} else {
		files = u.Files
		svcs = stringsOf(platform["services"])
		if pkgs := stringsOf(platform["packages"]); len(pkgs) > 0 {
			for _, n := range pkgs {
				n = f.resolve(n)
				ok := cn.hasPackage(ctx, f, n)
				v.see(ok, ok)
			}
		}
	}
	for _, file := range files {
		fv, err := in.fileVerdict(ctx, cn, f, file, instance, resolve)
		if err != nil {
			return v, err
		}
		v.found = v.found || fv.found
		v.differed = v.differed || fv.differed
	}
	for _, file := range u.SecretFiles {
		// a secret the host already holds is the key's own: not read, not compared — its presence is enough;
		// a missing one is generated and delivered by the apply
		_, _, exists, err := cn.stateOf(ctx, file.Path)
		if err != nil {
			return v, err
		}
		v.see(exists, exists)
	}
	for _, svc := range svcs {
		err := cn.serviceEnabled(ctx, svc)
		v.see(err == nil, err == nil)
	}
	return v, nil
}

// fileVerdict is one file of the unit against the host: its content's sha256 and its mode. A file the host does
// not have differs and is not found; a URL file that is not of this machine's disk (a registry's, the web's) is
// judged by its presence and mode alone — its bytes are not pack's to hash here.
func (in *Installation) fileVerdict(ctx context.Context, cn *conn, f osFacts, file render.File, instance int, resolve func(string) (string, error)) (verdict, error) {
	var mode uint32
	if file.Mode != nil {
		mode = uint32(file.Mode.Perm())
	}
	if len(file.URL) > 0 {
		url := f.resolve(file.URL)
		if local, ok := strings.CutPrefix(url, "file://"); ok {
			sum, err := shaOfFile(local)
			if err != nil {
				return verdict{}, fmt.Errorf("%s: %w", local, err)
			}
			return cn.contentVerdict(ctx, file.Path, sum, mode)
		}
		if sum, ok, err := hashLoopbackURL(ctx, url); err != nil {
			return verdict{}, fmt.Errorf("%s: %w", url, err)
		} else if ok {
			// a loopback URL is this machine's own server (a registry mirror's): its bytes can be hashed here
			return cn.contentVerdict(ctx, file.Path, sum, mode)
		}
		_, _, exists, err := cn.stateOf(ctx, file.Path)
		return verdict{found: exists, differed: !exists}, err
	}
	data := file.Content
	if file.PerInstance() {
		if instance >= len(file.Instances) || file.Instances[instance] == nil {
			return verdict{}, nil // the instance's section was left empty: no file
		}
		data = file.Instances[instance]
	}
	s, err := substitute(string(data), resolve)
	if err != nil {
		return verdict{}, fmt.Errorf("%s: %w", file.Path, err)
	}
	return cn.contentVerdict(ctx, file.Path, shaOf([]byte(f.resolve(s))), mode)
}

// contentVerdict is a path against the content and the mode it should have.
func (c *conn) contentVerdict(ctx context.Context, path, sum string, mode uint32) (verdict, error) {
	has, hasMode, exists, err := c.stateOf(ctx, path)
	if err != nil {
		return verdict{}, fmt.Errorf("%s: %w", path, err)
	}
	if !exists {
		return verdict{}, nil
	}
	ok := has == sum && (mode == 0 || modeOf(hasMode) == mode)
	return verdict{found: true, differed: !ok}, nil
}

// kernelVerdict are the key's kernel drop-ins against what the packages' entry would write.
func (in *Installation) kernelVerdict(ctx context.Context, cn *conn, key string, p *render.Packages) (verdict, error) {
	var v verdict
	sysctlF, modulesF := kernelFiles(key)
	if len(p.Modules) > 0 {
		var b strings.Builder
		for _, m := range p.Modules {
			b.WriteString(m + "\n")
		}
		fv, err := cn.contentVerdict(ctx, modulesF, shaOf([]byte(b.String())), 0o644)
		if err != nil {
			return v, err
		}
		v.found = v.found || fv.found
		v.differed = v.differed || fv.differed
	}
	if len(p.Sysctls) > 0 {
		var b strings.Builder
		for _, k := range slices.Sorted(maps.Keys(p.Sysctls)) {
			fmt.Fprintf(&b, "%s=%s\n", k, p.Sysctls[k])
		}
		fv, err := cn.contentVerdict(ctx, sysctlF, shaOf([]byte(b.String())), 0o644)
		if err != nil {
			return v, err
		}
		v.found = v.found || fv.found
		v.differed = v.differed || fv.differed
	}
	return v, nil
}

// serviceEnabled: the unit is enabled on the host.
func (c *conn) serviceEnabled(ctx context.Context, svc string) error {
	_, err := c.exec(ctx, []string{"systemctl", "is-enabled", svc}, nil, nil, nil)
	return err
}

// inspectContainers is a key with an image against its hosts: every container there and running the unit's
// image.
func (in *Installation) inspectContainers(ctx context.Context, u render.Unit) (target.Hold, error) {
	cs, err := in.containers(u)
	if err != nil {
		return "", err
	}
	found, differed := false, false
	for _, c := range cs {
		out, err := c.docker(ctx, nil, nil, nil, "container", "inspect", "-f", "{{.Config.Image}}", c.Name)
		if err != nil {
			differed = true // not there, or the engine does not answer as expected
			continue
		}
		found = true
		if strings.TrimSpace(string(out)) != c.Image {
			differed = true
		}
	}
	switch {
	case !found:
		return target.HoldAbsent, nil
	case differed:
		return target.HoldDiffers, nil
	}
	return target.HoldMatches, nil
}

// hashLoopbackURL is the sha256 of what an http(s) URL on the loopback serves — this machine's own server, a
// registry mirror's proxy of it. ok is false for any other URL: its bytes are not pack's to fetch here.
func hashLoopbackURL(ctx context.Context, url string) (sum string, ok bool, err error) {
	if !strings.HasPrefix(url, "http://127.0.0.1") && !strings.HasPrefix(url, "http://localhost") &&
		!strings.HasPrefix(url, "https://127.0.0.1") && !strings.HasPrefix(url, "https://localhost") {
		return "", false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", true, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, nil // the server is not up: content cannot be judged, the presence below decides
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", true, nil // answered, but not the artifact: a sum that matches nothing — the apply's fetch says why
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", true, err
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

var _ target.Inspector = (*Installation)(nil)
