#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end test of APVA as a VerticalPodAutoscaler custom recommender
# (https://github.com/kubernetes/autoscaler/issues/10395).
#
# A real Kubernetes API (k3s 1.35 control plane, or any cluster in $KUBECONFIG) with KWOK
# simulating nodes and pods, the real VPA CRD, and test/e2e-aws/fake.py playing Prometheus.
# APVA runs with --vpa-recommender --auto-resize, with the chart's RBAC, as its own
# ServiceAccount (impersonated), and must:
#
#   web     (VPA naming recommender "apva")  → write status.recommendation, split across the
#            pod's containers, honouring containerPolicies; never patch web itself
#   api     (VPA using the default recommender) → leave the VPA's status and api alone
#   worker  (no VPA)                         → resize it as usual (APVA's own auto-resize)
#   security APVA's identity can write VPA status, but not VPA specs, other namespaces,
#            or anything else
#
# Needs: go, python3, kubectl, curl, jq, KWOK managing nodes annotated
# kwok.x-k8s.io/node=fake with test/e2e-aws/kwok-stages.sh stages, and the VPA CRD
# ($VPA_CRD, a path or URL; default: the vertical-pod-autoscaler-$VPA_VERSION release).
# RBAC: rendered with `helm template` if helm is installed, else from $APVA_RBAC_MANIFEST.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
VPA_VERSION=${VPA_VERSION:-1.8.0}
VPA_CRD=${VPA_CRD:-https://raw.githubusercontent.com/kubernetes/autoscaler/vertical-pod-autoscaler-$VPA_VERSION/vertical-pod-autoscaler/deploy/vpa-v1-crd-gen.yaml}
FAKE=http://127.0.0.1:9300
ADMIN=http://127.0.0.1:8002
KUBE=http://127.0.0.1:8001 # for APVA: impersonates APVA's ServiceAccount
SA=system:serviceaccount:apva:apva
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  reset_cluster
}
reset_cluster() {
  kubectl -n shop delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
  kubectl delete ns shop --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  kubectl delete nodes -l eks.amazonaws.com/nodegroup=gpu --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

result() { curl -fsS localhost:18080/api/v1/result; }
dump() {
  echo "---- VPAs"; kubectl -n shop get vpa -o json | jq '.items[] | {name: .metadata.name, status}' || true
  echo "---- pods"; kubectl -n shop get pods -o wide || true
  echo "---- apva activity"; result | jq -r '(.autoResize.events + .vpa.events) | sort_by(.time) | .[] | "\(.time) \(.outcome) \(.vpa // .workload.name) \(.reason)"' || true
  echo "---- apva log (tail)"; tail -30 "$work/apva.log" || true
}
wait_for() {
  local what=$1 secs=$2; shift 2
  local deadline=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS > deadline)); then echo "FAIL: timed out waiting for: $what"; dump; exit 1; fi
    sleep 3
  done
  echo "    ok: $what"
}
fail() { echo "FAIL: $*"; dump; exit 1; }
rolled_out() { kubectl -n shop rollout status "deploy/$1" --timeout=5s; }
# CPU quantity -> millicores
milli() { awk -v q="$1" 'BEGIN { if (q ~ /m$/) { sub(/m$/, "", q); print int(q) } else print int(q * 1000 + 0.5) }'; }
vpa_status() { kubectl -n shop get vpa "$1" -o json | jq -c '.status // {}'; }
rec_cpu() { # rec_cpu <vpa> <container> <target|lowerBound|upperBound|uncappedTarget>
  kubectl -n shop get vpa "$1" -o json | jq -r --arg c "$2" --arg f "$3" \
    '.status.recommendation.containerRecommendations[] | select(.containerName == $c) | .[$f].cpu'
}
has_rec() { kubectl -n shop get vpa "$1" -o json | jq -e '.status.recommendation.containerRecommendations | length == 2'; }
pods_cpu() { kubectl -n shop get pods -l "app=$1" -o jsonpath='{range .items[*]}{.spec.containers[0].resources.requests.cpu}{"\n"}{end}' | sort -u | tr '\n' ' '; }
template_cpu() { kubectl -n shop get deploy "$1" -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}'; }
pod_names() { kubectl -n shop get pods -l "app=$1" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort | tr '\n' ' '; }
worker_resized() { [[ $(pods_cpu worker) != "1 " ]]; }
event() { result | jq -e --arg w "$1" --arg o "$2" --arg r "$3" '[.autoResize.events[], .vpa.events[]] | any((.workload.name == $w or .vpa == $w) and .outcome == $o and (.reason | contains($r)))'; }

