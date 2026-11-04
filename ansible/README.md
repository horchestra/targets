# ansible — classic hosts as a target

`target: ansible` runs `pack-target-ansible`, a program of its own (a module with its own `go.mod` and `main.go`; see
[docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md)). A pack goes to hosts that SSH
reaches — VMs, bare metal — named by an
ansible inventory or by the environment's `hosts`. A key runs no container: the target installs the OS packages its
entry names, delivers its rendered config files to its hosts and starts and enables its systemd services.

**Ansible is only a transport.** Every step is ad-hoc commands (`ansible <pattern> -m <module> -a …`) of the built-in
modules `package`, `apt`, `copy`, `get_url`, `file`, `service`, `setup`, `wait_for` and `command`. The saga — hooks,
order, waits, rollback — is pack's; no
YAML
of the operator's is ever run.

## Settings (`env.yaml`)

| Setting                                      | Default                      | Meaning                                                                                                             |
|----------------------------------------------|------------------------------|---------------------------------------------------------------------------------------------------------------------|
| `inventory`                                  | —                            | An ansible inventory file. Without one the hosts come from `hosts` and a temporary 0600 INI inventory is generated. |
| `hosts.<name>.host`                          | —                            | The address ansible connects to — which is also how a neighbour reaches it. Required for every host.                |
| `hosts.<name>.user`, `.port`, `.become`      | ssh's own                    | `ansible_user`, `ansible_port`, `ansible_become`.                                                                   |
| `ansible`                                    | `ansible` from `PATH`        | The binary.                                                                                                         |
| `become`, `become_method`, `ssh_private_key` | —                            | `--become`, `--become-method`, `--private-key`.                                                                     |
| `state`                                      | `~/.pack/ansible-state.json` | The file that keeps the installation's record and the credentials pack provisioned.                                 |
| `secrets.<name>.<key>`                       | —                            | The value `${secret:<name>/<key>}` stands for, when nothing else holds it.                                          |

## A key's entry (`keys:`)

| Entry      | Meaning                                                                                                                                                                                                                       |
|------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `packages` | OS packages installed on the key's hosts: a list for every host, or a map `{debian-13: […], fedora: […], RedHat: […]}`, the most exact name a host goes by winning. Over what the pack says, or an OS the pack does not know. |
| `hosts`    | Which hosts the key runs on, instance `i` on the `i`-th. Default: every host of the environment; with an `inventory` of your own, name them.                                                                                  |
| `services` | The systemd units the key manages, over what the pack says: started and enabled at apply, restarted when a file of the key changed, verified `active` at wait, stopped at stop and remove.                                    |

`replicas`, `resources`, `labels` and `reach` are the environment's too, but this target has no scheduler: `resources`
say nothing here. A key is reached at its first host's address; with replicas, an instance per host.

```yaml
target: ansible
become: true
hosts:
  a: { host: 10.0.0.11, user: ops }
  b: { host: 10.0.0.12, user: ops }
keys:
  zookeeper: { replicas: 2, hosts: [ a, b ], packages: [ openjdk-17-jre-headless ], services: [ zookeeper ] }
```

## Steps

| Step   |                                                                                                                                                                                                                                                                                                                                                                                                                           |
|--------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| apply  | a pack with `Packages`: the entry of the host's OS, in its order, with `${os:…}` resolved. Otherwise the key's packages (`package`), its files (`copy`, the mode kept, `${secret:…}` resolved). Starts and enables the services; restarts them only where a file changed, what a playbook's `notify` does. `Content` of the pack's root is the image's and is not delivered here: a host gets the files of its OS's entry |
| wait   | `systemctl is-active` per service until every one is active on every host, or the timeout                                                                                                                                                                                                                                                                                                                                 |
| stop   | `service state=stopped`                                                                                                                                                                                                                                                                                                                                                                                                   |
| hook   | the hook's command (`command`, never a shell) on the key's first host                                                                                                                                                                                                                                                                                                                                                     |
| remove | stops the services, deletes the files the key delivered. **The packages stay**: another key may need them                                                                                                                                                                                                                                                                                                                 |

## Facts (`pack discover`)

Each host's, by ad-hoc commands of built-in modules: `setup` (the OS and its version, the architecture, CPUs, memory,
the mounts and their free space, the resolvers, the clock, the service manager), `ss -Htlnup` (what listens, and which
process, where ss can tell), `docker version` (the engine, where there is one). A host without ss or docker tells
none of that, and is no failure; a host that does not answer is told as unreachable, with why, and the others' facts
are still told.

