package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/horchestra/pack/pkg/target"
)

// defaultClass is the annotation that makes a storage class the cluster's default.
const defaultClass = "storageclass.kubernetes.io/is-default-class"

// Discover tells the facts of the cluster: its nodes (OS, architecture, what is allocatable), the version of its API
// and its storage classes, the default first. Read only.
func (in *Installation) Discover(ctx context.Context, hosts []string) (*target.Facts, error) {
	in.connect()
	if in.clientErr != nil {
		return nil, in.clientErr
	}
	cs := in.clientset
	f := &target.Facts{At: time.Now().UTC().Truncate(time.Second), Hosts: map[string]target.HostFacts{}}
	if v, err := cs.Discovery().ServerVersion(); err == nil {
		f.Kubernetes = v.GitVersion
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("the nodes: %w", err)
	}
	for _, n := range nodes.Items {
		if len(hosts) > 0 && !slices.Contains(hosts, n.Name) {
			continue
		}
		f.Hosts[n.Name] = nodeFacts(n)
	}
	classes, err := cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, c := range classes.Items {
			if c.Annotations[defaultClass] == "true" {
				f.StorageClasses = append([]string{c.Name}, f.StorageClasses...)
			} else {
				f.StorageClasses = append(f.StorageClasses, c.Name)
			}
		}
	}
	return f, nil
}

// nodeFacts are a node's: what it can give the pods (allocatable), where it is reached.
func nodeFacts(n corev1.Node) target.HostFacts {
	info := n.Status.NodeInfo
	os, version := osOfImage(info.OSImage)
	h := target.HostFacts{OS: os, OSVersion: version, Arch: info.Architecture, Runtimes: []string{runtimeOf(info.ContainerRuntimeVersion)}}
	if cpu, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
		h.CPUs = int(cpu.Value())
	}
	if mem, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
		h.MemoryMB = mem.Value() >> 20
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			h.Address = a.Address
			break
		}
	}
	return h
}

// osOfImage is the OS of a node's image: "Debian GNU/Linux 12 (bookworm)" is debian 12.
func osOfImage(image string) (string, string) {
	fields := strings.Fields(image)
	if len(fields) == 0 {
		return "", ""
	}
	for _, f := range fields[1:] {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' {
			return strings.ToLower(fields[0]), strings.SplitN(f, ".", 2)[0]
		}
	}
	return strings.ToLower(fields[0]), ""
}

// runtimeOf is the container runtime's name: containerd://1.7.2 is containerd.
func runtimeOf(v string) string {
	name, _, _ := strings.Cut(v, "://")
	return name
}
