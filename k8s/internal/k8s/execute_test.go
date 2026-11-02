package k8s

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

// fake gives the installation controller-runtime's fake cluster, with
// funcs, when given, in front of it.
func fake(in *Installation, funcs ...interceptor.Funcs) client.Client {
	b := crfake.NewClientBuilder().WithScheme(scheme.Scheme).WithStatusSubresource(&appsv1.StatefulSet{}, &batchv1.Job{})
	for _, f := range funcs {
		b = b.WithInterceptorFuncs(f)
	}
	c := b.Build()
	in.once.Do(func() {})
	in.client, in.clientset = c, k8sfake.NewClientset()
	poll = 10 * time.Millisecond
	return c
}

func key(in *Installation, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: in.namespace, Name: name}
}

func unitOf(t *testing.T, rel *render.Release, key string) *render.Unit {
	t.Helper()
	for i := range rel.Units {
		if rel.Units[i].Key == key {
			return &rel.Units[i]
		}
	}
	t.Fatalf("no unit %s", key)
	return nil
}

// The record goes to the ConfigMap pack-<installation> and comes back.
func TestRecord(t *testing.T) {
	in, _ := adh(t)
	c := fake(in)
	if _, ok, err := in.ReadInstalled(); ok || err != nil {
		t.Fatalf("nothing is installed yet: %v %v", ok, err)
	}
	if err := in.WriteInstalled([]byte(`{"name":"prod"}`)); err != nil {
		t.Fatal(err)
	}
	if err := in.WriteInstalled([]byte(`{"name":"prod","state":"installed"}`)); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := in.ReadInstalled()
	if !ok || err != nil || string(rec) != `{"name":"prod","state":"installed"}` {
		t.Fatalf("%s %v %v", rec, ok, err)
	}
	cm := &corev1.ConfigMap{}
	err = c.Get(context.Background(), key(in, "pack-prod"), cm)
	if err != nil || cm.Labels[labelPart] != "record" {
		t.Errorf("the record's ConfigMap: %v %v", cm.Labels, err)
	}
}

// pack ps: every record pack keeps in the namespace, nothing else.
func TestList(t *testing.T) {
	in, _ := adh(t)
	c := fake(in)
	ctx := context.Background()
	if err := in.WriteInstalled([]byte(`{"name":"prod"}`)); err != nil {
		t.Fatal(err)
	}
	other := &corev1.ConfigMap{Name: "pack-dev", Namespace: in.namespace,
		Labels: map[string]string{labelInstance: "dev", labelManagedBy: "pack", labelPart: "record"},
		Data:   map[string]string{"record.json": `{"name":"dev"}`}}
	stray := &corev1.ConfigMap{Name: "stray", Namespace: in.namespace,
		Data: map[string]string{"record.json": `{}`}}
	for _, o := range []client.Object{other, stray} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	records, err := in.records()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rec := range records {
		names = append(names, string(rec))
	}
	if !slices.Contains(names, `{"name":"prod"}`) || !slices.Contains(names, `{"name":"dev"}`) || len(names) != 2 {
		t.Errorf("records: %q", names)
	}
}

// pack deletes last act: the record's ConfigMap goes; the installation's
// claims and Secrets stay.
func TestDeleteInstalled(t *testing.T) {
	in, _ := adh(t)
	c := fake(in)
	ctx := context.Background()
	if err := in.WriteInstalled([]byte(`{"name":"prod"}`)); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{Name: "data-prod-zookeeper-0", Namespace: in.namespace,
		Labels: map[string]string{labelInstance: "prod", labelManagedBy: "pack", labelPart: "data"}}
	secret := &corev1.Secret{Name: "prod-kafka-sasl", Namespace: in.namespace,
		Labels: map[string]string{labelInstance: "prod", labelManagedBy: "pack", labelPart: "credentials"}}
	for _, o := range []client.Object{claim, secret} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.DeleteInstalled(); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key(in, "pack-prod"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("the record is still there: %v", err)
	}
	for _, o := range []client.Object{claim, secret} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
			t.Errorf("%s is gone: %v", o.GetName(), err)
		}
	}
}

