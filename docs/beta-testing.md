# APVA beta testing guide

Thanks for helping test APVA! This takes about 10 minutes if your cluster already runs
Prometheus. APVA is **read-only**: it only queries Prometheus and never changes your
workloads.

## What you need

- A Kubernetes cluster with **Prometheus** scraping cAdvisor and **kube-state-metrics**
  (kube-prometheus-stack or the prometheus-community `prometheus` chart both work)
- Optional: **Cilium Hubble** metrics (for the service graph) and the **NVIDIA DCGM
  exporter** (for GPU analysis). APVA works without them and says what it skipped.
- Go 1.22+ on your laptop (option A), or Helm (option B)

## Option A — run on your laptop (fastest, nothing installed in the cluster)

```bash
git clone https://github.com/gade-raghav/apva.git && cd apva

# In another terminal: expose Prometheus locally (adjust namespace/service to yours)
kubectl -n monitoring port-forward svc/prometheus-server 9090:80

go run ./cmd/apva --prometheus-url=http://localhost:9090
open http://localhost:8080
```

Useful flags: `--namespaces=team-a,team-b`, `--window=72h` (default 24h),
`--headroom=0.2` (default 0.15), `--once` (print JSON and exit).

## Option B — run in the cluster (prebuilt image)

```bash
git clone https://github.com/gade-raghav/apva.git && cd apva

helm install apva ./charts/apva -n apva --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80

kubectl -n apva port-forward svc/apva 8080:8080
open http://localhost:8080
```

This pulls the public image `ghcr.io/gade-raghav/apva:v0.1.0` (amd64 and arm64).

## Just want to look first?

```bash
go run ./cmd/apva --demo      # built-in sample cluster, no Prometheus needed
```

## What we'd love to hear

Please open a GitHub issue with the label `beta-feedback` (or reply in Slack) covering:

1. **Accuracy** — do the downsize/upsize suggestions match your intuition? Any that are
   clearly wrong?
2. **Traffic-aware holds** — did APVA hold back a downsize because callers were growing?
   Was that right?
3. **GPU** — if you run GPUs, were idle/underused GPUs flagged correctly?
4. **Setup friction** — anything that didn't work with your Prometheus / labels?
5. **What's missing** — what would make you trust it enough to act on it?

Please don't share cluster names or anything sensitive in public issues.

## Known limitations (v0.1)

- Workload names are derived from pod names; unusual naming may group pods oddly.
- Recommendations are per pod and based on p95 over the window; very spiky workloads may
  need a longer window.
- No automatic apply — by design, for now.
