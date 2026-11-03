package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

// Two containers that publish one port on one host are refused.
// place is the placement of a key: its replicas (0 — none) and the entries of its `keys:` the target reads.
func place(replicas int, raw map[string]any) render.Placement {
	p := render.Placement{Raw: raw}
	if replicas > 0 {
		p.Replicas = &replicas
	}
	return p
}

func configure(t *testing.T, name string, s map[string]string) *Installation {
	t.Helper()
	in, err := Factory(name, s)
	if err != nil {
		t.Fatal(err)
	}
	return in.(*Installation)
}

func TestHosts(t *testing.T) {
	in := configure(t, "dev", map[string]string{"hosts_a_address": "10.0.0.1", "hosts_b_address": "b.lan", "hosts_b_host": "ssh://b"})
	res, err := in.Resolve("zk", place(2, map[string]any{"hosts": []any{"a", "b"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Instances) != 2 || res.Instances[0].Address != "10.0.0.1" || res.Instances[1].Address != "b.lan" || in.platformFor("zk")["replicas"] != int64(2) {
		t.Errorf("%+v", res)
	}
	for name, p := range map[string]render.Placement{
		"no hosts":        place(2, nil),
		"unknown host":    place(2, map[string]any{"hosts": []any{"x"}}),
		"one host twice":  place(2, map[string]any{"hosts": []any{"a", "a"}}),
		"replicas differ": place(3, map[string]any{"hosts": []any{"a", "b"}}),
		"publish":         place(1, map[string]any{"hosts": []any{"a"}, "publish": map[string]any{"pg": 1}}),
	} {
		if _, err := in.Resolve("pg", p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := in.Resolve("pg", place(0, map[string]any{"hosts": []any{"a", "b"}})); err == nil {
		t.Error("a key that does not scale on two hosts")
	}
	if _, err := Factory("dev", map[string]string{"hosts_a_host": "ssh://a"}); err == nil {
		t.Error("a host without an address")
	}
}

func TestPortsFree(t *testing.T) {
	a, b := &engine{name: "a"}, &engine{name: "b"}
	pg := []render.Port{{Name: "pg", Port: 5432}}
	c := func(name string, e *engine) container {
		return container{Name: name, Engine: e, Ports: pg, Publish: map[string]int{"pg": 5432}}
	}
	if err := portsFree([]container{c("x", a), c("y", b)}); err != nil {
		t.Error(err)
	}
	if err := portsFree([]container{c("x", a), c("y", a)}); err == nil {
		t.Error("5432 twice on a")
	}
}

func TestQuantity(t *testing.T) {
	for s, want := range map[string]float64{"500m": 0.5, "2": 2, "512Mi": 512 << 20, "1G": 1e9, "1.5Gi": 1.5 * (1 << 30)} {
		if got, err := quantity(s); err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	if _, err := quantity("lots"); err == nil {
		t.Error("lots is no quantity")
	}
}

func TestSubstitute(t *testing.T) {
	s, err := substitute("password=${secret:pg/password}\n", func(ref string) (string, error) { return "s3cret", nil })
	if err != nil || s != "password=s3cret\n" {
		t.Errorf("%q %v", s, err)
	}
}

// A state file keeps each installation's record and credentials apart, and
// the engine is never asked.
func TestFileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "docker.json")
	open := func(name string) *Installation {
		inst, err := Factory(name, target.Settings{"state": path, "docker": "/nonexistent/docker"})
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*Installation)
	}
	prod, dev := open("prod"), open("dev")
	if _, exists, err := prod.ReadInstalled(); err != nil || exists {
		t.Fatalf("nothing installed yet: %v %v", exists, err)
	}
	if err := prod.WriteInstalled([]byte(`{"name":"prod","state":"installed"}`)); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteInstalled([]byte(`{"name":"dev"}`)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := prod.keep(ctx, []render.Credential{{Name: "prod-pg", Leaf: "password", Value: "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	rec, exists, err := prod.ReadInstalled()
	if err != nil || !exists || string(rec) != `{"name":"prod","state":"installed"}` {
		t.Errorf("prod: %s %v %v", rec, exists, err)
	}
	if v, err := prod.resolver(ctx, nil)("prod-pg/password"); err != nil || v != "s3cret" {
		t.Errorf("kept credential: %q %v", v, err)
	}
	if _, ok, _ := dev.state.secret(ctx, "prod-pg", "password"); ok {
		t.Error("dev sees prod's credential")
	}
	// a keytab a job published is no text: kept byte for byte
	keytab := string([]byte{0x05, 0x02, 0x00, 0xff, 0xfe, 'k'})
	if err := prod.state.keep(ctx, "adh-keytabs", "hdfs-nn", keytab); err != nil {
		t.Fatal(err)
	}
	if v, err := prod.resolver(ctx, nil)("adh-keytabs/hdfs-nn"); err != nil || v != keytab {
		t.Errorf("binary credential: %q %v", v, err)
	}
	if err := prod.WriteInstalled([]byte("not json")); err == nil {
		t.Error("a record that is not JSON is refused")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi, err)
	}
}

// pack delete on a state file: the installation's record goes — its
// credentials stay, as they do on an engine — the other installations stay.
func TestFileStateDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "docker.json")
	settings := target.Settings{"state": path, "docker": "/nonexistent/docker"}
	open := func(name string) *Installation {
		inst, err := Factory(name, settings)
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*Installation)
	}
	prod, dev := open("prod"), open("dev")
	if err := prod.WriteInstalled([]byte(`{"name":"prod"}`)); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteInstalled([]byte(`{"name":"dev"}`)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := prod.keep(ctx, []render.Credential{{Name: "pg", Leaf: "password", Value: "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	if err := prod.DeleteInstalled(); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := prod.ReadInstalled(); err != nil || exists {
		t.Errorf("prod's record is gone: %v %v", exists, err)
	}
	if v, ok, _ := prod.state.secret(ctx, "pg", "password"); !ok || v != "s3cret" {
		t.Error("prod's credentials stay, as the data volumes do")
	}
	if rec, exists, err := dev.ReadInstalled(); err != nil || !exists || string(rec) != `{"name":"dev"}` {
		t.Errorf("dev stays: %s %v %v", rec, exists, err)
	}
	// deleting what is not there is no error and writes nothing
	if err := prod.DeleteInstalled(); err != nil {
		t.Error(err)
	}
}

// A setting of another target, or a typo, is refused.
func TestUnknownSetting(t *testing.T) {
	for _, s := range []target.Settings{{"namespace": "data"}, {"netwrok": "x"}, {"kubeconfig": "x"}} {
		if _, err := Factory("dev", s); err == nil {
			t.Errorf("%v accepted", s)
		}
	}
	if _, err := Factory("dev", target.Settings{"target": "docker", "network": "n", "secrets_pg_password": "x", "hosts_a_address": "a"}); err != nil {
		t.Error(err)
	}
}

// What a job leaves in publish/<name>/<key> is published; its working
// directories — deeper, or dotted — are not.
func TestPublished(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range []struct{ name, data string }{
		{"publish/", ""}, {"publish/adh-tls/", ""},
		{"publish/adh-tls/ca.crt", "CA"}, {"publish/adh-tls/dn-keystore.jks.1", "JKS"},
		{"publish/adh-tls/.ca.key", "no"}, {"publish/.work/ca.key", "no"},
		{"publish/adh-tls/hosts/x.crt", "no"}, {"publish/stray", "no"},
	} {
		h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(f.name, "/") {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(f.data))
	}
	_ = tw.Close()
	got, err := target.Published(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []target.Publication{{Name: "adh-tls", Key: "ca.crt", Value: "CA"}, {Name: "adh-tls", Key: "dn-keystore.jks.1", Value: "JKS"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("published: %v", got)
	}
}

// A value longer than an argument of argv may be (a CA bundle) is kept in
// chunks: every label fits, and they join back to the value.
func TestValueLabels(t *testing.T) {
	small := valueLabels("s3cret")
	if len(small) != 2 || small[1] != labelValue+"=czNjcmV0" {
		t.Errorf("small: %v", small)
	}
	big := strings.Repeat("x", 200<<10)
	args := valueLabels(big)
	var enc strings.Builder
	n := 0
	for i := 1; i < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i], "=")
		if len(args[i]) > 128<<10 {
			t.Fatalf("label %s is %d bytes", k, len(args[i]))
		}
		if k == labelChunks {
			if v != fmt.Sprint(n) {
				t.Errorf("chunks %s, %d labels", v, n)
			}
			continue
		}
		if k != fmt.Sprintf("%s.%d", labelValue, n) {
			t.Errorf("label %d is %s", n, k)
		}
		enc.WriteString(v)
		n++
	}
	if dec, err := base64.StdEncoding.DecodeString(enc.String()); err != nil || string(dec) != big {
		t.Errorf("the chunks do not join back: %v", err)
	}
}

// With a store in the environment the credentials — what a job published, what pack generated — are kept in it, not
// on the engine: the store is shared with the targets of the same installation.
func TestCredentialsInTheStore(t *testing.T) {
	var mu sync.Mutex
	kv := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _ := strings.CutPrefix(r.URL.Path, "/v1/secret/data/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			d, ok := kv[path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": d}})
		case http.MethodPost:
			var b struct{ Data map[string]any }
			_ = json.NewDecoder(r.Body).Decode(&b)
			kv[path] = b.Data
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	inst, err := Factory("dev", target.Settings{"store_provider": "openbao", "store_addr": srv.URL, "store_token": "t"})
	if err != nil {
		t.Fatal(err)
	}
	in := inst.(*Installation)
	if !in.usesStore() {
		t.Fatal("the store is not plugged in")
	}
	ctx := context.Background()
	if err := in.keep(ctx, []render.Credential{
		{Name: "tls-nn", Leaf: "server.key", Value: "k"},
		{Name: "dev-kafka-jaas-password", Leaf: "password", Value: "p", Store: "pack/dev/dev-kafka-jaas-password", Field: "password"},
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if kv["pack/dev/tls-nn"]["server.key"] != "k" {
		t.Errorf("a published credential is in the store: %v", kv)
	}
	if _, has := kv["pack/dev/dev-kafka-jaas-password"]; has {
		t.Error("a credential the store already holds is not written again")
	}
	mu.Unlock()
	resolve := in.resolver(ctx, nil)
	if v, err := resolve("tls-nn/server.key"); err != nil || v != "k" {
		t.Errorf("read back through ${secret:…}: %q %v", v, err)
	}
	if _, err := resolve("tls-nn/nothing"); err == nil {
		t.Error("a field nobody wrote")
	}
}

// A readiness that is a command is run by pack with docker exec: the image may have no shell for the
// engine's own healthcheck.
func TestExecProbe(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	// the fake docker: `exec <name> <argv…>` succeeds for the command "ok"
	script := "#!/bin/sh\n[ \"$1\" = exec ] && [ \"$3\" = ok ]\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	eng := &engine{bin: bin}
	for _, tc := range []struct {
		name   string
		health []string
		want   bool
	}{
		{"the command answers", []string{"CMD", "ok", "-c", "x"}, true},
		{"the command fails", []string{"CMD", "bad"}, false},
		{"a shell form is the engine's", []string{"CMD-SHELL", "ok"}, false},
		{"no readiness", nil, false},
	} {
		if got := execProbe(context.Background(), container{Name: "c", Engine: eng, Health: tc.health}); got != tc.want {
			t.Errorf("%s: execProbe = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPublish(t *testing.T) {
	in := configure(t, "dev", nil)
	res, err := in.Resolve("pg", place(0, map[string]any{"publish": map[string]any{"pg": int64(15432)}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "dev-pg.dev" || in.platformFor("pg")["publish"].(map[string]any)["pg"] != int64(15432) {
		t.Errorf("%+v", res)
	}
	if _, err := in.Resolve("pg", place(2, map[string]any{"publish": map[string]any{"pg": int64(15432)}})); err == nil {
		t.Error("two replicas cannot publish one host port")
	}
	if _, err := in.Resolve("pg", place(0, map[string]any{"colour": "red"})); err == nil {
		t.Error("an entry of the key the target does not read is refused")
	}
}

// The runtime half of a key's placement — compose's words — comes from the environment: the container's name and
// hostname, the address its hostname is, a fixed address, the volumes the installation's keys share and the host's
// paths.
func TestRuntime(t *testing.T) {
	in := configure(t, "adh-show", map[string]string{"network": "adh.local", "network_subnet": "10.99.0.0/24"})
	res, err := in.Resolve("kdc", place(0, map[string]any{
		"name": "kdc", "hostname": "kdc.adh.local", "ip": "10.99.0.10",
		"mounts":      []any{"stand:/etc/ldap-kdc/stand:ro", "/var/run/docker.sock:/var/run/docker.sock:ro"},
		"sysctls":     map[string]any{"net.ipv4.ip_unprivileged_port_start": "0"},
		"securityOpt": []any{"label=disable"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Service != "kdc" || res.Address != "kdc.adh.local" {
		t.Errorf("kdc resolves to %s at %s", res.Service, res.Address)
	}
	c, err := in.container(render.Unit{Key: "kdc", Image: "ldap-kdc"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(in.runtimeArgs(c), " ")
	for _, want := range []string{"--ip 10.99.0.10", "--sysctl net.ipv4.ip_unprivileged_port_start=0", "--security-opt label=disable",
		"--mount type=volume,source=adh-show-stand,target=/etc/ldap-kdc/stand,readonly", "--volume /var/run/docker.sock:/var/run/docker.sock:ro"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q miss %q", args, want)
		}
	}
	if c.Name != "kdc" || c.Hostname != "kdc" || c.Domain != "adh.local" {
		t.Errorf("container %s, hostname %s, domain %s", c.Name, c.Hostname, c.Domain)
	}
	if _, err = in.Resolve("krb5-conf", place(0, map[string]any{"mounts": []any{"stand/krb5:/out"}, "restart": "on-failure"})); err != nil {
		t.Fatal(err)
	}
	j, err := in.container(render.Unit{Key: "krb5-conf", Image: "ldap-kdc", RestartPolicy: "Never"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "adh-show-krb5-conf" || j.Restart != "on-failure" || !j.Completes ||
		len(j.Shared) != 1 || j.Shared[0].Subpath != "krb5" || j.Shared[0].Volume != "adh-show-stand" {
		t.Errorf("krb5-conf: %+v", j)
	}
	if _, _, err := in.parseMounts("x", []string{"stand:/x:z,ro,rw", "relative"}); err == nil {
		t.Error("a mount without a target is refused")
	}
}

// How a neighbour on another target reaches a key: the engine's external address and the ports it publishes; across
// hosts the host of each instance and every port as itself; what the operator says wins.
func TestReach(t *testing.T) {
	in := configure(t, "dev", map[string]string{"external_address": "docker.lan"})
	res, err := in.Resolve("nn", place(0, map[string]any{"publish": map[string]any{"https": int64(9871)}}))
	if err != nil {
		t.Fatal(err)
	}
	if rc := res.Reach; rc == nil || len(rc.Instances) != 1 || rc.Instances[0] != "docker.lan" || rc.Ports["https"] != 9871 || rc.SamePorts {
		t.Errorf("one engine: %+v", res.Reach)
	}
	said := place(0, nil)
	said.Reach = &render.Reach{Instances: []string{"edge.lan"}, Ports: map[string]int{"https": 443}}
	if res, err = in.Resolve("nn", said); err != nil {
		t.Fatal(err)
	}
	if rc := res.Reach; rc == nil || rc.Instances[0] != "edge.lan" || rc.Ports["https"] != 443 {
		t.Errorf("the operator says: %+v", res.Reach)
	}

	if res, _ := configure(t, "dev", nil).Resolve("nn", place(0, nil)); res.Reach != nil {
		t.Errorf("an engine that is told no external address says nothing: %+v", res.Reach)
	}

	spread := configure(t, "dev", map[string]string{"hosts_a_address": "10.0.0.1", "hosts_b_address": "b.lan"})
	if res, err = spread.Resolve("zk", place(2, map[string]any{"hosts": []any{"a", "b"}})); err != nil {
		t.Fatal(err)
	}
	if rc := res.Reach; rc == nil || !rc.SamePorts || len(rc.Instances) != 2 || rc.Instances[0] != "10.0.0.1" || rc.Instances[1] != "b.lan" {
		t.Errorf("across hosts: %+v", res.Reach)
	}
}

// An action of the pack runs in the key's first instance as it runs: docker exec, its output the step's events.
func TestExecInTheInstance(t *testing.T) {
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "docker"), filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n[ \"$1\" = exec ] && echo dumped\n[ \"$3\" != fail ]\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := configure(t, "dev", map[string]string{"docker": bin})
	u := render.Unit{Key: "pg", Image: "images/postgres:16", Replicas: 2}
	var said []string
	st := target.Step{Key: "pg", Kind: "exec", Name: "backup", Unit: &u, Command: []string{"pg_dump", "--file=/var/lib/postgresql/data/b"}}
	if err := in.Execute(context.Background(), st, func(l string) { said = append(said, l) }); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(log)
	if strings.TrimSpace(string(args)) != "exec dev-pg-0 pg_dump --file=/var/lib/postgresql/data/b" || len(said) != 1 || said[0] != "dumped" {
		t.Errorf("args %q, said %v", args, said)
	}
	st.Command = []string{"fail"}
	if err := in.Execute(context.Background(), st, nil); err == nil || !strings.Contains(err.Error(), "exec backup in dev-pg-0") {
		t.Errorf("a command that fails: %v", err)
	}
}
