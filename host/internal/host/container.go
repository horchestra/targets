package host

import (
	"fmt"
	"io/fs"
	"maps"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/horchestra/pack/pkg/render"
)

const (
	labelInstallation = "pack.horchestra.io/installation"
	labelKey          = "pack.horchestra.io/key"
	labelPart         = "pack.horchestra.io/part" // workload | hook | data | files | publish | shared
	labelInstance     = "pack.horchestra.io/instance"
	labelHash         = "pack.horchestra.io/hash"
	labelContainer    = "pack.horchestra.io/container"
)

// container is one container of a key as its host's engine runs it — the one model both `pack template` and the
// steps (docker create) are made from.
type container struct {
	Conn     *conn
	Name     string
	Key      string
	Instance int
	Part     string
	Image    string
	// Entrypoint and Cmd: the unit's command and args.
	Entrypoint, Cmd []string
	Env             []render.EnvVar
	Ports           []render.Port
	// PublishAs: a hook publishes into publish/<PublishAs>/, which the target makes for it — the image needs no mkdir.
	PublishAs string
	Publish   map[string]int // port name → host port
	// Aliases are its other names on the network: on one host, the key's.
	Aliases        []string
	Restart        string // unless-stopped | no
	CPUs           string
	CPUShares      int64
	Memory         int64
	MemReservation int64
	Health         []string // a shell command
	Labels         map[string]string
	Volumes        []mount // claims: volume → path
	// the runtime half
	Hostname, Domain string
	IP, User         string
	DNS, GroupAdd    []string
	SecurityOpt      []string
	Sysctls          map[string]string
	Shared           []shared
	Binds            []bind
	// Completes: it runs to completion, whatever its restart policy
	Completes bool
	Libraries []mount // image → path
	// Files are the instance's config files, ${secret:…} unresolved, kept in the container's files volume on its
	// host and mounted file by file.
	Files       []file
	SecretFiles []secretFile
}

type mount struct{ Source, Target string }

// shared is a mount of a volume the installation's keys share.
type shared struct {
	Volume, Subpath, Target string
	ReadOnly                bool
}

// bind is a mount of a path of the host, as docker -v takes it.
type bind struct{ Source, Target, Options string }

type file struct {
	Path string
	Mode *fs.FileMode
	Data []byte
}

type secretFile struct {
	Path string
	Mode *fs.FileMode
	Ref  render.SecretRef
}

