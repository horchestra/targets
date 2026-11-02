package k8s

import (
	"context"
	"fmt"
	"regexp"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/horchestra/pack/pkg/render"
)

// Manifests: a component that needs something of the cluster itself — the RBAC of a log shipper that lists pods —
// says so with a file that is applied, not mounted (x-pack-file apply=k8s), rendered from a template of its own like
// any other file. The target applies its documents with the key's objects, labelled as they are, and takes them
// away with the key. If one of them is a ServiceAccount named like the key's service (<installation>-<key>), the
// pods run as it, and know NODE_NAME and POD_NAMESPACE.
//
// What goes away with the key are the kinds an access is made of: ServiceAccount, Role, RoleBinding, ClusterRole,
// ClusterRoleBinding. The cluster-wide ones carry the namespace in a label, so that an installation of the same name
// in another namespace keeps its own.
const (
	partAccess     = "access"
	labelNamespace = "pack.horchestra.io/namespace"
	manifestTarget = "k8s"
)

var docSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

// manifestObjects are the cluster objects a unit ships to be applied, not mounted. The render of this pack format has no
// such file yet, so a unit ships none; the objects of the key are labelled and taken away with it all the same.
func (in *Installation) manifestObjects(render.Unit) ([]obj, error) { return nil, nil }

// hasAccount: one of the unit's manifests is the ServiceAccount of its pods.
func (in *Installation) hasAccount(u render.Unit) bool {
	objs, _ := in.manifestObjects(u)
	svc := in.service(u.Key)
	for _, o := range objs {
		if md, _ := o["metadata"].(map[string]any); o["kind"] == "ServiceAccount" && md["name"] == svc {
			return true
		}
	}
	return false
}

// removeAccess deletes what a key's manifests made that is cluster-wide: by the key's labels and the namespace's,
// whatever namespace the kind is looked for in (it has none). The namespaced ones go with the rest of the key.
func (in *Installation) removeAccess(ctx context.Context, key string, timeout time.Duration, events func(string)) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	sel := labels.SelectorFromSet(labels.Set{labelName: key, labelInstance: in.installation, labelManagedBy: "pack",
		labelPart: partAccess, labelNamespace: in.namespace})
	for _, l := range []client.ObjectList{&rbacv1.ClusterRoleBindingList{}, &rbacv1.ClusterRoleList{}} {
		if err := c.List(ctx, l, client.MatchingLabelsSelector{Selector: sel}); err != nil {
			return err
		}
		items, err := meta.ExtractList(l)
		if err != nil {
			return err
		}
		for _, it := range items {
			o := it.(client.Object)
			if err := client.IgnoreNotFound(c.Delete(ctx, o, background)); err != nil {
				return fmt.Errorf("delete %s: %w", named(c, o), err)
			}
			if events != nil {
				events(named(c, o) + " deleted")
			}
		}
	}
	return nil
}