## Logs and health (`pack diagnose`)

The logs of a key are, on each of its hosts, the tail of the journal of its services (`services` of its entry), or
of the host's errors when it names none (`journalctl -p err`); its health is `systemctl is-active` of its services.

## Packages from a registry

A package repository can be kept in the registry the packs are in, as an artifact whose files keep their paths
(`oci-packer oci://registry -f pack.yaml`, `repository: mirrors/docker-debian:29`, `from: dir://repo/`). The target
carries `oci-proxy` to each host while it installs: it listens on the host's loopback, gives the files as an ordinary
tree, `http://127.0.0.1:8081/<repo>:<tag>/<file>`, and is gone after, on a failure too. The hosts need the registry, not
the internet.

| Setting                                      | Default | Meaning                                                              |
|----------------------------------------------|---------|----------------------------------------------------------------------|
| `mirror_registry`                            | —       | The registry, as the hosts reach it. Without it no `oci-proxy` runs. |
| `mirror_plain_http`                          | `false` | The registry speaks http.                                            |
| `mirror_port`                                | `8081`  | The port on the host's loopback.                                     |
| `mirror_username_env`, `mirror_password_env` | —       | The variables that hold the registry's credentials.                  |

The pack is told where to look by a value of its own (`mirror`, `release_url`):
`mirror: http://127.0.0.1:8081/mirrors/docker-debian:29`. A host with no way to the internet still needs what `Prepare`
names, what the packages depend on and what ansible's modules need (`python3-apt` on Debian): from a mirror of the OS's
own repository, or there beforehand. `make oci-proxy` builds the programs the target embeds (linux/amd64, linux/arm64).

`oci-proxy` runs on its own too, configured by the environment alone: `OCI_PROXY_REGISTRY`, `OCI_PROXY_ADDR`,
`OCI_PROXY_PLAIN_HTTP`, `OCI_PROXY_USER`, `OCI_PROXY_PASSWORD`. On SIGTERM it finishes the requests under way and exits
0.

## Secrets and state

`${secret:…}` and secret files are resolved from the step's credentials, then the state, then `secrets.<name>.<key>`;
the value travels in the arguments of the `copy` module (encrypted by SSH, visible in the process list of the machine
that runs ansible for the command's run). The record and the credentials are kept in the `state` file (JSON, 0600, in a
0700 directory, written by rename under a lock; credentials in plain text).

## The log

The target logs at the level pack gives it
([docs/commands.md](https://github.com/horchestra/pack/blob/main/docs/commands.md#the-log)):
at `debug` each step and each ad-hoc command — its module, its hosts, how long, how it ended, never its arguments (a
`copy` carries a file, which may be rendered with a secret); at `trace` each line ansible writes. Run by hand, it
logs at `PACK_LOG_LEVEL` in `PACK_LOG_FORMAT` (`console` or `json`).

## Requirements

`ansible` where the target runs, and SSH from there to every host; `package` and `service` ask the host for a package
manager and systemd. No volumes, no images, no failover.

## In a container

`ghcr.io/horchestra/targets/ansible:<version>` is the program with ansible-core and the ssh client, so that the machine
pack runs on needs neither; pack runs it with `runtime: {type: docker}`
([docs/targets.md](https://github.com/horchestra/pack/blob/main/docs/targets.md#where-a-target-runs-the-runtime)).
`state` stays at its default: the container's home is the release's `.pack/home/ansible`, and the state outlives the
container there.

```yaml
# release.yaml
targets:
  ansible:
    image: oci://ghcr.io/horchestra/targets/ansible:0.2.0
```

```yaml
# env.yaml
target: ansible
ssh_private_key: /home/pack/.ssh/id
runtime:
  type: docker
  mounts:
    - ~/.ssh/deploy:/home/pack/.ssh/id:ro
    - ~/.ssh/known_hosts:/home/pack/.ssh/known_hosts:ro
  env:
    - ANSIBLE_HOST_KEY_CHECKING          # passed by name, when the hosts' keys are not in known_hosts
hosts:
  a:
    host: 10.0.0.11
```

`ssh_agent: true` gives the container the machine's agent instead of a key. A host the machine reaches at its own
loopback (a tunnel, a VM's forwarded port) is `host.docker.internal` from a container of Docker Desktop, or
`network: host` on Linux.
