package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// poll is how often a wait looks at the cluster again.
var poll = 2 * time.Second

// connect makes the installation's way to the API server on first use: the
// kubeconfig and context the settings name — by default client-go's loading
// rules ($KUBECONFIG, ~/.kube/config) and the kubeconfig's current context;
// in a pod with neither, its service account. A test gives fakes instead.
func (in *Installation) connect() {
	in.once.Do(func() {
		if in.client != nil {
			return
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if len(in.kubeconfig) > 0 {
			rules.ExplicitPath = expandHome(in.kubeconfig)
		}
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
			&clientcmd.ConfigOverrides{CurrentContext: in.context}).ClientConfig()
		if err != nil {
			in.clientErr = fmt.Errorf("the cluster: %w", err)
			return
		}
		cfg.UserAgent = "pack-provider-k8s"
		in.restConfig = cfg
		if in.client, in.clientErr = client.New(cfg, client.Options{Scheme: scheme.Scheme}); in.clientErr != nil {
			return
		}
		in.clientset, in.clientErr = kubernetes.NewForConfig(cfg)
	})
}

// cluster is the installation's client: any object, typed or unstructured,
// finds its resource through the scheme and the cluster's kinds.
func (in *Installation) cluster() (client.Client, error) {
	in.connect()
	return in.client, in.clientErr
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// apply is a server-side apply of each object in turn, as field manager
// pack, taking over fields another manager holds. Each object is told as
// kubectl tells it: created, configured or unchanged.
func (in *Installation) apply(ctx context.Context, objs []obj, events func(string)) error {
	if len(objs) == 0 {
		return nil
	}
	c, err := in.cluster()
	if err != nil {
		return err
	}
	for _, o := range objs {
		// through JSON: an object of ours holds Go types unstructured does not
		raw, err := json.Marshal(o)
		if err != nil {
			return err
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(raw); err != nil {
			return err
		}
		res, err := in.applyOne(ctx, c, u)
		if err != nil {
			return fmt.Errorf("apply %s %s: %w", u.GetKind(), u.GetName(), err)
		}
		if events != nil {
			events(strings.ToLower(u.GetKind()) + "/" + u.GetName() + " " + res)
		}
	}
	return nil
}

func (in *Installation) applyOne(ctx context.Context, c client.Client, u *unstructured.Unstructured) (string, error) {
	if len(u.GetNamespace()) == 0 {
		namespaced, err := c.IsObjectNamespaced(u)
		if err != nil {
			return "", err
		}
		if namespaced {
			u.SetNamespace(in.namespace)
		}
	}
	u.SetManagedFields(nil)
	u.SetResourceVersion("")
	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(u.GroupVersionKind())
	created := false
	if err := c.Get(ctx, client.ObjectKeyFromObject(u), before); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		created = true
	}
	// the server's answer goes into u
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("pack"), client.ForceOwnership); err != nil {
		return "", err
	}
	switch {
	case created:
		return "created", nil
	case before.GetResourceVersion() == u.GetResourceVersion():
		return "unchanged", nil
	}
	return "configured", nil
}

// removed are the kinds remove deletes: a key's workload, Service and
// config Secrets; its claims outlive it.
func (in *Installation) removed() []client.ObjectList {
	lists := []client.ObjectList{&appsv1.StatefulSetList{}, &batchv1.JobList{}, &corev1.ServiceList{}, &corev1.SecretList{},
		&corev1.ServiceAccountList{}, &rbacv1.RoleBindingList{}, &rbacv1.RoleList{}}
	if in.external() {
		// the class that delivers the key's secrets goes with it; the kind is the CSI driver's, asked for only when used
		spc := &unstructured.UnstructuredList{}
		spc.SetGroupVersionKind(schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClassList"})
		lists = append(lists, spc)
	}
	return lists
}

// matching are the objects of the lists' kinds the selector picks.
func (in *Installation) matching(ctx context.Context, c client.Client, sel string, lists []client.ObjectList) ([]client.Object, error) {
	s, err := labels.Parse(sel)
	if err != nil {
		return nil, err
	}
	var out []client.Object
	for _, l := range lists {
		l = l.DeepCopyObject().(client.ObjectList)
		if err := c.List(ctx, l, client.InNamespace(in.namespace), client.MatchingLabelsSelector{Selector: s}); err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(l)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, it.(client.Object))
		}
	}
	return out, nil
}

// named is an object as kubectl names it: <kind>/<name>.
func named(c client.Client, o client.Object) string {
	gvk, err := apiutil.GVKForObject(o, c.Scheme())
	if err != nil {
		return o.GetName()
	}
	return strings.ToLower(gvk.Kind) + "/" + o.GetName()
}

// background deletes an object's dependants after it — a Job's pods too,
// which the API would orphan.
var background = client.PropagationPolicy(metav1.DeletePropagationBackground)

