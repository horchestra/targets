package k8s

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/horchestra/pack/pkg/target"
)

// execer runs a command in a container of a pod, its output to stdout and stderr: remotecommand, or a test's.
type execer func(ctx context.Context, pod, container string, command []string, stdout, stderr io.Writer) error

// exec runs the step's command in the key's first pod as it runs (<service>-0), its first container — with the
// pod's files, data and network: an action of the pack (pack run), a backup that reads the server where it is.
func (in *Installation) exec(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if len(st.Command) == 0 {
		return fmt.Errorf("exec %s: no command", st.Name)
	}
	in.connect()
	if in.clientErr != nil {
		return in.clientErr
	}
	name := in.service(st.Key) + "-0"
	pod, err := in.clientset.CoreV1().Pods(in.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("exec %s: the pod %s: %w", st.Name, name, err)
	}
	if pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("exec %s: the pod %s does not run (%s)", st.Name, name, pod.Status.Phase)
	}
	run := in.execIn
	if run == nil {
		run = in.remoteExec
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errOut bytes.Buffer
	err = run(ctx, name, pod.Spec.Containers[0].Name, st.Command, &out, &errOut)
	if events != nil {
		sc := bufio.NewScanner(&out)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); len(l) > 0 {
				events(l)
			}
		}
	}
	if err != nil {
		if msg := strings.TrimSpace(errOut.String()); len(msg) > 0 {
			return fmt.Errorf("exec %s in %s: %w: %s", st.Name, name, err, msg)
		}
		return fmt.Errorf("exec %s in %s: %w", st.Name, name, err)
	}
	return nil
}

// remoteExec is exec of the API server: websockets, SPDY where the cluster speaks nothing else, as kubectl.
func (in *Installation) remoteExec(ctx context.Context, pod, container string, command []string, stdout, stderr io.Writer) error {
	req := in.clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(in.namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	spdy, err := remotecommand.NewSPDYExecutor(in.restConfig, "POST", req.URL())
	if err != nil {
		return err
	}
	ex := spdy
	if ws, err := remotecommand.NewWebSocketExecutor(in.restConfig, "GET", req.URL().String()); err == nil {
		ex, _ = remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
	}
	return ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}
