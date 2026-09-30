# Contributing to APVA

Thanks for your interest! APVA is in private beta; contribution access is currently by
invitation.

## Developer Certificate of Origin (DCO)

Every commit must be signed off, certifying you wrote it or have the right to submit it
under the Apache 2.0 license ([developercertificate.org](https://developercertificate.org/)):

```bash
git commit -s -m "engine: add GPU idle detection"
```

This adds `Signed-off-by: Your Name <you@example.com>`. PRs without sign-off fail CI.

## Development

```bash
make test     # unit tests
make build    # builds ./bin/apva
make run      # runs against $PROMETHEUS_URL (default http://localhost:9090)
make e2e      # end-to-end tests on a local kind cluster (needs kind, kubectl, helm)
```

Requirements: Go 1.22+. Optional for e2e: Docker, kind, kubectl, Helm.

## Pull requests

1. Open an issue first for anything non-trivial.
2. Keep PRs focused; include tests.
3. `make test lint` must pass.
4. One maintainer approval is required to merge (see [GOVERNANCE.md](GOVERNANCE.md)).
