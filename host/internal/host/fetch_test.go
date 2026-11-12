package host

import (
	"strings"
	"testing"
)

// A registry file's URL becomes the mirror proxy's on the host's loopback; anything else is the author's.
func TestOCIFetchURL(t *testing.T) {
	in := &Installation{mirror: &mirror{port: "8081"}}
	digest := strings.Repeat("0", 64)
	cases := []struct{ in, want string }{
		{"oci://registry.example/packs/openbao@sha256:" + digest + "/templates/dump.sql",
			"http://127.0.0.1:8081/packs/openbao@sha256:" + digest + "/templates/dump.sql"},
		{"oci://packs/tembo:0.2.0/templates/tembo.env.ptmpl",
			"http://127.0.0.1:8081/packs/tembo:0.2.0/templates/tembo.env.ptmpl"},
		{"http://upstream.example/dists/InRelease", "http://upstream.example/dists/InRelease"},
	}
	for _, c := range cases {
		got, err := in.ociFetchURL(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
		} else if got != c.want {
			t.Errorf("%s = %q, want %q", c.in, got, c.want)
		}
	}
	// no mirror: a registry file cannot be fetched
	bare := &Installation{}
	if _, err := bare.ociFetchURL("oci://packs/p:1/templates/f"); err == nil {
		t.Error("no mirror, no error")
	}
	if got, err := bare.ociFetchURL("http://upstream.example/f"); err != nil || got != "http://upstream.example/f" {
		t.Errorf("author's url: %q, %v", got, err)
	}
}
