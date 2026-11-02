package ansible

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

var errInactive = errors.New("inactive")

// fakeRunner records the calls instead of running ansible; fail makes the
// calls of the module return it.
type fakeRunner struct {
	calls []fakeCall
	fail  map[string]error
	// os is the OS family each host says to the module setup
	os map[string]string
}

type fakeCall struct{ pattern, module, args string }

func (f *fakeRunner) Run(_ context.Context, pattern, module, args string, events func(string)) error {
	f.calls = append(f.calls, fakeCall{pattern, module, args})
	if events != nil {
		events(pattern + " | " + module + " ok")
		if module == "setup" {
			events(`        "ansible_os_family": "` + f.os[pattern] + `"`)
		}
	}
	if err := f.fail[module]; err != nil {
		return err
	}
	return nil
}

func (f *fakeRunner) of(module string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls {
		if c.module == module {
			out = append(out, c)
		}
	}
	return out
}

// configure binds the target to an installation over a state file in a temp
// directory, with the settings given.
func configure(t *testing.T, settings target.Settings) *Installation {
	t.Helper()
	if settings == nil {
		settings = target.Settings{}
	}
	if _, ok := settings["state"]; !ok {
		settings["state"] = filepath.Join(t.TempDir(), "state.json")
	}
	inst, err := Factory("prod", settings)
	if err != nil {
		t.Fatal(err)
	}
	in := inst.(*Installation)
	// what Resolve took of the key's placement, for the keys the tests run steps of
	in.platform["pg"] = map[string]any{"hosts": []any{"a", "b"}, "packages": []any{"postgresql-15"}, "services": []any{"postgresql"}}
	in.platform["client"] = map[string]any{}
	return in
}

