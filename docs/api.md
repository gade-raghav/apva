# HTTP API

Everything the dashboard shows comes from this API. All responses are JSON, except
`/metrics` (Prometheus text format) and the probes. Until the first analysis has finished,
the `/api/v1/*` endpoints answer `503 {"error": "analysis not ready yet"}`.

| Endpoint | Returns |
|---|---|
| `GET /api/v1/result` | the complete latest result (below) |
| `GET /api/v1/recommendations` | `{"generatedAt", "items": [Recommendation…]}`. Filters: `?namespace=shop`, `?action=downsize` (matches the CPU, memory or GPU action) |
| `GET /api/v1/summary` | the summary object |
| `GET /api/v1/graph` | `{"nodes": [...], "links": [...]}` |
| `GET /api/v1/version` | `{"version": "v0.5.0"}` |
| `GET /metrics` | Prometheus metrics (below) |
| `GET /healthz` · `GET /readyz` | liveness · readiness (ready after the first analysis) |
| `GET /` | the dashboard |

## The result

Abridged output of `apva --demo --once`:

```json
{
  "generatedAt": "2026-10-04T05:55:15Z",
  "window": "24h0m0s",
  "summary": {
    "workloads": 9, "downsize": 5, "upsize": 1, "held": 1, "idleGpuWorkloads": 1,
    "cpuSavingsCores": 7.915, "memSavingsBytes": 21633171456, "gpuSavings": 1,
    "autoResized": 0
  },
  "recommendations": [
    {
      "workload": {"namespace": "shop", "name": "recommender"},
      "replicas": 1,
      "cpu":    {"current": 1, "p95": 0.35, "recommended": 0.405, "changePct": -59.5, "action": "downsize"},
      "memory": {"current": 4294967296, "p95": 1887436800, "recommended": 2170552320, "changePct": -49.5, "action": "downsize"},
      "gpu":    {"requested": 1, "avgUtilPct": 18, "p95UtilPct": 31, "action": "share"},
      "upstream": [{"from": {"namespace": "shop", "name": "frontend"}, "ratePerSec": 60, "trend": 0.97}],
      "cpuSavingsCores": 0.595, "memSavingsBytes": 2124414976, "gpuSavings": 0,
      "confidence": "high",
      "reasons": [
        "GPU p95 utilisation 31.0% — consider MIG or time-slicing to share it",
        "CPU p95 0.350 cores vs request 1.000",
        "memory p95 1800Mi vs request 4096Mi"
      ]
    }
  ],
  "graph": {
    "nodes": [{"id": "ml/batch-embedder", "status": "idle-gpu", "replicas": 1, "hasGpu": true, "cpuUtil": 0.025}],
    "links": [{"source": "shop/catalog", "target": "shop/postgres", "ratePerSec": 20, "trend": 0.95}]
  },
  "warnings": ["service graph edges off: no Cilium Hubble flow metrics found (optional)"],
  "autoResize": {
    "enabled": true, "dryRun": false, "minConfidence": "high", "cooldown": "30m0s",
    "events": [
      {"time": "…", "workload": {"namespace": "shop", "name": "frontend"}, "kind": "deployments",
       "outcome": "applied", "reason": "CPU 300m → 150m, memory 192Mi → 96Mi per pod",
       "cpu": {"from": 0.3, "to": 0.15}, "memory": {"from": 201326592, "to": 100663296}}
    ]
  },
  "nodeGroups": {
    "enabled": true, "provider": "aws", "cluster": "prod", "groups": ["gpu"], "consolidate": true,
    "events": [
      {"time": "…", "group": "gpu", "outcome": "applied",
       "reason": "scale gpu 2 → 3 nodes so shop/web fits (1 more pod(s) of 3910m / 256Mi)"}
    ]
  }
}
```

Units: CPU in **cores**, memory in **bytes**, GPU in devices and utilisation in percent.
CPU and memory values are **per pod**; savings are across all replicas.

| Field | Values |
|---|---|
| `cpu.action`, `memory.action` | `ok`, `downsize`, `upsize`, `set-request` (no request configured), `hold` (callers' traffic is rising), `no-data` |
| `gpu.action` | `ok`, `idle`, `share`, `no-data` |
| `confidence` | `high`, `medium`, `low` |
| `graph.nodes[].status` | `ok`, `over`, `under`, `hold`, `idle-gpu`, `unknown` |
| `autoResize.events[].outcome` | `applied`, `dry-run`, `waiting` (nodes being added), `skipped`, `failed` |
| `nodeGroups.events[].outcome` | `applied`, `dry-run`, `waiting`, `delegated` (Karpenter / Cluster Autoscaler will add nodes), `skipped`, `failed` |

`nodeGroups.enabled` is `true` only when a provider manages node groups. Capacity events
(for example `delegated`) can appear without one.

## Metrics

```
apva_workloads 9
apva_recommendations{action="downsize"} 5
apva_recommendations{action="upsize"} 1
apva_recommendations{action="hold"} 1
apva_potential_savings{resource="cpu_cores"} 7.915
apva_potential_savings{resource="memory_bytes"} 2.1633171456e+10
apva_potential_savings{resource="gpu"} 1
```

Set `serviceMonitor.enabled=true` in the Helm chart to have the Prometheus Operator scrape
them.