reset_cluster

echo "==> VerticalPodAutoscaler CRD (vertical-pod-autoscaler-$VPA_VERSION)"
if [[ $VPA_CRD == http* ]]; then curl -fsSL -o "$work/vpa-crd.yaml" "$VPA_CRD"; else cp "$VPA_CRD" "$work/vpa-crd.yaml"; fi
kubectl apply --server-side -f "$work/vpa-crd.yaml" >/dev/null
kubectl wait --for condition=established --timeout=60s crd/verticalpodautoscalers.autoscaling.k8s.io >/dev/null

echo "==> APVA's ServiceAccount, RBAC and admission policies (from the Helm chart)"
kubectl create namespace apva --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create namespace shop --dry-run=client -o yaml | kubectl apply -f - >/dev/null
if command -v helm >/dev/null; then
  helm template apva "$root/charts/apva" -n apva --kube-version "$(kubectl version -o json | jq -r .serverVersion.gitVersion | cut -d+ -f1)" \
    --set vpaRecommender.enabled=true --set autoResize.enabled=true --set analysis.namespaces=shop \
    --show-only templates/serviceaccount.yaml --show-only templates/rbac.yaml \
    --show-only templates/admission-policy.yaml >"$work/rbac.yaml"
else
  [[ -n ${APVA_RBAC_MANIFEST:-} ]] || { echo "need helm, or APVA_RBAC_MANIFEST"; exit 1; }
  cp "$APVA_RBAC_MANIFEST" "$work/rbac.yaml"
fi
kubectl apply -f "$work/rbac.yaml" >/dev/null

echo "==> kubectl proxies, fake Prometheus"
kubectl proxy --port=8002 >"$work/proxy-admin.log" 2>&1 & pids+=($!)
kubectl proxy --port=8001 --as=$SA >"$work/proxy-apva.log" 2>&1 & pids+=($!)
wait_for "kubectl proxies" 30 curl -fsS $KUBE/version
python3 "$root/test/e2e-aws/fake.py" $ADMIN 9300 >"$work/fake.log" 2>&1 & pids+=($!)
wait_for "fake Prometheus" 30 curl -fsS $FAKE/state
for w in web api worker; do curl -fsS -X POST "$FAKE/control/usage?workload=$w&cpu=0.1&mem=200" >/dev/null; done

echo "==> workloads: web (VPA → apva), api (VPA → default recommender), worker (no VPA)"
kubectl apply -f "$here/workload.yaml" >/dev/null
for w in web api worker; do wait_for "$w rolled out" 90 rolled_out $w; done
web_pods=$(pod_names web) api_pods=$(pod_names api)

echo "==> APVA (--vpa-recommender --auto-resize), as its ServiceAccount"
(cd "$root" && go build -o "$work/apva" ./cmd/apva)
"$work/apva" --listen=:18080 --prometheus-url=$FAKE --kube-api=$KUBE --namespaces=shop \
  --window=15m --refresh=5s --vpa-recommender \
  --auto-resize --auto-resize-min-confidence=medium --auto-resize-cooldown=20s >"$work/apva.log" 2>&1 & pids+=($!)
wait_for "APVA ready" 60 curl -fsS localhost:18080/readyz

