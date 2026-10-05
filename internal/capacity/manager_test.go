// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
)

// fakeCluster is an in-memory API server for nodes, pods, cordons and evictions.
type fakeCluster struct {
	nodes      []map[string]any
	pods       []map[string]any
	pdbBlocked map[string]bool // pod name -> eviction answers 429
	evicted    []string
}

func node(name, group string, cpu, mem, gpu string, ready bool) map[string]any {
	alloc := map[string]any{"cpu": cpu, "memory": mem, "pods": "110"}
	if gpu != "" {
		alloc["nvidia.com/gpu"] = gpu
	}
	st := "False"
	if ready {
		st = "True"
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]any{testGroupLabel: group}, "annotations": map[string]any{}},
		"spec":     map[string]any{"providerID": "aws:///us-east-1a/i-" + name},
		"status":   map[string]any{"allocatable": alloc, "conditions": []any{map[string]any{"type": "Ready", "status": st}}},
	}
}

func pod(ns, name, nodeName, owner, cpu, mem, gpu string) map[string]any {
	req := map[string]any{"cpu": cpu, "memory": mem}
	if gpu != "" {
		req["nvidia.com/gpu"] = gpu
	}
	md := map[string]any{"name": name, "namespace": ns}
	if owner != "" {
		md["ownerReferences"] = []any{map[string]any{"kind": owner, "controller": true}}
	}
	return map[string]any{
		"metadata": md,
		"spec":     map[string]any{"nodeName": nodeName, "containers": []any{map[string]any{"resources": map[string]any{"requests": req}}}},
		"status":   map[string]any{"phase": "Running"},
	}
}

func roundtrip(in, out any) error {
	b, _ := json.Marshal(in)
	return json.Unmarshal(b, out)
}

func (f *fakeCluster) Get(_ context.Context, path string, out any) error {
	switch {
	case path == "/api/v1/nodes":
		return roundtrip(map[string]any{"items": f.nodes}, out)
	case strings.HasPrefix(path, "/api/v1/pods"):
		return roundtrip(map[string]any{"items": f.pods}, out)
	}
	return kube.ErrNotFound
}

func (f *fakeCluster) StrategicMergePatch(_ context.Context, path string, patch any) error {
	name := path[strings.LastIndex(path, "/")+1:]
	p := patch.(map[string]any)
	for _, n := range f.nodes {
		md := n["metadata"].(map[string]any)
		if md["name"] != name {
			continue
		}
		n["spec"].(map[string]any)["unschedulable"] = p["spec"].(map[string]any)["unschedulable"]
		for k, v := range p["metadata"].(map[string]any)["annotations"].(map[string]any) {
			if v == nil {
				delete(md["annotations"].(map[string]any), k)
			} else {
				md["annotations"].(map[string]any)[k] = v
			}
		}
	}
	return nil
}

func (f *fakeCluster) Create(_ context.Context, path string, _ any) error {
	parts := strings.Split(path, "/") // /api/v1/namespaces/<ns>/pods/<name>/eviction
	name := parts[6]
	if f.pdbBlocked[name] {
		return &kube.StatusError{Code: http.StatusTooManyRequests, Message: "Cannot evict pod as it would violate the pod's disruption budget."}
	}
	f.evicted = append(f.evicted, name)
	var keep []map[string]any
	for _, p := range f.pods {
		if p["metadata"].(map[string]any)["name"] != name {
			keep = append(keep, p)
		}
	}
	f.pods = keep
	return nil
}

const testGroupLabel = "example.com/nodegroup"

// fakeAWS is a fake Provider.
type fakeAWS struct {
	ng         Group
	desired    []int
	terminated []string
}

