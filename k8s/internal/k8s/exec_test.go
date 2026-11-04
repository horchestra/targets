package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/horchestra/pack/pkg/target"
)

// An action of the pack runs in the key's first pod as it runs, its first container; its output is the step's
// events, what it says on stderr is in the error when it fails.
func TestExecInThePod(t *testing.T) {
	in := &Installation{installation: "dev", namespace: "pack"}
	in.once.Do(func() {})
	in.clientset = k8sfake.NewClientset(&corev1.Pod{
		Name: "dev-pg-0", Namespace: "pack",
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})
	var got string
	in.execIn = func(_ context.Context, pod, container string, command []string, stdout, stderr io.Writer) error {
		got = pod + "/" + container + ": " + strings.Join(command, " ")
		if command[0] == "fail" {
			fmt.Fprintln(stderr, "no such file")
			return errors.New("exit code 1")
		}
		fmt.Fprintln(stdout, "dumped")
		return nil
	}
	var said []string
	st := target.Step{Key: "pg", Kind: "exec", Name: "backup", Command: []string{"pg_dump", "--file=/var/lib/postgresql/data/b"}}
	if err := in.Execute(context.Background(), st, func(l string) { said = append(said, l) }); err != nil {
		t.Fatal(err)
	}
	if got != "dev-pg-0/postgres: pg_dump --file=/var/lib/postgresql/data/b" || len(said) != 1 || said[0] != "dumped" {
		t.Errorf("ran %q, said %v", got, said)
	}
	st.Command = []string{"fail"}
	if err := in.Execute(context.Background(), st, nil); err == nil || !strings.Contains(err.Error(), "exit code 1: no such file") {
		t.Errorf("a command that fails: %v", err)
	}
	st.Key = "zk"
	if err := in.Execute(context.Background(), st, nil); err == nil || !strings.Contains(err.Error(), "dev-zk-0") {
		t.Errorf("a key with no pod: %v", err)
	}
}
