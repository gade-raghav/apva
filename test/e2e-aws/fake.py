#!/usr/bin/env python3
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
"""Fake Prometheus + fake AWS (EKS managed node group + Auto Scaling) for the AWS e2e test.

It talks to a REAL Kubernetes API (via `kubectl proxy`): UpdateNodegroupConfig creates
KWOK nodes labelled eks.amazonaws.com/nodegroup=gpu, TerminateInstanceInAutoScalingGroup
deletes exactly that node. Prometheus answers are computed from the live pods and the
usage the test sets through /control/usage.
"""
import json, re, sys, threading, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

KUBE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8001"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9300
GROUP, NS = "gpu", "shop"
ng = {"nodegroupName": GROUP, "status": "ACTIVE", "instanceTypes": ["g5.xlarge"],
      "scalingConfig": {"minSize": 2, "maxSize": 4, "desiredSize": 2}}
usage = {"web": {"cpu": 3.4, "mem": 220 * 2**20}}
calls, lock, seq = [], threading.Lock(), [0]

def kube(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(KUBE + path, data=data, method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read() or b"{}")

def group_nodes():
    items = kube("GET", "/api/v1/nodes?labelSelector=eks.amazonaws.com%2Fnodegroup%3D" + GROUP)["items"]
    return sorted(n["metadata"]["name"] for n in items)

def reconcile():
    """Create KWOK nodes until the group has desiredSize nodes (scale-in happens by Terminate)."""
    while len(group_nodes()) < ng["scalingConfig"]["desiredSize"]:
        seq[0] += 1
        name = f"{GROUP}-{seq[0]}"
        kube("POST", "/api/v1/nodes", {
            "apiVersion": "v1", "kind": "Node",
            "metadata": {"name": name, "annotations": {"kwok.x-k8s.io/node": "fake"},
                         "labels": {"eks.amazonaws.com/nodegroup": GROUP, "type": "kwok"}},
            "spec": {"providerID": f"aws:///us-east-1a/i-{name}"},
            "status": {"allocatable": {"cpu": "4", "memory": "16Gi", "pods": "110"},
                       "capacity": {"cpu": "4", "memory": "16Gi", "pods": "110"}}})
        calls.append(f"node created {name}")

def qty(v):
    m = re.fullmatch(r"([0-9.]+)(m|Ki|Mi|Gi)?", v)
    n, unit = float(m.group(1)), m.group(2)
    return n * {None: 1, "m": 1e-3, "Ki": 2**10, "Mi": 2**20, "Gi": 2**30}[unit]

def pods():
    return kube("GET", f"/api/v1/namespaces/{NS}/pods")["items"]

def workload(pod):  # web-<hash>-<suffix> -> web
    return pod["metadata"]["name"].rsplit("-", 2)[0]

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, obj, ctype="application/json"):
        b = obj if isinstance(obj, bytes) else json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", ctype); self.end_headers(); self.wfile.write(b)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/api/v1/query":
            q, res = parse_qs(u.query)["query"][0], []
            for p in pods():
                wl, running = workload(p), p["status"].get("phase") == "Running"
                reqs = [c.get("resources", {}).get("requests", {}) for c in p["spec"]["containers"]]
                v = None
                if "container_cpu_usage" in q and running: v = usage.get(wl, {}).get("cpu", 0)
                elif "container_memory_working_set" in q and running: v = usage.get(wl, {}).get("mem", 0)
                elif 'resource="cpu"' in q: v = sum(qty(r.get("cpu", "0")) for r in reqs)
                elif 'resource="memory"' in q: v = sum(qty(r.get("memory", "0")) for r in reqs)
                elif "kube_pod_status_phase" in q: v = 1 if running else 0
                if v is not None:
                    res.append({"metric": {"namespace": NS, "pod": p["metadata"]["name"]}, "value": [0, str(v)]})
            return self.send(200, {"status": "success", "data": {"resultType": "vector", "result": res}})
        if u.path == f"/clusters/e2e/node-groups/{GROUP}":
            with lock:
                return self.send(200, {"nodegroup": ng})
        if u.path == "/state":
            return self.send(200, {"nodegroup": ng, "nodes": group_nodes(), "calls": calls})
        self.send(404, {"message": "not found"})

    def do_POST(self):
        u = urlparse(self.path)
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
        if not self.headers.get("Authorization", "").startswith("AWS4-HMAC-SHA256") and not u.path.startswith("/control"):
            return self.send(403, {"message": "unsigned request"})
        with lock:
            if u.path == f"/clusters/e2e/node-groups/{GROUP}/update-config":
                sc = json.loads(body)["scalingConfig"]
                if not ng["scalingConfig"]["minSize"] <= sc["desiredSize"] <= ng["scalingConfig"]["maxSize"]:
                    return self.send(400, {"message": "desiredSize out of range"})
                calls.append(f"UpdateNodegroupConfig desired={sc['desiredSize']}")
                ng["scalingConfig"]["desiredSize"] = sc["desiredSize"]
                reconcile()
                return self.send(200, {"update": {"status": "InProgress"}})
            if u.path == "/" and "TerminateInstanceInAutoScalingGroup" in body:
                f = parse_qs(body)
                inst, dec = f["InstanceId"][0], f["ShouldDecrementDesiredCapacity"][0] == "true"
                name = inst[len("i-"):]
                if name not in group_nodes():
                    return self.send(400, b"<ErrorResponse><Error><Code>ValidationError</Code><Message>no such instance</Message></Error></ErrorResponse>", "text/xml")
                if dec and ng["scalingConfig"]["desiredSize"] - 1 < ng["scalingConfig"]["minSize"]:
                    return self.send(400, b"<ErrorResponse><Error><Code>ValidationError</Code><Message>below min</Message></Error></ErrorResponse>", "text/xml")
                kube("DELETE", f"/api/v1/nodes/{name}")
                if dec:
                    ng["scalingConfig"]["desiredSize"] -= 1
                calls.append(f"TerminateInstance {inst} decrement={dec}")
                return self.send(200, b"<TerminateInstanceInAutoScalingGroupResponse/>", "text/xml")
            if u.path == "/control/usage":
                q = parse_qs(u.query)
                usage.setdefault(q["workload"][0], {})["cpu"] = float(q["cpu"][0])
                return self.send(200, usage)
            if u.path == "/control/min":
                ng["scalingConfig"]["minSize"] = int(parse_qs(u.query)["value"][0])
                return self.send(200, ng)
        self.send(404, {"message": "not found"})

if __name__ == "__main__":
    reconcile()
    print(f"fake prometheus + AWS on :{PORT}, kube at {KUBE}, nodes {group_nodes()}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
