# Research: k8s "rooms" platform: runtimes, storage, messaging, libraries (as of 2026-10-16)

Legend: **VERIFIED** means I read the source directly; the URL is cited. **INFERRED** means it comes from background knowledge or reasoning, or from a secondary source I could not open.

Access notes: GitHub web pages were readable through WebFetch. `raw.githubusercontent.com`, `proxy.golang.org`, the npm registry and PyPI were readable through curl. The GitHub API, `kubernetes.io`, `docs.k3s.io`, `docs.litellm.ai`, `redhat.com` and `docs.okd.io` were not reachable, so for those I read the doc sources on GitHub instead (kubernetes/website, k3s-io/docs, openshift-docs, che-operator).

---

## 1. Sysbox

- **Latest release: v0.7.1 (31 Jul 2026). VERIFIED** (https://github.com/nestybox/sysbox/releases)
  - Adds Ubuntu 24.04 with kernel 6.8+ under containerd 2.x.
  - Fixes a sysbox-fs deadlock and a bug with volumes mounted at `/run`.
- **v0.7.0 (2 Jun 2026). VERIFIED** (same page)
  - Adds k8s 1.33, 1.34 and 1.35. Deprecates 1.29 to 1.31.
  - Adds compatibility with k8s user namespaces, which needs containerd 2.0.5+ or CRI-O.
  - Ports runc CVE fixes.
- **Supported k8s versions: 1.32 to 1.35 only. VERIFIED** (https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md)
  - The doc says "Other versions of Kubernetes are not supported".
  - **k8s 1.36 and 1.37 are not listed.** Current k3s is v1.37.1, so the newest k3s lines are already outside the supported matrix.
- **containerd. VERIFIED** (install-k8s.md)
  - containerd 2.0.5+ is recommended.
  - containerd 2.0.1 to 2.0.4 have a userns bug.
  - With containerd older than 2.0.5, the installer installs CRI-O and switches the kubelet to it.
- **Node OS. VERIFIED** (install-k8s.md): Ubuntu Bionic, Focal, Jammy or Noble only, with kernel 5.4+.
- **Distros tested. VERIFIED** (install-k8s.md): kubeadm, EKS, AKS, GKE, RKE, RKE2 and Lokomotive. **k3s is not listed.**
- **k3s support: not supported. VERIFIED**
  - Issue #841 (https://github.com/nestybox/sysbox/issues/841) is still open. In Oct 2024 the maintainer said sysbox-deploy-k8s does not work on k3s because there is no separate kubelet binary to reconfigure, and that there are "no plans" to fix it.
  - The community workaround is to install CRI-O and Sysbox by hand and rewrite the k3s-agent unit.
- **Containerd 2 sandbox failure on k3s/RKE2: unresolved. VERIFIED**
  - Issue #1006 (https://github.com/nestybox/sysbox/issues/1006), opened May 2026 and still open: Sysbox 0.7.0 with containerd 2.x fails with `mount through procfd: operation not permitted` (sysfs).
  - Reproduced on RKE2 1.35 with containerd 2.2.3, on amd64 and arm64.
  - Community forks are experimenting with fixes. Nothing has been merged.
- **arm64: supported. VERIFIED**
  - Every release ships arm64 `.deb` packages.
  - The README says the host must be on a "supported architecture (e.g., amd64, arm64)".
- **RuntimeClass and node label. VERIFIED** (install-k8s.md): the RuntimeClass is `runtimeClassName: sysbox-runc`, and nodes need the label `sysbox-install=yes`.
- **Maintenance. VERIFIED** (https://github.com/nestybox/sysbox README)
  - The README says Docker is "the main sponsor" but Sysbox is "not officially supported by Docker". Support is "best effort". Sysbox-EE has been discontinued.
  - There are 212 open issues.
  - The sysbox-runc master branch had a commit on 2026-10-12, so the project is still alive (proxy.golang.org).
- **Risks**
  - Support for new k8s versions lags by about 2 to 3 minors.
  - The installer swaps the CRI (CRI-O) and restarts the kubelet.
  - Only Ubuntu nodes are supported.
  - **INFERRED:** treat Sysbox as an optional runtime tier, not the foundation of a k3s single-node install.

## 2. Kubernetes user namespaces (`hostUsers: false`)

- **Feature gate history. VERIFIED** (https://raw.githubusercontent.com/kubernetes/website/main/content/en/docs/reference/command-line-tools-reference/feature-gates/UserNamespacesSupport.md)
  - `UserNamespacesSupport`: alpha in 1.28 to 1.29, beta off by default in 1.30 to 1.32, **beta on by default in 1.33 to 1.35**, **GA and locked in 1.36**.
  - KEP-127 kep.yaml shows stable at v1.36.
  - Blog post: https://kubernetes.io/blog/2026/04/23/kubernetes-v1-36-userns-ga/ (found by search; I did not read it).
  - `UserNamespacesPodSecurityStandards` was removed in 1.35.
- **Requirements. VERIFIED** (https://raw.githubusercontent.com/kubernetes/website/main/content/en/docs/concepts/workloads/pods/user-namespaces.md)
  - Linux kernel 6.3+, because tmpfs needs idmap support.
  - Idmap mount support on `/var/lib/kubelet/pods` and on every filesystem that backs a pod volume. Supported filesystems are btrfs, ext4, xfs, fat, tmpfs and overlayfs.
  - **NFS is not supported**, because the NFS client has no idmap mounts.
  - Low-level runtime: crun 1.9+ (1.13+ recommended) or runc 1.2+.
  - CRI: containerd 2.0+ or CRI-O 1.25+.
  - Not allowed in a userns pod: hostNetwork, hostIPC, hostPID and `volumeDevices`. KEP-5607 proposes an alpha gate to allow hostNetwork.
  - Kubelet ID ranges: you can configure a `kubelet` user with subuid/subgid entries and the `getsubids` binary. Ranges must be multiples of 65536 and at least 65536 × maxPods.
  - Since 1.33, `userNamespaces.idsPerPod` in the kubelet config sets the per-pod size (default 65536).
- **Capabilities inside a userns pod. VERIFIED** (same doc)
  - Capabilities apply only within the pod's userns. `CAP_SYS_ADMIN` is confined to the namespace.
  - Pod Security Admission Baseline relaxes `runAsUser` and `runAsNonRoot` checks and allows `procMount` for userns pods.
  - Restricted still requires the default procMount.
- **local-path volumes. INFERRED:** they are hostPath directories on the node filesystem (ext4 or xfs), so they are idmap-capable on a 6.3+ kernel.
- **ProcMountType feature gate. VERIFIED** (feature-gates/ProcMountType.md; kubernetes/enhancements keps/sig-node/4265-proc-mount/kep.yaml)
  - Alpha 1.12 to 1.30, beta off 1.31 to 1.32, beta on 1.33 to 1.35, **GA in 1.36**.
  - The KEP risk section and the OpenShift docs both say `Unmasked` should only be used with `hostUsers: false`.
- **k3s and user namespaces**
  - Current k3s releases bundle containerd 2.2.7 (1.34 and 1.35 lines) or 2.3.4 (1.36 and 1.37 lines), with runc 1.4.2. **VERIFIED** (https://github.com/k3s-io/k3s/releases, 30 Sep 2026: v1.37.1+k3s1, v1.36.5, v1.35.9, v1.34.12)
  - So the runtime prerequisites are met on k3s 1.33+ (on by default) and 1.36+ (GA).
  - k3s issue #12332 ("hostUsers: false and VOLUME /run ... permission denied", closed Jan 2026) shows people run userns pods on rootful k3s. **VERIFIED**
  - Userns on *rootless* k3s fails (#12918, closed as stale). **VERIFIED**
  - **INFERRED:** k3s has no special userns docs. Default kubelet ID ranges apply unless you create the `kubelet` user, subuid entries and `getsubids`.
- **Prior art: Red Hat / OpenShift nested containers. VERIFIED**
  - OpenShift 4.20+ ships `restricted-v3` and `nested-container` SCCs with `userNamespaceLevel: RequirePodLevel` (https://raw.githubusercontent.com/openshift/openshift-docs/main/modules/nodes-pods-user-namespaces-configuring.adoc).
  - The example pod there uses `hostUsers: false`, `runAsUser: 1000`, `procMount: Unmasked`, and caps SETUID and SETGID.
  - Eclipse Che / OpenShift Dev Spaces "container run capabilities" (OpenShift 4.20+) default to:
    - pod annotation `io.kubernetes.cri-o.Devices: /dev/fuse,/dev/net/tun`
    - container securityContext `procMount: Unmasked`, `allowPrivilegeEscalation: true`, caps add SETGID, SETUID, CHOWN
    - SCC that drops KILL and MKNOD, runs as UID 1000, uses SELinux type `container_engine_t`, and requires a pod-level userns
  - Sources: https://raw.githubusercontent.com/eclipse-che/che-operator/main/api/v2/checluster_types.go and https://raw.githubusercontent.com/eclipse-che/che-operator/main/pkg/deploy/container-capabilities/container_run.go
  - In this setup Podman runs **rootless (non-root user) inside the userns pod**, not rootful.
- **Feasibility: rootful-in-userns Podman or dockerd. INFERRED, needs a spike**
  - **Podman.** Use `hostUsers: false` plus caps (SYS_ADMIN, NET_ADMIN, SETUID/SETGID, MKNOD, etc., all namespaced), `procMount: Unmasked`, and seccomp Unconfined (or a custom profile allowing mount, unshare and keyctl). Kernel 5.11+ lets a userns-owning process mount native overlayfs, so fuse-overlayfs is not required. You need `/dev/net/tun` for pasta/slirp, or netavark bridge with NET_ADMIN.
    - **Caveat 1:** `/dev/fuse` and `/dev/net/tun` cannot be requested via a pod annotation on containerd. The `io.kubernetes.cri-o.Devices` annotation is CRI-O-only. On containerd you need a device plugin (e.g. squat/generic-device-plugin) or a CDI device.
    - **Caveat 2:** nested containers need cgroup delegation. With cgroup v2 and containerd 2 the pod's cgroup is namespaced, but write access to it depends on the runtime. crun/runc in userns pods normally make the cgroup writable. Verify this.
  - **dockerd** inside a userns pod is less proven.
    - dockerd mounts sysfs, which needs a netns owned by the userns. The pod netns is created by the CRI before or outside the userns, which is likely the same class of problem as Sysbox #1006.
    - The `docker:dind-rootless` image, or running dockerd with `--userns-remap` off and a nested network namespace, may be needed.
    - Recommendation: **default to Podman, with a docker-compatible socket (section 3)**. Gate dockerd behind Sysbox or Kata.
  - Red Hat's older "How to use Podman inside of Kubernetes" (redhat.com/sysadmin/podman-inside-kubernetes) predates userns. It uses privileged mode or a fuse device plugin. Found by search only.

## 3. Podman

- **Latest versions. VERIFIED** (proxy.golang.org)
  - `github.com/containers/podman/v6` v6.1.4, tagged 2026-10-12.
  - v5.8.9 on the 5.x branch, same day.
  - main reports version 6.2.0-dev.
- **Podman 6.0 (about 24 Jun 2026). INFERRED** (secondary sources: linuxiac, LWN)
  - Drops CNI, cgroups v1, iptables, slirp4netns, Windows 10 and Intel Mac.
  - Requires Netavark/Aardvark 2.0 and Buildah 1.44.
- **Additional image stores**
  - `storage.conf` option `[storage.options] additionalimagestores = ["/path"]`. The doc describes these as "Paths to additional container image stores. Usually these are read/only and stored on remote network shares." **VERIFIED** (https://raw.githubusercontent.com/containers/container-libs/main/storage/docs/containers-storage.conf.5.md)
  - **Populating the store. INFERRED** (Red Hat blog "Exploring additional image stores in Podman", via search): a writer runs `podman --root /shared/store pull <img>`, or uses `--storage-opt`.
  - Readers see those images layered underneath their own store and cannot delete them. They must use the same storage driver (overlay).
  - **ReadOnlyMany or RWX volume. INFERRED:** works as long as the filesystem is mounted read-only and readers never take write locks.
    - Use a single writer job (e.g. a CronJob that pulls a curated image set) on a separate RWX volume, then mount it RO into rooms.
    - Images added while a reader is running may need a restart or re-read, but they are generally picked up.
    - With a userns pod, the RO volume also needs idmap support, so **NFS cannot be used with `hostUsers: false`** (see section 2). Options are a hostPath or local volume populated per node (DaemonSet), a CephFS idmap-capable mount, or an image volume / OCI artifact.
  - A related feature is `additionallayerstores` (lazy pull, e.g. stargz-store). **INFERRED**
- **Docker compatibility. VERIFIED** (https://raw.githubusercontent.com/containers/podman/main/docs/source/markdown/podman-system-service.1.md)
  - `podman system service` serves "a compatibility layer offering support for the Docker v1.40 API" plus the native libpod API.
  - Supports systemd socket activation and `--time=0` to run forever.
  - Point `DOCKER_HOST=unix:///run/podman/podman.sock` (rootful) or `$XDG_RUNTIME_DIR/podman/podman.sock` (rootless). The `docker` CLI and `docker compose` v2 then work against it. **INFERRED**, well-established practice.
- **Testcontainers. VERIFIED via search snippets** (https://java.testcontainers.org/supported_docker_environment/, https://node.testcontainers.org/supported-container-runtimes)
  - Set `DOCKER_HOST` to the podman socket.
  - Rootless mode needs `TESTCONTAINERS_RYUK_DISABLED=true`.
  - Rootful Ryuk works (Java 1.19+).
  - Set `short-name-mode="disabled"` in registries.conf (or use fully qualified names) to avoid interactive prompts.

## 4. k3s defaults

- **Packaged components. VERIFIED**
  - Packaged AddOns are coredns, traefik, local-storage and metrics-server. ServiceLB is embedded and can be disabled the same way.
  - Disable any of them with `--disable=<name>`.
  - Source: https://raw.githubusercontent.com/k3s-io/docs/main/docs/installation/packaged-components.md
- **Traefik. VERIFIED**
  - Default ingress is Traefik v3 (3.7.13 in current releases).
  - It is exposed via a ServiceLB LoadBalancer on 80/443 across all nodes.
  - Customise it with HelmChartConfig. Gateway API is optional (`providers.kubernetesGateway.enabled`).
  - Source: https://raw.githubusercontent.com/k3s-io/docs/main/docs/networking/networking-services.md
- **NetworkPolicy. VERIFIED**
  - Enforced by default through the embedded kube-router netpol controller library ("no other kube-router functionality").
  - Disable with `--disable-network-policy`. Disable it when you bring your own CNI.
  - Disabling it does not remove the iptables rules.
  - Source: same networking-services.md.
- **local-path storage. VERIFIED**
  - k3s bundles local-path-provisioner v0.0.37, storing data under `/var/lib/rancher/k3s/storage`.
  - The README says: "No support for the volume capacity limit currently... The capacity limit will be ignored".
  - Node-path mode is RWO (plus RWOP). RWX and ROX are supported only with `sharedFileSystemPath`, meaning a filesystem already mounted on all nodes.
  - Sources: https://raw.githubusercontent.com/k3s-io/docs/main/docs/add-ons/storage.md and https://raw.githubusercontent.com/rancher/local-path-provisioner/master/README.md
- **Quota options**
  1. **local-path plus XFS project quotas. VERIFIED**
     - The upstream example `examples/quota` uses custom setup and teardown scripts. They receive `VOL_SIZE_BYTES`.
     - It needs `ALLOW_UNSAFE_HELPER_POD_TEMPLATE=true`, because the helper pod is privileged.
     - The storage path must be an XFS mountpoint mounted with `pquota`.
     - Cheapest option for single-node.
  2. **TopoLVM v0.42.0** (2026-10-05). **VERIFIED** (proxy.golang.org)
     - LVM thin or thick, capacity-aware scheduling, real size enforcement, online expansion.
     - Needs a volume group on each node.
  3. **OpenEBS LVM LocalPV v1.10.1** (2026-09). **VERIFIED** (proxy.golang.org)
     - Similar to TopoLVM.
     - OpenEBS ZFS LocalPV is also available; I did not check its version.
  4. **Longhorn v1.13.0** (2026-09-29). **VERIFIED** (proxy.golang.org)
     - Replicated block storage with enforced size, snapshots and backups, and RWX via a share-manager (NFS).
     - Note: NFS-backed RWX does not work with userns pods.
     - Heavier footprint.
  - **Recommendation. INFERRED**
    - Single-node: XFS pquota via local-path, or TopoLVM.
    - Multi-node: TopoLVM or OpenEBS LVM for RWO room disks, plus Longhorn if replication is needed.

## 5. NATS (embedded)

- **Versions. VERIFIED** (proxy.golang.org)
  - nats-server v2.15.1 (2026-10-09, go 1.26).
  - nats.go v1.54.0 (2026-09-18).
- **Embedding API. VERIFIED** (nats-server v2.15.1 source, `server/opts.go` and `server/server.go`)
  - `server.NewServer(&server.Options{...})`, then `go s.Start()`, then `s.ReadyForConnections(d)`.
  - `s.InProcessConn()` is available for an in-process client.
  - Options fields:
    - `ServerName` (unique per node; required for JetStream clustering)
    - `JetStream: true`, `StoreDir`, `JetStreamMaxMemory`, `JetStreamMaxStore`, `JetStreamDomain`
    - `Cluster: server.ClusterOpts{Name, Host, Port, Advertise, NoAdvertise, PoolSize, Compression}`
    - `Routes: server.RoutesFromStr("nats://a:6222,nats://b:6222")`
    - `LeafNode` and `Gateway` options also exist
- **JetStream clustering. VERIFIED** (https://raw.githubusercontent.com/nats-io/nats.docs/master/running-a-nats-service/configuration/clustering/jetstream_clustering/README.md)
  - Raft-based. Each node needs a unique `server_name` plus a cluster name.
  - Quorum is n/2+1. The doc recommends 3 or 5 JetStream nodes.
  - Configure a `$SYS` account user.
- **Client API (nats.go `jetstream` package). VERIFIED**
  - Stream config: `jetstream.StreamConfig{Subjects: []string{"inbox.>"}, Replicas: 3, MaxAge: ttl, Duplicates: window, Retention: ...}`.
  - Consumer config: `ConsumerConfig{Durable, FilterSubject / FilterSubjects, AckPolicy, MaxDeliver, InactiveThreshold}`.
  - Publish with `jetstream.WithMsgID(id)`, which sets the header `Nats-Msg-Id` (`MsgIDHeader`). Dedup happens within the `Duplicates` window.
  - Server stream config also supports `AllowMsgTTL` (per-message TTL via `WithMsgTTL`), `SubjectDeleteMarkerTTL`, `AllowAtomicPublish`, `AllowMsgSchedules` and `AllowMsgCounter`.
- **Pattern. INFERRED**
  - One stream `INBOX` with subjects `inbox.*.*` (workspace.agent).
  - One durable consumer per recipient with `FilterSubject: inbox.<ws>.<agent>`.
  - Use LimitsPolicy plus MaxAge for TTL. WorkQueue retention only allows non-overlapping consumers, which also fits here.
  - Many thousands of durable consumers each carry Raft overhead when R3. Consider R1 consumers or ephemeral/ordered consumers for low-value recipients, or a KV per agent cursor.
- **Kubernetes caveats. INFERRED** (standard NATS Helm chart practice)
  - Use a StatefulSet with a headless Service, so routes go to `nats-0.nats-headless.<ns>.svc.cluster.local:6222` and so on.
  - Set `ServerName` from the pod name so it stays stable across restarts. The Raft peer identity derives from it.
  - Put JetStream storage on a PVC per replica, never emptyDir.
  - Use lame-duck mode on shutdown. Set `PodManagementPolicy: Parallel` so pods can form a quorum.
  - Embedding inside the operator or API binary couples NATS restarts to app deploys. Use PodDisruptionBudget maxUnavailable=1 and a rolling update.
  - On single-node, run R1 (no cluster block).
- **Federation. INFERRED, standard NATS features**
  - Gateways join clusters into a supercluster with JetStream placement by cluster or tags.
  - Leafnodes extend a hub to edge servers with a separate `JetStreamDomain`, and use stream mirrors/sources across domains.

## 6. Go libraries (proxy.golang.org, VERIFIED versions)

| Module | Latest | Date | Notes |
|---|---|---|---|
| connectrpc.com/connect | v1.21.1 | 2026-10-14 | active |
| github.com/bufbuild/buf | v1.74.0 | 2026-10-14 | active |
| @connectrpc/connect-web (npm) | 2.2.0 | | |
| github.com/coreos/go-oidc/v3 | v3.21.0 | 2026-09-01 | active |
| golang.org/x/oauth2 | v0.37.0 | 2026-08-25 | |
| github.com/zitadel/oidc/v3 | v3.51.16 | 2026-10-15 | alternative OIDC RP/OP |
| github.com/crewjam/saml | v0.5.1 | 2025-04-14 | main branch's last commit is 2025-05-09; 64 issues and 26 PRs open; 5 past advisories (2 critical signature bypasses, 2020 and 2022) |
| github.com/russellhaering/gosaml2 | v0.12.0 | 2026-08-05 | active; goxmldsig v1.6.1 (2026-08-04) |
| github.com/zitadel/saml | v0.4.1 | 2025-10-30 | IdP-focused |
| sigs.k8s.io/controller-runtime | v0.25.2 | 2026-09-30 | built on k8s.io/* v0.37 and go 1.26; `client.Apply(ctx, runtime.ApplyConfiguration, ...)` is available for typed server-side apply |
| sigs.k8s.io/kubebuilder/v4 | v4.16.0 | 2026-09-10 | |
| k8s.io/client-go | v0.37.1 | 2026-09-23 | |
| github.com/coder/websocket | v1.8.15 | 2026-06-15 | gorilla/websocket's last release is v1.5.3 (2024) |
| github.com/SherClockHolmes/webpush-go | v1.4.0 | 2025-01-02 | master's last commit is 2026-04-22: low activity but alive |

- **SAML. INFERRED recommendation:** prefer **russellhaering/gosaml2** for the SP role. It is actively maintained and has had recent XML-dsig hardening. crewjam/saml is effectively stagnant, with no commits for about 17 months.
- **Web terminal**
  - client-go `tools/remotecommand.NewWebSocketExecutor` (and `NewFallbackExecutor`) speaks the v5 websocket exec subprotocol. **VERIFIED** (client-go source)
  - Feature gate `TranslateStreamCloseWebsocketRequests` has been beta and on by default since 1.30. KEP-4006 targets stable in v1.38. **VERIFIED**
  - Pattern: browser xterm.js connects over WebSocket to the Go API, which validates the session and calls the apiserver pods/exec via `NewWebSocketExecutor` with a `TerminalSizeQueue`. **INFERRED**
- **Helm-free manifests. INFERRED**
  - Embed YAML or Go applyconfigurations and use server-side apply with a fixed fieldManager and ForceOwnership.
  - On k3s you can also drop manifests into `/var/lib/rancher/k3s/server/manifests` (AddOn controller). **VERIFIED** (packaged-components.md)

## 7. Frontend (npm, VERIFIED versions)

- **Versions**
  - react 19.3.1, vite 8.3.4, typescript 7.0.2 (the Go-native tsc).
  - @tanstack/react-query 5.104.3.
  - svelte 5.57.2, @sveltejs/kit 3.1.0.
  - @xterm/xterm 6.0.0, with @xterm/addon-fit 0.11.0 and @xterm/addon-attach 0.12.0. The old `xterm` package name is deprecated in favour of `@xterm/*`.
  - @novnc/novnc 1.7.0.
- **Recommendation. INFERRED**
  - React + Vite + TS + TanStack Query, with connect-web plus TanStack Query integration (`@connectrpc/connect-query`), served as a static SPA from the Go binary.
  - SvelteKit is lighter, but the Connect/TanStack ecosystem and xterm/noVNC wrappers are more mature in React.
  - **Typescript 7.0** changes the tooling (native compiler). Check that Vite plugins are compatible.
- **Web Push.** webpush-go v1.4.0 works (VAPID) but is low-activity. Alternatively, implement RFC 8291/8292 directly, which is about 300 lines with `crypto/ecdh`. **INFERRED**

## 8. LiteLLM proxy

- **Latest version: litellm 1.105.1 (PyPI, 2026-10-14). VERIFIED**
- **Security note: supply-chain compromise. VERIFIED via search**
  - On 2026-03-24, PyPI versions 1.82.7 and 1.82.8 were published with a credential stealer (TeamPCP / Trivy-token chain).
  - Pin versions and image digests, and deploy from the official image with a hash pin.
  - Sources: https://www.netspi.com/blog/executive-blog/ai-ml-pentesting/litellm-supply-chain-compromise/ and https://www.comet.com/site/blog/litellm-supply-chain-attack/
- **Postgres is required for virtual keys. VERIFIED** (litellm 1.105.1 wheel)
  - The `schema.prisma` datasource is `provider = "postgresql"`.
  - Management endpoints error with: "DB not connected. This endpoint needs a database; set DATABASE_URL to a PostgreSQL connection string".
  - You also need `LITELLM_MASTER_KEY` (sk-…).
  - Redis is recommended for multi-replica rate-limit and budget sync. **INFERRED**
- **`POST /key/generate` body. VERIFIED** (`GenerateKeyRequest` in `litellm/proxy/_types.py`)
  - Fields: `key_alias`, `duration`, `models`, `max_budget`, `budget_duration` (e.g. "30d"), `soft_budget`, `user_id`, `team_id`, `metadata` (dict), `tpm_limit`, `rpm_limit`, `model_max_budget`, `model_rpm_limit`, `model_tpm_limit`, `tags`.
- **Other key endpoints. VERIFIED** (`key_management_endpoints.py`)
  - `/key/info`, `/v2/key/info`, `/key/update`, `/key/delete`, `/key/block`, `/key/unblock`, `/key/regenerate`, `/key/{key}/reset_spend`, `/key/list`, `/key/bulk_update`.
- **Spend endpoints. VERIFIED** (`spend_management_endpoints.py`)
  - `/spend/logs` and `/spend/logs/v2`, `/spend/keys`, `/spend/users`, `/spend/tags`, `/key/spend/report`, `/global/spend/report`, `/global/spend/keys`, `/global/spend/teams`, `/global/spend/models`, `/global/activity`.
  - Teams, users and budgets have their own endpoints (`/team/new`, `/user/info`, `/budget/new`, `/customer/new`).
- **Pattern. INFERRED:** one team per workspace and one key per room/agent, with `max_budget` + `budget_duration` and `metadata: {room, workspace}`. Poll `/key/info` for `spend`.
