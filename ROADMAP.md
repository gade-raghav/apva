# Roadmap

## v0.1 — Recommend (current)
- [x] CPU / memory right-sizing from Prometheus (p95 + headroom)
- [x] GPU utilisation analysis (NVIDIA DCGM exporter)
- [x] Service graph from Hubble flow metrics
- [x] Traffic-aware context on recommendations
- [x] HTTP API + built-in visualiser
- [x] Helm chart, CI, kind-based e2e

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
- [ ] Safe, opt-in automatic apply with guardrails and rollback

## Community milestones
- Private beta with invited CNCF community members
- Public open-source release
- CNCF Sandbox application
