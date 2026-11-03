// Package k8s is Kubernetes as a target. A key becomes a StatefulSet and a
// headless Service, <installation>-<key> (a Job when it runs to completion);
// its config files a Secret per directory, each file mounted over the image
// by subPath; its claims volumeClaimTemplates.
// The target's half of a key's placement is replicas, nodeSelector,
// priorityClassName, storageClass, user, mounts. It performs steps through the API server
// with client-go, with whatever authentication the kubeconfig names.
package k8s

import (
	"fmt"
	"strings"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/secretstore"
	"github.com/horchestra/pack/pkg/target"
)

// Factory makes the Kubernetes target of an installation. Of the environment's set it reads namespace (default
// "default"), kubeconfig and context (client-go's loading rules, in a pod its service account), and of each key's
// entry under `keys:` replicas, nodeSelector, priorityClassName, storageClass, user and mounts.
func Factory(installation string, set map[string]string) (target.Installation, error) {
	settings := target.Settings(set)
	if err := settings.Check("k8s", append([]string{"namespace", "kubeconfig", "context", "secret_delivery", "vault_role", "vault_addr",
		"vault_mount", "vault_cacert_path", "image_pull_policy", "image_registry", "cluster_domain"}, secretstore.SettingKeys...), nil); err != nil {
		return nil, err
	}
	switch p := settings.Get("secret_delivery", providerNative); p {
	case providerNative, providerCSI:
	case providerInjector:
		return nil, fmt.Errorf("secret_delivery %s: not done yet — use %s, or %s", p, providerCSI, providerNative)
	default:
		return nil, fmt.Errorf("secret_delivery %q: %s, %s", p, providerNative, providerCSI)
	}
	return &Installation{
		installation: installation,
		namespace:    settings.Get("namespace", "default"),
		kubeconfig:   settings.Get("kubeconfig", ""),
		context:      settings.Get("context", ""),
		settings:     settings,
		platform:     map[string]map[string]any{},
	}, nil
}

// Installation is the target bound to one installation in one namespace.
type Installation struct {
	installation, namespace string
	kubeconfig, context     string
	settings                map[string]string

	// the cluster's clients, made on first use; a test sets fakes. client
	// takes any object; clientset is for what is no object, a pod's log.
	once       sync.Once
	client     client.Client
	clientset  kubernetes.Interface
	clientErr  error
	restConfig *rest.Config
	// execIn runs a command in a pod's container: nil — the API server's exec (remoteExec); a test sets its own.
	execIn execer
	// storeMu serializes keep's read-modify-write of a secret of the store: steps run side by side, KV v2 has no CAS
	storeMu sync.Mutex

	// platform is the target's half of each key's placement as Resolve took it: what the unit of the key is run with.
	// Resolve is called for the installed version and the wanted one before any step, in this process.
	platMu   sync.Mutex
	platform map[string]map[string]any
}

func (in *Installation) Namespace() string { return in.namespace }

// service is a key's StatefulSet and Service name.
func (in *Installation) service(key string) string { return in.installation + "-" + key }

// Resolve the headless Service <installation>-<key> at <service>.<namespace>.svc; with replicas given by the
// environment, that many pods <service>-<i> at <service>-<i>.<service>.<namespace>.svc. With cluster_domain
// (cluster.local) the names end in .svc.<cluster_domain> and a key with no replicas has its one pod as the instance:
// what a JVM calls its own host (_HOST, the canonical name of the pod) is then the host the key is known by, and
// Kerberos principals and certificates made for the hosts of the system are the ones the pod uses. Replicas are 1
// when the environment says nothing.
func (in *Installation) Resolve(key string, p render.Placement) (*render.Resolved, error) {
	own, err := in.platformOf(key, p)
	if err != nil {
		return nil, err
	}
	in.platMu.Lock()
	in.platform[key] = own
	in.platMu.Unlock()
	svc := in.service(key)
	suffix := ".svc"
	clusterDomain := in.setting("cluster_domain", "")
	if len(clusterDomain) > 0 {
		suffix += "." + strings.Trim(clusterDomain, ".")
	}
	res := &render.Resolved{Service: svc, Address: svc + "." + in.namespace + suffix}
	if p.Replicas != nil || len(clusterDomain) > 0 {
		n, _ := toInt(own["replicas"])
		if p.Replicas == nil {
			n = 1
		}
		for i := range n {
			name := fmt.Sprintf("%s-%d", svc, i)
			res.Instances = append(res.Instances, render.Instance{Name: name, Address: name + "." + svc + "." + in.namespace + suffix})
		}
	}
	// the cluster's Service is not reachable from another target: the operator says where the key is exposed
	res.Reach = p.Reach
	return res, nil
}

// platformFor is the target's half of the key's placement, as Resolve took it.
func (in *Installation) platformFor(key string) map[string]any {
	in.platMu.Lock()
	defer in.platMu.Unlock()
	return in.platform[key]
}

func (in *Installation) platformOf(key string, p render.Placement) (map[string]any, error) {
	own := map[string]any{"replicas": int64(1)}
	if p.Replicas != nil {
		own["replicas"] = int64(*p.Replicas)
	}
	for k, v := range p.Raw {
		switch k {
		case "nodeSelector", "priorityClassName", "storageClass", "user", "mounts":
			own[k] = v
		default:
			return nil, fmt.Errorf("%s: keys.%s.%s: the k8s target has replicas, nodeSelector, priorityClassName, storageClass, user, mounts", key, key, k)
		}
	}
	return own, nil
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
