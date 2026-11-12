package agent

import (
	"net/http/httptest"
	"testing"
)

// Convert reads a tag and a digest alike.
func TestConvert(t *testing.T) {
	cases := []struct{ path, path2, ref, file string }{
		{"/debian/dists:trixie/dists/trixie/InRelease", "/debian/dists", "trixie", "dists/trixie/InRelease"},
		{"/packs/openbao@sha256:abc/templates/dump.sql", "/packs/openbao", "sha256:abc", "templates/dump.sql"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		ref, opts, err := Convert(r)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if ref.Path != c.path2 || ref.Ref != c.ref {
			t.Errorf("%s = %s %s", c.path, ref.Path, ref.Ref)
		}
		if opts["org.opencontainers.image.title"] != c.file {
			t.Errorf("%s: file %q", c.path, opts["org.opencontainers.image.title"])
		}
	}
	for _, bad := range []string{"/norepo", "/repo:tag"} {
		r := httptest.NewRequest("GET", bad, nil)
		if _, _, err := Convert(r); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
