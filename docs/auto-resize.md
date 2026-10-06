# Automatic resizing

By default APVA only recommends. With `--auto-resize` (Helm: `autoResize.enabled=true`) it
also applies its CPU and memory recommendations to Deployments and StatefulSets.

## In place or rolling update

Kubernetes can change a running pod's CPU and memory through the `pods/resize` subresource.
This is in-place pod resize, beta and on by default in 1.33 and GA in 1.35. Usually no
container restarts, unless a container's `resizePolicy` asks for one.

| `--resize-mode` / `autoResize.resizeMode` | What APVA does |
|---|---|
| `auto` (default) | **In place** when the cluster is 1.33+ and every pod can grow on the node it runs on. Otherwise a **rolling update** of the pod template, with the reason in the activity log |
| `in-place` | Only in place; anything that can't be done in place is skipped, with the reason |
| `rollout` | Always a rolling update (the pre-v0.4 behaviour) |

An in-place resize is not possible, so APVA uses a rolling update, when:
- **a node has no room** for its pods to grow, because a pod can't move during an in-place
  resize (the rolling update path can add nodes first);
- **the pods are Guaranteed QoS** (requests = limits), because changing only requests would
  change the QoS class, which Kubernetes refuses;
- the cluster is **older than 1.33**, or its nodes don't support it. The first refusal is
  remembered and APVA stops trying.

Shrinking in place needs no surge pod and no spare node: it always fits.

**After an in-place resize** the pod template still has the old requests, because changing
it would roll the pods. Instead:
- APVA records the size in the `apva.io/in-place-requests` annotation on the workload;
- **any new pod** (a scale-out, an eviction, a node drain, a restart) **is brought to that
  size** on the next round, even while it is still Pending;
- if you change the template's requests yourself, the annotation is ignored and your
  template wins;
- the next rolling update by APVA writes the size into the template and removes the
  annotation.

This is the same trade-off as the VPA's `InPlaceOrRecreate` mode.

## Guardrails

A workload is resized only when **all** of these hold:

| Guardrail | Default | Why |
|---|---|---|
| Recommendation confidence ≥ `--auto-resize-min-confidence` | `high` (needs ≥ 24h of data) | don't act on thin data |
| Not targeted by a HorizontalPodAutoscaler | always | the HPA target utilisation already provides headroom; changing requests changes its maths |
| Not targeted by a VerticalPodAutoscaler | always | the VPA owns its requests; two resizers would fight. To get APVA's numbers there, make APVA the VPA's recommender ([vpa.md](vpa.md)) |
| Not annotated `apva.io/auto-resize: "off"` | always | per-workload opt-out |
| Running pods match the spec | always | never stack a change on an unfinished rollout |
| Not resized within `--auto-resize-cooldown` | `30m` | let new pods produce data first |
| The new pods fit on the nodes, including the rollout's surge pod | always, upsizes **and** downsizes | never leave pods `Pending`. Karpenter / Cluster Autoscaler add nodes, or the EKS provider adds them first. See [aws.md](aws.md) |

And every change is bounded:

- **Downsizing is stepped**: a request shrinks by at most `--auto-resize-max-down` (default
  50%) per step, so big cuts happen gradually over several cooldowns.
- **Upsizing is never above a container's limit**: APVA does not raise limits.
- **Multi-container pods** are scaled proportionally, keeping each container's share.
- Workloads whose recommendation is `hold` (callers' traffic is rising) are never shrunk.

Use `--auto-resize-dry-run` to see every decision in the dashboard without changing anything.

## Outcomes in the activity log

| Outcome | Meaning |
|---|---|
| `applied` | the workload was patched; a rolling update follows |
| `dry-run` | it would have been patched |
| `waiting` | nodes are being added first; APVA retries next round |
| `skipped` | a guardrail said no; the reason says which |
| `failed` | an API call failed; the reason has the error |

Permissions and the admission policies that limit what APVA may change are in
[security.md](security.md).

See [configuration.md](configuration.md) for every flag and annotation, and
[architecture.md](architecture.md#2-the-actuator-auto-resize) for the exact order of checks.

## What APVA writes

On each resized workload:

| Annotation | Contents |
|---|---|
| `apva.io/last-resized` | time of the change (drives the cooldown) |
| `apva.io/previous-requests` | JSON of the requests before the change, per container |
| `apva.io/last-resize-summary` | e.g. `CPU 300m → 150m, memory 192Mi → 96Mi per pod` |

To undo a change: `kubectl rollout undo deploy/<name>` (or set the requests from
`apva.io/previous-requests`), then annotate the workload `apva.io/auto-resize=off`.

## Permissions

With `autoResize.enabled=true` the chart mounts a service account token and creates a
ClusterRole allowing `get/list/patch` on `deployments` and `statefulsets` and `get/list` on
`horizontalpodautoscalers`, and `get/list` on `pods` and `nodes` for the capacity check.
`aws.enabled` adds node cordoning and pod eviction ([aws.md](aws.md)).

Running outside the cluster: start `kubectl proxy` and pass `--kube-api=http://127.0.0.1:8001`.

## Try it

```bash
./test/demo/up.sh   # kind + Prometheus + demo shop + APVA with auto-resize on
```
