package host

import (
	"context"
	"io"
	"reflect"
	"testing"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/targets/host/internal/agentpb"
	"google.golang.org/grpc"
)

type fakeExecStream struct {
	grpc.ServerStreamingClient[agentpb.ExecEvent]
	lines []string
	done  bool
	code  int32
}

func (s *fakeExecStream) Recv() (*agentpb.ExecEvent, error) {
	if len(s.lines) > 0 {
		line := s.lines[0]
		s.lines = s.lines[1:]
		return &agentpb.ExecEvent{Line: line}, nil
	}
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	return &agentpb.ExecEvent{Done: true, Exit: s.code}, nil
}

type fakeAgentClient struct {
	agentpb.AgentClient
	calls   [][]string
	outputs map[string][]string
	fail    map[string]bool
	codes   map[string]int
}

func (c *fakeAgentClient) Exec(_ context.Context, request *agentpb.ExecRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ExecEvent], error) {
	c.calls = append(c.calls, append([]string(nil), request.Argv...))
	stream := &fakeExecStream{lines: append([]string(nil), c.outputs[request.Argv[0]]...)}
	code := c.codes[request.Argv[0]]
	if len(request.Argv) > 1 && request.Argv[1] == "install" {
		code = c.codes[request.Argv[0]+" install"]
	}
	stream.code = int32(code)
	if c.fail[request.Argv[0]] {
		stream.code = 1
	}
	return stream, nil
}

func TestPackageInstallChecksCandidateBeforeInstalling(t *testing.T) {
	tests := []struct {
		name        string
		family      string
		packageName string
		outputs     map[string][]string
		fail        map[string]bool
		install     []string
		query       []string
		check       []string
		codes       map[string]int
		calls       int
	}{
		{
			name: "debian current", family: "debian", packageName: "amneziawg",
			outputs: map[string][]string{"dpkg-query": {"ii 1.0"}, "apt-cache": {"Installed: 1.0", "Candidate: 1.0"}}, calls: 2,
		},
		{
			name: "debian update", family: "debian", packageName: "amneziawg",
			outputs: map[string][]string{"dpkg-query": {"ii 1.0"}, "apt-cache": {"Installed: 1.0", "Candidate: 1.1"}},
			install: []string{"apt-get", "install", "-y", "amneziawg"}, calls: 3,
		},
		{
			name: "debian absent", family: "debian", packageName: "amneziawg", fail: map[string]bool{"dpkg-query": true},
			install: []string{"apt-get", "install", "-y", "amneziawg"}, calls: 2,
		},
		{
			name: "debian pin matches", family: "debian", packageName: "amneziawg=1.0",
			outputs: map[string][]string{"dpkg-query": {"ii 1.0"}}, calls: 1,
		},
		{
			name: "debian pin differs", family: "debian", packageName: "amneziawg=1.1",
			outputs: map[string][]string{"dpkg-query": {"ii 1.0"}},
			install: []string{"apt-get", "install", "-y", "amneziawg=1.1"}, calls: 2,
		},
		{
			name: "rhel current", family: "rhel", packageName: "amneziawg",
			outputs: map[string][]string{"rpm": {"1.0-1"}},
			check:   []string{"dnf", "--quiet", "check-update", "amneziawg"}, calls: 2,
		},
		{
			name: "rhel update", family: "rhel", packageName: "amneziawg",
			outputs: map[string][]string{"rpm": {"1.0-1"}, "dnf": {"amneziawg.noarch 1.1-1 repo"}},
			codes:   map[string]int{"dnf": 100},
			check:   []string{"dnf", "--quiet", "check-update", "amneziawg"},
			install: []string{"dnf", "install", "-y", "amneziawg"}, calls: 3,
		},
		{
			name: "rhel pin matches", family: "rhel", packageName: "amneziawg=1.0-1",
			outputs: map[string][]string{"rpm": {"1.0-1"}}, calls: 1,
		},
		{
			name: "rhel pin with epoch matches", family: "rhel", packageName: "xz-libs=1:5.8.1-4.fc43",
			outputs: map[string][]string{"rpm": {"1:5.8.1-4.fc43"}},
			query:   []string{"rpm", "-q", "--queryformat", "%{EVR}\\n", "xz-libs"}, calls: 1,
		},
		{
			name: "rhel pin among multiple installed", family: "rhel", packageName: "kernel-core=2.0-2",
			outputs: map[string][]string{"rpm": {"1.0-1\n2.0-2"}},
			query:   []string{"rpm", "-q", "--queryformat", "%{EVR}\\n", "kernel-core"}, calls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeAgentClient{outputs: test.outputs, fail: test.fail, codes: test.codes}
			connection := &conn{h: &host{}, sess: &session{cli: client}}
			if err := connection.installNames(context.Background(), osFacts{family: test.family}, []string{test.packageName}, nil); err != nil {
				t.Fatalf("%T: %v", err, err)
			}
			if len(client.calls) != test.calls {
				t.Fatalf("calls = %+v, want %d", client.calls, test.calls)
			}
			if test.query != nil && !reflect.DeepEqual(client.calls[0], test.query) {
				t.Fatalf("query call = %+v, want %+v", client.calls[0], test.query)
			}
			if test.check != nil && !reflect.DeepEqual(client.calls[1], test.check) {
				t.Fatalf("update check = %+v, want %+v", client.calls[1], test.check)
			}
			if test.install != nil && !reflect.DeepEqual(client.calls[len(client.calls)-1], test.install) {
				t.Fatalf("install call = %+v, want %+v", client.calls[len(client.calls)-1], test.install)
			}
			if test.install == nil {
				for _, call := range client.calls {
					if len(call) > 1 && call[1] == "install" {
						t.Fatalf("unexpected install call: %+v", call)
					}
				}
			}
		})
	}
}

func TestInspectSeesAnAvailablePackageUpdate(t *testing.T) {
	client := &fakeAgentClient{
		outputs: map[string][]string{"rpm": {"1.0-1"}, "dnf": {"amneziawg.noarch 1.1-1 repo"}},
		codes:   map[string]int{"dnf": 100},
	}
	h := &host{}
	connection := &conn{h: h, sess: &session{
		cli:  client,
		info: &agentpb.InfoResponse{Id: "fedora", Family: "rhel"},
	}}
	h.c = connection
	in := &Installation{platform: map[string]map[string]any{"app": {}}}
	unit := render.Unit{Key: "app", Packages: map[string]render.Packages{
		"rhel": {Names: []string{"amneziawg"}},
	}}
	got, err := in.inspectHost(context.Background(), connection, unit, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.found || !got.differed {
		t.Fatalf("an installed package with an update must be found and differ: %+v", got)
	}
}
