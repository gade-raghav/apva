// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
)

// Group is a provider's node group (an EKS managed node group, a GKE node pool, ...).
type Group struct {
	Name, Status                  string // Status: "ACTIVE" when it can be changed
	MinSize, MaxSize, DesiredSize int
}

// Provider adds and removes nodes for one cloud. APVA ships an Amazon EKS provider
// (internal/aws); others implement the same four calls.
type Provider interface {
	Name() string       // e.g. "aws"
	GroupLabel() string // node label holding a node's group name
	DescribeGroup(ctx context.Context, group string) (Group, error)
	SetDesiredSize(ctx context.Context, g Group, desired int) error
	// RemoveNode removes exactly this node and lowers its group's desired size by one.
	RemoveNode(ctx context.Context, n *Node) error
}

// Config tunes the manager.
type Config struct {
	DryRun bool

	// Node group management (needs a Provider).
	Groups map[string]bool // node groups APVA may resize (allowlist)
	// ExternalAutoscaler: a Cluster Autoscaler adds nodes for Pending pods, so upsizes on
	// unmanaged groups may go ahead. Karpenter nodes are detected and treated this way.
	ExternalAutoscaler bool
	ScaleUpTimeout     time.Duration // how long to wait for new nodes to become Ready
	Consolidate        bool          // drain and remove under-used nodes
	ConsolidateBelow   float64       // a node is a candidate when every resource is below this fraction requested
	DrainTimeout       time.Duration // give up (and uncordon) after this long
	Cooldown           time.Duration // minimum time between two changes to one node group
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{
		ScaleUpTimeout: 15 * time.Minute, Consolidate: true, ConsolidateBelow: 0.5,
		DrainTimeout: 10 * time.Minute, Cooldown: 10 * time.Minute,
	}
}

// Event is one node group decision, shown in the dashboard next to workload resizes.
type Event struct {
	Time    time.Time `json:"time"`
	Group   string    `json:"group"`
	Outcome string    `json:"outcome"` // applied | dry-run | waiting | skipped | failed
	Reason  string    `json:"reason"`
}

// Decision is the answer to "may this workload be resized upwards now?".
type Decision int

const (
	Fits    Decision = iota // go ahead
	Waiting                 // node group is scaling up; try again next round
	Blocked                 // won't fit and APVA can't (or may not) add capacity
)

// Annotation APVA puts on a node it is draining (value: RFC3339 start time).
const AnnoDraining = "apva.io/draining"

type pendingScaleUp struct {
	targetReady int
	since       time.Time
	workload    collector.WorkloadKey
}

// Manager checks capacity and manages EKS node groups. Safe for concurrent use.
type Manager struct {
	K        API
	Provider Provider // nil: capacity checks only
	Cfg      Config
	Log      *slog.Logger
	Now      func() time.Time

	mu         sync.Mutex
	pending    map[string]*pendingScaleUp
	lastChange map[string]time.Time
	history    []Event
	last       map[string]Event
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) managed(group string) bool {
	return m.Provider != nil && group != "" && m.Cfg.Groups[group]
}

func (m *Manager) groupLabel() string {
	if m.Provider == nil {
		return ""
	}
	return m.Provider.GroupLabel()
}

// History returns recent node group decisions, newest first.
func (m *Manager) History() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.history...)
}

func (m *Manager) record(group, outcome, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]Event{}
	}
	ev := Event{Time: m.now().UTC(), Group: group, Outcome: outcome, Reason: reason}
	if prev, ok := m.last[group]; ok && prev.Outcome == outcome && prev.Reason == reason && outcome != "applied" {
		return
	}
	m.last[group] = ev
	m.history = append([]Event{ev}, m.history...)
	if len(m.history) > 100 {
		m.history = m.history[:100]
	}
	if m.Log != nil {
		m.Log.Info("node group", "nodegroup", group, "outcome", outcome, "reason", reason)
	}
}

