# Positioning: how APVA relates to existing projects

APVA is designed to **complement** the existing cloud native ecosystem, not replace it.
It consumes data other projects produce and emits recommendations other projects can act on.

## One-line differentiator

> APVA right-sizes CPU, memory **and GPU** using the **service dependency graph**, so
> recommendations account for who calls whom — not just one workload in isolation.

## Landscape

| Project | What it does | Relationship to APVA |
|---|---|---|
| Kubernetes HPA | Scales replica count on metrics | APVA can suggest targets; HPA acts |
| Kubernetes VPA | Recommends/sets CPU & memory requests per workload | Closest overlap. APVA adds GPU, the service graph, and cross-workload context; can export VPA-compatible recommendations |
| KEDA (CNCF graduated) | Event-driven autoscaling | APVA can provide scaling hints / scalers; KEDA acts |
| Karpenter | Node provisioning | Operates at node level; APVA at workload level. Better requests → better bin-packing |
| OpenCost (CNCF incubating) | Cost allocation & monitoring | Planned integration for cost of waste; APVA focuses on the "what size should it be" question |
| Goldilocks, Robusta KRR | Per-workload request recommendations | Single-workload, CPU/memory only. APVA adds GPU + dependency context |
| Cilium Hubble, Pixie, Kiali | Network/service visibility | APVA **consumes** Hubble flow metrics; it's not a network observability tool |
| Kepler | Energy measurement | Complementary signal for future carbon-aware recommendations |

## Why the dependency graph matters

1. **Explainability** — "`inference` is over-provisioned because its only caller,
   `checkout`, dropped 40% in traffic" is more actionable than a bare number.
2. **Safer downsizing** — a workload with a fast-growing upstream shouldn't be shrunk
   even if its own recent usage is low.
3. **Coordinated scaling (roadmap)** — predicted load can be propagated down the graph so
   downstream services are ready before traffic arrives.

## Why GPU matters

AI/ML workloads make GPU the most expensive resource in many clusters, and GPUs are
commonly requested whole and left idle. APVA surfaces GPU utilisation next to CPU/memory
and flags idle or underused accelerators.
