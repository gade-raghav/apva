# APVA — Autoscale Provisioning Visualising Adaptor

> Traffic-aware right-sizing for Kubernetes workloads, with first-class GPU support.

APVA watches how your workloads talk to each other **and** how much CPU, memory and GPU
they actually use, then tells you (or your autoscaler) how big each workload should be.

Most right-sizing tools look at one workload at a time. APVA adds the service graph: it
knows that `checkout` calls `inference`, so when checkout traffic drops, it can say with
confidence that the GPU-backed `inference` deployment is over-provisioned too.

```
            ┌──────────────────── Kubernetes cluster ─────────────────────┐
            │                                                             │
 metrics ──▶│ Prometheus  (cAdvisor, kube-state-metrics, DCGM GPU exporter)│
 flows   ──▶│ Hubble / eBPF flow metrics                                   │
            │        │                                                    │
            │        ▼                                                    │
            │  ┌───────────┐   ┌────────────┐   ┌──────────────────────┐   │
            │  │ Collector │──▶│  Engine    │──▶│ Recommendations      │   │
            │  └───────────┘   │ (analyse,  │   │  • HTTP API / UI     │   │
            │                  │  predict)  │   │  • Recommendation CR │   │
            │                  └────────────┘   │  • VPA / KEDA hints  │   │
            │                                   └──────────────────────┘   │
            └─────────────────────────────────────────────────────────────┘
```

## Status

**Early development (v0.1, pre-alpha).** APVA is recommend-only by default: it never changes
your workloads unless you turn on [automatic resizing](docs/auto-resize.md) (`--auto-resize`).
See [ROADMAP.md](ROADMAP.md).

## What it does today

| Capability | How |
|---|---|
| CPU / memory right-sizing | p95 usage over a look-back window + configurable headroom, compared with current requests |
| GPU right-sizing | GPU utilisation from the NVIDIA DCGM exporter; flags idle or under-used GPUs |
| Service graph | Workload-to-workload request rates from Hubble flow metrics |
| Traffic-aware confidence | Recommendations for a workload include its upstream callers and their traffic trend |
| Automatic resizing (opt-in) | Patches Deployment/StatefulSet requests with guardrails: confidence threshold, cooldown, 50% max downsize step, never above limits, skips HPA-managed and opted-out workloads — see [docs/auto-resize.md](docs/auto-resize.md) |
| **Node-aware resizing** | Checks every resize will fit (including the rollout's surge pod); works with Karpenter / Cluster Autoscaler; with the **Amazon EKS provider** (opt-in) scales managed node groups up *before* resizing and drains + removes under-used nodes *after* — see [docs/aws.md](docs/aws.md) |
| Visualiser | Built-in web UI: service graph coloured by waste, with a recommendations table |
| API | `GET /api/v1/recommendations`, `GET /api/v1/graph`, `GET /healthz` |

## Quick start

Prerequisites: a cluster with Prometheus. Hubble and the DCGM exporter are optional; APVA
works without them and skips graph or GPU analysis.

```bash
helm install apva ./charts/apva \
  --namespace apva --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80

kubectl -n apva port-forward svc/apva 8080:8080
open http://localhost:8080
```

Run locally against any Prometheus:

```bash
make build
./bin/apva --prometheus-url=http://localhost:9090 --listen=:8080
```

**See it live in 5 minutes** (kind + Prometheus + a demo shop, auto-resize on):

```bash
./test/demo/up.sh        # then open http://localhost:8080
```

**Beta testers:** start with [docs/beta-testing.md](docs/beta-testing.md).

## How APVA compares

See [docs/positioning.md](docs/positioning.md). In short: APVA complements the Kubernetes
autoscalers (HPA/VPA), KEDA and Karpenter rather than replacing them, and adds the
network dependency graph and GPU awareness that single-workload recommenders lack.

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md). All commits must be
signed off under the [DCO](https://developercertificate.org/).

## Community

- Code of Conduct: [CNCF Code of Conduct](CODE_OF_CONDUCT.md)
- Governance: [GOVERNANCE.md](GOVERNANCE.md)
- Security: [SECURITY.md](SECURITY.md)

## License and attribution

Apache License 2.0 — see [LICENSE](LICENSE). Created by [Raghav Gade](https://github.com/gade-raghav); redistributions must keep the [NOTICE](NOTICE) file.
