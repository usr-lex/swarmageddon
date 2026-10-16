# 0001. Kubernetes distribution and nested containers

- Status: Proposed
- Date: 2026-10-16

## Context

Every room needs its own container engine: agents build images, run
`docker compose`, and run test suites that start containers. The brief asks us
to prefer a RuntimeClass that allows unprivileged nested containers, such as
Sysbox, and fall back to privileged Docker-in-Docker. The owner has approved
both, and pointed out that Podman can do the same job and can share an image
cache between engines.

The platform must run on a single-node k3s cluster with no flags, and on
multi-node clusters. Running an agent with permissions bypassed is a supported
mode, and room isolation must hold under it. A privileged container is not an
isolation boundary: root in it can take over the node. remote-labs ran
privileged rooms and said so in its caveats.

## What we found (2026-10, sources in the research notes)

- **Sysbox does not fit our baseline.**
  - Its latest release, v0.7.1 (July 2026), supports Kubernetes 1.32 to 1.35
    only, and only on Ubuntu worker nodes.
  - It has no k3s support, and its maintainers have said there are no plans
    for any (nestybox/sysbox#841).
  - Pods fail to start on containerd 2.x (nestybox/sysbox#1006, open since
    May 2026), and every current k3s ships containerd 2.
  - Support is best effort.
- **Kubernetes user namespaces are GA.**
  - `hostUsers: false` is GA in Kubernetes 1.36, after being beta and on by
    default since 1.33. `procMount: Unmasked` is also GA in 1.36.
  - Requirements: Linux 6.3 or later, containerd 2.0 or later with runc 1.2
    or later, and volumes on filesystems that support idmapped mounts (ext4,
    xfs, btrfs, tmpfs, overlayfs; not NFS).
  - Current k3s releases ship containerd 2.2 or 2.3 with runc 1.4.
- **Podman in a user-namespaced pod has been done in production, but not our
  way yet.**
  - OpenShift 4.20+ and Eclipse Che run *rootless* Podman inside user
    namespace pods, with `procMount: Unmasked`, the SETUID, SETGID and CHOWN
    capabilities, and `/dev/fuse` and `/dev/net/tun` exposed through a CRI-O
    annotation.
  - On containerd, those devices need a device plugin or CDI.
  - A Podman running as root inside the pod's user namespace (where its
    capabilities only apply to the namespace) is plausible but **unverified**.
  - dockerd is riskier in that setup, because it wants to mount sysfs.
- **Podman exposes a Docker-compatible API** (`podman system service`), which
  the `docker` CLI, `docker compose` and Testcontainers can use through
  `DOCKER_HOST`.
- **Podman can read images from shared stores** (`additionalimagestores`):
  extra, usually read-only stores, filled by a single writer.

## Options

1. **Sysbox first, privileged second (the brief).** It would fall back to
   privileged on every k3s cluster, which is our default target, so in
   practice most installs would be privileged.
2. **User-namespaced pods running Podman first, then Sysbox where the cluster
   already has it, then privileged.**
3. **Kata Containers (one VM per room).** The strongest isolation, but it
   needs nested virtualization or bare metal and a second runtime to install
   and run. That is too much for a one-command install on a laptop VM.
4. **Privileged only.** Simple, but isolation does not hold under bypassed
   permissions.

## Decision

Option 2. The operator chooses an isolation tier per cluster (an admin can
override it per workspace), and preflight explains each tier:

| Tier | When | Room pod | Engine | Isolation |
|---|---|---|---|---|
| `userns` (default) | K8s ≥ 1.33 with user namespaces on, containerd ≥ 2.0 or CRI-O, kernel ≥ 6.3, home and engine volumes on an idmap-capable filesystem | `hostUsers: false`, `procMount: Unmasked`, the capabilities Podman needs (namespaced), `/dev/fuse` and `/dev/net/tun` through a small device plugin we ship | Podman, root inside the pod's user namespace | Holds: root in the room maps to an unprivileged range on the node |
| `sysbox` | A `sysbox-runc` RuntimeClass already exists | `runtimeClassName: sysbox-runc` | Podman | Holds |
| `privileged` | Neither of the above, and an admin explicitly accepts it in the wizard | `privileged: true` | Podman | **Does not hold.** The wizard and the fleet view say so |

- **Podman is the engine in every tier**, with its Docker-compatible socket at
  `/var/run/docker.sock`, so the `docker` CLI, `docker compose` and
  Testcontainers work unchanged. The room image installs the real `docker`
  CLI and compose plugin, pointed at Podman's socket.
- **Kubernetes distribution:** k3s is the reference distribution for
  single-node installs, CI and `make dev` (k3d). Any conformant Kubernetes ≥
  1.33 works; ≥ 1.36 is recommended.
- **The first task of M1 is a spike** that proves the `userns` tier on k3s
  with containerd 2: Podman builds an image, runs `compose`, and runs a
  Testcontainers suite inside a user-namespaced pod, both as root in the
  namespace and as the agent user through `sudo`. If that fails on
  containerd, the fallback plan, in order:
  1. Rootless Podman as the agent user, the way OpenShift and Che do it.
  2. Making `privileged` the default on k3s, with the warning above, and
     telling the owner before going further.
- **Images reach rooms through the pull-through registry cache everywhere.**
  Where the cluster has a ReadWriteMany volume on an idmap-capable
  filesystem, rooms also mount a read-only Podman image store, filled by a
  warm-up job with the images a workspace declares. Not in M1; it lands in M5
  with disk hygiene.

## Consequences

- On a stock k3s cluster with a current kernel, rooms are isolated without
  privileged pods, and without installing anything extra beyond our device
  plugin.
- Docker-only behavior can break tools that depend on Docker internals (for
  example, mounting dockerd's own socket into a `docker:dind` container, or
  some buildx drivers). The e2e suite runs `docker build`, `docker compose up`
  and a Testcontainers test in a room so we notice.
- Older kernels or clusters (Kubernetes < 1.33, or a kernel < 6.3) only get
  the `privileged` tier, after the admin accepts the warning.
- NFS-backed StorageClasses can't hold room volumes in the `userns` tier;
  preflight checks for idmap support by starting a test pod with a volume.
