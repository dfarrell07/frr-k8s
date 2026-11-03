# FRR graceful restart route-retention reproducer

The [archive](gr-replay-reproducer.tar.gz) is a standalone IPv6 case with one stable helper and no Kubernetes
or exporter. It keeps the router's kernel namespace and 120-second timers.
Its README lists requirements, commands and result limits.

Archive SHA256: `feba2aab767b0bafb81d02f99f49e0133a6c4560b05e59f9300c6873bb3ea974`.

The [validation build](https://github.com/dfarrell07/frr-k8s/actions/runs/41717930802)
uses FRR `4007568de4ba2465f5a49783c2baf772bf503611`, based on upstream
`3215a128095aac836d3e2f736112b6ef5d003eae`. Download the patched image with
GitHub CLI and Podman:

```sh
gh run download 41717930802 --repo dfarrell07/frr-k8s \
  --name image-tar-frr-gr --dir image
podman load -i image/frr-gr.tar
podman pull quay.io/frrouting/frr@sha256:89e5e1ec8b17c5dffaf57dbac58a29ebeb3731a84e5bc09dad1662adff2ba7f5
tar -xzf gr-replay-reproducer.tar.gz
cd gr-replay-reproducer
python3 run-runtime.py --image \
  quay.io/frrouting/frr@sha256:89e5e1ec8b17c5dffaf57dbac58a29ebeb3731a84e5bc09dad1662adff2ba7f5 \
  --output before
python3 run-runtime.py --image localhost/frr-gr-validation:4007568d --output after
```

The baseline above is unpatched FRR 10.6.2; the patched image is the exact
master candidate. Each observation lasts 135 seconds. Inspect the summary and
logs; sampled route retention does not establish continuous forwarding or
complete platform coverage.

For a fresh build, use the pinned [source preparation and build recipe](https://github.com/dfarrell07/frr-k8s/blob/5ba15774856aca61e4d6b9e47800ee72b66d60bf/.github/workflows/ci.yaml#L140)
and [Containerfile](https://github.com/dfarrell07/frr-k8s/blob/5ba15774856aca61e4d6b9e47800ee72b66d60bf/.github/frr-gr/Dockerfile).
The downloadable CI artifacts have a seven-day retention period.