// containers are a key's containers: <service> for a key that does not scale, <service>-<i> for each replica.
func (in *Installation) containers(u render.Unit) ([]container, error) {
	var out []container
	for i := range max(u.Replicas, 1) {
		c, err := in.container(u, i)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (in *Installation) container(u render.Unit, i int) (container, error) {
	if len(u.Image) == 0 {
		return container{}, fmt.Errorf("component %s has no image: a container step needs one", u.Component)
	}
	plat := in.platformFor(u.Key)
	svc := in.serviceOf(u.Key, plat)
	cn, err := in.connOf(u.Key, plat, i)
	if err != nil {
		return container{}, err
	}
	c := container{Conn: cn, Name: svc, Key: u.Key, Instance: i, Part: "workload", Image: u.Image,
		Entrypoint: u.Command, Cmd: u.Args, Env: u.Env, Ports: u.Ports, Restart: "unless-stopped"}
	if u.Replicas > 0 {
		c.Name = fmt.Sprintf("%s-%d", svc, i)
		if !in.spread() {
			c.Aliases = []string{svc}
		}
	}
	if u.RestartPolicy == "Never" {
		c.Restart, c.Completes = "no", true
	}
	if r := strOf(plat, "restart"); len(r) > 0 {
		c.Restart = r
	}
	c.Aliases = append(c.Aliases, listOf(plat, "aliases")...)
	if h := strOf(plat, "hostname"); len(h) > 0 && u.Replicas == 0 {
		c.Hostname, c.Domain = splitHost(h)
	} else if !in.spread() {
		c.Hostname, c.Domain = c.Name, in.network
	} else {
		c.Hostname = c.Name
	}
	c.IP, c.User = strOf(plat, "ip"), strOf(plat, "user")
	c.DNS, c.GroupAdd, c.SecurityOpt = listOf(plat, "dns"), listOf(plat, "groupAdd"), listOf(plat, "securityOpt")
	if c.Sysctls = mapOf(plat, "sysctls"); len(c.Sysctls) == 0 {
		c.Sysctls = nil
	}
	if c.Shared, c.Binds, err = in.parseMounts(u.Key, listOf(plat, "mounts")); err != nil {
		return c, err
	}
	if in.spread() {
		// a neighbour reaches the instance at <host address>:<port>
		c.Publish = map[string]int{}
		for _, p := range u.Ports {
			c.Publish[p.Name] = p.Port
		}
	} else if pub, ok := plat["publish"].(map[string]any); ok {
		c.Publish = map[string]int{}
		for name, v := range pub {
			if !slices.ContainsFunc(u.Ports, func(p render.Port) bool { return p.Name == name }) {
				return c, fmt.Errorf("publish: %s is no port of %s", name, u.Key)
			}
			port, _ := toInt(v)
			c.Publish[name] = port
		}
	}
	if r := u.Resources; r != nil {
		var err error
		if q, ok := r.Limits["cpu"]; ok {
			cpu, e := quantity(q)
			c.CPUs, err = strconv.FormatFloat(cpu, 'f', -1, 64), e
		}
		if q, ok := r.Requests["cpu"]; ok && err == nil {
			cpu, e := quantity(q)
			c.CPUShares, err = int64(math.Round(cpu*1024)), e
		}
		if q, ok := r.Limits["memory"]; ok && err == nil {
			m, e := quantity(q)
			c.Memory, err = int64(m), e
		}
		if q, ok := r.Requests["memory"]; ok && err == nil {
			m, e := quantity(q)
			c.MemReservation, err = int64(m), e
		}
		if err != nil {
			return c, fmt.Errorf("resources: %w", err)
		}
	}
	if rd := u.Readiness; rd != nil {
		// probed at the container's own name, not 127.0.0.1: a service bound to the address its name resolves to
		// answers there, and so does one bound to every address
		host := c.Name
		switch {
		case rd.TCP != nil:
			p := strconv.Itoa(rd.TCP.Port)
			c.Health = []string{"CMD-SHELL", "nc -z " + host + " " + p + " || bash -c '</dev/tcp/" + host + "/" + p + "'"}
		case rd.HTTP != nil:
			url := fmt.Sprintf("http://%s:%d%s", host, rd.HTTP.Port, rd.HTTP.Path)
			c.Health = []string{"CMD-SHELL", "curl -fsS -o /dev/null " + url + " || wget -q -O /dev/null " + url}
		case len(rd.Exec) > 0:
			c.Health = append([]string{"CMD"}, rd.Exec...)
		}
	}
	c.Labels = map[string]string{}
	maps.Copy(c.Labels, u.Annotations)
	maps.Copy(c.Labels, u.Labels)
	c.Labels[labelInstallation] = in.installation
	c.Labels[labelKey] = u.Key
	c.Labels[labelPart] = c.Part
	c.Labels[labelInstance] = strconv.Itoa(i)

	for _, d := range u.Data {
		c.Volumes = append(c.Volumes, mount{d.Name + "-" + c.Name, d.Path})
	}
	for _, f := range u.Files {
		data := f.Content
		if f.PerInstance() {
			if i >= len(f.Instances) || f.Instances[i] == nil {
				continue // the instance's section was left empty: no file
			}
			data = f.Instances[i]
		}
		c.Files = append(c.Files, file{f.Path, f.Mode, data})
	}
	for _, f := range u.SecretFiles {
		ref := f.Secret
		if f.PerInstance {
			ref.Key = fmt.Sprintf("%s.%d", ref.Key, i)
		}
		c.SecretFiles = append(c.SecretFiles, secretFile{f.Path, f.Mode, ref})
	}
	return c, nil
}

// publishes are a container's published ports, <host port>:<port>.
func publishes(c container) []any {
	var ports []any
	for _, p := range c.Ports {
		if host, ok := c.Publish[p.Name]; ok {
			ports = append(ports, fmt.Sprintf("%d:%d", host, p.Port))
		}
	}
	return ports
}

// containerName is s as a container or volume name: [a-zA-Z0-9][a-zA-Z0-9_.-]*.
func containerName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		}
		return '-'
	}, s)
	return strings.TrimLeft(s, "_.-")
}

// shellJoin quotes argv for sh -c.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// parseMounts splits the platform's mounts into the installation's volumes and the host's paths.
func (in *Installation) parseMounts(key string, specs []string) ([]shared, []bind, error) {
	var vols []shared
	var binds []bind
	for _, s := range specs {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) < 2 || len(parts[0]) == 0 || !strings.HasPrefix(parts[1], "/") {
			return nil, nil, fmt.Errorf("%s: mount %q is <source>:<target>[:<options>]", key, s)
		}
		opts := ""
		if len(parts) == 3 {
			opts = parts[2]
		}
		if strings.HasPrefix(parts[0], "/") {
			binds = append(binds, bind{parts[0], parts[1], opts})
			continue
		}
		vol, sub, _ := strings.Cut(parts[0], "/")
		ro := false
		for o := range strings.SplitSeq(opts, ",") {
			switch o {
			case "", "z", "Z", "rw":
			case "ro":
				ro = true
			default:
				return nil, nil, fmt.Errorf("%s: mount %q: a volume takes ro only", key, s)
			}
		}
		vols = append(vols, shared{Volume: containerName(in.installation + "-" + vol), Subpath: path.Clean("/" + sub)[1:], Target: parts[1], ReadOnly: ro})
	}
	return vols, binds, nil
}