// remove deletes the objects of the lists' kinds the selector picks and
// waits until they are gone.
func (in *Installation) remove(ctx context.Context, sel string, lists []client.ObjectList, timeout time.Duration, events func(string)) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	objs, err := in.matching(ctx, c, sel, lists)
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := client.IgnoreNotFound(c.Delete(ctx, o, background)); err != nil {
			return fmt.Errorf("delete %s: %w", named(c, o), err)
		}
		if events != nil {
			events(named(c, o) + " deleted")
		}
	}
	return in.gone(ctx, c, sel, lists, timeout)
}

// gone waits until no object of the lists' kinds matches the selector.
func (in *Installation) gone(ctx context.Context, c client.Client, sel string, lists []client.ObjectList, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		objs, err := in.matching(ctx, c, sel, lists)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return nil
		}
		if err := pause(ctx, deadline); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			var left []string
			for _, o := range objs {
				left = append(left, named(c, o))
			}
			return fmt.Errorf("still there after %s: %s", timeout, strings.Join(left, ", "))
		}
	}
}

// deleteJob deletes the Job name, its pods with it, and — wait — waits until
// it is gone; a Job that is not there is no error.
func (in *Installation) deleteJob(ctx context.Context, name string, wait bool, timeout time.Duration) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	job := &batchv1.Job{Name: name, Namespace: in.namespace}
	if err := client.IgnoreNotFound(c.Delete(ctx, job, background)); err != nil || !wait {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := c.Get(ctx, client.ObjectKeyFromObject(job), job); apierrors.IsNotFound(err) {
			return nil
		}
		if err := pause(ctx, deadline); err != nil {
			return fmt.Errorf("job %s: still there after %s", name, timeout)
		}
	}
}

// pause waits a poll's time: ctx's error when it is done first, an error
// when the deadline has passed.
func pause(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Now().After(deadline) {
		return context.DeadlineExceeded
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(poll):
		return nil
	}
}

// scaleToZero scales the StatefulSet name to no replicas.
func (in *Installation) scaleToZero(ctx context.Context, name string, events func(string)) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	sts := &appsv1.StatefulSet{Name: name, Namespace: in.namespace}
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`))
	if err := c.Patch(ctx, sts, patch, client.FieldOwner("pack")); err != nil {
		return fmt.Errorf("scale statefulset %s: %w", name, err)
	}
	if events != nil {
		events("statefulset/" + name + " scaled")
	}
	return nil
}

// rollout waits until the StatefulSet name has every replica ready at its
// current revision, as kubectl rollout status does, telling each change of
// what it waits for.
func (in *Installation) rollout(ctx context.Context, name string, timeout time.Duration, events func(string)) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		sts := &appsv1.StatefulSet{}
		err := c.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: name}, sts)
		switch {
		case apierrors.IsNotFound(err):
			return fmt.Errorf("statefulset %s: not found", name)
		case err != nil:
			last = err.Error() // the API server may be back at the next look
		default:
			msg, done := rolloutStatus(sts)
			if done {
				events(msg)
				return nil
			}
			if msg != last {
				events(msg)
				last = msg
			}
		}
		if err := pause(ctx, deadline); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("statefulset %s not rolled out within %s: %s", name, timeout, last)
		}
	}
}

// rolloutStatus is kubectl's: what a StatefulSet's rollout waits for, or
// that it is complete.
func rolloutStatus(sts *appsv1.StatefulSet) (string, bool) {
	s := sts.Status
	if s.ObservedGeneration == 0 || sts.Generation > s.ObservedGeneration {
		return "waiting for the statefulset spec update to be observed", false
	}
	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	if s.ReadyReplicas < replicas {
		return fmt.Sprintf("waiting for %d pods to be ready: %d ready", replicas, s.ReadyReplicas), false
	}
	if ru := sts.Spec.UpdateStrategy.RollingUpdate; sts.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType &&
		ru != nil && ru.Partition != nil && *ru.Partition > 0 {
		if s.UpdatedReplicas < replicas-*ru.Partition {
			return fmt.Sprintf("waiting for the partitioned roll out: %d of %d pods updated", s.UpdatedReplicas, replicas-*ru.Partition), false
		}
		return fmt.Sprintf("partitioned roll out complete: %d new pods", s.UpdatedReplicas), true
	}
	if s.UpdateRevision != s.CurrentRevision {
		return fmt.Sprintf("waiting for the rolling update: %d pods at revision %s", s.UpdatedReplicas, s.UpdateRevision), false
	}
	return fmt.Sprintf("rolling update complete: %d pods at revision %s", s.CurrentReplicas, s.CurrentRevision), true
}

// jobLogs are the last lines of the newest pod of the Job name — through
// the typed client: a pod's log is text, no object.
func (in *Installation) jobLogs(ctx context.Context, name string) ([]byte, error) {
	c, err := in.cluster()
	if err != nil {
		return nil, err
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(in.namespace), client.MatchingLabels{"job-name": name}); err != nil || len(pods.Items) == 0 {
		return nil, err
	}
	newest := slices.MaxFunc(pods.Items, func(a, b corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	return in.clientset.CoreV1().Pods(in.namespace).GetLogs(newest.Name, &corev1.PodLogOptions{Container: newest.Spec.Containers[0].Name, TailLines: new(int64(50))}).DoRaw(ctx)
}
