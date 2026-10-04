# Changelog

All notable changes to APVA. Versions follow [semantic versioning](https://semver.org);
before v1.0, minor versions may change behaviour.

## v0.3.0 — Release 1 (AWS): node-aware resizing

### Added
- **Capacity check before every resize.** Upsizes and downsizes alike: all replicas must
  fit, and so must the rollout's surge pod. A resize that wouldn't fit is held or skipped,
  with a reason, instead of leaving pods `Pending`.
- **Pluggable node provider** (`capacity.Provider`), with **Amazon EKS managed node groups**
  as the first provider (`--aws-cluster`, `--aws-nodegroups`, Helm `aws.*`):
  - **scale up first:** raise the group's desired size, wait for Ready nodes, then resize
    the pods;
  - **consolidate after:** drain the least-used node through the Eviction API
    (PodDisruptionBudgets respected), then terminate exactly that instance with
    `ShouldDecrementDesiredCapacity`;
  - **guardrails:** allowlist, min/max size, one change per group with a cooldown,
    scale-up and drain timeouts, no scale-down while pods are Pending, safe-to-evict /
    `kube-system` / `emptyDir` / bare-pod checks, dry-run, and drains that survive an APVA
    restart.
- **Karpenter detection** (`karpenter.sh/nodepool`) and `--node-autoscaler-present` for the
  Cluster Autoscaler: resizes that don't fit yet go ahead, and the autoscaler adds nodes.
- A standard-library AWS client: SigV4, EKS Pod Identity, IRSA and env credentials. Still
  zero third-party Go dependencies.
- Dashboard: node group decisions in the activity log; new outcomes `waiting` and
  `delegated`.
- **Simulated-EKS end-to-end test** in CI (k3s + KWOK + fake EKS / Auto Scaling /
  Prometheus). No AWS account needed.
- Docs: [architecture](docs/architecture.md), [configuration](docs/configuration.md),
  [API](docs/api.md), [testing](docs/testing.md), [AWS](docs/aws.md), and a docs index.

### Changed
- Helm chart 0.3.0 defaults to the v0.3.0 image. Auto-resize now needs `get/list` on pods and nodes (for the capacity check). The Helm
  chart adds this RBAC.
- Cooldown messages are stable, so the activity log isn't flooded.

## v0.2.0 — Auto-resize

### Added
- Opt-in **auto-resize** (`--auto-resize`) of Deployment/StatefulSet requests. Guardrails:
  - minimum confidence, cooldown and a 50% maximum downsize step;
  - never above container limits;
  - skips HPA-managed and `apva.io/auto-resize: "off"` workloads, and waits for rollouts;
  - saves the previous requests in an annotation;
  - dry-run mode.
- Dashboard: auto-resize column and activity log.
- `test/demo/up.sh`: a 5-minute kind demo with a load-generating "shop".
- Replica counts ignore pods a rollout replaced.

## v0.1.0 — Recommendations

### Added
- CPU/memory recommendations from p95 usage + headroom, with a tolerance band and floors.
- GPU analysis from NVIDIA DCGM (idle / share).
- Service graph from Cilium Hubble flows, and a traffic-aware hold on downsizing.
- Confidence levels and per-recommendation reasons.
- Dashboard, JSON API, `/metrics`, Helm chart, kind end-to-end test, `--demo` mode.
