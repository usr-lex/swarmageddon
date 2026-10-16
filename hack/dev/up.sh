#!/bin/sh
# `make dev`: a k3d cluster with a local registry, every image built and
# pushed to it, Keycloak and the mock model in the platform namespace, and
# the chart installed with hack/dev/values.yaml. Prints the URL and token.
#
# Environment:
#   DEV_CLUSTER    cluster name (default swarm-dev)
#   K3S_IMAGE      k3s node image (default rancher/k3s:v1.36.2-k3s1)
#   DEV_K3D_ARGS   extra `k3d cluster create` arguments, e.g. a proxy for
#                  containerd: -e CONTAINERD_HTTPS_PROXY=http://...@server:*
#   DEV_PRELOAD    images to preload into the nodes (see below)
#   SKIP_IMAGES=1  reuse the images already in the registry
#   K3D, KUBECTL, HELM  tool paths (the Makefile sets them)
set -eu

cd "$(dirname "$0")/../.."

CLUSTER=${DEV_CLUSTER:-swarm-dev}
K3S_IMAGE=${K3S_IMAGE:-rancher/k3s:v1.36.2-k3s1}
REGISTRY_NAME=swarm-registry.localhost
REGISTRY_PORT=5005
IN_CLUSTER_REGISTRY=k3d-$REGISTRY_NAME:$REGISTRY_PORT
K3D=${K3D:-k3d}
KUBECTL=${KUBECTL:-kubectl}
HELM=${HELM:-helm}
CONTEXT=k3d-$CLUSTER
SYSTEM_NS=swarmageddon-system
PLATFORM_NS=swarmageddon-platform

info() { printf '==> %s\n' "$*"; }
kc() { "$KUBECTL" --context "$CONTEXT" "$@"; }

if ! "$K3D" registry list -o json | grep -q "\"k3d-$REGISTRY_NAME\""; then
	info "Creating registry $IN_CLUSTER_REGISTRY (localhost:$REGISTRY_PORT on this machine)"
	"$K3D" registry create "$REGISTRY_NAME" --port "$REGISTRY_PORT"
fi

if ! "$K3D" cluster list -o json | grep -q "\"name\":\"$CLUSTER\""; then
	info "Creating cluster $CLUSTER ($K3S_IMAGE)"
	# Kubernetes >= 1.35 refuses cgroup v1 hosts unless told otherwise.
	cgroup_args=
	if [ "$(docker info --format '{{.CgroupVersion}}')" = 1 ]; then
		cgroup_args="--k3s-arg=--kubelet-arg=fail-cgroupv1=false@server:*"
	fi
	# Where the nodes can't pull from the internet themselves, preload
	# images pulled by this machine's docker (k3s imports the tarball at
	# start), e.g. DEV_PRELOAD="$(curl -fsSL .../k3s-images.txt) keycloak/keycloak:26.8.1".
	preload_args=
	if [ -n "${DEV_PRELOAD:-}" ]; then
		mkdir -p hack/.k3d
		for img in $DEV_PRELOAD; do docker pull -q "$img" >/dev/null; done
		# shellcheck disable=SC2086
		docker save -o hack/.k3d/preload.tar $DEV_PRELOAD
		preload_args="--volume=$PWD/hack/.k3d/preload.tar:/var/lib/rancher/k3s/agent/images/preload.tar@server:*"
	fi
	# shellcheck disable=SC2086 # lists of arguments
	"$K3D" cluster create "$CLUSTER" \
		--image "$K3S_IMAGE" \
		--registry-use "$IN_CLUSTER_REGISTRY" \
		--port "8443:443@loadbalancer" \
		--wait --timeout 300s \
		--k3s-arg="--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:*" \
		--k3s-arg="--kubelet-arg=eviction-minimum-reclaim=imagefs.available=1%,nodefs.available=1%@server:*" \
		$cgroup_args $preload_args ${DEV_K3D_ARGS:-}
fi

if [ "${SKIP_IMAGES:-}" != 1 ]; then
	info "Building images and pushing them to localhost:$REGISTRY_PORT"
	make dev-images DEV_REGISTRY="localhost:$REGISTRY_PORT"
fi

info "Dev services: Keycloak and the mock model in $PLATFORM_NS"
kc create namespace "$PLATFORM_NS" --dry-run=client -o yaml | kc apply -f -
kc -n "$PLATFORM_NS" create configmap keycloak-realm \
	--from-file=swarmageddon.json=hack/keycloak/realm.json --dry-run=client -o yaml | kc apply -f -
sed "s|REGISTRY/|$IN_CLUSTER_REGISTRY/|" hack/dev/extras.yaml | kc apply -f -

info "Installing the chart"
upgrade=false
"$HELM" --kube-context "$CONTEXT" status swarmageddon -n "$SYSTEM_NS" >/dev/null 2>&1 && upgrade=true
# The welcome Job's pod template is immutable; start it afresh.
kc -n "$SYSTEM_NS" delete job welcome --ignore-not-found
"$HELM" --kube-context "$CONTEXT" upgrade --install swarmageddon charts/swarmageddon \
	--namespace "$SYSTEM_NS" --create-namespace -f hack/dev/values.yaml
if [ "$upgrade" = true ]; then
	# Same :dev tags with new content: restart to pull them.
	kc -n "$SYSTEM_NS" rollout restart deployment/swarmageddon-operator deployment/control-center
	kc -n "$PLATFORM_NS" rollout restart deployment/mockmodel
fi

info "Waiting for the welcome Job (the URL and setup token)"
i=0
until kc -n "$SYSTEM_NS" get pods -l job-name=welcome \
	-o jsonpath='{.items[*].status.phase}' 2>/dev/null | grep -qE 'Running|Succeeded|Failed'; do
	i=$((i + 1))
	if [ "$i" -gt 60 ]; then
		info "The welcome Job hasn't started; check: $KUBECTL --context $CONTEXT -n $SYSTEM_NS get pods"
		break
	fi
	sleep 5
done
[ "$i" -gt 60 ] || kc -n "$SYSTEM_NS" logs -f job/welcome || true

cat <<EOF

Control center through the k3d load balancer: https://localhost:8443/
Or: $KUBECTL --context $CONTEXT -n $SYSTEM_NS port-forward svc/control-center 8443:443
Keycloak (realm swarmageddon, users alice/oscar/victor/nobody, password = user name):
  $KUBECTL --context $CONTEXT -n $PLATFORM_NS port-forward svc/keycloak 8080:8080
Mock model in the cluster: http://mockmodel.$PLATFORM_NS.svc:9999
Tear down: make dev-down
EOF
