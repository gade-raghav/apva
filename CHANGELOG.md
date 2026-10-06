# Changelog

All notable changes to APVA. Versions follow [semantic versioning](https://semver.org);
before v1.0, minor versions may change behaviour.

## v0.5.0 — VPA recommender (2026-10-06)

Helm chart 0.5.0 defaults to the v0.5.0 image. The chart version was not bumped for
v0.4.0, so chart 0.5.0 is the first to include both releases.

### Added
- **APVA as a Vertical Pod Autoscaler custom recommender** (`--vpa-recommender`, Helm
  `vpaRecommender.enabled`). For VPAs that set `spec.recommenders: [{name: apva}]`, APVA
  writes `status.recommendation`: per-container target, bounds and uncapped target, with
  the traffic-aware hold, VPA container policies honoured, and `RecommendationProvided` /
  `LowConfidence` conditions. The VPA's updater and admission controller apply it. No VPA
  change needed (AEP-3919). Discussion:
  [kubernetes/autoscaler#10395](https://github.com/kubernetes/autoscaler/issues/10395).
  See [docs/vpa.md](docs/vpa.md).
- `apva-vpa-recommender` role: list VPAs, patch only `verticalpodautoscalers/status`, get
  deployments/statefulsets; namespaced when `analysis.namespaces` is set.
- VPA recommender end-to-end test (`make e2e-vpa`): Kubernetes 1.35, KWOK, the real VPA
  CRD, APVA as its own ServiceAccount.
- The dashboard's activity feed shows VPA status writes.

### Changed
- Auto-resize **skips every workload targeted by a VerticalPodAutoscaler**, whichever
  recommender it uses, so the two never resize the same pods. The resizer role gets
  `list verticalpodautoscalers` for this.

## v0.4.0 — In-place resize and least privilege (2026-10-05)

### Added
- **In-place pod resize** (`--resize-mode`, Helm `autoResize.resizeMode`; default `auto`).
  On Kubernetes 1.33+ (GA in 1.35) APVA resizes running pods through `pods/resize`: no
  restart, no surge pod, and shrinking needs no spare node. It falls back to a rolling
  update when a node has no room, the pods are Guaranteed QoS, or the cluster doesn't
  support it, with the reason in the activity log. The size is recorded in
  `apva.io/in-place-requests`, and new pods (scale-out, evictions, drains, even while
  Pending) are brought to it. A changed template wins.
- **ValidatingAdmissionPolicies** (`security.admissionPolicy.enabled`, on by default,
  Kubernetes 1.30+). APVA's identity may only change container resource requests and
  `apva.io/*` annotations on workloads, and only cordon nodes. Images, env, commands,
  volumes, security contexts, service accounts, scheduling, replicas, labels, creates,
  deletes and system namespaces are all denied.
- [docs/security.md](docs/security.md): permission matrix, admission policies, AWS, the
  pod's hardening, the dashboard's exposure.
- The simulated-EKS end-to-end test now runs on Kubernetes 1.35, with APVA as its own
  least-privilege service account. It covers in-place resize, rollout fallback with nodes
  first, consolidation with in-place shrink, and checks that the policies deny everything
  else.

### Changed
- **RBAC split by purpose and minimised:**
  - `capacity-reader`: list pods and nodes;
  - `resizer`: get/patch deployments and statefulsets (no `list`), list HPAs,
    patch `pods/resize`. It is bound **per namespace** when `analysis.namespaces` is set;
  - `node-manager` (EKS): patch nodes, create evictions.
- The capacity check runs before every **rolling update**. In-place resizes use a per-node
  fit check instead.

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
