#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
#
# AWS end-to-end test without an AWS account.
#
# A real Kubernetes API (k3s control plane, or any cluster in $KUBECONFIG) with KWOK
# simulating the nodes, plus test/e2e-aws/fake.py playing Prometheus and AWS (an EKS
# managed node group "gpu" whose nodes are KWOK nodes). APVA runs with --aws-cluster and
# must, on its own:
#
#   phase 1  web needs ~3.9 cores per pod; nothing fits the rollout's surge pod
#            → scale the node group 2 → 3 FIRST, wait for the node, then resize the pods
#            → afterwards remove the node left empty (3 → 2), exactly that instance
#   phase 2  usage drops → shrink the pods FIRST (adding a node for the rollout's surge
#            pod if the full nodes have no room), then drain under-used nodes through the
#            Eviction API and remove them, down to the node group's minimum (1)
#
# Needs: go, python3, kubectl, curl, jq, and KWOK managing nodes annotated
# kwok.x-k8s.io/node=fake (the workflow starts it; see .github/workflows/e2e-aws.yml).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
FAKE=http://127.0.0.1:9300
KUBE=http://127.0.0.1:8001
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
  echo "---- deployment requests"; kubectl -n shop get deploy web -o jsonpath='{.spec.template.spec.containers[0].resources.requests}{"\n"}' || true
  echo "---- apva activity"; curl -fsS localhost:18080/api/v1/result | jq '{autoResize: .autoResize.events, nodeGroups: .nodeGroups.events}' || true
  echo "---- apva log (tail)"; tail -40 "$work/apva.log" || true
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
nodes_is() { [[ $(kubectl get nodes -l eks.amazonaws.com/nodegroup=gpu --no-headers 2>/dev/null | grep -c ' Ready') == "$1" ]]; }
cpu_request_is() { [[ $(kubectl -n shop get deploy web -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}') == "$1" ]]; }
rolled_out() { kubectl -n shop rollout status deploy/web --timeout=5s; }
called() { curl -fsS $FAKE/state | jq -e --arg c "$1" '.calls | any(startswith($c))'; }

reset_cluster # leftovers from an earlier run

echo "==> kubectl proxy, fake Prometheus + AWS"
kubectl proxy --port=8001 >"$work/proxy.log" 2>&1 & pids+=($!)
wait_for "kubectl proxy" 30 curl -fsS $KUBE/readyz
python3 "$here/fake.py" $KUBE 9300 >"$work/fake.log" 2>&1 & pids+=($!)
wait_for "fake AWS" 30 curl -fsS $FAKE/state
wait_for "2 KWOK nodes in node group gpu" 60 nodes_is 2

echo "==> workload: web, 2 × 1 core"
kubectl apply -f "$here/workload.yaml" >/dev/null
wait_for "web rolled out" 90 rolled_out

echo "==> APVA with --auto-resize --aws-cluster"
(cd "$root" && go build -o "$work/apva" ./cmd/apva)
AWS_ACCESS_KEY_ID=AKIAFAKE AWS_SECRET_ACCESS_KEY=fake AWS_REGION=us-east-1 \
APVA_AWS_EKS_ENDPOINT=$FAKE APVA_AWS_AUTOSCALING_ENDPOINT=$FAKE/ \
  "$work/apva" --listen=:18080 --prometheus-url=$FAKE --kube-api=$KUBE --namespaces=shop \
  --window=15m --refresh=5s \
  --auto-resize --auto-resize-min-confidence=medium --auto-resize-cooldown=20s \
  --aws-cluster=e2e --aws-nodegroups=gpu --aws-nodegroup-cooldown=15s \
  --aws-scale-up-timeout=2m --aws-drain-timeout=2m >"$work/apva.log" 2>&1 & pids+=($!)
wait_for "APVA ready" 60 curl -fsS localhost:18080/readyz

echo "==> phase 1: pods need more than the nodes have → nodes first, then pods"
wait_for "node group scaled up (UpdateNodegroupConfig desired=3)" 120 called "UpdateNodegroupConfig desired=3"
wait_for "3 Ready nodes" 60 nodes_is 3
wait_for "web resized to 3910m per pod" 120 cpu_request_is 3910m
wait_for "web rolled out on the new capacity" 120 rolled_out
wait_for "empty node removed (TerminateInstance)" 120 called "TerminateInstance"
wait_for "back to 2 nodes" 60 nodes_is 2
wait_for "web still fully available" 60 rolled_out

echo "==> phase 2: usage drops → pods first, then drain and remove a node"
curl -fsS -X POST "$FAKE/control/min?value=1" >/dev/null
curl -fsS -X POST "$FAKE/control/usage?workload=web&cpu=0.3" >/dev/null
wait_for "web shrunk (50% step: 1955m)" 120 cpu_request_is 1955m
wait_for "a node drained and removed (1 node left)" 240 nodes_is 1
wait_for "web fully available on the remaining node" 120 rolled_out

created=$(curl -fsS $FAKE/state | jq '[.calls[] | select(startswith("node created"))] | length')
terms=$(curl -fsS $FAKE/state | jq '[.calls[] | select(startswith("TerminateInstance"))] | length')
[[ $terms == $((created - 1)) ]] || { echo "FAIL: $created nodes created, $terms terminated; expected exactly one left"; dump; exit 1; }
curl -fsS $FAKE/state | jq -e '.nodegroup.scalingConfig.desiredSize == 1' >/dev/null \
  || { echo "FAIL: node group desired size should be 1"; dump; exit 1; }

echo "---- what APVA did"
curl -fsS $FAKE/state | jq -r '.calls[]'
curl -fsS localhost:18080/api/v1/result | jq -r '(.autoResize.events + .nodeGroups.events) | sort_by(.time) | .[] | "\(.time)  \(.outcome)  \(.workload.name // ("node group " + .group))  \(.reason)"'
echo "PASS: APVA AWS end-to-end test"