// apply, wait and remove of a key, through client-go: the objects are
// applied server-side and told as kubectl tells them; wait follows the
// StatefulSet's rollout; remove takes the workload, its Service and config
// Secrets, and leaves the rest of the namespace alone.
func TestExecute(t *testing.T) {
	in, rel := adh(t)
	c := fake(in)
	ctx := context.Background()
	u := unitOf(t, rel, "zookeeper")
	var events []string
	tell := func(s string) { events = append(events, s) }

	if err := in.Execute(ctx, target.Step{N: 1, Kind: "apply", Key: "zookeeper", Unit: u}, tell); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"statefulset/prod-zookeeper created", "service/prod-zookeeper created"} {
		if !slices.Contains(events, want) {
			t.Errorf("no %q in %q", want, events)
		}
	}
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, key(in, "prod-zookeeper"), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 3 {
		t.Errorf("replicas: %v", sts.Spec.Replicas)
	}

	events = nil
	if err := in.Execute(ctx, target.Step{N: 1, Kind: "apply", Key: "zookeeper", Unit: u}, tell); err != nil {
		t.Fatal(err)
	}
	// the fake moves resourceVersion on every apply, a no-op too: an API
	// server says unchanged there, the fake configured
	if !slices.ContainsFunc(events, func(s string) bool {
		return s == "statefulset/prod-zookeeper unchanged" || s == "statefulset/prod-zookeeper configured"
	}) {
		t.Errorf("the second apply: %q", events)
	}

	// the rollout: not ready, then ready at one revision
	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	err := in.Execute(wctx, target.Step{N: 2, Kind: "wait", Key: "zookeeper", Unit: u, Timeout: "50ms"}, tell)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "not rolled out") {
		t.Errorf("a StatefulSet with no ready pods: %v", err)
	}
	if err := c.Get(ctx, key(in, "prod-zookeeper"), sts); err != nil {
		t.Fatal(err)
	}
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: max(sts.Generation, 1), Replicas: 3, ReadyReplicas: 3,
		CurrentReplicas: 3, UpdatedReplicas: 3, CurrentRevision: "r1", UpdateRevision: "r1"}
	if err := c.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	if err := in.Execute(ctx, target.Step{N: 2, Kind: "wait", Key: "zookeeper", Unit: u, Timeout: "2s"}, tell); err != nil {
		t.Fatal(err)
	}

	// someone else's Secret in the namespace, and the key's claim, stay
	other := &corev1.Secret{Name: "other", Namespace: in.namespace}
	if err := c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	events = nil
	if err := in.Execute(ctx, target.Step{N: 3, Kind: "remove", Key: "zookeeper"}, tell); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(events, "statefulset/prod-zookeeper deleted") {
		t.Errorf("remove: %q", events)
	}
	stss, svcs, secrets := &appsv1.StatefulSetList{}, &corev1.ServiceList{}, &corev1.SecretList{}
	for _, l := range []client.ObjectList{stss, svcs, secrets} {
		if err := c.List(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if len(stss.Items)+len(svcs.Items) != 0 {
		t.Errorf("left: %v %v", stss.Items, svcs.Items)
	}
	if len(secrets.Items) != 1 || secrets.Items[0].Name != "other" {
		t.Errorf("secrets after remove: %v", secrets.Items)
	}
}

// A hook is a Job of the hook's image; it is waited for, and deleted when
// it succeeded.
func TestHook(t *testing.T) {
	in, rel := adh(t)
	ctx := context.Background()
	u := unitOf(t, rel, "zookeeper")
	// the Job controller, as far as the hook needs one: the Job succeeds
	c := fake(in, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, k, o, opts...); err != nil {
			return err
		}
		if job, ok := o.(*batchv1.Job); ok {
			if img := job.Spec.Template.Spec.Containers[0].Image; img != "zk-tools:1" {
				t.Errorf("the hook's image: %s", img)
			}
			job.Status.Succeeded = 1
		}
		return nil
	}})
	st := target.Step{N: 4, Kind: "hook", Key: "zookeeper", Name: "init", Unit: u, Image: "zk-tools:1", Command: []string{"init"}}
	if err := in.Execute(ctx, st, func(string) {}); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 0 {
		t.Errorf("the hook's Job is left: %v %v", jobs.Items, err)
	}
}

// ${secret:<name>/<key>} is read from the namespace's Secret.
func TestResolver(t *testing.T) {
	in, _ := adh(t)
	c := fake(in)
	ctx := context.Background()
	s := &corev1.Secret{Name: "pg", Namespace: in.namespace, Data: map[string][]byte{"password": []byte("s3cret")}}
	if err := c.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	resolve := in.resolver(ctx, nil)
	if v, err := resolve("pg/password"); err != nil || v != "s3cret" {
		t.Errorf("%q %v", v, err)
	}
	if _, err := resolve("pg/user"); err == nil {
		t.Error("a key the Secret does not have")
	}
	if _, err := resolve("nope/x"); err == nil {
		t.Error("a Secret that is not there")
	}
}

func TestRolloutStatus(t *testing.T) {
	three := int32(3)
	sts := &appsv1.StatefulSet{Generation: 2, Spec: appsv1.StatefulSetSpec{Replicas: &three}}
	for _, c := range []struct {
		status appsv1.StatefulSetStatus
		done   bool
	}{
		{appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 3}, false},
		{appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 2}, false},
		{appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 3, CurrentRevision: "a", UpdateRevision: "b"}, false},
		{appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 3, CurrentRevision: "b", UpdateRevision: "b"}, true},
	} {
		sts.Status = c.status
		if msg, done := rolloutStatus(sts); done != c.done {
			t.Errorf("%+v: %s, done %v", c.status, msg, done)
		}
	}
}
