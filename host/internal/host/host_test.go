package host

import (
	"archive/tar"
	"bytes"
	"testing"

	"github.com/horchestra/pack/pkg/render"
)

func factory(t *testing.T, settings map[string]string) *Installation {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	in, err := Factory("prod", settings)
	if err != nil {
		t.Fatal(err)
	}
	return in.(*Installation)
}

func TestFactorySettings(t *testing.T) {
	if _, err := Factory("prod", map[string]string{"nonsense": "1"}); err == nil {
		t.Error("a setting the target does not know is taken")
	}
	if _, err := Factory("prod", map[string]string{"hosts_a_port": "22"}); err == nil {
		t.Error("a host without host is taken")
	}
	in := factory(t, map[string]string{
		"hosts_b_host": "10.0.0.2", "hosts_b_user": "pack", "hosts_b_insecure": "true",
		"hosts_a_host": "10.0.0.1",
	})
	if len(in.order) != 2 || in.order[0].name != "a" || in.order[1].name != "b" {
		t.Errorf("hosts = %v", in.order)
	}
	if !in.order[1].sudo() {
		t.Error("a user that is not root runs the agent under sudo")
	}
	if in.order[0].sudo() {
		t.Error("root needs no sudo")
	}
	if in.local != nil {
		t.Error("hosts named, and the local one too")
	}
}

func TestFactoryLocal(t *testing.T) {
	in := factory(t, nil)
	if in.local == nil || len(in.order) != 1 {
		t.Fatalf("no hosts: the local machine is the one host, got %v", in.order)
	}
	hosts, err := in.keyHosts("db", map[string]any{})
	if err != nil || len(hosts) != 1 {
		t.Errorf("keyHosts = %v, %v", hosts, err)
	}
}

func TestResolveContainersLocal(t *testing.T) {
	in := factory(t, nil)
	res, err := in.Resolve("db", render.Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "prod-db.prod" {
		t.Errorf("address = %q", res.Address)
	}
	if in.platformFor("db")["replicas"] != int64(1) {
		t.Errorf("replicas = %v", in.platformFor("db")["replicas"])
	}
}

func TestResolveSpread(t *testing.T) {
	in := factory(t, map[string]string{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	two := 2
	res, err := in.Resolve("db", render.Placement{Replicas: &two, Raw: map[string]any{"hosts": []any{"a", "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "10.0.0.1" || len(res.Instances) != 2 || res.Instances[1].Address != "10.0.0.2" {
		t.Errorf("resolved = %+v", res)
	}
	if _, err := in.Resolve("x", render.Placement{Raw: map[string]any{"hosts": []any{"c"}}}); err == nil {
		t.Error("a host that is no host of the environment is taken")
	}
	if _, err := in.Resolve("x", render.Placement{Raw: map[string]any{"publish": map[string]any{"http": 8080}}}); err == nil {
		t.Error("publish across hosts is taken")
	}
}

func TestResolvePackaged(t *testing.T) {
	in := factory(t, map[string]string{"hosts_a_host": "10.0.0.1"})
	own, err := in.platformOf("db", render.Placement{Raw: map[string]any{"packages": []any{"postgresql"}, "services": []any{"postgresql"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !packaged(own) {
		t.Error("packages and services make a packaged key")
	}
	if _, err := in.platformOf("db", render.Placement{Raw: map[string]any{"services": []any{"x"}, "mounts": []any{"/a:/b"}}}); err == nil {
		t.Error("a packaged key takes a container's property")
	}
	if _, err := in.platformOf("db", render.Placement{Raw: map[string]any{"unknown": 1}}); err == nil {
		t.Error("an unknown placement entry is taken")
	}
}

func TestSubstitute(t *testing.T) {
	resolve := func(ref string) (string, error) {
		if ref == "ca/cert" {
			return "PEM", nil
		}
		return "", &testErr{ref}
	}
	s, err := substitute("a ${secret:ca/cert} b", resolve)
	if err != nil || s != "a PEM b" {
		t.Errorf("substitute = %q, %v", s, err)
	}
	if _, err := substitute("${secret:no/key}", resolve); err == nil {
		t.Error("an unknown secret resolves")
	}
}

type testErr struct{ ref string }

func (e *testErr) Error() string { return "no " + e.ref }

func TestState(t *testing.T) {
	in := factory(t, nil)
	if _, ok, err := in.state.read(); err != nil || ok {
		t.Fatalf("read = %v, %v", ok, err)
	}
	if err := in.state.write([]byte(`{"keys":{}}`)); err != nil {
		t.Fatal(err)
	}
	b, ok, err := in.state.read()
	if err != nil || !ok || string(b) != `{"keys":{}}` {
		t.Fatalf("read = %q, %v, %v", b, ok, err)
	}
	if err := in.state.keep("ca", "cert", "PEM"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := in.state.secret("ca", "cert")
	if err != nil || !ok || v != "PEM" {
		t.Fatalf("secret = %q, %v, %v", v, ok, err)
	}
	if err := in.state.remove(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := in.state.read(); ok {
		t.Error("the record is there after remove")
	}
}

func TestPublished(t *testing.T) {
	out, err := published(mkTar(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].name != "ca" || out[0].key != "cert" || out[0].value != "PEM" {
		t.Errorf("published = %+v", out)
	}
}

// mkTar is a tar of publish/<name>/<key>, as docker cp gives it: a dotted name and a deep path are the job's own.
func mkTar(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	put := func(name, data string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	put("publish/ca/cert", "PEM")
	put("publish/.hidden/key", "no")
	put("publish/ca/deep/key", "no")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
