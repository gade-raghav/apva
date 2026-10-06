# Roadmap

## v0.1 — Recommend
- [x] CPU / memory right-sizing from Prometheus (p95 + headroom)
- [x] GPU utilisation analysis (NVIDIA DCGM exporter)
- [x] Service graph from Hubble flow metrics
- [x] Traffic-aware context on recommendations
- [x] HTTP API + built-in visualiser
- [x] Helm chart, CI, kind-based e2e
- [x] Opt-in automatic resizing of Deployments/StatefulSets with guardrails (pulled forward from v1.0)

## Release 1 — AWS (v0.3)
- [x] Capacity check before any upsize: never resize pods into `Pending`
- [x] EKS managed node groups: scale up before pods grow, wait for Ready nodes
- [x] Consolidation after pods shrink: PDB-respecting drain, terminate exactly the drained instance
- [x] EKS Pod Identity / IRSA credentials, no AWS SDK dependency
- [x] Pluggable node provider (EKS first); Karpenter / Cluster Autoscaler aware
- [x] Simulated EKS end-to-end test in CI (k3s + KWOK + fake AWS), no AWS account needed
- [ ] Real-EKS end-to-end run (needs an AWS account)

## v0.4 — In place and least privilege
- [x] In-place pod resize (no restarts) with rolling-update fallback
- [x] Least-privilege RBAC, namespaced writes
- [x] ValidatingAdmissionPolicies limiting APVA to resource requests
- [ ] Instance-type changes (launch template versions), e.g. A10G → H100
- [ ] Self-managed node groups

## v0.5 — VPA recommender
- [x] APVA as a VPA custom recommender (writes VPA status; the VPA applies it)
- [x] Auto-resize leaves VPA-managed workloads to the VPA
- [ ] Feedback from SIG Autoscaling ([kubernetes/autoscaler#10395](https://github.com/kubernetes/autoscaler/issues/10395))

## v0.2 — Integrate
- [ ] `Recommendation` custom resource written by an in-cluster controller
- [x] Feed recommendations to VPA (done in v0.5 as a custom recommender)
- [ ] KEDA hints
- [ ] Multi-cluster support
- [ ] Cost estimates (OpenCost integration)

## v0.3 — Predict
- [ ] Time-series forecasting of demand (seasonality, trend)
- [ ] Dependency-aware scaling: propagate predicted load along the service graph
- [ ] GPU sharing suggestions (MIG / time-slicing)

## v1.0 — Act (opt-in)
- [ ] Automatic rollback when a resize leads to OOMKills or CPU throttling

## Community milestones
- Private beta with invited CNCF community members
- Public open-source release
- CNCF Sandbox application
