#!/usr/bin/env bash
# Exercise the unchanged in-cluster constructor without touching host credentials.
# Needs Go and podman; FRR_IMAGE selects an already cached image.
set -euo pipefail
cd "$(dirname "$0")/.."
image=${FRR_IMAGE:-quay.io/frrouting/frr:10.6.2}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/serviceaccount"
CGO_ENABLED=0 go test -c -o "$tmp/metrics-constructor.test" ./cmd/metrics
podman run --rm --pull=never --network none --cap-drop=all \
  --security-opt=no-new-privileges \
  -v "$tmp/metrics-constructor.test:/metrics-constructor.test:ro,Z" \
  -v "$tmp/serviceaccount:/var/run/secrets/kubernetes.io/serviceaccount:rw,Z" \
  --entrypoint /metrics-constructor.test "$image" \
  -test.run '^TestMetricsHandlerInClusterConstructor$' -test.v -test.timeout 45s \
  -constructor-fixture
