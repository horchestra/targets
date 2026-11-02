package ansible

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"github.com/horchestra/pack/pkg/target"
)

// ociProxy holds the program of oci-proxy for the hosts' architectures: `make oci-proxy` builds them.
//
//go:embed all:ociproxy
var ociProxy embed.FS

const fileMode0600 = fs.FileMode(0o600)

const (
	mirrorUnit    = "pack-oci-proxy"
	mirrorBinary  = "/opt/pack/oci-proxy"
	mirrorEnvFile = "/run/pack-oci-proxy.env"
)

// mirror is how oci-proxy runs on a host while the target installs: mirror_registry names the registry that keeps the
// package repositories; nil when the environment names none, and the hosts take their packages from where the pack says.
type mirror struct {
	registry  string
	plainHTTP bool
	port      string
	user      string
	password  string
}

func mirrorOf(s target.Settings) (*mirror, error) {
	reg := s.Get("mirror_registry", "")
	if len(reg) == 0 {
		return nil, nil
	}
	m := &mirror{registry: reg, port: s.Get("mirror_port", "8081")}
	if v := s.Get("mirror_plain_http", ""); len(v) > 0 {
		var err error
		if m.plainHTTP, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("mirror_plain_http: %w", err)
		}
	}
	userEnv, passEnv := s.Get("mirror_username_env", ""), s.Get("mirror_password_env", "")
	if (len(userEnv) == 0) != (len(passEnv) == 0) {
		return nil, fmt.Errorf("mirror_username_env and mirror_password_env go together")
	}
	if len(userEnv) > 0 {
		m.user, m.password = os.Getenv(userEnv), os.Getenv(passEnv)
		if len(m.user) == 0 || len(m.password) == 0 {
			return nil, fmt.Errorf("mirror: %s and %s are not set", userEnv, passEnv)
		}
	}
	return m, nil
}

// withMirror runs what installs on a host with oci-proxy listening on the host's loopback, and takes it away after: the
// host keeps nothing of it and opens nothing to the outside.
func withMirror(ctx context.Context, r Runner, host string, f osFacts, m *mirror, events func(string), install func() error) error {
	if m == nil {
		return install()
	}
	if err := startMirror(ctx, r, host, f, m, events); err != nil {
		return fmt.Errorf("oci-proxy: %w", err)
	}
	defer stopMirror(context.WithoutCancel(ctx), r, host, events)
	return install()
}

func startMirror(ctx context.Context, r Runner, host string, f osFacts, m *mirror, events func(string)) error {
	name := "ociproxy/oci-proxy-linux-" + f.arch
	program, err := ociProxy.ReadFile(name)
	if err != nil {
		return fmt.Errorf("this target has no oci-proxy for %s (make oci-proxy): %w", f.arch, err)
	}
	tmp, err := os.CreateTemp("", "oci-proxy-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(program); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := r.Run(ctx, host, "file", "path="+quote(dirOf(mirrorBinary))+" state=directory", events); err != nil {
		return err
	}
	if err := r.Run(ctx, host, "copy", "src="+quote(tmp.Name())+" dest="+quote(mirrorBinary)+" mode=0755", events); err != nil {
		return err
	}
	env := "OCI_PROXY_REGISTRY=" + m.registry + "\nOCI_PROXY_ADDR=127.0.0.1:" + m.port + "\n"
	if m.plainHTTP {
		env += "OCI_PROXY_PLAIN_HTTP=true\n"
	}
	if len(m.user) > 0 {
		env += "OCI_PROXY_USER=" + m.user + "\nOCI_PROXY_PASSWORD=" + m.password + "\n"
	}
	mode := fileMode0600
	if err := copyFile(ctx, r, host, mirrorEnvFile, []byte(env), &mode, nil); err != nil { // no events: it holds the password
		return err
	}
	// one left by a run that died
	if err := command(ctx, r, host, []string{"sh", "-c", "systemctl stop " + mirrorUnit + " 2>/dev/null || true"}, events); err != nil {
		return err
	}
	argv := []string{"systemd-run", "--unit=" + mirrorUnit, "--collect", "-p", "EnvironmentFile=" + mirrorEnvFile, mirrorBinary}
	if err := command(ctx, r, host, argv, events); err != nil {
		return err
	}
	return r.Run(ctx, host, "wait_for", "host=127.0.0.1 port="+m.port+" timeout=30", events)
}

func stopMirror(ctx context.Context, r Runner, host string, events func(string)) {
	_ = command(ctx, r, host, []string{"sh", "-c", "systemctl stop " + mirrorUnit + " 2>/dev/null || true"}, events)
	_ = removeFile(ctx, r, host, mirrorBinary, events)
	_ = removeFile(ctx, r, host, mirrorEnvFile, events)
}
