#!/usr/bin/env bash
# Exercise the default Kubernetes authentication path without host credentials.
# Needs Go and podman (or CONTAINER_RUNTIME); FRR_IMAGE selects a cached image.
set -euo pipefail
cd "$(dirname "$0")/.."
image=${FRR_IMAGE:-quay.io/frrouting/frr:10.6.2}
container_runtime=${CONTAINER_RUNTIME:-podman}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
CGO_ENABLED=0 go test -c -o "$tmp/metrics-constructor.test" ./cmd/metrics
# Let the engine own the empty fixture under either user namespace.
"$container_runtime" run --rm --pull=never --network none --cap-drop=all \
  --security-opt=no-new-privileges \
  -v "$tmp/metrics-constructor.test:/metrics-constructor.test:ro,Z" \
  -v /var/run/secrets/kubernetes.io/serviceaccount \
  --entrypoint /metrics-constructor.test "$image" \
  -test.run '^TestMetricsHandlerInClusterConstructor$' -test.v -test.timeout 45s \
  -constructor-fixture
