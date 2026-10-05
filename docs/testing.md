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

- **Kubernetes:** a real **1.35** control plane, k3s started with `--disable-agent`, so the
  API server, scheduler, controllers, RBAC and admission are real but there are no kubelets.
- **Nodes and pods:** [KWOK](https://kwok.sigs.k8s.io) simulates them.
  `test/e2e-aws/kwok-stages.sh` makes KWOK containers report
  `status.containerStatuses[].resources` like a 1.35 kubelet, so the API server accepts
  in-place resizes.
- **AWS and Prometheus:** `test/e2e-aws/fake.py` plays EKS (`DescribeNodegroup`,
  `UpdateNodegroupConfig`), Auto Scaling (`TerminateInstanceInAutoScalingGroup`) and
  Prometheus. A node group change creates or deletes KWOK nodes. Unsigned AWS requests are
  rejected.
- **APVA** runs **as its own service account**: a `kubectl proxy --as` impersonates it,
  with the chart's RBAC and ValidatingAdmissionPolicies installed from `helm template`.
  Every call APVA makes has to pass the production permissions.

The test asserts this sequence:

| Phase | Expected |
|---|---|
| 1. in place | `web` (4 × 1 core, 2 per 4-core node) needs 1.84 cores/pod and its nodes have room → pods resized **in place**: same pod names, template still `1`, no node group call |
| 2. rollout + nodes | It needs 2.99 cores/pod; growing in place won't fit → **rolling update**. The node group is scaled 2 → 4 first, the template becomes 2990m, rolled out, and the in-place annotation is cleared |
| 3. shrink + consolidate | Usage drops to 0.3 and `minSize` drops to 1 → pods shrink **in place** (1495m, then further), and the template stays 2990m. Under-used nodes are drained through the Eviction API and terminated. Pods recreated from the larger template are brought to the in-place size so they fit → **1 node** with all 4 pods running. Each created node except one was terminated exactly once |
| 4. security | `kubectl auth can-i` as APVA: no Secrets, ConfigMaps, pod creation, deletes, other namespaces or RBAC. Patches by APVA's identity that change the image, env, replicas or service account, or label a node, are **denied by the admission policies** |

Run it locally against any cluster with KWOK:

```bash
./test/e2e-aws/kwok-stages.sh > stages.yaml
kwok --kubeconfig=$KUBECONFIG --manage-all-nodes=false \
  --manage-nodes-with-annotation-selector=kwok.x-k8s.io/node=fake --config=stages.yaml &
make e2e-aws    # needs helm (or APVA_RBAC_MANIFEST=<rendered rbac + policies>)
```

or copy the two setup steps from the workflow (k3s + KWOK, both single binaries from
GitHub releases).

This test has caught real bugs before release:
- **Shrinking is also a rolling update.** On full nodes the surge pod stayed Pending
  forever, so the capacity check now runs before every rolling update.
- **New pods start at the template size.** After an in-place shrink, a pod recreated by a
  node drain came back at the template's larger size and didn't fit. APVA now brings new
  pods to the in-place size, including while they are Pending.

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
