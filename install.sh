#!/bin/sh
# Installs swarmageddon on a machine without Kubernetes (docs/design.md §3.1):
#
#   curl -sfL https://raw.githubusercontent.com/usr-lex/swarmageddon/main/install.sh | sh
#
# 1. installs k3s with its defaults (skipped if k3s is already installed),
# 2. waits for the node to be Ready,
# 3. applies install.yaml and prints the welcome Job's output: the control
#    center's URL and the setup token.
#
# Environment:
#   SWARMAGEDDON_MANIFEST  install.yaml URL or path (default: latest release)
#   INSTALL_K3S_*          passed through to the k3s installer
set -eu

MANIFEST=${SWARMAGEDDON_MANIFEST:-https://github.com/usr-lex/swarmageddon/releases/latest/download/install.yaml}
NAMESPACE=swarmageddon-system

info() { printf '==> %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

SUDO=
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || fail "run as root, or install sudo"
	SUDO=sudo
fi

[ "$(uname -s)" = Linux ] || fail "k3s needs Linux; on other systems, install into an existing cluster with kubectl apply -f $MANIFEST"
command -v curl >/dev/null 2>&1 || fail "curl is required"

if command -v k3s >/dev/null 2>&1; then
	info "k3s is already installed; using it"
else
	info "Installing k3s"
	curl -sfL https://get.k3s.io | $SUDO sh -
fi

kubectl() {
	$SUDO k3s kubectl "$@"
}

info "Waiting for the node to be Ready"
i=0
until kubectl get nodes 2>/dev/null | grep -q ' Ready'; do
	i=$((i + 1))
	[ "$i" -le 120 ] || fail "the node did not become Ready in 10 minutes; see: sudo journalctl -u k3s"
	sleep 5
done

info "Applying $MANIFEST"
kubectl apply --server-side --force-conflicts -f "$MANIFEST"

info "Waiting for the control center (this takes a few minutes on a fresh node)"
i=0
until kubectl -n "$NAMESPACE" logs -f job/welcome 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -le 120 ] || fail "the welcome Job did not start; see: sudo k3s kubectl -n $NAMESPACE get pods"
	sleep 5
done
