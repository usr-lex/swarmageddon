#!/bin/sh
# `make dev-down`: deletes the dev cluster and its registry.
set -eu
K3D=${K3D:-k3d}
"$K3D" cluster delete "${DEV_CLUSTER:-swarm-dev}"
"$K3D" registry delete k3d-swarm-registry.localhost 2>/dev/null || true
