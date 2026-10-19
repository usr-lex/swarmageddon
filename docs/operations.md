# Operating swarmageddon

This guide is for the person who installs and runs swarmageddon. Everything
after the install happens in the browser; this page explains what happens
underneath, and what to do when something goes wrong.

## Install

On a machine without Kubernetes (a spare server or a laptop VM, Linux on
amd64 or arm64):

```sh
curl -sfL https://github.com/usr-lex/swarmageddon/releases/latest/download/install.sh | sh
```

It installs k3s, then runs the same install as below.

On an existing cluster (Kubernetes 1.33 or later; 1.36 or later recommended):

```sh
helm install swarmageddon oci://ghcr.io/usr-lex/swarmageddon/charts/swarmageddon \
  -n swarmageddon-system --create-namespace --wait
# or
kubectl apply -f https://github.com/usr-lex/swarmageddon/releases/latest/download/install.yaml \
  && kubectl -n swarmageddon-system logs -f job/welcome
```

Both print the control center's address, the setup token, and the
fingerprint of the certificate authority the control center's certificate
comes from. Check the fingerprint your browser shows before typing the token.

The install creates only the operator and the control center, in the
`swarmageddon-system` namespace. The web wizard deploys everything else.

### Building from source

Until a release is published (or to run your own changes), build the
images and install the chart from the repository:

```sh
make images REGISTRY=registry.example.com/swarm VERSION=0.1.0   # docker buildx
# or: make images CONTAINER_TOOL=podman REGISTRY=... VERSION=...
for i in room runner operator control-center messaging warden egress mockmodel devices; do
  docker push registry.example.com/swarm/$i:0.1.0   # or podman push
done
helm install swarmageddon charts/swarmageddon -n swarmageddon-system --create-namespace \
  --set image.registry=registry.example.com/swarm --set image.tag=0.1.0
```

The operator deploys every other image from the same registry and tag.
Give each build a new tag: nodes keep an image they already pulled under the
same tag (`IfNotPresent`). `--set image.pullPolicy=Always` covers only the
chart's own pods (operator, control center), not the rooms and services the
operator creates. Behind a TLS-intercepting proxy, add
`BUILD_CA=/path/to/ca.pem` to `make images`.

### OpenShift

Install with `helm install` (as above): the chart sees the
`security.openshift.io` API and ships two security context constraints,
`swarmageddon-rooms` (user-namespaced rooms, runners and preflight probes)
and `swarmageddon-devices` (the device plugin), and lets its own pods use
`nonroot-v2`. The operator binds the right one in each namespace it creates.
`helm template` and `install.yaml` can't see the cluster's APIs: add
`--set openshift.enabled=true` there. Network policies follow the cluster's
DNS (`openshift-dns`, port 5353) by themselves.

On arm64 OpenShift Local (CRC) on Apple silicon, the node advertises SVE2
and OpenSSL crashes in LiteLLM (SIGILL, exit 132, no output). Set
`OPENSSL_armcap=0` for it:
`kubectl patch platform default --type merge -p '{"spec":{"services":{"gateway":{"env":{"OPENSSL_armcap":"0"}}}}}'`.

### Getting the setup token again

```sh
kubectl -n swarmageddon-system get secret setup-token -o jsonpath='{.data.token}' | base64 -d
```

The token stops working once an admin is set up. If every admin account is
lost, issue a new one (anyone who can run this already controls the cluster):

```sh
kubectl annotate platform default swarmageddon.dev/reset-setup-token=now --overwrite
```

### Reaching the control center

- With an ingress controller (k3s has Traefik): `https://<node-ip>/`.
- Without one: the operator switches the control center's Service to a
  NodePort. `kubectl get platform default -o jsonpath='{.status.url}'` shows the
  address.
- Always possible:
  `kubectl -n swarmageddon-system port-forward svc/control-center 8443:443`,
  then `https://localhost:8443/`.

The wizard's **Hostname and TLS** step sets a real name. It uses
cert-manager if it is installed. Otherwise it keeps the self-signed authority
and shows how to trust it on each operating system.

## What runs where

| Namespace | What |
|---|---|
| `swarmageddon-system` | operator, control center |
| `swarmageddon-platform` | PostgreSQL, Forgejo, messaging (three replicas), LiteLLM, registry cache, ntfy |
| `ws-<workspace>` | one per workspace: its rooms (one StatefulSet each) and CI runner pods |

```sh
kubectl get platform default -o yaml          # wizard progress, preflight, service conditions
kubectl get workspaces
kubectl -n ws-acme get rooms                   # phase of each room
kubectl -n ws-acme describe room athena        # conditions: forge account, keys, gateway key, pod
```

