package local

import (
	"path/filepath"
	"testing"

	"github.com/horchestra/pack/pkg/target"
)

func TestRegister(t *testing.T) {
	Register()
	Register()
	installation, err := target.Open("host", "local-test", map[string]string{
		"state": filepath.Join(t.TempDir(), "host-state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Namespace() != "local-test" {
		t.Fatalf("namespace = %q", installation.Namespace())
	}
	if closer, ok := installation.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
