# Automatic resizing

By default APVA only recommends. With `--auto-resize` (Helm: `autoResize.enabled=true`) it
also applies its CPU and memory recommendations by patching the requests of Deployments and
StatefulSets. This triggers a normal rolling update.

## Guardrails

A workload is resized only when **all** of these hold:

| Guardrail | Default | Why |
|---|---|---|
| Recommendation confidence ≥ `--auto-resize-min-confidence` | `high` (needs ≥ 24h of data) | don't act on thin data |
| Not targeted by a HorizontalPodAutoscaler | always | the HPA target utilisation already provides headroom; changing requests changes its maths |
| Not annotated `apva.io/auto-resize: "off"` | always | per-workload opt-out |
| Running pods match the spec | always | never stack a change on an unfinished rollout |
| Not resized within `--auto-resize-cooldown` | `30m` | let new pods produce data first |
| Larger pods fit on the nodes (upsizes only) | always | never resize pods into `Pending`; on EKS APVA can add nodes first — see [aws.md](aws.md) |

And every change is bounded:

- **Downsizing is stepped**: a request shrinks by at most `--auto-resize-max-down` (default
  50%) per step, so big cuts happen gradually over several cooldowns.
- **Upsizing is never above a container's limit**: APVA does not raise limits.
- **Multi-container pods** are scaled proportionally, keeping each container's share.
- Workloads whose recommendation is `hold` (callers' traffic is rising) are never shrunk.

Use `--auto-resize-dry-run` to see every decision in the dashboard without changing anything.

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
