# Testing

APVA has four layers of tests. Three of them run on every pull request.

| Layer | Command | In CI | Needs |
|---|---|---|---|
| Unit tests | `make test` | `CI` | Go |
| End-to-end on kind | `make e2e` | `E2E` | Docker, kind, kubectl, helm |
| End-to-end, simulated Amazon EKS | `make e2e-aws` | `E2E (AWS, simulated)` | Go, Python 3, kubectl, jq, a cluster with KWOK (CI starts k3s) |
| Live demo | `./test/demo/up.sh` | no | Docker, kind, kubectl, helm |

`make lint` (gofmt + go vet) and a Helm lint/template also run in `CI`, along with a
check that every commit is signed off (DCO).

## Unit tests

`go test -race ./...` covers every package. Highlights:

- **aws:** SigV4 signing reproduces the AWS documented example signature. Also covered:
  credentials from env, EKS Pod Identity and IRSA (STS, with caching); EKS and Auto
  Scaling requests and error parsing; providerID → instance ID.
- **capacity:**
  - rollout fit including the surge pod;
  - scale-up → waiting → fits;
  - scale-up timeout, `maxSize`, "needs a larger instance type", dry-run;
  - Karpenter and Cluster Autoscaler delegation;
  - consolidation: drain → terminate → cooldown, `minSize`, pods that won't fit, bare
    pods, PDB refusal and drain timeout;
  - no consolidation right after a scale-up or while pods are Pending.
- **actuator:**
  - stepped downsizing and the limit cap;
  - opt-out, cooldown, HPA, rollout-in-progress and dry-run guardrails;
  - capacity gating of every resize;
  - de-duplicated history.
- **collector / recommender / engine / api:** queries, aggregation (replaced pods don't
  count as replicas), sizing, holds, GPU, the summary and the endpoints.

## End-to-end on kind

`test/e2e/run.sh`:
1. creates a kind cluster, installs Prometheus and two test workloads, and installs APVA
   with Helm;
2. waits until APVA recommends **downsizing** the idle one and **upsizing** the busy one;
3. checks `/metrics` and the dashboard.

`KEEP=true` keeps the cluster afterwards.

## End-to-end, simulated Amazon EKS (no AWS account)

`test/e2e-aws/run.sh` with `.github/workflows/e2e-aws.yml`:

- **Kubernetes:** a real control plane, k3s started with `--disable-agent`, so the API
  server, scheduler and controllers are real but there are no kubelets.
- **Nodes and pods:** [KWOK](https://kwok.sigs.k8s.io) simulates them. Pods are scheduled,
  rolled out and evicted by the real Kubernetes code.
- **AWS and Prometheus:** `test/e2e-aws/fake.py` plays EKS (`DescribeNodegroup`,
  `UpdateNodegroupConfig`), Auto Scaling (`TerminateInstanceInAutoScalingGroup`) and
  Prometheus. A node group change creates or deletes KWOK nodes labelled
  `eks.amazonaws.com/nodegroup=gpu`. Unsigned AWS requests are rejected.
- **APVA** runs as a process with `--auto-resize --aws-cluster=e2e --aws-nodegroups=gpu`.

The test asserts this sequence:

| Phase | Expected |
|---|---|
| 1 | `web` (2 × 1 core) uses ~3.4 cores/pod. Nothing fits the rollout's surge pod → `UpdateNodegroupConfig desired=3` → 3 Ready nodes → `web` resized to 3910m → rolled out → the node left empty is terminated → back to 2 nodes, app available |
| 2 | Usage drops to 0.3 cores, `minSize` lowered to 1 → `web` shrinks (1955m, a 50% step) → under-used nodes drained through the Eviction API and terminated → 1 node, app available. Each created node except one was terminated exactly once |

Run it locally against any cluster with KWOK:

```bash
kwok --kubeconfig=$KUBECONFIG --manage-all-nodes=false \
  --manage-nodes-with-annotation-selector=kwok.x-k8s.io/node=fake --config=stage-fast.yaml &
make e2e-aws
```

or copy the two setup steps from the workflow (k3s + KWOK, both single binaries from
GitHub releases).

This test found a real bug before release: shrinking pods is also a rolling update, and
on full nodes the surge pod stayed Pending forever. That's why the capacity check now runs
before **every** resize.

## Testing on real AWS

There is no real-EKS run yet. The plan:
- a manually triggered workflow that creates a small EKS cluster with `eksctl`;
- it runs the same scenario in dry-run and then live;
- it deletes everything afterwards.

It needs an AWS account connected through GitHub OIDC. Until then, run APVA with
`autoResize.dryRun=true` on a test cluster and compare its activity log with what you would
do by hand.

## The live demo

`test/demo/up.sh` creates a kind cluster with Prometheus and a demo shop of 10 mis-sized
workloads that generate real CPU and memory load. It installs APVA with auto-resize on, a
15m window, `minConfidence=medium` and a 5m cooldown. The workloads include:
- an HPA-managed service, which APVA skips;
- an opted-out service;
- a service capped at its limit;
- a StatefulSet.

Options:
- `CILIUM=1` adds Cilium and Hubble for service-graph edges (experimental);
- `DRY_RUN=1` shows decisions without applying them.
