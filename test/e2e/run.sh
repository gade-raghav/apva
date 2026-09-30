#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end test: kind cluster + Prometheus + test workloads + APVA, then assert that
# APVA recommends downsizing the idle workload and upsizing the busy one.
# Requires: docker, kind, kubectl, helm, curl, jq.
set -euo pipefail

CLUSTER=${CLUSTER:-apva-e2e}
IMG=apva:e2e
KEEP=${KEEP:-false}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

cleanup() { [[ "$KEEP" == "true" ]] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --wait 120s

echo "==> building and loading APVA image"
docker build -t "$IMG" "$root"
kind load docker-image "$IMG" --name "$CLUSTER"

echo "==> installing Prometheus (with kube-state-metrics, cAdvisor scraping)"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
helm repo update >/dev/null
helm install prometheus prometheus-community/prometheus -n monitoring --create-namespace \
  --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false \
  --set prometheus-node-exporter.enabled=false \
  --set server.global.scrape_interval=15s --set server.persistentVolume.enabled=false \
  --wait --timeout 5m

echo "==> deploying test workloads"
kubectl apply -f "$here/workload.yaml"
kubectl -n e2e rollout status deploy/idle-web --timeout=180s
kubectl -n e2e rollout status deploy/busy-worker --timeout=180s

echo "==> installing APVA (short windows for the test)"
helm install apva "$root/charts/apva" -n apva --create-namespace \
  --set image.repository=apva --set image.tag=e2e --set image.pullPolicy=Never \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80 \
  --set analysis.window=10m --set analysis.recentWindow=5m --set analysis.refresh=30s \
  --set analysis.namespaces=e2e --wait --timeout 3m

kubectl -n apva port-forward svc/apva 18080:8080 >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true; cleanup' EXIT
sleep 3

echo "==> waiting for APVA to see enough data (up to 10 minutes)"
deadline=$((SECONDS + 600))
while true; do
  body=$(curl -fsS localhost:18080/api/v1/recommendations 2>/dev/null || echo '{}')
  idle=$(jq -r '.items[]? | select(.workload.name=="idle-web") | .cpu.action' <<<"$body")
  busy=$(jq -r '.items[]? | select(.workload.name=="busy-worker") | .cpu.action' <<<"$body")
  echo "    idle-web cpu=${idle:-?}  busy-worker cpu=${busy:-?}"
  if [[ "$idle" == "downsize" && "$busy" == "upsize" ]]; then
    break
  fi
  if (( SECONDS > deadline )); then
    echo "FAIL: expected idle-web=downsize and busy-worker=upsize"
    jq . <<<"$body" || true
    kubectl -n apva logs deploy/apva --tail=50 || true
    exit 1
  fi
  sleep 20
done

jq '.items[] | select(.workload.name=="idle-web") | .replicas' <<<"$body" | grep -qx 2 \
  || { echo "FAIL: idle-web should have 2 replicas"; exit 1; }
curl -fsS localhost:18080/metrics | grep -q '^apva_workloads ' || { echo "FAIL: /metrics"; exit 1; }
curl -fsS localhost:18080/ | grep -q '<title>APVA</title>' || { echo "FAIL: UI"; exit 1; }

echo "PASS: APVA end-to-end test"
