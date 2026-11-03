package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestDiscover(t *testing.T) {
	in := &Installation{}
	in.once.Do(func() {})
	in.clientset = k8sfake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{OSImage: "Debian GNU/Linux 12 (bookworm)", Architecture: "amd64", ContainerRuntimeVersion: "containerd://1.7.2"},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
		}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "slow"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{defaultClass: "true"}}},
	)
	f, err := in.Discover(context.Background(), []string{"n1"})
	if err != nil {
		t.Fatal(err)
	}
	h := f.Hosts["n1"]
	if len(f.Hosts) != 1 || h.OS != "debian" || h.OSVersion != "12" || h.CPUs != 4 || h.MemoryMB != 8192 || h.Address != "10.0.0.5" || h.Runtimes[0] != "containerd" {
		t.Errorf("node = %+v", h)
	}
	if len(f.StorageClasses) != 2 || f.StorageClasses[0] != "fast" {
		t.Errorf("classes = %v: the default first", f.StorageClasses)
	}
}