echo "==> web: APVA writes the VPA's recommendation"
wait_for "web-vpa has a recommendation for both containers" 90 has_rec web-vpa
app=$(milli "$(rec_cpu web-vpa app target)") proxy=$(milli "$(rec_cpu web-vpa proxy target)")
proxy_uncapped=$(milli "$(rec_cpu web-vpa proxy uncappedTarget)")
echo "    web-vpa targets: app ${app}m, proxy ${proxy}m (uncapped ${proxy_uncapped}m)"
((app < 300)) || fail "app target ${app}m should be below its 300m request (usage is 100m/pod)"
((proxy >= 50 && $(milli "$(rec_cpu web-vpa proxy lowerBound)") >= 50)) || fail "proxy target and lowerBound must respect minAllowed 50m"
((proxy_uncapped <= proxy)) || fail "uncappedTarget ${proxy_uncapped}m should ignore minAllowed"
((app >= 2 * proxy_uncapped && app <= 4 * proxy_uncapped)) || fail "app:proxy should follow the 3:1 request split (app ${app}m, proxy ${proxy_uncapped}m)"
((app >= $(milli "$(rec_cpu web-vpa app lowerBound)") && app <= $(milli "$(rec_cpu web-vpa app upperBound)"))) || fail "target outside its bounds"
vpa_status web-vpa | jq -e '.conditions | any(.type == "RecommendationProvided" and .status == "True")' >/dev/null || fail "RecommendationProvided condition missing"
wait_for "APVA reports web-vpa written" 30 event shop/web-vpa written ""
echo "    ok: proportional split, minAllowed honoured, bounds and conditions set"

echo "==> worker (no VPA): APVA resizes it itself"
wait_for "worker resized below 1 core" 120 worker_resized

echo "==> APVA never acts on VPA-targeted workloads"
wait_for "api skipped: the VPA owns its requests" 30 event api skipped "VerticalPodAutoscaler shop/api-vpa"
wait_for "web skipped: the VPA owns its requests" 30 event web skipped "VerticalPodAutoscaler shop/web-vpa"
[[ $(pods_cpu web) == "300m " && $(template_cpu web) == 300m && $(pod_names web) == "$web_pods" ]] || fail "web was resized by APVA"
[[ $(pods_cpu api) == "1 " && $(template_cpu api) == 1 && $(pod_names api) == "$api_pods" ]] || fail "api was resized by APVA"
[[ $(vpa_status api-vpa) == "{}" ]] || fail "APVA wrote to api-vpa, which uses the default recommender: $(vpa_status api-vpa)"
echo "    ok: web and api untouched, api-vpa status untouched"

echo "==> security: what APVA's identity can't do"
can() { kubectl auth can-i "$@" --as=$SA 2>/dev/null; }
[[ $(can patch verticalpodautoscalers --subresource=status -n shop) == yes ]] || fail "APVA should be able to write VPA status in shop"
for t in "patch verticalpodautoscalers -n shop" "update verticalpodautoscalers -n shop" "create verticalpodautoscalers -n shop" \
         "delete verticalpodautoscalers -n shop" "patch verticalpodautoscalers --subresource=status -n default" \
         "get secrets -n shop" "create pods -n shop" "delete deployments -n shop" "patch deployments -n default"; do
  [[ $(can $t) == no ]] || fail "APVA can $t"
done
! kubectl -n shop --as=$SA patch vpa api-vpa --type merge -p '{"spec":{"recommenders":[{"name":"apva"}]}}' >/dev/null 2>&1 \
  || fail "APVA could change a VPA's spec"
echo "    ok: VPA status only, in shop only; no VPA spec changes"

echo "---- what APVA did"
result | jq -r '(.autoResize.events + .vpa.events) | sort_by(.time) | .[] | "\(.time)  \(.outcome)  \(.vpa // .workload.name)  \(.reason)"'
kubectl -n shop get vpa web-vpa -o json | jq '.status.recommendation'
echo "PASS: APVA VPA recommender end-to-end test"
