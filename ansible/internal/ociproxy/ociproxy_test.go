package ociproxy

import (
	"net/http/httptest"
	"testing"

	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestConvert(t *testing.T) {
	r := httptest.NewRequest("GET", "/mirrors/docker-debian:29/dists/trixie/InRelease", nil)
	ref, opts, err := Convert(r)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Path != "/mirrors/docker-debian" || ref.Ref != "29" || opts[ocispecv1.AnnotationTitle] != "dists/trixie/InRelease" {
		t.Errorf("%+v %v", ref, opts)
	}
	for _, bad := range []string{"/mirrors/docker", "/:1/a", "/a:/b", "/a:1", "/a:1/"} {
		if _, _, err := Convert(httptest.NewRequest("GET", bad, nil)); err == nil {
			t.Errorf("%s: must be refused", bad)
		}
	}
}
