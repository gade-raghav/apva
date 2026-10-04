# Contributing to APVA

Thanks for your interest! APVA is early (pre-alpha) and feedback is as valuable as code: see
[docs/beta-testing.md](docs/beta-testing.md) and open issues labelled `beta-feedback`.

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
make e2e-aws  # simulated Amazon EKS end-to-end test (needs a cluster running KWOK)
make demo     # the dashboard with the built-in sample cluster, no cluster needed
```

Requirements: Go 1.22+. Optional for e2e: Docker, kind, kubectl, Helm, Python 3, jq.

Start with [docs/architecture.md](docs/architecture.md) for how the code fits together, and
[docs/testing.md](docs/testing.md) for what each test covers. APVA has **no third-party Go
dependencies**. Please keep it that way, or explain why in the PR.

New cloud? Implement `capacity.Provider` (five methods) next to `internal/aws/provider.go`,
and add a simulated end-to-end test like `test/e2e-aws`.

## Pull requests

1. Open an issue first for anything non-trivial.
2. Keep PRs focused; include tests.
3. `make test lint` must pass.
4. One maintainer approval is required to merge (see [GOVERNANCE.md](GOVERNANCE.md)).