func (a *fakeAWS) Name() string                                         { return "fake" }
func (a *fakeAWS) GroupLabel() string                                   { return testGroupLabel }
func (a *fakeAWS) DescribeGroup(context.Context, string) (Group, error) { return a.ng, nil }
func (a *fakeAWS) SetDesiredSize(_ context.Context, _ Group, d int) error {
	a.desired = append(a.desired, d)
	a.ng.DesiredSize = d
	return nil
}
func (a *fakeAWS) RemoveNode(_ context.Context, n *Node) error {
	a.terminated = append(a.terminated, "i-"+n.Name)
	a.ng.DesiredSize--
	return nil
}

func newAWS(min, max, desired int) *fakeAWS {
	return &fakeAWS{ng: Group{Name: "gpu", Status: "ACTIVE", MinSize: min, MaxSize: max, DesiredSize: desired}}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newManager(f *fakeCluster, a *fakeAWS, c *clock) *Manager {
	cfg := DefaultConfig()
	cfg.Groups = map[string]bool{"gpu": true}
	m := &Manager{K: f, Cfg: cfg, Now: c.now}
	if a != nil {
		m.Provider = a
	}
	return m
}

var inference = collector.WorkloadKey{Namespace: "ml", Name: "inference"}

// Two 4-core GPU nodes, each running one 3-core inference pod: an upsize to 3.5 cores
// fits; an upsize to 5 cores fits on no node at all.
func twoNodes() *fakeCluster {
	return &fakeCluster{
		nodes: []map[string]any{node("a", "gpu", "4", "16Gi", "1", true), node("b", "gpu", "4", "16Gi", "1", true)},
		pods: []map[string]any{
			pod("ml", "inference-7d9f8b6c4d-x2k9p", "a", "ReplicaSet", "3", "4Gi", "1"),
			pod("ml", "inference-7d9f8b6c4d-q8w7f", "b", "ReplicaSet", "3", "4Gi", "1"),
		},
	}
}

func TestEnsureNeedsRoomForTheSurgePod(t *testing.T) {
	m := newManager(twoNodes(), newAWS(1, 4, 2), &clock{time.Now()})
	// 3.5 cores fits once the old pod on the node goes, but the surge pod needs a
	// free GPU too: both GPUs are taken, so a third node is needed for the rollout.
	if d, why := m.Ensure(context.Background(), inference, 3.5, 4<<30, 2); d != Waiting || !strings.Contains(why, "scaling node group gpu 2 → 3") {
		t.Fatalf("got %v %q", d, why)
	}
}

func TestEnsureFitsWithoutGPUs(t *testing.T) {
	f := &fakeCluster{
		nodes: []map[string]any{node("a", "gpu", "4", "16Gi", "", true), node("b", "gpu", "4", "16Gi", "", true)},
		pods: []map[string]any{
			pod("shop", "web-7d9f8b6c4d-x2k9p", "a", "ReplicaSet", "1", "1Gi", ""),
			pod("shop", "web-7d9f8b6c4d-q8w7f", "b", "ReplicaSet", "1", "1Gi", ""),
		},
	}
	m := newManager(f, newAWS(1, 4, 2), &clock{time.Now()})
	if d, why := m.Ensure(context.Background(), collector.WorkloadKey{Namespace: "shop", Name: "web"}, 2, 1<<30, 2); d != Fits {
		t.Fatalf("got %v %q", d, why)
	}
}

func TestScaleUpThenWaitThenFit(t *testing.T) {
	f, a, c := twoNodes(), newAWS(1, 4, 2), &clock{time.Now()}
	m := newManager(f, a, c)
	ctx := context.Background()
	if d, _ := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Waiting || len(a.desired) != 1 || a.desired[0] != 3 {
		t.Fatalf("first round should scale 2 → 3, desired calls %v", a.desired)
	}
	c.t = c.t.Add(time.Minute)
	if d, why := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Waiting || !strings.Contains(why, "1 of 3") && !strings.Contains(why, "2 of 3") {
		t.Fatalf("second round should wait, got %v %q", d, why)
	}
	if len(a.desired) != 1 {
		t.Fatalf("must not scale twice while waiting: %v", a.desired)
	}
	f.nodes = append(f.nodes, node("c", "gpu", "4", "16Gi", "1", true)) // the new node joins
	if d, why := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Fits {
		t.Fatalf("should fit once the node is Ready, got %v %q", d, why)
	}
	if h := m.History(); len(h) != 1 || h[0].Outcome != "applied" {
		t.Errorf("history = %+v", h)
	}
}

func TestBlockedCases(t *testing.T) {
	ctx := context.Background()
	t.Run("pod larger than any node", func(t *testing.T) {
		m := newManager(twoNodes(), newAWS(1, 4, 2), &clock{time.Now()})
		if d, why := m.Ensure(ctx, inference, 5, 4<<30, 2); d != Blocked || !strings.Contains(why, "larger instance type") {
			t.Fatalf("got %v %q", d, why)
		}
	})
	t.Run("at max size", func(t *testing.T) {
		m := newManager(twoNodes(), newAWS(1, 2, 2), &clock{time.Now()})
		if d, why := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Blocked || !strings.Contains(why, "raise maxSize") {
			t.Fatalf("got %v %q", d, why)
		}
	})
	t.Run("node group not in allowlist", func(t *testing.T) {
		m := newManager(twoNodes(), newAWS(1, 4, 2), &clock{time.Now()})
		m.Cfg.Groups = map[string]bool{"other": true}
		if d, why := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Blocked || !strings.Contains(why, "Karpenter") {
			t.Fatalf("got %v %q", d, why)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		a := newAWS(1, 4, 2)
		m := newManager(twoNodes(), a, &clock{time.Now()})
		m.Cfg.DryRun = true
		if d, why := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Blocked || !strings.Contains(why, "would first scale gpu 2 → 3") || len(a.desired) != 0 {
			t.Fatalf("got %v %q, calls %v", d, why, a.desired)
		}
	})
	t.Run("scale-up timeout", func(t *testing.T) {
		c := &clock{time.Now()}
		m := newManager(twoNodes(), newAWS(1, 4, 2), c)
		m.Ensure(ctx, inference, 3.5, 4<<30, 2)
		c.t = c.t.Add(16 * time.Minute)
		if d, _ := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Blocked || m.History()[0].Outcome != "failed" {
			t.Fatalf("got %v, history %+v", d, m.History())
		}
	})
}

// Three CPU nodes; c is nearly empty after a downsize, and its pod fits on a.
func consolidationCluster() *fakeCluster {
	return &fakeCluster{
		nodes: []map[string]any{
			node("a", "gpu", "4", "16Gi", "", true), node("b", "gpu", "4", "16Gi", "", true), node("c", "gpu", "4", "16Gi", "", true),
		},
		pods: []map[string]any{
			pod("shop", "api-7d9f8b6c4d-x2k9p", "a", "ReplicaSet", "2", "4Gi", ""),
			pod("shop", "api-7d9f8b6c4d-q8w7f", "b", "ReplicaSet", "3", "8Gi", ""),
			pod("shop", "web-5c8d7f9b6-bcd42", "c", "ReplicaSet", "500m", "1Gi", ""),
			pod("kube-system", "aws-node-x2k9p", "c", "DaemonSet", "25m", "64Mi", ""),
		},
	}
}

func TestConsolidateDrainsAndTerminates(t *testing.T) {
	f, a, c := consolidationCluster(), newAWS(1, 5, 3), &clock{time.Now()}
	m := newManager(f, a, c)
	ctx := context.Background()

	m.Consolidate(ctx) // round 1: cordon c and evict its pod
	if len(f.evicted) != 1 || f.evicted[0] != "web-5c8d7f9b6-bcd42" {
		t.Fatalf("evicted %v", f.evicted)
	}
	md := f.nodes[2]["metadata"].(map[string]any)
	if md["annotations"].(map[string]any)[AnnoDraining] == nil || f.nodes[2]["spec"].(map[string]any)["unschedulable"] != true {
		t.Fatalf("node c should be cordoned and annotated: %v", f.nodes[2])
	}
	m.Consolidate(ctx) // round 2: only the DaemonSet pod is left: terminate i-c
	if len(a.terminated) != 1 || a.terminated[0] != "i-c" || a.ng.DesiredSize != 2 {
		t.Fatalf("terminated %v, desired %d", a.terminated, a.ng.DesiredSize)
	}
	f.nodes = f.nodes[:2]
	c.t = c.t.Add(time.Minute)
	m.Consolidate(ctx) // cooldown: nothing more happens
	if len(a.terminated) != 1 || len(f.evicted) != 1 {
		t.Fatalf("cooldown not respected: terminated %v evicted %v", a.terminated, f.evicted)
	}
}

func TestConsolidateRespectsGuardrails(t *testing.T) {
	ctx := context.Background()
	t.Run("min size", func(t *testing.T) {
		f, a := consolidationCluster(), newAWS(3, 5, 3)
		newManager(f, a, &clock{time.Now()}).Consolidate(ctx)
		if len(f.evicted) != 0 {
			t.Fatalf("must not shrink below minSize: %v", f.evicted)
		}
	})
	t.Run("pods would not fit elsewhere", func(t *testing.T) {
		f := consolidationCluster()
		f.pods[2] = pod("shop", "web-5c8d7f9b6-bcd42", "c", "ReplicaSet", "1500m", "1Gi", "") // a has 2 free, b 1
		f.pods[0] = pod("shop", "api-7d9f8b6c4d-x2k9p", "a", "ReplicaSet", "3", "4Gi", "")
		m := newManager(f, newAWS(1, 5, 3), &clock{time.Now()})
		m.Consolidate(ctx)
		if len(f.evicted) != 0 || !strings.Contains(m.History()[0].Reason, "would not fit") {
			t.Fatalf("evicted %v, history %+v", f.evicted, m.History())
		}
	})
	t.Run("bare pod", func(t *testing.T) {
		f := consolidationCluster()
		f.pods[2] = pod("shop", "debug", "c", "", "100m", "64Mi", "")
		m := newManager(f, newAWS(1, 5, 3), &clock{time.Now()})
		m.Consolidate(ctx)
		if len(f.evicted) != 0 {
			t.Fatalf("bare pods must block a drain: %v", f.evicted)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		f, a := consolidationCluster(), newAWS(1, 5, 3)
		m := newManager(f, a, &clock{time.Now()})
		m.Cfg.DryRun = true
		m.Consolidate(ctx)
		if len(f.evicted) != 0 || m.History()[0].Outcome != "dry-run" {
			t.Fatalf("dry run changed something: evicted %v history %+v", f.evicted, m.History())
		}
	})
	t.Run("PDB blocks then times out", func(t *testing.T) {
		f, a, c := consolidationCluster(), newAWS(1, 5, 3), &clock{time.Now()}
		f.pdbBlocked = map[string]bool{"web-5c8d7f9b6-bcd42": true}
		m := newManager(f, a, c)
		m.Consolidate(ctx) // cordons, eviction refused
		c.t = c.t.Add(time.Minute)
		m.Consolidate(ctx) // still waiting
		if m.History()[0].Outcome != "waiting" {
			t.Fatalf("history %+v", m.History())
		}
		c.t = c.t.Add(15 * time.Minute)
		m.Consolidate(ctx) // gives up and uncordons
		if f.nodes[2]["spec"].(map[string]any)["unschedulable"] != false || len(a.terminated) != 0 {
			t.Fatalf("node should be uncordoned and kept: %v, terminated %v", f.nodes[2], a.terminated)
		}
		if h := m.History()[0]; h.Outcome != "failed" || !strings.Contains(h.Reason, "timed out") {
			t.Fatalf("history %+v", h)
		}
	})
}

func TestLoadParsesRequests(t *testing.T) {
	f := consolidationCluster()
	cl, err := Load(context.Background(), f, testGroupLabel)
	if err != nil {
		t.Fatal(err)
	}
	c := cl.Nodes["c"]
	if c.ProviderID != "aws:///us-east-1a/i-c" || c.Group != "gpu" || !c.Ready || fmt.Sprintf("%.3f", c.Requested[CPU]) != "0.525" || c.Requested[Pods] != 2 {
		t.Fatalf("node c = %+v", c)
	}
}

func TestKarpenterAndExternalAutoscalerGetTheUpsize(t *testing.T) {
	ctx := context.Background()
	t.Run("karpenter detected", func(t *testing.T) {
		f := twoNodes()
		for _, n := range f.nodes {
			n["metadata"].(map[string]any)["labels"] = map[string]any{KarpenterLabel: "gpu-pool"}
		}
		m := newManager(f, nil, &clock{time.Now()})
		if d, why := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Fits {
			t.Fatalf("got %v %q", d, why)
		}
		if h := m.History(); len(h) != 1 || h[0].Outcome != "delegated" || !strings.Contains(h[0].Reason, "Karpenter (nodepool gpu-pool)") {
			t.Fatalf("history %+v", h)
		}
	})
	t.Run("cluster autoscaler flag", func(t *testing.T) {
		m := newManager(twoNodes(), nil, &clock{time.Now()})
		m.Cfg.ExternalAutoscaler = true
		if d, _ := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Fits {
			t.Fatalf("got %v", d)
		}
	})
	t.Run("no autoscaler: blocked", func(t *testing.T) {
		m := newManager(twoNodes(), nil, &clock{time.Now()})
		if d, _ := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Blocked {
			t.Fatalf("got %v", d)
		}
	})
}

func TestNoConsolidationRightAfterScaleUpOrWithPendingPods(t *testing.T) {
	ctx := context.Background()
	f, a, c := twoNodes(), newAWS(1, 4, 2), &clock{time.Now()}
	m := newManager(f, a, c)
	m.Ensure(ctx, inference, 3.5, 4<<30, 2) // scale 2 -> 3
	f.nodes = append(f.nodes, node("c", "gpu", "4", "16Gi", "1", true))
	c.t = c.t.Add(time.Hour) // long past the scale-up cooldown
	if d, _ := m.Ensure(ctx, inference, 3.5, 4<<30, 2); d != Fits {
		t.Fatal("should fit")
	}
	m.Consolidate(ctx) // the new, still empty node c must survive: cooldown restarted
	if len(f.evicted) != 0 || f.nodes[2]["spec"].(map[string]any)["unschedulable"] == true {
		t.Fatalf("consolidated the node just added: %+v", m.History())
	}

	f2 := consolidationCluster()
	f2.pods = append(f2.pods, pod("shop", "web-5c8d7f9b6-zz2bb", "", "ReplicaSet", "100m", "64Mi", ""))
	m2 := newManager(f2, newAWS(1, 5, 3), &clock{time.Now()})
	m2.Consolidate(ctx)
	if len(f2.evicted) != 0 {
		t.Fatalf("must not consolidate while a pod is Pending: %v", f2.evicted)
	}
}

func TestFitsInPlace(t *testing.T) {
	ctx := context.Background()
	m := newManager(twoNodes(), nil, &clock{time.Now()}) // each node: 4 cores, one 3-core inference pod
	if ok, why := m.FitsInPlace(ctx, inference, 3.9, 4<<30); !ok {
		t.Fatalf("+0.9 core fits on a 4-core node with 1 free: %s", why)
	}
	if ok, why := m.FitsInPlace(ctx, inference, 4.5, 4<<30); ok || !strings.Contains(why, "no room to grow") {
		t.Fatalf("+1.5 core must not fit in place: %v %q", ok, why)
	}
	if ok, _ := m.FitsInPlace(ctx, inference, 1, 1<<30); !ok {
		t.Fatal("shrinking always fits")
	}
}
