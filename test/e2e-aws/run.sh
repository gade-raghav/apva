#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
#
# AWS end-to-end test without an AWS account.
#
# A real Kubernetes API (k3s 1.35 control plane, or any cluster in $KUBECONFIG) with KWOK
# simulating the nodes, plus test/e2e-aws/fake.py playing Prometheus and AWS (an EKS
# managed node group "gpu" whose nodes are KWOK nodes). APVA runs with the chart's RBAC
# and ValidatingAdmissionPolicies, as its own ServiceAccount (impersonated), and must:
#
#   phase 1  pods need a bit more and there is room on their nodes
#            → resize IN PLACE: same pods, no restart, pod template untouched
#   phase 2  pods need more than their nodes can give them in place
#            → rolling update instead, and the node group grows 2 → 4 FIRST
#   phase 3  usage drops → shrink IN PLACE, then drain under-used nodes (Eviction API) and
#            remove exactly those instances; pods recreated from the (larger) template are
#            brought to the in-place size so they fit → down to 1 node
#   security APVA's identity can't read secrets, change images/env/replicas, label
#            nodes, or touch other namespaces
#
# Needs: go, python3 (+ pyyaml), kubectl, curl, jq, and KWOK managing nodes annotated
# kwok.x-k8s.io/node=fake with test/e2e-aws/kwok-stages.sh stages (the workflow starts it).
# RBAC: rendered with `helm template` if helm is installed, else from $APVA_RBAC_MANIFEST.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
FAKE=http://127.0.0.1:9300
ADMIN=http://127.0.0.1:8002 # for the fake AWS (creates/deletes nodes)
KUBE=http://127.0.0.1:8001  # for APVA: impersonates APVA's ServiceAccount
SA=system:serviceaccount:apva:apva
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  reset_cluster
}
# KWOK pods have no kubelet to confirm deletion: force-delete them before the namespace.
reset_cluster() {
  kubectl -n shop delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
  kubectl delete ns shop --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  kubectl delete nodes -l eks.amazonaws.com/nodegroup=gpu --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

dump() {
  echo "---- fake AWS state"; curl -fsS $FAKE/state | jq . || true
  echo "---- nodes"; kubectl get nodes -o wide || true
  echo "---- pods"; kubectl -n shop get pods -o wide || true
  echo "---- pod requests"; kubectl -n shop get pods -o jsonpath='{range .items[*]}{.metadata.name} {.spec.containers[0].resources.requests.cpu}{"\n"}{end}' || true
  echo "---- template requests"; kubectl -n shop get deploy web -o jsonpath='{.spec.template.spec.containers[0].resources.requests}{"\n"}' || true
  echo "---- apva activity"; curl -fsS localhost:18080/api/v1/result | jq -r '(.autoResize.events + .nodeGroups.events) | sort_by(.time) | .[] | "\(.time) \(.outcome) \(.workload.name // .group) \(.reason)"' || true
  echo "---- apva log (tail)"; tail -30 "$work/apva.log" || true
}

# wait_for "<description>" <seconds> <command...>
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
# exactly $1 nodes in the group, all Ready and schedulable
nodes_is() {
  local out; out=$(kubectl get nodes -l eks.amazonaws.com/nodegroup=gpu --no-headers 2>/dev/null)
  [[ $(grep -c . <<<"$out") == "$1" && $(awk '$2 == "Ready"' <<<"$out" | grep -c .) == "$1" ]]
}
template_cpu_is() { [[ $(kubectl -n shop get deploy web -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}') == "$1" ]]; }
pods_cpu() { kubectl -n shop get pods -o jsonpath='{range .items[*]}{.spec.containers[0].resources.requests.cpu}{"\n"}{end}' | sort -u | tr '\n' ' '; }
pods_cpu_is() { [[ $(pods_cpu) == "$1 " ]]; }
pod_names() { kubectl -n shop get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort | tr '\n' ' '; }
rolled_out() { kubectl -n shop rollout status deploy/web --timeout=5s; }
all_running() { [[ $(kubectl -n shop get pods --no-headers | grep -c ' Running') == 4 && $(kubectl -n shop get pods --no-headers | wc -l) == 4 ]]; }
called() { curl -fsS $FAKE/state | jq -e --arg c "$1" '.calls | any(startswith($c))'; }
calls() { curl -fsS $FAKE/state | jq --arg c "$1" '[.calls[] | select(startswith($c))] | length'; }

reset_cluster # leftovers from an earlier run

echo "==> APVA's ServiceAccount, RBAC and admission policies (from the Helm chart)"
kubectl create namespace apva --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create namespace shop --dry-run=client -o yaml | kubectl apply -f - >/dev/null
if command -v helm >/dev/null; then
  helm template apva "$root/charts/apva" -n apva --kube-version "$(kubectl version -o json | jq -r .serverVersion.gitVersion | cut -d+ -f1)" \
    --set autoResize.enabled=true --set analysis.namespaces=shop \
    --set aws.enabled=true --set aws.cluster=e2e --set aws.region=us-east-1 --set aws.nodegroups=gpu \
    --show-only templates/serviceaccount.yaml --show-only templates/rbac.yaml \
    --show-only templates/admission-policy.yaml >"$work/rbac.yaml"
else
  [[ -n ${APVA_RBAC_MANIFEST:-} ]] || { echo "need helm, or APVA_RBAC_MANIFEST"; exit 1; }
  cp "$APVA_RBAC_MANIFEST" "$work/rbac.yaml"
fi
kubectl apply -f "$work/rbac.yaml" >/dev/null

echo "==> kubectl proxies, fake Prometheus + AWS"
kubectl proxy --port=8002 >"$work/proxy-admin.log" 2>&1 & pids+=($!)
kubectl proxy --port=8001 --as=$SA >"$work/proxy-apva.log" 2>&1 & pids+=($!)
wait_for "kubectl proxies" 30 curl -fsS $KUBE/version
python3 "$here/fake.py" $ADMIN 9300 >"$work/fake.log" 2>&1 & pids+=($!)
wait_for "fake AWS" 30 curl -fsS $FAKE/state
wait_for "2 KWOK nodes in node group gpu" 60 nodes_is 2

echo "==> workload: web, 4 × 1 core, 2 per node"
kubectl apply -f "$here/workload.yaml" >/dev/null
wait_for "web rolled out" 90 rolled_out
before=$(pod_names)

echo "==> APVA (--auto-resize --resize-mode=auto --aws-cluster), as its ServiceAccount"
(cd "$root" && go build -o "$work/apva" ./cmd/apva)
AWS_ACCESS_KEY_ID=AKIAFAKE AWS_SECRET_ACCESS_KEY=fake AWS_REGION=us-east-1 \
APVA_AWS_EKS_ENDPOINT=$FAKE APVA_AWS_AUTOSCALING_ENDPOINT=$FAKE/ \
  "$work/apva" --listen=:18080 --prometheus-url=$FAKE --kube-api=$KUBE --namespaces=shop \
  --window=15m --refresh=5s \
  --auto-resize --auto-resize-min-confidence=medium --auto-resize-cooldown=20s --resize-mode=auto \
  --aws-cluster=e2e --aws-nodegroups=gpu --aws-nodegroup-cooldown=15s \
  --aws-scale-up-timeout=2m --aws-drain-timeout=2m >"$work/apva.log" 2>&1 & pids+=($!)
wait_for "APVA ready" 60 curl -fsS localhost:18080/readyz

echo "==> phase 1: a bit more CPU, room on the nodes → in place"
wait_for "pods resized in place to 1840m" 120 pods_cpu_is 1840m
[[ $(pod_names) == "$before" ]] || fail "pods were replaced; expected the same pods ($before), got $(pod_names)"
template_cpu_is 1 || fail "the pod template must not change for an in-place resize"
[[ $(calls "UpdateNodegroupConfig") == 0 ]] || fail "no node change expected for an in-place resize"
echo "    ok: same pods, template untouched, no node change"

echo "==> phase 2: more than the nodes can give in place → nodes first, then a rolling update"
curl -fsS -X POST "$FAKE/control/usage?workload=web&cpu=2.6" >/dev/null
wait_for "node group scaled up (UpdateNodegroupConfig desired=4)" 120 called "UpdateNodegroupConfig desired=4"
wait_for "4 Ready nodes" 60 nodes_is 4
wait_for "template rolled to 2990m" 120 template_cpu_is 2990m
wait_for "web rolled out" 120 rolled_out
kubectl -n shop get deploy web -o jsonpath='{.metadata.annotations}' | grep -q in-place-requests && fail "in-place state should be cleared by a rolling update"
echo "    ok: in-place state cleared"

echo "==> phase 3: usage drops → shrink in place, then drain and remove nodes"
curl -fsS -X POST "$FAKE/control/min?value=1" >/dev/null
curl -fsS -X POST "$FAKE/control/usage?workload=web&cpu=0.3" >/dev/null
wait_for "pods shrunk in place (50% step: 1495m)" 120 pods_cpu_is 1495m
template_cpu_is 2990m || fail "the template must stay at 2990m (in-place shrink)"
wait_for "drained down to 1 node" 300 nodes_is 1
wait_for "all 4 pods running on it" 120 all_running

created=$(calls "node created")
terms=$(calls "TerminateInstance")
[[ $terms == $((created - 1)) ]] || fail "$created nodes created, $terms terminated; expected exactly one left"
curl -fsS $FAKE/state | jq -e '.nodegroup.scalingConfig.desiredSize == 1' >/dev/null || fail "node group desired size should be 1"

echo "==> security: what APVA's identity can't do"
can() { kubectl auth can-i "$@" --as=$SA 2>/dev/null; }
for t in "get secrets -n shop" "list configmaps -n shop" "create pods -n shop" "delete deployments -n shop" \
         "patch deployments -n default" "patch pods -n shop" "delete nodes" "create clusterrolebindings"; do
  [[ $(can $t) == no ]] || fail "APVA can $t"
done
echo "    ok: RBAC denies secrets, configmaps, pod creation, deletes, other namespaces, RBAC changes"
deny() { ! kubectl -n shop --as=$SA patch deploy web --type strategic -p "$1" >/dev/null 2>"$work/deny.log" && grep -q ValidatingAdmissionPolicy "$work/deny.log"; }
deny '{"spec":{"template":{"spec":{"containers":[{"name":"app","image":"evil:latest"}]}}}}' || fail "image change was not denied"
deny '{"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":"X","value":"1"}]}]}}}}' || fail "env change was not denied"
deny '{"spec":{"replicas":9}}' || fail "replicas change was not denied"
deny '{"spec":{"template":{"spec":{"serviceAccountName":"admin"}}}}' || fail "serviceAccountName change was not denied"
node=$(kubectl get nodes -l eks.amazonaws.com/nodegroup=gpu -o jsonpath='{.items[0].metadata.name}')
! kubectl --as=$SA label node "$node" evil=1 >/dev/null 2>&1 || fail "node label change was not denied"
echo "    ok: admission policy denies image/env/replicas/serviceAccount changes and node labels"

echo "---- what APVA did"
curl -fsS $FAKE/state | jq -r '.calls[]'
curl -fsS localhost:18080/api/v1/result | jq -r '(.autoResize.events + .nodeGroups.events) | sort_by(.time) | .[] | "\(.time)  \(.outcome)  \(.workload.name // ("node group " + .group))  \(.reason)"'
echo "PASS: APVA AWS end-to-end test"
