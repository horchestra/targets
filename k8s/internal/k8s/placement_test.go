package k8s

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
	batchv1 "k8s.io/api/batch/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// A reference with no registry host goes to the environment's registry; one that names a registry stays.
func TestImage(t *testing.T) {
	in := &Installation{settings: map[string]string{"image_registry": "localhost:5001/"}}
	for ref, want := range map[string]string{
		"ee/adh/adh-base:4.4.0":     "localhost:5001/ee/adh/adh-base:4.4.0",
		"images/zookeeper:3.9":      "localhost:5001/images/zookeeper:3.9",
		"ghcr.io/ks-tool/tembo:0.2": "ghcr.io/ks-tool/tembo:0.2",
		"registry:5000/x/y:1":       "registry:5000/x/y:1",
		"localhost:5001/ee/adh/x:1": "localhost:5001/ee/adh/x:1",
		"":                          "",
	} {
		if got := in.image(ref); got != want {
			t.Errorf("image(%q) = %q, want %q", ref, got, want)
		}
	}
	if got := (&Installation{}).image("ee/adh/x:1"); got != "ee/adh/x:1" {
		t.Errorf("no registry named: %q", got)
	}
}

// platform.<key>.user is the pod's user and the group of its volumes.
func TestPlatformUser(t *testing.T) {
	in, rel := adh(t)
	for user, want := range map[string][3]int64{"70": {70, 70, 70}, "70:71": {70, 71, 71}} {
		u := *unitOf(t, rel, "zookeeper")
		in.platform["zookeeper"] = map[string]any{"user": user}
		pod, _, _, err := in.podSpec(u, nil)
		if err != nil {
			t.Fatal(err)
		}
		sc, _ := pod["securityContext"].(obj)
		if sc["runAsUser"] != want[0] || sc["runAsGroup"] != want[1] || sc["fsGroup"] != want[2] {
			t.Errorf("user %s: securityContext %v, want %v", user, sc, want)
		}
	}
	in.platform["zookeeper"] = map[string]any{}
	if pod, _, _, _ := in.podSpec(*unitOf(t, rel, "zookeeper"), nil); pod["securityContext"] != nil {
		t.Errorf("no user, a securityContext: %v", pod["securityContext"])
	}
}

// The Job of an earlier run of a key that runs to completion is deleted before the key is applied again: a Job's
// pod template cannot be changed.
func TestApplyReplacesTheJob(t *testing.T) {
	in, rel := adh(t)
	c := fake(in)
	ctx := context.Background()
	u := *unitOf(t, rel, "zookeeper")
	u.RestartPolicy = "Never"
	old := &batchv1.Job{Name: in.service("zookeeper"), Namespace: in.namespace, Labels: map[string]string{"earlier": "run"}}
	if err := c.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := in.Execute(ctx, target.Step{N: 1, Kind: "apply", Key: "zookeeper", Unit: &u}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, key(in, in.service("zookeeper")), job); err != nil {
		t.Fatal(err)
	}
	if _, left := job.Labels["earlier"]; left {
		t.Errorf("the Job of the earlier run is still there: %v", job.Labels)
	}
}

// With cluster_domain the hosts are the FQDNs of the pods, a key of scale single included; without it they are
// what they were.
func TestResolveClusterDomain(t *testing.T) {
	two := 2
	in := &Installation{installation: "adh", namespace: "ns", settings: map[string]string{}, platform: map[string]map[string]any{}}
	res, err := in.Resolve("nn", render.Placement{})
	if err != nil || len(res.Instances) != 0 || res.Address != "adh-nn.ns.svc" {
		t.Fatalf("without a domain: %+v %v", res, err)
	}
	in.settings["cluster_domain"] = "cluster.local"
	res, err = in.Resolve("nn", render.Placement{})
	if err != nil || len(res.Instances) != 1 || res.Instances[0].Address != "adh-nn-0.adh-nn.ns.svc.cluster.local" || res.Address != "adh-nn.ns.svc.cluster.local" {
		t.Fatalf("single: %+v %v", res, err)
	}
	res, err = in.Resolve("zk", render.Placement{Replicas: &two})
	if err != nil || len(res.Instances) != 2 || res.Instances[1].Address != "adh-zk-1.adh-zk.ns.svc.cluster.local" {
		t.Fatalf("replicas: %+v %v", res, err)
	}
}

// platform.<key>.mounts are hostPath volumes of the pod's container.
func TestPlatformMounts(t *testing.T) {
	in, rel := adh(t)
	u := *unitOf(t, rel, "zookeeper")
	in.platform["zookeeper"] = map[string]any{"mounts": []any{"/opt/x/demo:/opt/adh/demo:ro", "/opt/x/w:/work"}}
	pod, _, _, err := in.podSpec(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pod["containers"].([]any)[0].(obj)["volumeMounts"].([]any) {
		mm := m.(obj)
		ro, _ := mm["readOnly"].(bool)
		got[mm["mountPath"].(string)] = ro
	}
	if ro, ok := got["/opt/adh/demo"]; !ok || !ro {
		t.Errorf("/opt/adh/demo: %v", got)
	}
	if ro, ok := got["/work"]; !ok || ro {
		t.Errorf("/work: %v", got)
	}
	var paths []string
	for _, v := range pod["volumes"].([]any) {
		if hp, ok := v.(obj)["hostPath"].(obj); ok {
			paths = append(paths, hp["path"].(string))
		}
	}
	if !slices.Equal(paths, []string{"/opt/x/demo", "/opt/x/w"}) {
		t.Errorf("hostPath volumes: %v", paths)
	}
}

// Removing a key takes its cluster-wide access with it, and only its own installation's.
func TestRemoveAccess(t *testing.T) {
	in, _ := adh(t)
	c := fake(in)
	ctx := context.Background()
	mk := func(name, ns string) {
		l := in.labels("zookeeper")
		lbl := map[string]string{}
		for k, v := range l {
			lbl[k] = v.(string)
		}
		lbl[labelPart], lbl[labelNamespace] = partAccess, ns
		if err := c.Create(ctx, &rbacv1.ClusterRole{Name: name, Labels: lbl}); err != nil {
			t.Fatal(err)
		}
	}
	mk("mine", in.namespace)
	mk("another-namespace", "elsewhere")
	if err := in.removeAccess(ctx, "zookeeper", time.Second, func(string) {}); err != nil {
		t.Fatal(err)
	}
	left := &rbacv1.ClusterRoleList{}
	if err := c.List(ctx, left); err != nil || len(left.Items) != 1 || left.Items[0].Name != "another-namespace" {
		t.Errorf("left: %v %v", left.Items, err)
	}
}

// A unit with no image is for hosts, not for containers.
func TestObjectsNeedAnImage(t *testing.T) {
	in, _ := adh(t)
	if _, err := in.objects(render.Unit{Key: "docker", Component: "docker"}, nil, nil); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("a unit with no image: %v", err)
	}
}
