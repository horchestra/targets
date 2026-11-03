package ansible

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/horchestra/pack/pkg/target"
)

const setupAnswer = `db1 | SUCCESS => {
    "ansible_facts": {
        "ansible_architecture": "aarch64",
        "ansible_date_time": {"iso8601": "2026-11-03T12:00:00Z"},
        "ansible_distribution": "Debian",
        "ansible_distribution_major_version": "13",
        "ansible_dns": {"nameservers": ["10.0.0.2"]},
        "ansible_memtotal_mb": 7951,
        "ansible_mounts": [{"mount": "/", "size_total": 104857600000, "size_available": 52428800000}],
        "ansible_processor_vcpus": 4,
        "ansible_service_mgr": "systemd"
    },
    "changed": false
}`

func TestSetupFacts(t *testing.T) {
	h := setupFacts(adhocJSON(strings.Split(setupAnswer, "\n")))
	if h.OS != "debian" || h.OSVersion != "13" || h.Arch != "arm64" || h.CPUs != 4 || h.MemoryMB != 7951 ||
		len(h.Disks) != 1 || h.Disks[0].FreeMB != 50000 || h.DNS[0] != "10.0.0.2" || h.Time == nil || h.Runtimes[0] != "systemd" {
		t.Errorf("facts = %+v", h)
	}
	if adhocJSON([]string{"db1 | UNREACHABLE! => nothing"}) != nil {
		t.Error("an answer without JSON")
	}
}

func TestListening(t *testing.T) {
	got := listening([]string{
		"db1 | CHANGED | rc=0 >>",
		`tcp   LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*    users:(("sshd",pid=1,fd=3))`,
		`tcp   LISTEN 0      4096            [::]:22            [::]:*    users:(("sshd",pid=1,fd=4))`,
		`udp   UNCONN 0      0          127.0.0.1:323        0.0.0.0:*`,
	})
	want := []target.Port{{Port: 22, Proto: "tcp", Process: "sshd"}, {Port: 323, Proto: "udp"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ports = %+v", got)
	}
	if v := commandOutput([]string{"db1 | CHANGED | rc=0 >>", "29.1.0", ""}); len(v) != 1 || v[0] != "29.1.0" {
		t.Errorf("output = %v", v)
	}
	if v := engineVersion([]string{`{"Client":{"Version":"29.1.0"},`, `"Server":{"Version":"29.1.1"}}`}); v != "29.1.1" {
		t.Errorf("engine = %q", v)
	}
	if v := engineVersion([]string{`{"Client":{"Version":"29.1.0"},"Server":null}`}); len(v) > 0 {
		t.Errorf("no engine: %q", v)
	}
}

// A host that does not answer is told unreachable; the others' facts are still told.
func TestDiscoverAnUnreachableHost(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	in.runner = &oneDown{fakeRunner: &fakeRunner{}, down: "b"}
	facts, err := in.Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := facts.Hosts["a"]; !ok || len(facts.Unreachable) != 1 || len(facts.Unreachable["b"]) == 0 {
		t.Errorf("facts = %+v", facts)
	}
}

type oneDown struct {
	*fakeRunner
	down string
}

func (o *oneDown) Run(ctx context.Context, pattern, module, args string, events func(string)) error {
	if pattern == o.down {
		return errors.New("UNREACHABLE")
	}
	return o.fakeRunner.Run(ctx, pattern, module, args, events)
}

// The journal of the key's services on its hosts, or the host's errors when it names none.
func TestLogsAndHealth(t *testing.T) {
	in := configure(t, target.Settings{"hosts_a_host": "10.0.0.1", "hosts_b_host": "10.0.0.2"})
	r := &fakeRunner{}
	in.runner = r
	logs, err := in.Logs(context.Background(), "pg", 20)
	if err != nil || len(logs) != 2 {
		t.Fatalf("logs %v %v", logs, err)
	}
	calls := r.of("command")
	if len(calls) != 2 || !strings.Contains(calls[0].args, "journalctl --no-pager -n 20 -u postgresql") {
		t.Errorf("the services' journal: %+v", calls)
	}
	if _, err := in.Logs(context.Background(), "client", 5); err != nil || !strings.Contains(r.of("command")[2].args, "-p err") {
		t.Errorf("no services named: the host's errors: %+v %v", r.of("command"), err)
	}
	h, err := in.Health(context.Background())
	if err != nil || len(h["pg"].Instances) != 2 {
		t.Fatalf("health %+v %v", h, err)
	}
}
