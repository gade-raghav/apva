// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gade-raghav/apva/internal/kube"
)

// Consolidate runs one round of node consolidation for every managed node group: it
// resumes or finishes an in-flight drain, or picks at most one under-used node per group,
// checks its pods fit elsewhere in the group, cordons it and evicts its pods through the
// Eviction API (so PodDisruptionBudgets are respected). Once only DaemonSet pods remain it
// terminates that exact instance and lowers the group's desired size by one.
func (m *Manager) Consolidate(ctx context.Context) {
	if m.Provider == nil || !m.Cfg.Consolidate || len(m.Cfg.Groups) == 0 {
		return
	}
	cl, err := Load(ctx, m.K, m.groupLabel())
	if err != nil {
		m.record("*", "failed", "reading nodes and pods: "+err.Error())
		return
	}
	for _, p := range cl.Pods {
		if p.Node == "" {
			// Something is waiting to be scheduled (maybe onto the very nodes we would
			// remove); like the Cluster Autoscaler, don't scale down meanwhile.
			return
		}
	}
	byGroup := map[string][]*Node{}
	for _, n := range cl.Nodes {
		if m.managed(n.Group) {
			byGroup[n.Group] = append(byGroup[n.Group], n)
		}
	}
	groups := make([]string, 0, len(m.Cfg.Groups))
	for g := range m.Cfg.Groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		nodes := byGroup[g]
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
		if d := draining(nodes); d != nil {
			m.continueDrain(ctx, g, d)
			continue
		}
		m.startDrain(ctx, g, nodes)
	}
}

func draining(nodes []*Node) *Node {
	for _, n := range nodes {
		if n.Annotations[AnnoDraining] != "" {
			return n
		}
	}
	return nil
}

// movable returns the pods that would have to move off n, or why n can't be drained.
func movable(n *Node) ([]*Pod, string) {
	var out []*Pod
	for _, p := range n.Pods {
		if p.DaemonSet || p.Mirror {
			continue
		}
		safe := p.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]
		if v := p.Annotations["apva.io/safe-to-evict"]; v != "" {
			safe = v
		}
		switch {
		case safe == "false":
			return nil, fmt.Sprintf("pod %s/%s is marked safe-to-evict=false", p.Namespace, p.Name)
		case safe == "true":
		case !p.Controlled:
			return nil, fmt.Sprintf("pod %s/%s has no controller and would not come back", p.Namespace, p.Name)
		case p.Namespace == "kube-system":
			return nil, fmt.Sprintf("pod %s/%s is a kube-system pod", p.Namespace, p.Name)
		case p.LocalStorage:
			return nil, fmt.Sprintf("pod %s/%s uses local emptyDir storage", p.Namespace, p.Name)
		}
		out = append(out, p)
	}
	return out, ""
}

func utilisation(n *Node) float64 {
	u := 0.0
	for _, r := range []string{CPU, Memory, GPU} {
		if a := n.Allocatable[r]; a > 0 {
			if v := n.Requested[r] / a; v > u {
				u = v
			}
		}
	}
	return u
}

