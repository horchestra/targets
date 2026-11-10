package host

import (
	"testing"

	"github.com/horchestra/pack/pkg/render"
)

func TestHostHook(t *testing.T) {
	if hostHook(nil) {
		t.Fatal("a hook without a unit needs the container path's validation")
	}
	if hostHook(&render.Unit{Image: "example.test/agent:v1"}) {
		t.Fatal("container hook must run in a container")
	}
	if !hostHook(&render.Unit{Packages: map[string]render.Packages{"debian": {}}}) {
		t.Fatal("packages hook must run on the host")
	}
}
