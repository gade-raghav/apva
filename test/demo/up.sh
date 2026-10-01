#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
#
# Live demo: a kind cluster with Prometheus, a "shop" of workloads with realistic sizing
# mistakes (test/demo/workloads.yaml), and APVA with automatic resizing turned on.
#
#   ./test/demo/up.sh          # then open http://localhost:8080
#   CILIUM=1 ./test/demo/up.sh # also install Cilium + Hubble for the service graph (slower)
#   DRY_RUN=1 ./test/demo/up.sh  # show what APVA would resize without changing anything
#   ./test/demo/down.sh        # delete the cluster
#
# Requires: docker, kind, kubectl, helm.
set -euo pipefail

CLUSTER=${CLUSTER:-apva-demo}
CILIUM=${CILIUM:-0}
DRY_RUN=${DRY_RUN:-0}
PORT=${PORT:-8080}
IMG=apva:demo
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

for bin in docker kind kubectl helm; do
  command -v "$bin" >/dev/null || { echo "missing $bin; install it first (brew install $bin)"; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "Docker is not running; start Docker Desktop first"; exit 1; }

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> reusing kind cluster $CLUSTER"
  kubectl config use-context "kind-$CLUSTER" >/dev/null
else
  echo "==> creating kind cluster $CLUSTER"
  if [[ "$CILIUM" == "1" ]]; then
    printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnetworking:\n  disableDefaultCNI: true\n' |
      kind create cluster --name "$CLUSTER" --config - --wait 0s
  else
    kind create cluster --name "$CLUSTER" --wait 120s
  fi
fi

if [[ "$CILIUM" == "1" ]]; then
  echo "==> installing Cilium with Hubble flow metrics (for the service graph)"
  helm repo add cilium https://helm.cilium.io >/dev/null
  helm repo update cilium >/dev/null
  helm upgrade --install cilium cilium/cilium -n kube-system --wait --timeout 10m -f - <<'EOF'
ipam: {mode: kubernetes}
hubble:
  enabled: true
  relay: {enabled: false}
  metrics:
    enableOpenMetrics: false
    enabled:
      - "flow:labelsContext=source_namespace,source_workload,destination_namespace,destination_workload"
    serviceAnnotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9965"
EOF
  kubectl wait --for=condition=Ready nodes --all --timeout=5m
fi

echo "==> building APVA image and loading it into kind"
docker build -q -t "$IMG" "$root" >/dev/null
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null

echo "==> installing Prometheus (kube-state-metrics + cAdvisor)"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
helm repo update prometheus-community >/dev/null
helm upgrade --install prometheus prometheus-community/prometheus -n monitoring --create-namespace \
  --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false \
  --set prometheus-node-exporter.enabled=false \
  --set server.global.scrape_interval=15s --set server.persistentVolume.enabled=false \
  --wait --timeout 5m >/dev/null

echo "==> deploying the demo shop"
kubectl apply -f "$here/workloads.yaml" >/dev/null
kubectl -n shop rollout status deploy --timeout=5m >/dev/null
kubectl -n shop rollout status sts/redis --timeout=5m >/dev/null
if kubectl -n shop get pods --no-headers | grep -q Pending; then
  echo "WARNING: some pods are Pending; give Docker Desktop more CPU/memory (Settings → Resources)"
fi

echo "==> installing APVA with auto-resize"
helm upgrade --install apva "$root/charts/apva" -n apva --create-namespace \
  --set image.repository=apva --set image.tag=demo --set image.pullPolicy=Never \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80 \
  --set analysis.window=15m --set analysis.recentWindow=5m --set analysis.refresh=30s \
  --set analysis.namespaces=shop \
  --set autoResize.enabled=true --set autoResize.minConfidence=medium \
  --set autoResize.cooldown=5m --set autoResize.dryRun="$([[ "$DRY_RUN" == "1" ]] && echo true || echo false)" \
  --wait --timeout 3m >/dev/null
kubectl -n apva rollout restart deploy/apva >/dev/null # pick up a rebuilt image on re-runs
kubectl -n apva rollout status deploy/apva --timeout=2m >/dev/null

cat <<EOF

APVA is running with auto-resize ON.
  Dashboard:  http://localhost:$PORT   (refreshes every 10s)
  Watch it:   kubectl -n shop get deploy,sts -o custom-columns=NAME:.metadata.name,CPU:.spec.template.spec.containers[0].resources.requests.cpu,MEM:.spec.template.spec.containers[0].resources.requests.memory -w

What to expect: the first recommendations appear after ~1-2 minutes; APVA starts
resizing once it has a few minutes of data (~5 min), then again after each 5-minute
cooldown as over-provisioned workloads step down by at most 50% at a time.

Press Ctrl-C to stop the port-forward (the cluster keeps running; ./test/demo/down.sh deletes it).
EOF
kubectl -n apva port-forward svc/apva "$PORT":8080