// An unknown setting is refused; hosts_ and secrets_ pass.
func TestCheckSettings(t *testing.T) {
	_, err := Factory("prod", target.Settings{"hosts_a_host": "10.0.0.1", "secrets_pg_password": "x", "inventory": "testdata/inventory.ini", "become": "true"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Factory("prod", target.Settings{"namespaec": "x"})
	if err == nil || !strings.Contains(err.Error(), "no setting namespaec") {
		t.Fatalf("an unknown setting: %v", err)
	}
	_, err = Factory("prod", target.Settings{"become": "perhaps"})
	if err == nil || !strings.Contains(err.Error(), "become") {
		t.Fatalf("become is true or false: %v", err)
	}
}

// The hosts come from hosts_<name>_…; each needs its host.
func TestConfigureHosts(t *testing.T) {
	in := configure(t, target.Settings{
		"hosts_b_host": "10.0.0.2", "hosts_b_user": "ops", "hosts_b_port": "2222",
		"hosts_a_host": "10.0.0.1", "hosts_a_become": "true",
	})
	if len(in.order) != 2 || in.order[0].name != "a" || in.order[1].name != "b" {
		t.Fatalf("hosts by name: %v", in.order)
	}
	b := in.hosts["b"]
	if b.address != "10.0.0.2" || b.user != "ops" || b.port != "2222" {
		t.Errorf("host b: %+v", b)
	}
	if _, err := Factory("prod", target.Settings{"hosts_a_user": "ops"}); err == nil {
		t.Error("a host without host")
	}
	if _, err := Factory("prod", target.Settings{"hosts_a_color": "blue"}); err == nil {
		t.Error("a field a host has not")
	}
	_, err := Factory("prod", target.Settings{"inventory": "testdata/no-such.ini"})
	if err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Errorf("an inventory that is not there: %v", err)
	}
}

// Without hosts_ settings the inventory is the environment's own, and a
// temporary INI one is generated from the hosts otherwise.
func TestInventory(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2",
		"hosts_b_user": "ops", "hosts_b_port": "2222", "hosts_b_become": "true"})
	path, err := writeInventory(in.order)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "a ansible_host=10.0.0.1\n") ||
		!strings.Contains(string(b), "b ansible_host=10.0.0.2 ansible_user=ops ansible_port=2222 ansible_become=true\n") {
		t.Errorf("inventory:\n%s", b)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", fi, err)
	}

	// an external inventory: hosts are its names, validated nowhere
	in = configure(t, target.Settings{"inventory": "testdata/inventory.ini"})
	if _, err := in.Resolve("pg", render.Placement{Raw: map[string]any{"hosts": []any{"pg1"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Resolve("pg", render.Placement{}); err == nil {
		t.Error("no hosts named, and the inventory's are unknown here")
	}
}

// unit is one key for the execute and convert tests: packages, services, a
// config file with a placeholder, a secret file.
func unit() *render.Unit {
	mode := fs.FileMode(0o640)
	return &render.Unit{
		Key: "pg",
		Files: []render.File{{Path: "/etc/pg/postgresql.conf", Mode: &mode,
			Content: []byte("password=${secret:pg/password}")}},
		SecretFiles: []render.SecretFile{{Path: "/etc/pg/pg.keytab", Secret: render.SecretRef{Name: "pg", Key: "keytab"}}},
	}
}

var creds = []render.Credential{
	{Name: "pg", Leaf: "password", Value: "s3cret"},
	{Name: "pg", Leaf: "keytab", Value: "KEYTAB"},
}

// apply installs the packages on the key's hosts, delivers the files with
// the secrets resolved, starts and enables the services; the credentials are
// kept in the state.
func TestApply(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	r := &fakeRunner{}
	in.runner = r
	if err := in.Execute(context.Background(), target.Step{N: 1, Kind: "apply", Key: "pg", Unit: unit(), Credentials: creds},
		func(string) {}); err != nil {
		t.Fatal(err)
	}
	pkgs := r.of("package")
	if len(pkgs) != 1 || pkgs[0].pattern != "a:b" || pkgs[0].args != "name=postgresql-15 state=present" {
		t.Errorf("packages: %+v", pkgs)
	}
	copies := r.of("copy")
	if len(copies) != 4 {
		t.Fatalf("a file and a secret file per host: %+v", copies)
	}
	if !strings.Contains(copies[0].args, `"dest":"/etc/pg/postgresql.conf"`) ||
		!strings.Contains(copies[0].args, "password=s3cret") || !strings.Contains(copies[0].args, `"mode":"0640"`) {
		t.Errorf("the file, resolved, with its mode: %s", copies[0].args)
	}
	if !strings.Contains(copies[1].args, `"dest":"/etc/pg/pg.keytab"`) || !strings.Contains(copies[1].args, "KEYTAB") {
		t.Errorf("the secret file: %s", copies[1].args)
	}
	// the directory of each file is made before the file is copied into it
	dirs := r.of("file")
	if len(dirs) != 4 || dirs[0].args != "path='/etc/pg' state=directory" {
		t.Errorf("the directories of the files: %+v", dirs)
	}
	first := -1
	for i, c := range r.calls {
		if c.module == "file" && first < 0 {
			first = i
		}
		if c.module == "copy" {
			if first < 0 || first > i {
				t.Errorf("a file is copied before its directory is there: %+v", r.calls)
			}
			break
		}
	}
	svcs := r.of("service")
	if len(svcs) != 1 || svcs[0].args != "name='postgresql' state=started enabled=yes" {
		t.Errorf("services: %+v", svcs)
	}
	if v, ok, err := in.state.secret("pg", "password"); err != nil || !ok || v != "s3cret" {
		t.Errorf("the credential kept: %q %v %v", v, ok, err)
	}
}

// wait is the key's services active; stop stops them; remove stops them and
// deletes the delivered files — the packages stay.
func TestWaitStopRemove(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	r := &fakeRunner{}
	in.runner = r
	ctx := context.Background()
	if err := in.Execute(ctx, target.Step{N: 2, Kind: "wait", Key: "pg", Unit: unit(), Timeout: "5s"}, nil); err != nil {
		t.Fatal(err)
	}
	cmds := r.of("command")
	if len(cmds) != 2 || cmds[0].args != "'systemctl' 'is-active' 'postgresql'" {
		t.Errorf("is-active: %+v", cmds)
	}

	r.calls = nil
	if err := in.Execute(ctx, target.Step{N: 3, Kind: "stop", Key: "pg", Previous: unit()}, nil); err != nil {
		t.Fatal(err)
	}
	if svcs := r.of("service"); len(svcs) != 2 || svcs[0].args != "name='postgresql' state=stopped" {
		t.Errorf("stop: %+v", svcs)
	}

	r.calls = nil
	if err := in.Execute(ctx, target.Step{N: 4, Kind: "remove", Key: "pg", Previous: unit()}, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(r.of("file")); n != 4 {
		t.Errorf("a file and a secret file per host removed: %d", n)
	}
	for _, c := range r.of("file") {
		if !strings.HasSuffix(c.args, "state=absent") {
			t.Errorf("absent: %s", c.args)
		}
	}
	if n := len(r.of("package")); n != 0 {
		t.Errorf("the packages stay: %d", n)
	}

	// a key without services: wait and stop are nothing
	bare := &render.Unit{Key: "client"}
	r.calls = nil
	if err := in.Execute(ctx, target.Step{N: 5, Kind: "wait", Key: "client", Unit: bare}, nil); err != nil {
		t.Fatal(err)
	}
	if err := in.Execute(ctx, target.Step{N: 6, Kind: "stop", Key: "client", Previous: bare}, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Errorf("no calls for a key without services: %+v", r.calls)
	}
}

// A service not active within the timeout fails the wait.
func TestWaitTimeout(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	in.runner = &fakeRunner{fail: map[string]error{"command": errInactive}}
	err := in.Execute(context.Background(), target.Step{N: 2, Kind: "wait", Key: "pg", Unit: unit(), Timeout: "1ms"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("not active within the timeout: %v", err)
	}
}

// A hook is its command on the key's first host.
func TestHook(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	r := &fakeRunner{}
	in.runner = r
	st := target.Step{N: 4, Kind: "hook", Key: "pg", Name: "init", Unit: unit(), Command: []string{"initdb", "-D", "/var/lib/pg"}, Credentials: creds}
	if err := in.Execute(context.Background(), st, func(string) {}); err != nil {
		t.Fatal(err)
	}
	cmds := r.of("command")
	if len(cmds) != 1 || cmds[0].pattern != "a" || cmds[0].args != "'initdb' '-D' '/var/lib/pg'" {
		t.Errorf("the hook: %+v", cmds)
	}
	if _, ok, err := in.state.secret("pg", "password"); !ok || err != nil {
		t.Error("the step's credentials are kept")
	}
}

// apply, wait and hook of nothing are an error.
func TestNoUnit(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1"})
	in.runner = &fakeRunner{}
	ctx := context.Background()
	for _, st := range []target.Step{
		{N: 1, Kind: "apply", Key: "pg"},
		{N: 2, Kind: "wait", Key: "pg"},
		{N: 3, Kind: "hook", Key: "pg", Name: "init"},
		{N: 4, Kind: "remove", Key: "pg"},
	} {
		if err := in.Execute(ctx, st, nil); err == nil {
			t.Errorf("%s of no unit", st.Kind)
		}
	}
	if err := in.Execute(ctx, target.Step{N: 5, Kind: "dance", Key: "pg"}, nil); err == nil ||
		!strings.Contains(err.Error(), "no step kind") {
		t.Errorf("a step kind that is none: %v", err)
	}
}

// The record goes to the state file and comes back; pack ps lists every
// installation's; delete removes the entry, credentials with it.
func TestState(t *testing.T) {
	dir := t.TempDir()
	settings := target.Settings{"hosts_a_host": "10.0.0.1", "state": filepath.Join(dir, "state.json")}
	in := configure(t, settings)
	if _, ok, err := in.ReadInstalled(); ok || err != nil {
		t.Fatalf("nothing is installed yet: %v %v", ok, err)
	}
	if err := in.WriteInstalled([]byte(`{"name":"prod"}`)); err != nil {
		t.Fatal(err)
	}
	if err := in.state.keep("pg", "password", "s3cret"); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := in.ReadInstalled()
	if !ok || err != nil || string(rec) != `{"name":"prod"}` {
		t.Fatalf("%s %v %v", rec, ok, err)
	}
	if err := in.WriteInstalled([]byte("not json")); err == nil {
		t.Error("a record that is not JSON")
	}
	if err := in.DeleteInstalled(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := in.ReadInstalled(); ok {
		t.Error("the record is still there")
	}
	if _, ok, _ := in.state.secret("pg", "password"); !ok {
		t.Error("the credentials stay, as the data does")
	}
}

// ${secret:<name>/<key>}: the step's credentials first, then the state's,
// then the environment's secrets_<name>_<key>.
func TestResolver(t *testing.T) {
	in := configure(t, target.Settings{"secrets_pg_user": "admin"})
	if err := in.state.keep("pg", "password", "kept"); err != nil {
		t.Fatal(err)
	}
	resolve := in.resolver(creds)
	if v, _ := resolve("pg/password"); v != "s3cret" {
		t.Errorf("the step's credential: %q", v)
	}
	if v, _ := in.resolver(nil)("pg/password"); v != "kept" {
		t.Errorf("the state's: %q", v)
	}
	if v, _ := in.resolver(nil)("pg/user"); v != "admin" {
		t.Errorf("the environment's: %q", v)
	}
	if _, err := in.resolver(nil)("nope/x"); err == nil {
		t.Error("a secret nothing has")
	}
	if _, err := in.resolver(nil)("nokey"); err == nil {
		t.Error("no <name>/<key>")
	}
}

// Packages given by OS family: every host gets the list of its family, hosts of one family in one call, the family
// asked of a host once.
func TestApplyPackagesByFamily(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2", "hosts_c_host": "10.0.0.3"})
	r := &fakeRunner{os: map[string]string{"a": "RedHat", "b": "Debian", "c": "RedHat"}}
	in.runner = r
	u := unit()
	u.Files, u.SecretFiles = nil, nil
	in.platform["pg"] = map[string]any{
		"hosts": []any{"a", "b", "c"},
		"packages": map[string]any{
			"RedHat": []any{"moby-engine", "docker-cli"},
			"Debian": []any{"docker.io"},
		},
	}
	step := target.Step{N: 1, Kind: "apply", Key: "pg", Unit: u}
	if err := in.Execute(context.Background(), step, func(string) {}); err != nil {
		t.Fatal(err)
	}
	pkgs := r.of("package")
	if len(pkgs) != 2 ||
		pkgs[0].pattern != "b" || pkgs[0].args != "name=docker.io state=present" ||
		pkgs[1].pattern != "a:c" || pkgs[1].args != "name=moby-engine,docker-cli state=present" {
		t.Errorf("packages by family: %+v", pkgs)
	}
	if err := in.Execute(context.Background(), step, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.of("setup")); n != 3 {
		t.Errorf("a host is asked its family once, not %d setups for 3 hosts", n)
	}

	r.os["b"] = "Suse"
	in2 := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2", "hosts_c_host": "10.0.0.3"})
	in2.runner = r
	in2.platform["pg"] = in.platform["pg"]
	err := in2.Execute(context.Background(), step, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "Suse") || !strings.Contains(err.Error(), "Debian, RedHat") {
		t.Errorf("a family the pack has no packages for: %v", err)
	}
}

// The target's half of a key's placement is the entries of its keys: it reads; one it has no word for is refused.
func TestPlatformOf(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	own, err := in.platformOf("pg", render.Placement{Raw: map[string]any{
		"packages": []any{"postgresql-15", "postgresql-contrib"}, "hosts": []any{"b"}, "services": []any{"postgresql"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := stringsOf(own["packages"]); !slices.Equal(got, []string{"postgresql-15", "postgresql-contrib"}) {
		t.Errorf("packages: %v", got)
	}
	if got := stringsOf(own["hosts"]); !slices.Equal(got, []string{"b"}) {
		t.Errorf("hosts: %v", got)
	}
	if _, err := in.platformOf("pg", render.Placement{Raw: map[string]any{"replicas": 3}}); err == nil ||
		!strings.Contains(err.Error(), "packages, hosts, services") {
		t.Errorf("an entry the target has no word for: %v", err)
	}
}

// A key runs on the hosts its placement names — every host when it names none — and is reached at the first host's
// address; with replicas, an instance per host.
func TestResolve(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	res, err := in.Resolve("pg", render.Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Service != "prod-pg" || res.Address != "10.0.0.1" {
		t.Errorf("%s at %s", res.Service, res.Address)
	}
	if res.Instances != nil {
		t.Errorf("a key with no replicas has no instances: %v", res.Instances)
	}
	two := 2
	res, err = in.Resolve("zk", render.Placement{Replicas: &two})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Instances) != 2 || res.Instances[0].Name != "prod-zk-0" || res.Instances[0].Address != "10.0.0.1" ||
		res.Instances[1].Address != "10.0.0.2" {
		t.Errorf("instances: %v", res.Instances)
	}
	res, err = in.Resolve("pg", render.Placement{Raw: map[string]any{"hosts": []any{"b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "10.0.0.2" {
		t.Errorf("the key's host: %s", res.Address)
	}
	if got := stringsOf(in.platformFor("pg")["hosts"]); !slices.Equal(got, []string{"b"}) {
		t.Errorf("Resolve remembers the placement for the steps: %v", got)
	}
	if _, err := in.Resolve("pg", render.Placement{Raw: map[string]any{"hosts": []any{"nope"}}}); err == nil {
		t.Error("a host the environment has not")
	}
	res, err = in.Resolve("pg", render.Placement{Reach: &render.Reach{Instances: []string{"pg.example"}, Ports: map[string]int{"pg": 5432}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach == nil || res.Reach.Instances[0] != "pg.example" || res.Reach.Ports["pg"] != 5432 {
		t.Errorf("reach: %+v", res.Reach)
	}
}

// changing is a runner whose copy reports a change on the hosts named, the way ansible prints it.
type changing struct {
	fakeRunner
	changed map[string]bool
	facts   string
}

func (c *changing) Run(ctx context.Context, pattern, module, args string, events func(string)) error {
	if module == "setup" && events != nil {
		events(c.facts)
		c.calls = append(c.calls, fakeCall{pattern, module, args})
		return nil
	}
	if module == "copy" && c.changed[pattern] && events != nil {
		events(pattern + " | CHANGED => {")
	}
	return c.fakeRunner.Run(ctx, pattern, module, args, events)
}

const debian13 = `"ansible_distribution": "Debian", "ansible_distribution_major_version": "13", "ansible_distribution_release": "trixie", "ansible_os_family": "Debian", "ansible_architecture": "aarch64"`

// The entry of the most exact name is taken, the order is the pack's, ${os:…} is the host's, a package file goes through apt.
func TestApplyPackages(t *testing.T) {
	r := &changing{facts: debian13}
	in := &Installation{}
	u := render.Unit{Key: "docker", Packages: map[string]render.Packages{
		"fedora": {Names: []string{"wrong"}},
		"debian": {Names: []string{"wrong"}},
		"Debian-13": {
			Prepare:  []string{"ca-certificates"},
			Repo:     []render.File{{Path: "/etc/apt/k.asc", URL: "https://x/gpg"}, {Path: "/etc/apt/s", Content: []byte("Suites: ${os:codename}\n")}},
			Names:    []string{"engine", "https://x/${os:arch}.deb"},
			Files:    []render.File{{Path: "/etc/d.json", Content: []byte("{}")}},
			Services: []string{"docker"},
		},
	}}
	p, f, err := in.packagesOn(context.Background(), r, u, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.applyPackages(context.Background(), r, u, "a", p, f, nil, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range r.calls {
		if c.module != "file" && c.module != "setup" {
			got = append(got, c.module)
		}
	}
	want := []string{"package", "get_url", "copy", "apt", "apt", "package", "copy", "service"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if s := r.of("copy")[0].args; !strings.Contains(s, "Suites: trixie") {
		t.Errorf("the codename: %s", s)
	}
	if s := r.of("apt")[1].args; s != "deb='https://x/arm64.deb'" && s != "deb=https://x/arm64.deb" && s != `deb="https://x/arm64.deb"` {
		t.Errorf("the package file: %s", s)
	}
}

// An OS no one named is an error that says where to name it; the environment may name it without touching the pack.
func TestPackagesOfAnOSThePackDoesNotKnow(t *testing.T) {
	r := &changing{facts: `"ansible_distribution": "Alpine", "ansible_distribution_major_version": "3", "ansible_os_family": "Alpine"`}
	u := render.Unit{Key: "docker", Packages: map[string]render.Packages{"debian": {Names: []string{"x"}}}}
	in := &Installation{platform: map[string]map[string]any{}}
	if _, _, err := in.packagesOn(context.Background(), r, u, "a"); err == nil || !strings.Contains(err.Error(), "keys.docker.packages") {
		t.Errorf("no entry: %v", err)
	}
	in.platform["docker"] = map[string]any{"packages": map[string]any{"alpine": []any{"docker"}}, "services": []any{"docker"}}
	p, _, err := in.packagesOn(context.Background(), r, u, "a")
	if err != nil || p.Names[0] != "docker" || p.Services[0] != "docker" {
		t.Errorf("named in the environment: %+v, %v", p, err)
	}
}

// A quote in a file survives: the content does not go through ansible's key=value.
func TestCopyKeepsQuotes(t *testing.T) {
	r := &fakeRunner{}
	if err := copyFile(context.Background(), r, "a", "/f", []byte("x = '1'\n"), nil, nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.of("copy")[0].args), &got); err != nil || got["content"] != "x = '1'\n" {
		t.Errorf("args %s: %v", r.of("copy")[0].args, err)
	}
}

// A unit is restarted only when a file changed: the notify of a playbook.
func TestRestartOnlyWhenAFileChanged(t *testing.T) {
	for changed, restarts := range map[bool]int{true: 1, false: 0} {
		r := &changing{facts: debian13, changed: map[string]bool{"a": changed}}
		in := &Installation{}
		u := render.Unit{Key: "k", Packages: map[string]render.Packages{"debian": {
			Names: []string{"x"}, Files: []render.File{{Path: "/f", Content: []byte("x")}}, Services: []string{"svc"},
		}}}
		p, f, err := in.packagesOn(context.Background(), r, u, "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := in.applyPackages(context.Background(), r, u, "a", p, f, nil, nil); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range r.of("service") {
			if strings.Contains(c.args, "state=restarted") {
				n++
			}
		}
		if n != restarts {
			t.Errorf("changed=%v: %d restarts, %v", changed, n, r.of("service"))
		}
	}
}

// With mirror_registry, oci-proxy runs on the host's loopback while the host installs, and is gone after, also on a failure.
func TestMirrorAroundAnInstallation(t *testing.T) {
	r := &changing{facts: debian13}
	f, err := (&Installation{}).factsOf(context.Background(), r, "a")
	if err != nil {
		t.Fatal(err)
	}
	m := &mirror{registry: "reg:5000", plainHTTP: true, port: "8081"}
	boom := errors.New("boom")
	// the program is built by make oci-proxy: without it the target says so, and installs nothing
	if _, err := ociProxy.ReadFile("ociproxy/oci-proxy-linux-arm64"); err != nil {
		if err := withMirror(context.Background(), r, "a", f, m, nil, func() error { return boom }); err == nil || !strings.Contains(err.Error(), "make oci-proxy") {
			t.Fatalf("no program: %v", err)
		}
		t.Skip("oci-proxy is not built here")
	}
	if err := withMirror(context.Background(), r, "a", f, m, nil, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	cmds := r.of("command")
	if !strings.Contains(cmds[1].args, "systemd-run") || strings.Contains(cmds[1].args, "reg:5000") {
		t.Errorf("start, its configuration in the environment file: %v", cmds[1])
	}
	var env map[string]string
	for _, c := range r.of("copy") {
		var fields map[string]string
		if json.Unmarshal([]byte(c.args), &fields) == nil && fields["dest"] == mirrorEnvFile {
			env = fields
		}
	}
	if !strings.Contains(env["content"], "OCI_PROXY_REGISTRY=reg:5000\n") || !strings.Contains(env["content"], "OCI_PROXY_ADDR=127.0.0.1:8081\n") ||
		!strings.Contains(env["content"], "OCI_PROXY_PLAIN_HTTP=true\n") || env["mode"] != "0600" {
		t.Errorf("the environment file: %v", env)
	}
	if !strings.Contains(cmds[len(cmds)-1].args, "systemctl stop") {
		t.Errorf("not stopped: %v", cmds)
	}
	if len(r.of("wait_for")) != 1 {
		t.Errorf("does not wait for it: %v", r.calls)
	}
}

func TestMirrorSettings(t *testing.T) {
	t.Setenv("U", "me")
	t.Setenv("P", "secret")
	m, err := mirrorOf(target.Settings{"mirror_registry": "oci://reg", "mirror_plain_http": "true", "mirror_username_env": "U", "mirror_password_env": "P"})
	if err != nil || m.user != "me" || m.password != "secret" || !m.plainHTTP || m.port != "8081" {
		t.Errorf("%+v %v", m, err)
	}
	if m, err := mirrorOf(target.Settings{}); m != nil || err != nil {
		t.Errorf("none: %+v %v", m, err)
	}
	if _, err := mirrorOf(target.Settings{"mirror_registry": "r", "mirror_username_env": "U"}); err == nil {
		t.Error("a user without a password")
	}
	if _, err := mirrorOf(target.Settings{"mirror_registry": "r", "mirror_username_env": "NOPE", "mirror_password_env": "NOPE"}); err == nil {
		t.Error("variables that are not set")
	}
}