## Rooms

### Isolation tiers

Preflight picks one, and the control center shows it on every room:

| Tier | Needs | Isolation |
|---|---|---|
| `userns` | Kubernetes ≥ 1.33 with user namespaces, containerd ≥ 2.0 or CRI-O, Linux ≥ 6.3, volumes on ext4, xfs or btrfs | Root in a room is not root on the node |
| `sysbox` | an existing `sysbox-runc` RuntimeClass | Same |
| `privileged` | an admin's explicit acceptance in the wizard | **None**: an agent can take over the node. Use only on a machine dedicated to the agents |

### Lifecycle

Everything in the room page has a `kubectl` equivalent. Changing one room
never restarts another.

```sh
kubectl -n ws-acme patch room athena --type merge -p '{"spec":{"paused":true}}'   # pause (volumes stay)
kubectl -n ws-acme annotate room athena swarmageddon.dev/recreate=now             # new pod, same volumes
kubectl -n ws-acme annotate room athena swarmageddon.dev/rotate=now               # new keys and tokens
kubectl -n ws-acme annotate room athena swarmageddon.dev/update-now=now           # move to the new image now
kubectl -n ws-acme delete room athena                                             # volumes kept 7 days
```

### Inside a room

The Shell tab opens a shell as the agent. From `kubectl`:

```sh
kubectl -n ws-acme exec -it room-athena-0 -- runuser -u athena -- tmux attach -t agent   # the agent's session
kubectl -n ws-acme logs room-athena-0                                                    # supervisor, CLI and engine logs
```

In a room, `swarm agents` lists who the agent can message, and `swarm send
<to> <text>` sends as the room. `docker` talks to the room's own Podman.

## Upgrades

Upgrade the operator; it upgrades the rest.

```sh
helm upgrade swarmageddon oci://ghcr.io/usr-lex/swarmageddon/charts/swarmageddon -n swarmageddon-system
# or re-apply the new install.yaml
```

- Platform services are upgraded one at a time.
- Rooms show **update available** and move to the new image one at a time,
  each only when it is idle (no turn running and no approval waiting), or
  when someone clicks **Update now**. A workspace can instead allow updates
  only in a maintenance window, or only by hand.
- PostgreSQL major versions are never upgraded automatically.

## Backups

- **PostgreSQL** (control center, Forgejo, gateway): a nightly `pg_dump`
  CronJob writes to the `postgres-backups` volume. Copy it off the cluster.
- **Forgejo repositories**: on Forgejo's volume. Push mirrors to GitHub, set
  per repository in the workspace wizard, give an off-site copy.
- **Room homes**: the agents' work in progress, transcripts and memory, on
  each room's `home` volume. Snapshot them with your storage's tools if they
  matter.
- **Secrets** (keys, tokens, the internal CA): Kubernetes Secrets. Back up
  etcd, or `kubectl get secrets -A -l swarmageddon.dev/managed=true -o yaml`
  to an encrypted location.

## Disk

Disk fills up quietly, so swarmageddon watches it for you:

- Each room's volumes have a size. Whether that size is enforced depends on
  the StorageClass, and preflight says which. k3s's default `local-path`
  does not enforce sizes; there, the room's janitor and the warnings are the
  only limit.
- Every room prunes unused containers, images and build cache hourly, and
  more aggressively above 70% of its engine volume.
- Notifications: **disk low** at 80% of a room volume, and when a node has
  under 15% free.
- CI jobs run in pods that are deleted after each job, with capped scratch
  space. A runner pool refuses to start jobs when the node is low on disk.

## Troubleshooting

| Symptom | Look at |
|---|---|
| The setup page doesn't load | `kubectl -n swarmageddon-system get pods`; `kubectl -n swarmageddon-system logs deploy/swarmageddon-operator` |
| A preflight check fails | Its message says what to fix. Re-run preflight from the wizard after fixing it |
| A service stays "deploying" | `kubectl -n swarmageddon-platform get pods`, then that pod's events and logs |
| A room stays `Provisioning` | `kubectl -n ws-<ws> describe room <name>`: the first condition that is not `True` names the step |
| An agent doesn't answer messages | Activity tab: presence. `offline` means its supervisor isn't connected; see the room's logs |
| An agent shows "waiting on a human" | The Approvals inbox, or attach to the Agent tab: a dialog may be open in the CLI |
| SSH to the forge hangs inside a room | The room image already sets `IPQoS none`. If you changed `~/.ssh/config`, check that it is still set |
| A message is refused | The error names the rule (group link, mediated link, required prefix, size or rate limit). The workspace's messaging settings change it |
