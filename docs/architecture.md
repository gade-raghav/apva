# Architecture

APVA is one Go binary with no third-party dependencies. It runs a loop: **collect →
recommend → act → consolidate → publish**. The loop runs every `--refresh` (default 5m), or
once with `--once`.

```
            ┌──────────────┐   ┌───────────────┐   ┌──────────────┐   ┌──────────────────┐
Prometheus ─▶  collector   ├──▶  recommender  ├──▶   actuator    ├──▶ capacity planner  │
            │ PromQL →     │   │ sizes, holds, │   │ guardrails,  │   │ fit check,       │
            │ workloads,   │   │ GPU, confid.  │   │ patches      │   │ node provider    │
            │ edges        │   └───────┬───────┘   └──────┬───────┘   └────────┬─────────┘
            └──────────────┘           │                  │                    │
                                       ▼                  ▼                    ▼
                                 engine result ◀── auto-resize events   node group events
                                       │
                        dashboard · /api/v1/* · /metrics
```

| Package | Responsibility |
|---|---|
| `internal/collector` | Runs the PromQL queries and turns per-pod samples into per-workload usage, requests, replicas, GPU and traffic edges |
| `internal/recommender` | Pure sizing logic: a snapshot goes in, recommendations come out |
| `internal/engine` | The loop. Holds the latest result, rolls up the summary, builds the graph, calls the actuator and the node manager |
| `internal/actuator` | Auto-resize: guardrails, plans per container, patches the workload, keeps a de-duplicated history |
| `internal/capacity` | The capacity planner (`Ensure`), consolidation (`Consolidate`) and the `Provider` interface |
| `internal/aws` | SigV4 signing, credentials (env / EKS Pod Identity / IRSA), the EKS + Auto Scaling client, and the EKS `NodeProvider` |
| `internal/kube` | A minimal Kubernetes REST client (in-cluster or `kubectl proxy`) and quantity parsing |
| `internal/prom` | The Prometheus instant-query client |
| `internal/api` | The HTTP API, `/metrics` and the embedded dashboard (`ui/`) |
| `internal/demo` | A fake Prometheus with a sample cluster, used by `--demo` |

## 1. The collector

All queries are instant queries over the look-back window `W` (default 24h). The recent
window `R` (default 1h) is used for traffic trends.

| Purpose | Query (simplified) | Required |
|---|---|---|
| CPU p95 per pod | `quantile_over_time(0.95, rate(container_cpu_usage_seconds_total[5m])[W:5m])` | yes |
| Memory p95 per pod | `quantile_over_time(0.95, container_memory_working_set_bytes[W])` | yes |
| CPU / memory requests | `kube_pod_container_resource_requests{resource="cpu"/"memory"}` | yes |
| Running pods | `kube_pod_status_phase{phase="Running"}` | no (better replica counts) |
| GPU requests | `kube_pod_container_resource_requests{resource="nvidia_com_gpu"}` | no |
| GPU utilisation avg / p95 | `DCGM_FI_DEV_GPU_UTIL` over W | no |
| Traffic edges | `rate(hubble_flows_processed_total{source_workload!="",destination_workload!=""}[W])` and `[R]` | no |

Pods are grouped into workloads by name convention: `name-<hash>-<suffix>` for
Deployments, `name-<n>` for StatefulSets, `name-<suffix>` for DaemonSets and Jobs. Per-pod
values are combined conservatively: the **maximum** across pods. Pods that a rollout
replaced still count towards p95 but not towards replicas. An optional source that is
missing becomes a warning on the dashboard, never an error.

## The recommender

For CPU and memory, per pod:

```
need        = p95 × (1 + headroom)                    headroom default 0.15
recommended = round_up(max(need, floor))              floors 10m CPU, 32Mi memory; CPU to 5m, memory to 1Mi
upsize      if p95 > request or need > request × (1 + tolerance)   tolerance default 0.10
downsize    if recommended < request × (1 − tolerance)
set-request if there is no request; ok otherwise; no-data without metrics
```

- **Traffic-aware hold.** For each caller edge, `trend = recent rate / window rate`. If any
  caller's trend is ≥ 1.25, downsizes become `hold` and the reason names the caller.
- **GPU.** Requested but no DCGM data gives `no-data`. A p95 below 5% (with no rising
  callers) gives `idle`, and the GPUs count as savings. Below 40% gives `share` (suggests
  MIG or time-slicing). Otherwise `ok`.
- **Confidence.** `low` when there are no replicas or no metrics. `medium` when the window
  is under 24h or callers are rising. `high` otherwise.
- **Savings** = (current − recommended) × replicas, summed for the dashboard.

## 2. The actuator (auto-resize)

For each recommendation with a CPU or memory `upsize`/`downsize`, the actuator goes through
these steps in order. The first one that fails stops it, and the outcome is recorded.

