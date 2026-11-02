package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/secretstore"
)

// How the Secrets a pod reads get there.
//
//	native      pack makes a Secret of each credential it was given and the pod reads it (the default)
//	vault-csi   the secret store delivers them through the Secrets Store CSI driver and its Vault provider: the
//	            key gets a SecretProviderClass that names every secret it reads, the pod mounts it, and the driver
//	            makes the Secrets the pod's env and files already point at — pack never holds the values in the
//	            cluster. The store is the environment's (its address, mount and prefix), the role is vault_role.
//	vault-injector  the Vault Agent injector: not done yet.
//
// A secret a pod reads is a name and a key. Its place in the store is the credential's own Store and Field when pack
// took it from the store (generated, or referred to), else <prefix>/<name> and the key — where a job publishes it.
// A Secret somebody made in the cluster by hand is read from the store too in this mode: put it there.
const (
	providerNative   = "native"
	providerCSI      = "vault-csi"
	providerInjector = "vault-injector"
)

// external: a secret store delivers the Secrets.
func (in *Installation) external() bool {
	return in.setting("secret_delivery", providerNative) == providerCSI
}

type secretUse struct{ name, key string }

// secretsOf are the Secrets (name, key) a unit's pod reads, in order, once each.
func secretsOf(u render.Unit) []secretUse {
	seen := map[secretUse]bool{}
	var out []secretUse
	add := func(s secretUse) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, e := range u.Env {
		if e.Secret != nil {
			add(secretUse{e.Secret.Name, e.Secret.Key})
		}
	}
	for _, f := range u.SecretFiles {
		if f.PerInstance {
			for i := range max(u.Replicas, 1) {
				add(secretUse{f.Secret.Name, fmt.Sprintf("%s.%d", f.Secret.Key, i)})
			}
			continue
		}
		add(secretUse{f.Secret.Name, f.Secret.Key})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].key < out[j].key
	})
	return out
}

// secretProviderClass is the SecretProviderClass that delivers the unit's secrets; the pod gets its volume and
// mount, so that the driver makes the Secrets (secretObjects) the env and the files point at.
func (in *Installation) secretProviderClass(u render.Unit, creds []render.Credential, pod obj) (obj, error) {
	uses := secretsOf(u)
	if len(uses) == 0 {
		return nil, nil
	}
	role := in.setting("vault_role", "")
	if len(role) == 0 {
		return nil, fmt.Errorf("secret_delivery %s needs vault_role: the Vault role the pods log in with", providerCSI)
	}
	cfg := secretstore.FromSettings(func(k string) string { return in.setting(k, "") })
	addr := in.setting("vault_addr", cfg.Addr)
	if len(addr) == 0 {
		return nil, fmt.Errorf("secret_delivery %s needs vault_addr (or the environment's store.addr)", providerCSI)
	}
	mount := strings.Trim(in.setting("vault_mount", cfg.Mount), "/")
	if len(mount) == 0 {
		mount = "secret"
	}
	placed := map[secretUse]secretUse{} // (name, key) → (path, field)
	for _, c := range creds {
		if len(c.Store) > 0 {
			placed[secretUse{c.Name, c.Leaf}] = secretUse{c.Store, c.Field}
		}
	}

	var objects strings.Builder
	byName := map[string][]any{}
	var names []string
	for _, use := range uses {
		where, ok := placed[use]
		if !ok {
			where = secretUse{cfg.PathOf(in.installation, use.name), use.key}
		}
		object := use.name + "-" + use.key
		_, _ = fmt.Fprintf(&objects, "- objectName: %q\n  secretPath: %q\n  secretKey: %q\n", object, mount+"/data/"+strings.Trim(where.name, "/"), where.key)
		if _, seen := byName[use.name]; !seen {
			names = append(names, use.name)
		}
		byName[use.name] = append(byName[use.name], obj{"objectName": object, "key": use.key})
	}
	var secretObjects []any
	for _, n := range names {
		secretObjects = append(secretObjects, obj{"secretName": n, "type": "Opaque", "data": byName[n]})
	}
	params := obj{"roleName": role, "vaultAddress": addr, "objects": objects.String()}
	if ca := in.setting("vault_cacert_path", ""); len(ca) > 0 {
		params["vaultCACertPath"] = ca
	}
	name := in.service(u.Key) + "-secrets"
	spc := obj{"apiVersion": "secrets-store.csi.x-k8s.io/v1", "kind": "SecretProviderClass", "metadata": in.meta(name, u.Key, "config"),
		"spec": obj{"provider": "vault", "secretObjects": secretObjects, "parameters": params}}

	// the pod mounts it: the Secrets exist while a pod does
	addVolume(pod, obj{"name": "pack-secrets", "csi": obj{"driver": "secrets-store.csi.k8s.io", "readOnly": true,
		"volumeAttributes": obj{"secretProviderClass": name}}})
	if cs, ok := pod["containers"].([]any); ok && len(cs) > 0 {
		c := cs[0].(obj)
		mounts, _ := c["volumeMounts"].([]any)
		c["volumeMounts"] = append(mounts, obj{"name": "pack-secrets", "mountPath": "/mnt/pack-secrets", "readOnly": true})
	}
	return spc, nil
}

// setting is a key of the environment's set, or def.
func (in *Installation) setting(key, def string) string {
	if v, ok := in.settings[key]; ok && len(v) > 0 {
		return v
	}
	return def
}

// storeResolver reads a secret's value from the store, where the credential says it is, else where a job publishes
// it (<prefix>/<name>).
func (in *Installation) storeResolver(ctx context.Context, creds []render.Credential) func(name, key string) (string, error) {
	cfg := secretstore.FromSettings(func(k string) string { return in.setting(k, "") })
	var st secretstore.Store
	cache := map[string]map[string]string{}
	return func(name, key string) (string, error) {
		if st == nil {
			var err error
			if st, err = secretstore.Open(cfg); err != nil {
				return "", fmt.Errorf("store: %w", err)
			}
			if st == nil {
				return "", fmt.Errorf("secret_delivery %s needs the environment's store", providerCSI)
			}
		}
		path, field := cfg.PathOf(in.installation, name), key
		for _, c := range creds {
			if c.Name == name && c.Leaf == key && len(c.Store) > 0 {
				path, field = c.Store, c.Field
			}
		}
		fields, ok := cache[path]
		if !ok {
			var err error
			if fields, ok, err = st.Get(ctx, path); err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("secret %s/%s: the store has nothing at %s", name, key, path)
			}
			cache[path] = fields
		}
		v, ok := fields[field]
		if !ok {
			return "", fmt.Errorf("secret %s/%s: the store's %s has no field %s", name, key, path, field)
		}
		return v, nil
	}
}
