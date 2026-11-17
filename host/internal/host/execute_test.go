package host

import (
	"testing"

	"github.com/horchestra/pack/pkg/render"
)

func TestWorkloadKind(t *testing.T) {
	both := &render.Unit{
		Workload: render.WorkloadPackages,
		Image:    "images/openbao:2.6.5",
		Packages: map[string]render.Packages{"debian": {}},
	}
	if kind, err := workloadOf(both); err != nil || kind != render.WorkloadPackages {
		t.Fatalf("an explicit packages unit: %q, %v", kind, err)
	}
	old := &render.Unit{Image: "images/openbao:2.6.5", Packages: map[string]render.Packages{"debian": {}}}
	if kind, err := workloadOf(old); err != nil || kind != render.WorkloadPackages {
		t.Fatalf("a recorded unit with packages stays on the host: %q, %v", kind, err)
	}
	if _, err := workloadOf(&render.Unit{Workload: "future"}); err == nil {
		t.Fatal("an unknown workload kind must be refused")
	}
}

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