func (m *Manager) startDrain(ctx context.Context, group string, nodes []*Node) {
	if m.scalingUp(group) || !m.cooledDown(group) {
		return
	}
	ready := 0
	for _, n := range nodes {
		if n.Ready {
			ready++
		}
	}
	// Least-used schedulable node first.
	var cands []*Node
	for _, n := range nodes {
		if n.Schedulable() && n.ProviderID != "" && utilisation(n) < m.Cfg.ConsolidateBelow {
			cands = append(cands, n)
		}
	}
	if len(cands) == 0 {
		return
	}
	sort.SliceStable(cands, func(i, j int) bool { return utilisation(cands[i]) < utilisation(cands[j]) })

	ng, err := m.Provider.DescribeGroup(ctx, group)
	if err != nil {
		m.record(group, "failed", "describing node group: "+err.Error())
		return
	}
	if ng.DesiredSize-1 < ng.MinSize || ready-1 < ng.MinSize {
		return // already at minimum size
	}

	lastWhy := ""
	for _, n := range cands {
		pods, why := movable(n)
		if why != "" {
			lastWhy = why
			continue
		}
		// Will every pod fit on the other schedulable nodes of the group?
		var free []Resources
		for _, o := range nodes {
			if o != n && o.Schedulable() {
				free = append(free, o.Free())
			}
		}
		sort.Slice(pods, func(i, j int) bool { return pods[i].Requests[CPU] > pods[j].Requests[CPU] })
		ok := true
		for _, p := range pods {
			if place(free, p.Requests, 1) != 1 {
				ok, lastWhy = false, fmt.Sprintf("pods on %s would not fit on the other nodes", n.Name)
				break
			}
		}
		if !ok {
			continue
		}
		reason := fmt.Sprintf("drain %s (%.0f%% requested, %d pod(s) move to other nodes), then remove it: %s %d → %d nodes",
			n.Name, utilisation(n)*100, len(pods), group, ng.DesiredSize, ng.DesiredSize-1)
		if m.Cfg.DryRun {
			m.record(group, "dry-run", reason)
			return
		}
		if err := m.cordon(ctx, n.Name, true); err != nil {
			m.record(group, "failed", "cordoning "+n.Name+": "+err.Error())
			return
		}
		m.evict(ctx, pods)
		m.touch(group)
		m.record(group, "applied", reason)
		return
	}
	if lastWhy != "" {
		m.record(group, "skipped", "no node can be removed: "+lastWhy)
	}
}

func (m *Manager) continueDrain(ctx context.Context, group string, n *Node) {
	started, _ := time.Parse(time.RFC3339, n.Annotations[AnnoDraining])
	pods, _ := movable(n)
	var remaining []*Pod
	for _, p := range n.Pods {
		if !p.DaemonSet && !p.Mirror {
			remaining = append(remaining, p)
		}
	}
	if len(remaining) == 0 {
		if m.Cfg.DryRun {
			return
		}
		if err := m.Provider.RemoveNode(ctx, n); err != nil {
			m.record(group, "failed", fmt.Sprintf("removing %s: %v", n.Name, err))
			return
		}
		m.touch(group)
		m.record(group, "applied", fmt.Sprintf("removed %s; node group shrinks by one", n.Name))
		return
	}
	if !started.IsZero() && m.now().Sub(started) > m.Cfg.DrainTimeout {
		_ = m.cordon(ctx, n.Name, false)
		m.touch(group)
		m.record(group, "failed", fmt.Sprintf("drain of %s timed out after %s with %d pod(s) left (a PodDisruptionBudget?); uncordoned it", n.Name, m.Cfg.DrainTimeout, len(remaining)))
		return
	}
	if len(pods) < len(remaining) {
		// Something unmovable landed on the node meanwhile; give the node back.
		_ = m.cordon(ctx, n.Name, false)
		m.record(group, "skipped", "stopped draining "+n.Name+": a pod on it can no longer be evicted safely")
		return
	}
	if !m.Cfg.DryRun {
		m.evict(ctx, pods)
	}
	m.record(group, "waiting", fmt.Sprintf("draining %s: %d pod(s) left", n.Name, len(remaining)))
}

func (m *Manager) cordon(ctx context.Context, node string, on bool) error {
	var anno any // null removes the annotation
	if on {
		anno = m.now().UTC().Format(time.RFC3339)
	}
	return m.K.StrategicMergePatch(ctx, "/api/v1/nodes/"+node, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnoDraining: anno}},
		"spec":     map[string]any{"unschedulable": on},
	})
}

// evict asks the API server to evict each pod. A 429 means a PodDisruptionBudget does not
// allow it yet; the next round retries.
func (m *Manager) evict(ctx context.Context, pods []*Pod) {
	for _, p := range pods {
		err := m.K.Create(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", p.Namespace, p.Name), map[string]any{
			"apiVersion": "policy/v1", "kind": "Eviction",
			"metadata": map[string]string{"name": p.Name, "namespace": p.Namespace},
		})
		if err != nil && !kube.IsStatus(err, http.StatusTooManyRequests) && !kube.IsStatus(err, http.StatusNotFound) && m.Log != nil {
			m.Log.Warn("eviction failed", "pod", p.Namespace+"/"+p.Name, "err", err)
		}
	}
}

func (m *Manager) touch(group string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastChange == nil {
		m.lastChange = map[string]time.Time{}
	}
	m.lastChange[group] = m.now()
}
