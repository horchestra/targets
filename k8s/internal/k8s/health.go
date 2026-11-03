package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/horchestra/pack/pkg/target"
)

// pods are the installation's pods, of one key or of all ("").
func (in *Installation) pods(ctx context.Context, key string) ([]corev1.Pod, error) {
	in.connect()
	if in.clientErr != nil {
		return nil, in.clientErr
	}
	set := labels.Set{labelInstance: in.installation, labelManagedBy: "pack"}
	if len(key) > 0 {
		set[labelName] = key
	}
	l, err := in.clientset.CoreV1().Pods(in.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(set).String()})
	if err != nil {
		return nil, fmt.Errorf("the pods: %w", err)
	}
	return l.Items, nil
}

// Logs gives the tail of each pod of the key, of its first container — the previous run's too, when it restarted:
// what it said before it died.
func (in *Installation) Logs(ctx context.Context, key string, lines int) (map[string][]string, error) {
	pods, err := in.pods(ctx, key)
	if err != nil {
		return nil, err
	}
	tail := int64(max(lines, 1))
	out := map[string][]string{}
	for _, p := range pods {
		if len(p.Spec.Containers) == 0 {
			continue
		}
		opts := &corev1.PodLogOptions{Container: p.Spec.Containers[0].Name, TailLines: &tail}
		var got []string
		for _, previous := range []bool{true, false} {
			opts.Previous = previous
			b, err := in.clientset.CoreV1().Pods(in.namespace).GetLogs(p.Name, opts).DoRaw(ctx)
			if err != nil {
				continue // no previous run, or a pod not started: nothing to say of it
			}
			for l := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
				if len(l) > 0 {
					got = append(got, l)
				}
			}
		}
		out[p.Name] = got
	}
	return out, nil
}

// Health says how each key's pods are: ready when every pod is, with why one is not — the reason of the container
// that waits or terminated (CrashLoopBackOff, ImagePullBackOff, OOMKilled …).
func (in *Installation) Health(ctx context.Context) (map[string]target.Health, error) {
	pods, err := in.pods(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string]target.Health{}
	for _, p := range pods {
		key := p.Labels[labelName]
		h := out[key]
		h.Instances = append(h.Instances, podHealth(p))
		out[key] = h
	}
	for k, h := range out {
		h.Ready = !slices.ContainsFunc(h.Instances, func(i target.InstanceHealth) bool { return !i.Ready })
		out[k] = h
	}
	return out, nil
}

func podHealth(p corev1.Pod) target.InstanceHealth {
	ih := target.InstanceHealth{Name: p.Name, State: string(p.Status.Phase)}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ih.Ready = true
		}
	}
	if ih.Ready {
		return ih
	}
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil:
			ih.Reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil:
			ih.Reason = cs.State.Terminated.Reason
		case cs.LastTerminationState.Terminated != nil:
			ih.Reason = cs.LastTerminationState.Terminated.Reason
		}
		if len(ih.Reason) > 0 {
			return ih
		}
	}
	ih.Reason = "not ready"
	return ih
}