func fmtRes(r Resources) string {
	var parts []string
	if v := r[CPU]; v > 0 {
		parts = append(parts, kube.FormatCPU(v))
	}
	if v := r[Memory]; v > 0 {
		parts = append(parts, kube.FormatMemory(v))
	}
	if v := r[GPU]; v > 0 {
		parts = append(parts, fmt.Sprintf("%g GPU", v))
	}
	return strings.Join(parts, " / ")
}

// place counts how many pods of size req fit into the free maps (first fit, mutating).
func place(free []Resources, req Resources, want int) int {
	placed := 0
	for placed < want {
		ok := false
		for _, f := range free {
			if fits(f, req) {
				f.add(req, -1)
				placed++
				ok = true
				break
			}
		}
		if !ok {
			break
		}
	}
	return placed
}

// Ensure decides whether a workload whose pods will each request cpu cores and mem bytes
// can be resized now: all replicas must fit once the old pods are gone, and the rolling
// update's first new pod must fit while they are all still running (this matters for
// downsizes too). On a managed EKS node group without room it scales the group up
// and answers Waiting until the new nodes are Ready.
func (m *Manager) Ensure(ctx context.Context, w collector.WorkloadKey, cpu, mem float64, replicas int) (Decision, string) {
	cl, err := Load(ctx, m.K, m.groupLabel())
	if err != nil {
		return Fits, "" // can't see nodes (e.g. no RBAC): behave as before rather than block
	}
	var own []*Pod
	groups := map[string]bool{}
	for _, p := range cl.Pods {
		if p.Workload == w && !p.DaemonSet && p.Node != "" {
			own = append(own, p)
			if n := cl.Nodes[p.Node]; n != nil {
				groups[n.Group] = true
			}
		}
	}
	if len(own) == 0 {
		return Fits, ""
	}
	if replicas < len(own) {
		replicas = len(own)
	}
	req := own[0].Requests.clone()
	req[CPU], req[Memory] = cpu, mem

	var pool []*Node
	for _, n := range cl.Nodes {
		if groups[n.Group] && n.Schedulable() {
			pool = append(pool, n)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Name < pool[j].Name })
	// Surge: during the rolling update one new pod starts before an old one stops.
	var before, after []Resources
	for _, n := range pool {
		f := n.Free()
		before = append(before, f.clone())
		for _, p := range n.Pods {
			if p.Workload == w && !p.DaemonSet {
				f.add(p.Requests, 1) // these pods are replaced by the resize
			}
		}
		after = append(after, f)
	}
	surgeOK := place(before, req, 1) == 1
	fit := place(after, req, replicas)
	if fit >= replicas && surgeOK {
		for _, g := range groupList(groups) {
			if m.scalingUp(g) {
				// The nodes we added are ready. Restart the group's cooldown so consolidation
				// leaves them alone while this workload's rollout moves onto them.
				m.clearPending([]string{g})
				m.touch(g)
			}
		}
		return Fits, ""
	}

	missing := replicas - fit
	if !surgeOK && missing < 1 {
		missing = 1
	}
	need := fmt.Sprintf("%d more pod(s) of %s", missing, fmtRes(req))
	if len(groups) != 1 {
		return Blocked, "not enough free capacity for " + need + " (pods span several node pools)"
	}
	group := groupList(groups)[0]
	if !m.managed(group) {
		if strings.HasPrefix(group, "karpenter:") || m.Cfg.ExternalAutoscaler {
			who := "the cluster autoscaler"
			if strings.HasPrefix(group, "karpenter:") {
				who = "Karpenter (nodepool " + strings.TrimPrefix(group, "karpenter:") + ")"
			}
			m.record(group, "delegated", fmt.Sprintf("%s/%s needs %s; %s will add nodes for the Pending pods", w.Namespace, w.Name, need, who))
			return Fits, ""
		}
		where := "these nodes"
		if group != "" {
			where = "node group " + group
		}
		return Blocked, fmt.Sprintf("not enough free capacity on %s for %s; add nodes, allowlist the node group, or run Karpenter/Cluster Autoscaler (--node-autoscaler-present)", where, need)
	}

	ready := 0
	for _, n := range cl.Nodes {
		if n.Group == group && n.Ready {
			ready++
		}
	}
	m.mu.Lock()
	if m.pending == nil {
		m.pending = map[string]*pendingScaleUp{}
	}
	pend := m.pending[group]
	m.mu.Unlock()
	if pend != nil {
		if m.now().Sub(pend.since) > m.Cfg.ScaleUpTimeout {
			m.clearPending([]string{group})
			m.record(group, "failed", fmt.Sprintf("only %d of %d nodes Ready after %s", ready, pend.targetReady, m.Cfg.ScaleUpTimeout))
			return Blocked, "node group " + group + " did not scale up in time"
		}
		return Waiting, fmt.Sprintf("waiting for node group %s: %d of %d nodes Ready", group, ready, pend.targetReady)
	}

	// How many new pods fit on one fresh node of this group?
	perNode := 0
	for _, n := range cl.Nodes {
		if n.Group != group {
			continue
		}
		empty := n.Allocatable.clone()
		for _, p := range n.Pods {
			if p.DaemonSet || p.Mirror {
				empty.add(p.Requests, -1)
			}
		}
		if c := place([]Resources{empty}, req, 1000); c > perNode {
			perNode = c
		}
	}
	if perNode == 0 {
		return Blocked, fmt.Sprintf("one pod of %s does not fit on any node of %s; it needs a larger instance type", fmtRes(req), group)
	}
	extra := int(math.Ceil(float64(missing) / float64(perNode)))

	ng, err := m.Provider.DescribeGroup(ctx, group)
	if err != nil {
		m.record(group, "failed", "describing node group: "+err.Error())
		return Blocked, "could not read node group " + group
	}
	if ng.Status != "" && ng.Status != "ACTIVE" {
		return Waiting, fmt.Sprintf("node group %s is %s", group, ng.Status)
	}
	desired := ng.DesiredSize + extra
	if desired > ng.MaxSize {
		reason := fmt.Sprintf("needs %d more node(s) but %s is at %d of max %d; raise maxSize", extra, group, ng.DesiredSize, ng.MaxSize)
		m.record(group, "skipped", reason)
		return Blocked, reason
	}
	if !m.cooledDown(group) {
		return Waiting, "node group " + group + " changed recently (cooldown)"
	}
	reason := fmt.Sprintf("scale %s %d → %d nodes so %s/%s fits (%s)", group, ng.DesiredSize, desired, w.Namespace, w.Name, need)
	if m.Cfg.DryRun {
		m.record(group, "dry-run", reason)
		return Blocked, "would first " + reason
	}
	if err := m.Provider.SetDesiredSize(ctx, ng, desired); err != nil {
		m.record(group, "failed", "scaling up: "+err.Error())
		return Blocked, "could not scale node group " + group
	}
	m.mu.Lock()
	m.pending[group] = &pendingScaleUp{targetReady: ready + extra, since: m.now(), workload: w}
	if m.lastChange == nil {
		m.lastChange = map[string]time.Time{}
	}
	m.lastChange[group] = m.now()
	m.mu.Unlock()
	m.record(group, "applied", reason)
	return Waiting, fmt.Sprintf("scaling node group %s %d → %d first", group, ng.DesiredSize, desired)
}

func groupList(g map[string]bool) []string {
	var out []string
	for k := range g {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) clearPending(groups []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range groups {
		delete(m.pending, g)
	}
}

func (m *Manager) cooledDown(group string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastChange[group]
	return !ok || m.now().Sub(t) >= m.Cfg.Cooldown
}

func (m *Manager) scalingUp(group string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[group] != nil
}