1. Confidence ≥ `--auto-resize-min-confidence`.
2. Find the Deployment, otherwise the StatefulSet. Anything else is skipped.
3. `apva.io/auto-resize: "off"`/`"false"` → skip.
4. Targeted by any HPA → skip (the HPA's target utilisation already gives headroom).
5. `apva.io/last-resized` within `--auto-resize-cooldown` → skip.
6. **Plan** per container:
   - the spec's request sum must match what the running pods report, otherwise a rollout
     is in progress and APVA skips;
   - downsizes are limited to `--auto-resize-max-down` per step;
   - containers get proportional shares;
   - a request is never raised above the container's limit;
   - rounding to whole millicores or MiB.
7. **In place first** (`--resize-mode auto`/`in-place`, Kubernetes 1.33+, see
   [auto-resize.md](auto-resize.md#in-place-or-rolling-update)):
   - list the workload's pods by selector;
   - give up if any pod is Guaranteed QoS;
   - `FitsInPlace`: every node must have room for its pods' growth;
   - patch `pods/<name>/resize` for each pod;
   - record the size in `apva.io/in-place-requests`. Only annotations change; the template
     is untouched.

   If any of these fails, it falls back to steps 8–9 with the reason. Each round also
   brings pods that don't match the recorded size (created from the template later) to
   it. A workload's "current" requests are its template with that annotation applied.
8. **Capacity planner** (`Ensure`), for a rolling update:
   - `Fits` → continue;
   - `Waiting` → outcome `waiting`, nodes are being added;
   - `Blocked` → outcome `skipped` with the reason.
9. Dry-run → outcome `dry-run`. Otherwise a strategic-merge patch of the template's
   requests, plus the annotations `apva.io/last-resized`, `apva.io/previous-requests` and
   `apva.io/last-resize-summary`. `apva.io/in-place-requests` is removed.

The history keeps the last 100 decisions, newest first, and records a repeated identical
decision only once.

## 3. The capacity planner

`Ensure(workload, newCPU, newMem, replicas)` loads all nodes and active pods. A pod's
effective request is the sum of its containers, or its largest init container if that is
bigger, plus overhead. Then the planner:

1. Finds the node pools the workload's pods run on. A pool is the provider's group label
   (EKS: `eks.amazonaws.com/nodegroup`), or `karpenter:<nodepool>` for Karpenter nodes.
2. Simulates the rollout on the schedulable nodes of those pools:
   - **after**: free capacity plus the workload's own pods, which the resize replaces.
     All replicas must fit (first fit, over CPU, memory, GPU and pod slots);
   - **surge**: free capacity *without* releasing anything. The first new pod must fit.
3. If both fit, the answer is `Fits`. Otherwise:

   | Pool | Answer |
   |---|---|
   | Pods span several pools | `Blocked` |
   | Not managed, but Karpenter or `--node-autoscaler-present` | `Fits`; the event `delegated` says the autoscaler will add nodes |
   | Not managed | `Blocked`, with what's missing |
   | Managed, a scale-up already pending | `Waiting` until enough nodes are Ready (then `Fits` and the group's cooldown restarts), or `Blocked` after `--aws-scale-up-timeout` |
   | Managed | Computes the pods per fresh node (allocatable minus DaemonSet pods). `0` → `Blocked` ("needs a larger instance type"). Otherwise `extra = ⌈missing / per-node⌉`; above `maxSize` → `Blocked`; in cooldown → `Waiting`; otherwise `SetDesiredSize(desired + extra)` → `Waiting` |

### Consolidation

Consolidation runs once per round, after the actuator, for each allowlisted group. Nothing
happens while any pod in the cluster is Pending.

- **A node is already draining** (it carries `apva.io/draining`):
  - only DaemonSet pods left → `RemoveNode` (EKS: `TerminateInstanceInAutoScalingGroup`
    with `ShouldDecrementDesiredCapacity=true`);
  - past `--aws-drain-timeout` → uncordon and record `failed`;
  - a pod that can't be evicted appeared → uncordon;
  - otherwise evict again. A `429` from a PodDisruptionBudget simply waits for the next
    round.
- **Otherwise**, if the group is not scaling up and not in cooldown, and is above `minSize`:
  1. Take the candidates: schedulable nodes whose CPU, memory and GPU requests are all
     below `--aws-consolidate-below`, least-used first.
  2. Skip a node with a bare, `kube-system`, on-disk `emptyDir` or `safe-to-evict=false`
     pod.
  3. Check every movable pod fits on the group's other nodes.
  4. Cordon the node, annotate it and evict its pods through the Eviction API.

### Providers

```go
type Provider interface {
	Name() string
	GroupLabel() string                                    // node label naming a node's group
	DescribeGroup(ctx, group) (Group, error)               // min, max, desired, status
	SetDesiredSize(ctx, Group, desired int) error
	RemoveNode(ctx, *Node) error                           // exactly this node; desired − 1
}
```

The Amazon EKS provider is `internal/aws.NodeProvider`. It uses `eks:DescribeNodegroup`,
`eks:UpdateNodegroupConfig` and `autoscaling:TerminateInstanceInAutoScalingGroup`. Other
clouds implement the same five methods.

## 4. Order of operations

- **Growing:** the planner adds nodes first and answers `Waiting`. The pods are patched in
  a later round, once the nodes are Ready.
- **Shrinking:** pods are patched in this round's actuator step. Nodes are drained in the
  consolidation step that follows, and removed once empty.
- **Restarts:** the cooldowns, drains and previous requests live in annotations on the
  workloads and nodes, so a restarted APVA continues where it left off. A pending scale-up
  is re-derived; the nodes it asked for still arrive.

## 5. Permissions

| Mode | Kubernetes RBAC | AWS IAM |
|---|---|---|
| Recommend-only | none (no token mounted) | none |
| `autoResize.enabled` | list pods, nodes (cluster); get/patch deployments, statefulsets, list HPAs, patch pods/resize (namespaced to `analysis.namespaces` when set) | none |
| `+ aws.enabled` | + patch nodes, create pods/eviction | `eks:DescribeNodegroup`, `eks:UpdateNodegroupConfig`, `autoscaling:TerminateInstanceInAutoScalingGroup` |

On top of RBAC, ValidatingAdmissionPolicies limit what those patches may change. Details:
[security.md](security.md).
