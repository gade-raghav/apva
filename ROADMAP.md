# Roadmap

## v0.1 — Recommend (current)
- [x] CPU / memory right-sizing from Prometheus (p95 + headroom)
- [x] GPU utilisation analysis (NVIDIA DCGM exporter)
- [x] Service graph from Hubble flow metrics
- [x] Traffic-aware context on recommendations
- [x] HTTP API + built-in visualiser
- [x] Helm chart, CI, kind-based e2e
- [x] Opt-in automatic resizing of Deployments/StatefulSets with guardrails (pulled forward from v1.0)

## v0.2 — Integrate
- [ ] `Recommendation` custom resource written by an in-cluster controller
- [ ] Export recommendations as VPA objects (recommend-only mode) and KEDA hints
- [ ] Multi-cluster support
- [ ] Cost estimates (OpenCost integration)

## v0.3 — Predict
- [ ] Time-series forecasting of demand (seasonality, trend)
- [ ] Dependency-aware scaling: propagate predicted load along the service graph
- [ ] GPU sharing suggestions (MIG / time-slicing)

## v1.0 — Act (opt-in)
- [ ] Automatic rollback when a resize leads to OOMKills or CPU throttling
- [ ] In-place pod resize (no restarts) where the cluster supports it

## Community milestones
- Private beta with invited CNCF community members
- Public open-source release
- CNCF Sandbox application
