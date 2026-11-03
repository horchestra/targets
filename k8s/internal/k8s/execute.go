package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

const defaultTimeout = 10 * time.Minute

func (in *Installation) recordName() string { return "pack-" + in.installation }

// ReadInstalled reads the record pack keeps in the namespace: the ConfigMap
// pack-<installation>.
func (in *Installation) ReadInstalled() ([]byte, bool, error) {
	c, err := in.cluster()
	if err != nil {
		return nil, false, err
	}
	cm := &corev1.ConfigMap{}
	err = c.Get(context.Background(), client.ObjectKey{Namespace: in.namespace, Name: in.recordName()}, cm)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("the record %s: %w", in.recordName(), err)
	}
	rec, ok := cm.Data["record.json"]
	return []byte(rec), ok, nil
}

// WriteInstalled keeps the record in the ConfigMap pack-<installation>.
func (in *Installation) WriteInstalled(record []byte) error {
	cm := obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": obj{"name": in.recordName(), "namespace": in.namespace,
			"labels": obj{labelInstance: in.installation, labelManagedBy: "pack", labelPart: "record"}},
		"data": obj{"record.json": string(record)}}
	return in.apply(context.Background(), []obj{cm}, nil)
}

// records are what List lists: record.json of each ConfigMap managed by
// pack and part record in the namespace.
func (in *Installation) records() ([][]byte, error) {
	c, err := in.cluster()
	if err != nil {
		return nil, err
	}
	sel := labels.SelectorFromSet(labels.Set{labelManagedBy: "pack", labelPart: "record"})
	cms := &corev1.ConfigMapList{}
	if err := c.List(context.Background(), cms, client.InNamespace(in.namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("the records: %w", err)
	}
	var records [][]byte
	for _, cm := range cms.Items {
		if rec, ok := cm.Data["record.json"]; ok {
			records = append(records, []byte(rec))
		}
	}
	return records, nil
}

// DeleteInstalled removes the record's ConfigMap pack-<installation> and
// waits until it is gone — pack delete's last act; the installation's data,
// its claims and Secrets, stays.
func (in *Installation) DeleteInstalled() error {
	return in.remove(context.Background(),
		labelInstance+"="+in.installation+","+labelManagedBy+"=pack,"+labelPart+"=record",
		[]client.ObjectList{&corev1.ConfigMapList{}}, defaultTimeout, nil)
}

// Execute performs one step of the saga on the cluster.
func (in *Installation) Execute(ctx context.Context, st target.Step, events func(string)) error {
	svc := in.service(st.Key)
	timeout := defaultTimeout
	if len(st.Timeout) > 0 {
		if d, err := time.ParseDuration(st.Timeout); err == nil {
			timeout = d
		}
	}
	switch st.Kind {
	case "apply":
		if st.Unit == nil {
			return fmt.Errorf("apply %s: no unit", st.Key)
		}
		objs, err := in.objects(*st.Unit, st.Credentials, in.resolver(ctx, st.Credentials))
		if err != nil {
			return err
		}
		// a Job's pod template cannot be changed, and a run to completion runs again whenever it is applied: the
		// Job of an earlier run goes first
		if st.Unit.RestartPolicy == "Never" {
			if err := in.deleteJob(ctx, svc, true, timeout); err != nil {
				return err
			}
		}
		return in.apply(ctx, objs, events)
	case "wait":
		if st.Unit != nil && st.Unit.RestartPolicy == "Never" {
			return in.waitJob(ctx, svc, timeout, events)
		}
		return in.rollout(ctx, svc, timeout, events)
	case "stop":
		if err := in.scaleToZero(ctx, svc, events); err != nil {
			return err
		}
		c, err := in.cluster()
		if err != nil {
			return err
		}
		return in.gone(ctx, c, selector(in.labels(st.Key)), []client.ObjectList{&corev1.PodList{}}, timeout)
	case "remove":
		if err := in.remove(ctx, selector(in.labels(st.Key))+",pack.horchestra.io/part in (workload,config,hook,access)", in.removed(), timeout, events); err != nil {
			return err
		}
		return in.removeAccess(ctx, st.Key, timeout, events)
	case "hook":
		return in.hook(ctx, st, timeout, events)
	case "exec":
		return in.exec(ctx, st, timeout, events)
	}
	return fmt.Errorf("step %d: %q is no step kind", st.N, st.Kind)
}

// hook runs a hook as a Job: the key's pod — its files, env and libraries —
// with the hook's image and command; an exclusive hook gets the volumes of
// replica 0. The key's credentials and config Secrets are applied first, a
// hook may come before the key's first apply.
func (in *Installation) hook(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("hook %s: no unit", st.Name)
	}
	u := *st.Unit
	svc := in.service(st.Key)
	objs, err := in.objects(u, st.Credentials, in.resolver(ctx, st.Credentials))
	if err != nil {
		return err
	}
	var pre []obj
	for _, o := range objs {
		if o["kind"] == "Secret" {
			pre = append(pre, o)
		}
	}
	if err := in.apply(ctx, pre, events); err != nil {
		return err
	}
	pod, _, _, err := in.podSpec(u, nil)
	if err != nil {
		return err
	}
	c := pod["containers"].([]any)[0].(obj)
	c["image"], c["command"] = in.image(st.Image), toList(st.Command)
	delete(c, "args")
	delete(c, "readinessProbe")
	delete(c, "ports")
	pod["restartPolicy"] = "Never"
	if st.Exclusive {
		for _, d := range u.Data {
			addVolume(pod, obj{"name": d.Name, "persistentVolumeClaim": obj{"claimName": d.Name + "-" + svc + "-0"}})
		}
	} else {
		mounts, _ := c["volumeMounts"].([]any)
		var kept []any
		data := map[string]bool{}
		for _, d := range u.Data {
			data[d.Name] = true
		}
		for _, m := range mounts {
			if !data[m.(obj)["name"].(string)] {
				kept = append(kept, m)
			}
		}
		c["volumeMounts"] = kept
	}
	name := dnsLabel(fmt.Sprintf("%s-%s-%d", svc, st.Name, st.N))
	if st.Rollback {
		name = dnsLabel(fmt.Sprintf("%s-undo-%s-%d", svc, st.Name, st.N))
	}
	job := obj{"apiVersion": "batch/v1", "kind": "Job", "metadata": in.meta(name, st.Key, "hook"),
		"spec": obj{"backoffLimit": 0, "template": obj{"metadata": obj{"labels": in.labels(st.Key)}, "spec": pod}}}
	if err := in.deleteJob(ctx, name, true, timeout); err != nil {
		return err
	}
	if err := in.apply(ctx, []obj{job}, events); err != nil {
		return err
	}
	if err := in.waitJob(ctx, name, timeout, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	return in.deleteJob(ctx, name, false, 0)
}

// waitJob waits until a Job succeeds or fails, and tells its log either way.
func (in *Installation) waitJob(ctx context.Context, name string, timeout time.Duration, events func(string)) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var verdict error
	for {
		job := &batchv1.Job{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: name}, job); err == nil {
			if job.Status.Succeeded > 0 {
				break
			}
			if job.Status.Failed > 0 {
				verdict = fmt.Errorf("job %s failed", name)
				break
			}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err() // cut short: no log, at once
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			verdict = fmt.Errorf("job %s did not finish within %s", name, timeout)
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
	// the log, after the verdict and on a context of its own: a step that ran
	// out of time still says what its pod did
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logs, _ := in.jobLogs(lctx, name)
	for l := range strings.SplitSeq(strings.TrimSpace(string(logs)), "\n") {
		if len(l) > 0 {
			events(name + ": " + l)
		}
	}
	return verdict
}

// resolver reads ${secret:<name>/<key>} from the cluster's Secrets.
func (in *Installation) resolver(ctx context.Context, creds []render.Credential) func(string) (string, error) {
	cache := map[string]map[string][]byte{}
	fromStore := in.storeResolver(ctx, creds)
	return func(ref string) (string, error) {
		name, key, ok := strings.Cut(ref, "/")
		if !ok {
			return "", fmt.Errorf("${secret:%s}: <name>/<key>", ref)
		}
		if in.external() {
			return fromStore(name, key) // the cluster holds no Secret of ours: the store has the value
		}
		data, ok := cache[name]
		if !ok {
			c, err := in.cluster()
			if err != nil {
				return "", err
			}
			s := &corev1.Secret{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: name}, s); err != nil {
				return "", fmt.Errorf("secret %s: %w", name, err)
			}
			data = s.Data
			cache[name] = data
		}
		v, ok := data[key]
		if !ok {
			return "", fmt.Errorf("secret %s has no key %s", name, key)
		}
		return string(v), nil
	}
}

func selector(labels map[string]any) string {
	var parts []string
	for _, k := range []string{labelName, labelInstance, labelManagedBy} {
		parts = append(parts, k+"="+labels[k].(string))
	}
	return strings.Join(parts, ",")
}

// dnsLabel is s as a Kubernetes name: lower case, [a-z0-9-], at most 63.
func dnsLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, s)
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "-")
}

var (
	_ target.Installation = (*Installation)(nil)
	_ target.Recorder     = (*Installation)(nil)
)
