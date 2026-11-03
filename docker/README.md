# docker — Docker engines as a target

`target: docker` runs `pack-target-docker`, a program of its own (this directory is a module with its own `go.mod` and
`main.go`; see [docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md)). A pack goes to one Docker engine, or to several hosts,
each with its own engine. The target talks to every engine with the `docker` CLI: no swarm, no agent on the hosts.
Whatever `docker --host` / `--context` can reach will do (`unix://`, `tcp://`, `ssh://`), or a host the target reaches
itself through an [SSH tunnel to its Docker socket](#ssh-tunnel).

`pack init` downloads it into `.pack/bin` from the source `release.yaml` names (`targets: {docker: https://…}`, see
[docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md#using-one)). Built by hand:
`make docker` here puts it in `bin/`; `make dist` makes the archives a release publishes.

## Settings (`env.yaml`)

The keys the file flattens to (`hosts: {a: {address: …}}` is `hosts_a_address`). Any other key is refused before
anything is rendered.

| Setting                             | Default                 | Meaning                                                                                                                                            |
|-------------------------------------|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| `hosts.<name>.address`              | —                       | How a neighbour reaches the host: an IP or a DNS name. Required for every host; naming one switches the target to [several hosts](#several-hosts). |
| `hosts.<name>.host` / `.context`    | —                       | The host's engine, as `docker --host` (`ssh://user@host`, `tcp://host:2376`) or `docker --context`.                                                |
| `hosts.<name>.ssh`                  | —                       | The host's engine through an [SSH tunnel](#ssh-tunnel): `[user@]host[:port]`; with `socket`, `identity`, `knownhosts`, `insecure`.                 |
| `host`, `context`                   | docker's own            | One engine. Ignored when `hosts` are named.                                                                                                        |
| `docker`                            | `docker` from `PATH`    | The docker CLI binary.                                                                                                                             |
| `network`                           | the installation's name | The user-defined bridge network on each engine; containers find each other on it by name.                                                          |
| `network_subnet`, `network_gateway` | the engine's            | For keys with a fixed `ip`.                                                                                                                        |
| `state`                             | the engine keeps it     | A file that keeps the installation's record and the credentials pack provisioned.                                                                  |
| `secrets.<name>.<key>`              | —                       | The value `${secret:<name>/<key>}` stands for, when nothing else holds it.                                                                         |
| `external_address`                  | —                       | One engine: the name or IP a neighbour on **another target** reaches this engine at.                                                               |

The environment's secret store (`store:`) is given to the target by pack: credentials are kept there, not on the engine.

## A key's entry (`keys:`)

What the environment says of a key; `replicas`, `resources`, `env`, `labels`, `annotations` and `reach` are the same on
every target. The ones below are this target's:

| Entry                                                                                    | Meaning                                                                                                                                             |
|------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `hosts: [a, b]`                                                                          | Across hosts: which hosts the key runs on, instance `i` on the `i`-th. Required for every key once the environment names hosts.                     |
| `publish: {pg: 15432}`                                                                   | One engine: host ports by the component's port name; refused for more than one replica, and across hosts (where every port is published as itself). |
| `name`, `hostname`, `aliases`, `ip`, `dns`, `user`, `groupAdd`, `sysctls`, `securityOpt` | What a compose service says of its container.                                                                                                       |
| `mounts: ["<source>:<target>[:<options>]"]`                                              | A path of the host (starting with `/`), or a volume of the installation the keys that name it share, `<volume>[/<subpath>]` (option `ro`).          |
| `restart`                                                                                | The engine's restart policy instead of the pack's.                                                                                                  |

`resources` become `--cpus`, `--cpu-shares`, `--memory`, `--memory-reservation`; labels and annotations both become
container labels. An entry the target has no word for is refused.

```yaml
target: docker
hosts:
  a: { address: 10.0.0.11, ssh: ops@10.0.0.11 }
  b: { address: 10.0.0.12, host: ssh://ops@10.0.0.12 }
keys:
  zookeeper: { replicas: 2, hosts: [ a, b ] }
  postgres: { hosts: [ a ] }
```

## One engine, several hosts

With no `hosts` a key is the container `<installation>-<key>`, `<installation>-<key>-<i>` for each replica (each also
answers to `<installation>-<key>` on the network). A neighbour reaches an instance at `<container>.<network>` and the
component's port. With hosts, the operator decides where each key runs and pack only follows: a neighbour reaches an
instance at `<host address>:<port>`, every port published as itself. Two containers that publish one port on one host
collide; they go to different hosts. A hook runs on the host of replica 0. Moving a key to other hosts, or changing its
replicas, is a change `apply` performs; the claims stay on the old host.

## SSH tunnel

`hosts.<name>.ssh: ops@10.0.0.11` makes the target connect over SSH in its own process and forward a socket of its own
to the host's `/var/run/docker.sock`; the host needs sshd and the socket and nothing else. The connection is kept as a
long-lived one is (keepalive; the next request dials again, once). The host's key is checked against `known_hosts`
(`insecure: true` says it is not).

## What runs on an engine

Containers (`--restart unless-stopped`, `no` for a key that runs to completion; recreated only when what they run
changed); config files in a volume of the container's own, mounted file by file (Docker ≥ 26); claims as named
volumes that outlive the key; a publish volume for jobs; readiness as a healthcheck (`nc`/`/dev/tcp` or `curl`/`wget`
in the image); secret env reaching `docker create` through the CLI's environment, never its argv.

| Step   |                                                                                                      |
|--------|------------------------------------------------------------------------------------------------------|
| apply  | creates or recreates the key's containers, removes those it no longer has                            |
| wait   | until every container runs and is healthy; a job, until it exits 0, and then keeps what it published |
| stop   | `docker stop --time 30`                                                                              |
| hook   | a container of the hook's image and command, with replica 0's files and env                          |
| remove | the key's containers and files volumes; the claims stay                                              |

## Secrets, publishing, state

`${secret:<name>/<key>}`, a `secretRef` and a secret file are resolved when applying: the step's credentials, then
one kept in the state, then `secrets.<name>.<key>`. A job or hook may **publish** credentials for the keys after it (a
CA, keytabs, a root token) as files `/var/run/pack/publish/<name>/<key>`; when it exits 0 each becomes
`${secret:<name>/<key>}`. The record and the credentials are kept on the engine (labels of volumes that hold nothing),
or in the `state` file (0600), or — credentials — in the secret store.

## Not carried over from the former implementation

`pack template` writes the rendered config files only; the containers and a compose file per engine it used to write
(`compose: true`) are not produced by this version. Image-mount libraries have no counterpart in the render.

## Requirements

Docker ≥ 26 on every engine; every image on each engine or pullable by it; no failover across hosts.

## Facts (`pack discover`)

Each engine's: what `docker info` says of the machine it runs on — the OS, the architecture, CPUs, memory, the
engine's version and clock — and the ports its containers publish there. A host of `discover.hosts` is an engine's
name in `hosts`; the one engine is `engine`.

## In a container

`ghcr.io/horchestra/targets/docker:<version>` is the program with the docker CLI; pack runs it with
`runtime: {type: docker}` ([docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md#where-a-target-runs-the-runtime)).
The engine is reached through its socket, mounted, or over ssh as from a machine:

```yaml
# release.yaml
targets:
  docker:
    image: oci://ghcr.io/horchestra/targets/docker:0.2.0
```

```yaml
# env.yaml
target: docker
runtime:
  type: docker
  mounts:
    - /var/run/docker.sock:/var/run/docker.sock
```

`make images` builds the images of the three targets for this machine's architecture (`REGISTRY`, `VERSION`);
`PUSH=1` builds them for `linux/amd64` and `linux/arm64` with buildx (`IMAGE_PLATFORMS`) and pushes them, as a release
does.
