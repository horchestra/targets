# The host target

Classic hosts — VMs or bare metal that SSH reaches — as a target of [pack](https://github.com/horchestra/pack),
containers and OS packages alike. It covers what the docker and the ansible targets were:

- a key with an **image** is a container per instance on its host's engine: `<installation>-<key>`, or
  `<installation>-<key>-<i>` for replicas, on the installation's network, its config files in a volume of its own
  mounted file by file, its claims named volumes;
- a key without one is the OS **packages** its platform names, its **config files** delivered, its **systemd
  services** started and enabled.

The doing is the **agent**'s: a small binary (`cmd/agent`, linux/amd64 and linux/arm64, `make agent` builds them
into the driver) the target carries to each host — it checks the sha256 of what is there and uploads over the SSH
session's stdin only when it differs. The agent listens on the host's loopback, speaks `pack.agent.v1`
(`proto/agent.proto`) and lives exactly as long as the SSH session; the driver reaches it through the connection
and asks it to run the package manager, deliver a file or fetch a URL with its own HTTP client (no curl or wget
of the host's), drive systemctl/sysctl/modprobe or the docker CLI, and
serve the files of an OCI artifact over the loopback. No ansible, no python on the hosts — sshd is all a host
needs.

The SSH user is root, or has **passwordless sudo** (the agent runs under `sudo -n`). Keys: `identity`, else the
ssh agent, else `~/.ssh/id_ed25519, id_ecdsa, id_rsa` (a passphrase from `PACK_SSH_PASSPHRASE`); the host key is
checked against `known_hosts` unless `insecure: true`. A connection that dies is dialed again, the agent started
anew, and the saga repeats the step — every step is safe to repeat.

What a host needs, beyond sshd: **apt** or **dnf** and **systemd** for a packages key (Debian and RHEL families),
**sysctl** and **modprobe** (procps, kmod) when the pack sets `sysctls`/`modules`, the **docker CLI** and its
engine for a containers key, and a firewall when the key names `firewall` — **firewalld**, **ufw** or bare
**iptables**, whichever runs (bare iptables gets the OS's persistence installed: iptables-persistent on Debian,
iptables-services on RHEL). No curl or
wget: a URL a pack names is the agent's own download.

## Settings

| Setting                                   | Default                      | Meaning                                                                                 |
|-------------------------------------------|------------------------------|-----------------------------------------------------------------------------------------|
| `hosts.<name>.host`                       | —                            | The address SSH connects to — which is also how a neighbour reaches it. Required.       |
| `hosts.<name>.user`, `.port`              | `root`, `22`                 | The SSH user and port. A user that is not root needs NOPASSWD sudo.                     |
| `hosts.<name>.identity`                   | —                            | A private key file (else the ssh agent, else the default key files).                    |
| `hosts.<name>.knownhosts`, `.insecure`    | `~/.ssh/known_hosts`         | The host key's check; `insecure: true` turns it off.                                    |
| `network`, `network_subnet`, `network_gateway` | the installation's name | The containers' network on each host.                                                   |
| `state`                                   | `~/.pack/host-state.json`    | The file that keeps the installation's record and the credentials pack provisioned.      |
| `secrets.<name>.<key>`                    | —                            | The value `${secret:<name>/<key>}` stands for, when nothing else holds it.              |

Without `hosts` the target is **this machine**: the agent a process of it (sudo when pack is not root), a
container reached by name on the installation's network, a packages key at the loopback.

Of a key's entry under `keys:` (the placement) the target reads:

| Entry                     | A packages key | A containers key |
|---------------------------|----------------|------------------|
| `hosts`                   | the hosts it runs on (all of them when none) | instance *i* on the *i*-th |
| `packages`                | the package names — a list, or a map by OS (`debian-12`, `debian`, `rhel`) that adds an OS the pack does not know | — |
| `services`                | the systemd units to start and enable | — |
| `firewall`                | the rules to let through — `i:<iface>`, `<port>[/<proto>]` or `<cidr>:<port>[/<proto>]`, tcp when no proto; on firewalld, ufw or bare iptables, whatever the host runs | — |
| `publish`                 | — | the ports published on the host, on this machine alone (across hosts every port is published as itself) |
| `name`, `hostname`, `aliases`, `ip`, `dns`, `user`, `groupAdd`, `sysctls`, `securityOpt`, `mounts`, `restart` | — | the container's runtime, the words of a compose file |

A key is a **packages key** when its placement has `packages` or `services`, else a containers key; the two sets
of properties do not mix.

## Packages from a registry

A package repository can be kept in the registry the packs are in, as an artifact whose files keep their paths
(`oci-packer oci://registry -f pack.yaml`, `repository: mirrors/docker-debian:29`, `from: dir://repo/`). While a
host installs, its agent serves the files as an ordinary tree on the loopback,
`http://127.0.0.1:8081/<repo>:<tag>/<file>`, and is gone after, on a failure too. The hosts need the registry, not
the internet.

| Setting                                      | Default | Meaning                                                              |
|----------------------------------------------|---------|----------------------------------------------------------------------|
| `mirror_registry`                            | —       | The registry, as the hosts reach it. Without it no proxy runs.       |
| `mirror_plain_http`                          | `false` | The registry speaks http.                                            |
| `mirror_port`                                | `8081`  | The port on the host's loopback.                                     |
| `mirror_username_env`, `mirror_password_env` | —       | The variables that hold the registry's credentials.                  |

The pack is told where to look by a value of its own (`mirror`, `release_url`):
`mirror: http://127.0.0.1:8081/mirrors/docker-debian:29`.

## The agent's protocol

`proto/agent.proto` (`pack.agent.v1`), regenerated with `buf generate proto`:

```
Info() → os id, version, codename, family (debian | rhel), arch, kernel (uname -r)
Exec(argv, env, stdin) → stream of the output's lines; the last event the exit code
WriteFile(path, content, mode) → changed      RemoveFile(path) → removed
ProxyStart(registry, plain_http, credentials, port)      ProxyStop()
```

Every call carries the token the agent was started with as the metadata `pack-agent-token`; EOF on the agent's
stdin — the session's end — is its signal to exit.

## Building and testing

`make agent` builds the agents the driver embeds; `make host` (or `make build`) the driver. `go test ./...` tests
the module. The Dockerfile is the image pack runs the target from (`runtime: {type: docker}`); it builds the
agents in.
