# APVA — Autoscale Provisioning Visualising Adaptor

> Traffic-aware, GPU-aware right-sizing for Kubernetes, which can also apply its own
> recommendations safely, including adding and removing nodes on Amazon EKS.

[![CI](https://github.com/gade-raghav/apva/actions/workflows/ci.yml/badge.svg)](https://github.com/gade-raghav/apva/actions/workflows/ci.yml)
[![E2E](https://github.com/gade-raghav/apva/actions/workflows/e2e.yml/badge.svg)](https://github.com/gade-raghav/apva/actions/workflows/e2e.yml)
[![E2E (AWS, simulated)](https://github.com/gade-raghav/apva/actions/workflows/e2e-aws.yml/badge.svg)](https://github.com/gade-raghav/apva/actions/workflows/e2e-aws.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

APVA watches how much CPU, memory and GPU your workloads **actually use** and **who calls
whom**. From that it works out how big each workload should be. You can use the answer
three ways:

1. **Look:** a built-in dashboard and JSON API with recommendations and a service graph
   coloured by waste. Read-only, and the default.
2. **Let APVA resize pods:** opt-in auto-resize patches Deployment and StatefulSet requests.
   Guardrails include confidence, cooldowns, stepped downsizing, never going above limits
   and skipping HPA-managed workloads.
3. **Let APVA resize nodes too:** on Amazon EKS it grows a managed node group *before*
   pods get bigger, and drains and removes nodes *after* pods get smaller. It also works
   alongside Karpenter or the Cluster Autoscaler.

```
                          ┌─────────────────────────── APVA ───────────────────────────┐
 Prometheus ──────────────▶ Collector ─▶ Recommender ─▶ Actuator ─▶ Capacity planner   │
  cAdvisor, kube-state-    │  usage,       p95+headroom,   guardrails,   "will it fit?" │
  metrics, DCGM (GPU),     │  requests,    traffic hold,   patch         node provider: │
  Hubble flows (graph)     │  graph        GPU idle/share  requests      EKS / Karpenter│
                          │        │                                         │          │
                          │        ▼                                         ▼          │
                          │  Dashboard · JSON API · /metrics        Kubernetes API, AWS │
                          └─────────────────────────────────────────────────────────────┘
```

## Contents

- [Why APVA](#why-apva)
- [Status](#status)
- [Features](#features)
- [Quick start](#quick-start)
- [How a decision is made](#how-a-decision-is-made)
- [Configuration](#configuration)
- [Dashboard and API](#dashboard-and-api)
- [Testing](#testing)
- [Project layout](#project-layout)
- [How APVA compares](#how-apva-compares)
- [Roadmap](#roadmap)
- [Contributing, community, license](#contributing)
- [Documentation index](docs/README.md)

## Why APVA

- **Headroom gets counted twice.** A typical right-sizer sets requests to p95 plus a
  margin, then an HPA targeting 70% adds a second buffer. For example, a p95 of 1000m
  gives a request of 1150m, and the HPA scales out at about 805m per pod, so real
  utilisation ends up around 50%. APVA leaves HPA-managed workloads to the HPA and sizes
  the rest once.
- **Usage alone is not enough context.** A service that looks idle may be about to get busy
  because its callers' traffic is rising. With Cilium Hubble flow metrics, APVA sees the
  service graph and **holds a downsize** while upstream traffic grows.
- **GPUs are the expensive part.** They are requested in whole units and often sit idle.
  APVA reads NVIDIA DCGM utilisation and flags idle GPUs, and GPUs that could be shared
  with MIG or time-slicing.
- **Resizing pods without thinking about nodes breaks things.** A bigger pod that no node
  can hold sits in `Pending`. Even a *smaller* pod's rolling update can stall if there is
  no room for the surge pod. And shrinking pods only saves money once nodes go away. APVA
  plans pods and nodes together.

## Status

**v0.3 (pre-alpha), experimental.** Every feature that changes your cluster is opt-in.

| Version | What it added |
|---|---|
| v0.1 | Recommendations (CPU, memory, GPU), service graph, dashboard, API, Helm chart |
| v0.2 | Opt-in **auto-resize** of Deployments/StatefulSets with guardrails; kind demo |
| v0.3 | **Node-aware resizing**: capacity check before every resize; Karpenter / Cluster Autoscaler aware; **Amazon EKS provider** (scale up first, drain and remove after); simulated-EKS end-to-end test in CI |

See [CHANGELOG.md](CHANGELOG.md) and [ROADMAP.md](ROADMAP.md).

Tested so far:
- unit tests for every package;
- an end-to-end test on a kind cluster with Prometheus;
- a simulated EKS end-to-end test (real Kubernetes scheduling, simulated nodes and AWS).

**Not yet** run against a real EKS account, and GPU analysis has only been tested with
sample data. Feedback from real clusters is very welcome:
[docs/beta-testing.md](docs/beta-testing.md).

## Features

### 1. Recommendations (always on)

| | |
|---|---|
| **CPU and memory** | p95 usage over a look-back window (default 24h) plus headroom (default 15%), compared with current requests. Changes under 10% are ignored to avoid churn. Floors are 10m CPU and 32Mi memory. |
| **GPU** | NVIDIA DCGM utilisation. p95 below 5% means **idle** (and counts as savings). Below 40% suggests **sharing** (MIG / time-slicing). |
| **Service graph** | Workload-to-workload request rates from Hubble flow metrics. |
| **Traffic-aware hold** | If a caller's traffic in the last hour is 25%+ above its window average, downsizing is held. |
| **Confidence** | `high` needs 24h of data and no rising traffic. `medium` covers short windows or rising callers. `low` means no metrics. |
| **Explanations** | Every recommendation lists its reasons and callers. |

Hubble and DCGM are optional. Without them APVA says what it skipped and carries on.
Details: [docs/architecture.md](docs/architecture.md#the-recommender).

### 2. Auto-resize (opt-in: `--auto-resize`)

APVA patches the CPU and memory **requests** of Deployments and StatefulSets, which triggers
a normal rolling update. A workload is only touched when **all** of these guardrails pass:

- the recommendation's confidence is at least `--auto-resize-min-confidence` (default `high`);
- **no HPA targets it**, because the HPA's target utilisation is already headroom;
- it isn't annotated `apva.io/auto-resize: "off"`;
- its running pods match its spec, so APVA never stacks a change on an unfinished rollout;
- it wasn't resized within the cooldown (default 30m);
- **the new pods will fit** (see the next section).

On top of that, every change is bounded:
- a request shrinks by **at most 50% per step**;
- a request never rises above the **container limit**;
- multi-container pods are scaled proportionally;
- the previous requests are saved in the `apva.io/previous-requests` annotation so you can
  roll back.

`--auto-resize-dry-run` shows every decision without changing anything.
Details: [docs/auto-resize.md](docs/auto-resize.md).

### 3. Node-aware resizing (on with auto-resize; node changes opt-in)

Before **every** resize, upsize or downsize, APVA checks that all replicas fit and that
the rollout's first new pod fits while the old ones still run.

| Where the workload's nodes come from | If the new pods don't fit |
|---|---|
| A node group APVA manages (Amazon EKS provider, allowlisted) | **Nodes first:** raise the node group's desired size, wait until the nodes are Ready, then resize the pods |
| **Karpenter** (detected from `karpenter.sh/nodepool`) | Resize anyway; Karpenter provisions nodes for the Pending pods |
| A Cluster Autoscaler (`--node-autoscaler-present`) | Resize anyway; the autoscaler adds nodes |
| Anything else | Don't resize, and say why in the activity log |

After pods shrink, the **EKS provider consolidates**:
1. It picks the least-used node, where CPU, memory and GPU requests are all below 50%.
2. It checks every pod on that node fits on the other nodes.
3. It cordons the node and evicts the pods through the **Eviction API**, so
   PodDisruptionBudgets are respected.
4. It terminates **exactly that instance**, lowering the node group's desired size by one.

Node changes have their own guardrails:
- only allowlisted node groups are touched, always within their min and max size;
- one change at a time per group, with a cooldown;
- scale-up and drain timeouts: on a timeout APVA stops and uncordons the node;
- no scale-down while any pod is Pending;
- no drain of nodes running bare pods, `kube-system` pods, on-disk `emptyDir` pods or pods
  marked `safe-to-evict=false`;
- an in-flight drain survives an APVA restart, because it's recorded in the
  `apva.io/draining` annotation.

The AWS client uses only the standard library (SigV4, EKS Pod Identity, IRSA), so APVA
still has **zero third-party Go dependencies**. Node management sits behind a small
provider interface, so other clouds can follow.
Details: [docs/aws.md](docs/aws.md).

## Quick start

### See everything in 5 minutes (kind on your laptop)

Needs Docker, kind, kubectl and helm.

```bash
git clone https://github.com/gade-raghav/apva.git && cd apva
./test/demo/up.sh            # kind + Prometheus + a demo "shop" + APVA with auto-resize on
open http://localhost:8080
./test/demo/down.sh          # when you're done
```

The demo shop has deliberately mis-sized workloads. Within about 5–10 minutes you'll see
APVA:
- step over-provisioned services down;
- cap an under-provisioned one at its limit;
- leave an HPA-managed service alone.

`DRY_RUN=1 ./test/demo/up.sh` shows the decisions without applying them.

### Just look, with no cluster

```bash
go run ./cmd/apva --demo     # built-in sample cluster with GPUs and a service graph
```

### Read-only in your cluster

```bash
helm install apva ./charts/apva -n apva --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80
kubectl -n apva port-forward svc/apva 8080:8080
```

Or run it on your laptop against any Prometheus:
`go run ./cmd/apva --prometheus-url=http://localhost:9090`.

### Turn on auto-resize (dry-run first)

```bash
helm upgrade apva ./charts/apva -n apva --reuse-values \
  --set autoResize.enabled=true --set autoResize.dryRun=true
# watch the activity log; then:
helm upgrade apva ./charts/apva -n apva --reuse-values --set autoResize.dryRun=false
```

### On Amazon EKS with node groups

Create the IAM role ([docs/aws.md](docs/aws.md#setup)), then:

```bash
helm upgrade --install apva ./charts/apva -n apva --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80 \
  --set autoResize.enabled=true --set autoResize.dryRun=true \
  --set aws.enabled=true --set aws.cluster=prod --set aws.region=us-east-1 \
  --set aws.nodegroups="gpu-a10g\,general"
```

On **Karpenter** clusters, leave `aws.enabled` off. APVA resizes the pods and Karpenter
handles the nodes.

## How a decision is made

Every `--refresh` interval (default 5m) APVA:

1. **Collects** usage, requests, running pods, GPU utilisation and flows from Prometheus.
2. **Recommends** per workload: p95 + headroom, the tolerance band, floors, the
   traffic-aware hold, GPU idle/share, and a confidence level.
3. **Acts** (if auto-resize is on), for each workload with an upsize or downsize:
   guardrails → step limit and limit cap → **capacity planner** (fits / wait for nodes /
   blocked) → patch the requests and record the previous ones.
4. **Consolidates** (if a node provider is on): finishes or starts at most one drain per
   node group, then removes the drained instance.
5. **Publishes** the result to the dashboard, the API and `/metrics`, including every
   decision it *didn't* take and why.

Pods change first when shrinking and nodes change first when growing. Full walkthrough:
[docs/architecture.md](docs/architecture.md).

## Configuration

All flags, environment variables and Helm values are listed in
**[docs/configuration.md](docs/configuration.md)**. The ones you'll use most:

| Flag | Helm value | Default |
|---|---|---|
| `--prometheus-url` | `prometheus.url` | `http://localhost:9090` |
| `--window` / `--headroom` | `analysis.window` / `analysis.headroom` | `24h` / `0.15` |
| `--namespaces` | `analysis.namespaces` | all non-system |
| `--auto-resize` | `autoResize.enabled` | off |
| `--auto-resize-dry-run` | `autoResize.dryRun` | off |
| `--node-autoscaler-present` | `autoResize.nodeAutoscalerPresent` | off |
| `--aws-cluster` `--aws-nodegroups` | `aws.enabled` `aws.cluster` `aws.nodegroups` | off |

**Annotations** on your workloads:
- `apva.io/auto-resize: "off"`: never resize this workload.
- `apva.io/safe-to-evict: "false"`: never drain a node running this pod. APVA also honours
  `cluster-autoscaler.kubernetes.io/safe-to-evict`.

## Dashboard and API

The dashboard at `/` shows:
- headline tiles: workloads, over/under-provisioned, held, reclaimable CPU/memory, idle
  GPUs, auto-resized;
- the service graph coloured by status;
- a per-workload recommendations table with an auto-resize column;
- a details panel with reasons and callers;
- an **activity log** of every pod and node group decision.

| Endpoint | Returns |
|---|---|
| `GET /api/v1/result` | everything: summary, recommendations, graph, auto-resize and node group events |
| `GET /api/v1/recommendations?namespace=&action=` | filtered recommendations |
| `GET /api/v1/summary` · `GET /api/v1/graph` | the summary tiles · the service graph |
| `GET /metrics` | `apva_workloads`, `apva_recommendations{action}`, `apva_potential_savings{resource}` |
| `GET /healthz` · `GET /readyz` · `GET /api/v1/version` | probes and version |

Shapes and examples: [docs/api.md](docs/api.md).

## Testing

| Suite | Runs | What it proves |
|---|---|---|
| Unit tests (`make test`) | every PR | every package, including SigV4 against the AWS test vector, every guardrail, capacity and consolidation path |
| **E2E** (`make e2e`) | every PR | kind + Prometheus + real workloads: APVA recommends downsizing an idle workload and upsizing a busy one |
| **E2E (AWS, simulated)** (`make e2e-aws`) | every PR | real Kubernetes control plane (k3s) + **KWOK** nodes + fake EKS/Auto Scaling/Prometheus. APVA scales the node group 2 → 3 *before* resizing, removes the empty node, then shrinks pods and drains down to the minimum, with the app available throughout. **No AWS account needed.** |
| Demo (`./test/demo/up.sh`) | by hand | the dashboard with live auto-resize on a laptop |

Details and how to run each locally: [docs/testing.md](docs/testing.md).

## Project layout

```
cmd/apva/            main: flags, wiring
internal/collector/  PromQL queries → per-workload usage, requests, GPU, flows
internal/recommender/ sizing logic, holds, GPU idle/share, confidence
internal/engine/     periodic loop, roll-up, service graph, result
internal/actuator/   auto-resize: guardrails, patches, history
internal/capacity/   capacity planner, consolidation, Provider interface
internal/aws/        EKS / Auto Scaling client, SigV4, credentials, EKS node provider
internal/kube/       minimal Kubernetes REST client, quantities
internal/prom/       Prometheus client
internal/demo/       built-in sample cluster (--demo)
internal/api/        HTTP API and the embedded dashboard (ui/)
charts/apva/         Helm chart
test/e2e/            kind end-to-end test
test/e2e-aws/        simulated EKS end-to-end test (k3s + KWOK + fake AWS)
test/demo/           5-minute laptop demo
docs/                documentation (start at docs/README.md)
```

## How APVA compares

APVA **complements** the autoscalers rather than replacing them.

- **KEDA** and the **HPA** decide *how many* pods; APVA decides *how big* each pod is, and
  stays out of the way of HPA-managed workloads.
- **Karpenter** and the **Cluster Autoscaler** decide *which nodes* to run; APVA lets them
  add nodes, or manages EKS node groups itself where neither runs.
- Compared with **VPA, Goldilocks or KRR**, APVA adds the service graph, GPU awareness and
  node-aware application.

See [docs/positioning.md](docs/positioning.md).

## Roadmap

Highlights, from [ROADMAP.md](ROADMAP.md):
- a real-EKS end-to-end run;
- instance-type changes (e.g. A10G → H100);
- in-place pod resize (no restarts);
- automatic rollback on OOMKills;
- a `Recommendation` CRD;
- VPA/KEDA exports;
- OpenCost integration;
- forecasting.

## Contributing

Contributions and beta feedback are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).
Every commit must be signed off under the [DCO](https://developercertificate.org/)
(`git commit -s`).

## Community

- Kubernetes Slack: `#sig-autoscaling`
- Code of Conduct: [CNCF Code of Conduct](CODE_OF_CONDUCT.md) · Governance: [GOVERNANCE.md](GOVERNANCE.md) · Security: [SECURITY.md](SECURITY.md)

## License and attribution

Apache License 2.0, see [LICENSE](LICENSE). Created by
[Raghav Gade](https://github.com/gade-raghav). Redistributions must keep the
[NOTICE](NOTICE) file.
