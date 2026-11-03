# k8s — Kubernetes as a target

`target: k8s` runs `pack-target-k8s`, a program of its own (a module with its own `go.mod` and `main.go`; see
[docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md)). A pack goes to one namespace of a cluster: StatefulSets, Jobs,
Services and Secrets. The target performs each step against the API server with client-go, under whatever
authentication the kubeconfig names; it needs no `kubectl`.

## Settings (`env.yaml`)

| Setting                                                        | Default                                                        | Meaning                                                                                                                                                                                                                                             |
|----------------------------------------------------------------|----------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `namespace`                                                    | `default`                                                      | Where everything goes, the record included. It must exist.                                                                                                                                                                                          |
| `kubeconfig`                                                   | `$KUBECONFIG`, `~/.kube/config`; in a pod, its service account | The kubeconfig file.                                                                                                                                                                                                                                |
| `context`                                                      | the current one                                                | The kubeconfig's context.                                                                                                                                                                                                                           |
| `cluster_domain`                                               | none                                                           | The cluster's DNS domain (`cluster.local`). With it a key's hosts are the FQDNs of its pods — a key with no replicas too, as one pod — which is what a JVM's `_HOST` is, so Kerberos principals and certificates made for the hosts match the pods. |
| `image_registry`                                               | none                                                           | The registry that images with no registry host are pulled from.                                                                                                                                                                                     |
| `image_pull_policy`                                            | the cluster's                                                  | `imagePullPolicy` of every container.                                                                                                                                                                                                               |
| `secret_delivery`                                              | `native`                                                       | `native`: a Secret of each credential; `vault-csi`: the Secrets Store CSI driver with its Vault provider delivers them from the secret store; `vault-injector` is refused (not done yet).                                                           |
| `vault_role`, `vault_addr`, `vault_mount`, `vault_cacert_path` | the store's                                                    | With `vault-csi`: the role the pods log in with (required), the server the driver reads from, the KV v2 mount, the CA in the pod.                                                                                                                   |

Any other key is refused before anything is rendered.

## A key's entry (`keys:`)

| Entry                               | Meaning                                                                                             |
|-------------------------------------|-----------------------------------------------------------------------------------------------------|
| `replicas`                          | Pods of the key; 1 when the environment says nothing.                                               |
| `nodeSelector`                      | The pod's `nodeSelector`.                                                                           |
| `priorityClassName`, `storageClass` | The pod's class; `storageClassName` of the key's claims.                                            |
| `user`                              | The pod's `securityContext`: `runAsUser`, `runAsGroup`, `fsGroup` from `uid` or `uid:gid`, numeric. |
| `mounts`                            | Paths of the node mounted into the pod (`hostPath`), each `<node path>:<container path>[:ro]`.      |

`resources` become the container's `requests` and `limits`; labels go on the pods, annotations too, as annotations.
`reach` says where a key is exposed to a neighbour on another target (the cluster's Service means nothing to it).

```yaml
target: k8s
namespace: data
keys:
  zookeeper: { replicas: 3 }
  kafka: { replicas: 3, nodeSelector: { node-role.kubernetes.io/data: "true" }, storageClass: fast-ssd }
```

## What goes to the cluster

A key is a StatefulSet (`podManagementPolicy: Parallel`) with a headless Service `<installation>-<key>`; pods are
`<service>-<i>` at `<service>-<i>.<service>.<namespace>.svc`; a key that runs to completion is a Job with
`backoffLimit: 0`. Config files: a Secret per directory, each file mounted over the image by `subPath` (a per-instance
file by `subPathExpr: $(POD_NAME)`), a checksum annotation rolls the pods when one changes. Claims:
`volumeClaimTemplates`, which outlive the key. Readiness: a `readinessProbe`. The installation's record is the
ConfigMap `pack-<installation>`.

| Step   |                                                                                                                      |
|--------|----------------------------------------------------------------------------------------------------------------------|
| apply  | server-side apply of the key's objects (field manager `pack`), with `${secret:…}` in config Secrets resolved first   |
| wait   | until every replica is ready at its current revision, as `kubectl rollout status`; a Job, until it succeeds or fails |
| stop   | scales to 0 and waits for the pods to go                                                                             |
| hook   | a Job of the hook's image and command, with the key's files and env                                                  |
| remove | deletes the workload, the Service and the config Secrets; the claims stay                                            |

## Not in this target

A job that publishes credentials for the keys after it (docker does; here the certificates are cert-manager's), objects
a pack ships to be applied to the cluster itself (the RBAC of a log shipper), image-volume libraries and pod
anti-affinity (`spread`) have no counterpart in the render of this pack format yet.

## Requirements

Rights in the namespace for StatefulSets, Jobs, Services, Secrets, ConfigMaps, PVCs and pods (list, logs). The immutable
fields of a StatefulSet (`volumeClaimTemplates`, the selector) are not recreated on a change: apply refuses.

## In a container

`ghcr.io/horchestra/targets/k8s:<version>` is the program alone (client-go is in it); pack runs it with
`runtime: {type: docker}` ([docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md#where-a-target-runs-the-runtime)).
The kubeconfig is a mount; a cluster on the machine's loopback (kind, k3d) wants `network: host` on Linux.

```yaml
# release.yaml
targets:
  k8s:
    image: oci://ghcr.io/horchestra/targets/k8s:0.2.0
```

```yaml
# env.yaml
target: k8s
runtime:
  type: docker
  mounts:
    - ~/.kube/config:/home/pack/.kube/config:ro
```
