# Positioning: how APVA relates to existing projects

APVA is designed to **complement** the existing cloud native ecosystem, not replace it.
It consumes data other projects produce and emits recommendations other projects can act on.

## One-line differentiator

> APVA right-sizes CPU, memory **and GPU** using the **service dependency graph**, so
> recommendations account for who calls whom — not just one workload in isolation.

## Landscape

| Project | What it does | Relationship to APVA |
|---|---|---|
| Kubernetes HPA | Scales replica count on metrics | APVA sizes pods, the HPA sizes the count. Auto-resize skips HPA-managed workloads (the HPA target already gives headroom) |
| Kubernetes VPA | Recommends/sets CPU & memory requests per workload | Closest overlap. APVA adds GPU, the service graph, and cross-workload context; can export VPA-compatible recommendations |
| KEDA (CNCF graduated) | Event-driven horizontal scaling (incl. scale to zero) | KEDA decides *how many* pods, APVA *how big*. KEDA works through an HPA, so auto-resize leaves KEDA-scaled workloads alone; scaling hints for KEDA are on the roadmap |
| Karpenter | Node provisioning and consolidation | Detected automatically: APVA resizes pods, Karpenter adds and consolidates nodes. Better requests → better bin-packing |
| Cluster Autoscaler | Node group scaling for Pending pods | With `--node-autoscaler-present`, APVA resizes and the autoscaler adds nodes. Don't allowlist the same node groups in APVA's EKS provider |
| EKS managed node groups (no autoscaler) | Fixed-size groups | APVA's EKS provider grows a group before resizing pods and drains + removes under-used nodes afterwards |
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

## Why node-awareness matters

Resizing pods changes how many nodes you need. Most recommenders stop at the pod. A bigger
pod that no node can hold sits in `Pending`, and even a smaller pod's rolling update needs
room for its surge pod. APVA checks every resize against real node capacity. It either
hands the node change to the autoscaler you already run, or (on EKS without one) makes it
itself, in the safe order: nodes first when growing, pods first when shrinking.

## Why GPU matters

AI/ML workloads make GPU the most expensive resource in many clusters, and GPUs are
commonly requested whole and left idle. APVA surfaces GPU utilisation next to CPU/memory
and flags idle or underused accelerators.
