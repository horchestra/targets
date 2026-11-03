package docker

import (
	"reflect"
	"testing"

	"github.com/horchestra/pack/pkg/target"
)

func TestPublishedPorts(t *testing.T) {
	got := publishedPorts([]string{"0.0.0.0:8200->8200/tcp, [::]:8200->8200/tcp", "", "127.0.0.1:5432->5432/tcp, 0.0.0.0:53->53/udp", "[::1]:9000->9000/tcp"})
	want := []target.Port{{Port: 53, Proto: "udp", Process: "docker"}, {Port: 5432, Proto: "tcp", Process: "docker"}, {Port: 8200, Proto: "tcp", Process: "docker"}, {Port: 9000, Proto: "tcp", Process: "docker"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ports = %+v", got)
	}
}

func TestOSOf(t *testing.T) {
	for in, want := range map[string][2]string{
		"Debian GNU/Linux 13 (trixie)":      {"debian", "13"},
		"Docker Desktop":                    {"docker", ""},
		"Fedora Linux 42 (Container Image)": {"fedora", "42"},
	} {
		if os, v := osOf(in, ""); os != want[0] || v != want[1] {
			t.Errorf("%s: %s %s", in, os, v)
		}
	}
	if a := archOf("aarch64"); a != "arm64" {
		t.Errorf("arch %s", a)
	}
}

func TestInstanceHealth(t *testing.T) {
	for _, c := range []struct {
		state, status string
		ready         bool
	}{
		{"running", "Up 3 minutes (healthy)", true},
		{"running", "Up 3 minutes", true},
		{"running", "Up 10 seconds (health: starting)", false},
		{"running", "Up 3 minutes (unhealthy)", false},
		{"exited", "Exited (1) 2 minutes ago", false},
	} {
		if got := instanceHealth("c", c.state, c.status); got.Ready != c.ready || !got.Ready && len(got.Reason) == 0 {
			t.Errorf("%s %q: %+v", c.state, c.status, got)
		}
	}
}
