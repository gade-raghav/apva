// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package collector gathers per-workload resource usage and workload-to-workload
// traffic from Prometheus.
package collector

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gade-raghav/apva/internal/prom"
)

// WorkloadKey identifies a workload (Deployment, StatefulSet, ...) by namespace and name.
type WorkloadKey struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (k WorkloadKey) String() string { return k.Namespace + "/" + k.Name }

// WorkloadUsage is the per-pod resource picture of one workload. Values are per pod,
// because Kubernetes requests are set per pod.
type WorkloadUsage struct {
	Key      WorkloadKey `json:"key"`
	Replicas int         `json:"replicas"`

	CPURequestCores float64 `json:"cpuRequestCores"`
	CPUP95Cores     float64 `json:"cpuP95Cores"`
	MemRequestBytes float64 `json:"memRequestBytes"`
	MemP95Bytes     float64 `json:"memP95Bytes"`

	// HasCPUMetrics / HasMemMetrics distinguish "used zero" from "no data".
	HasCPUMetrics bool `json:"hasCpuMetrics"`
	HasMemMetrics bool `json:"hasMemMetrics"`

	GPURequested  float64 `json:"gpuRequested"`
	GPUAvgUtilPct float64 `json:"gpuAvgUtilPct"`
	GPUP95UtilPct float64 `json:"gpuP95UtilPct"`
	HasGPUMetrics bool    `json:"hasGpuMetrics"`
}

// Edge is observed traffic from one workload to another.
type Edge struct {
	From WorkloadKey `json:"from"`
	To   WorkloadKey `json:"to"`
	// RatePerSec is the average flow rate over the analysis window.
	RatePerSec float64 `json:"ratePerSec"`
	// RecentRatePerSec is the flow rate over the recent window (default 1h).
	RecentRatePerSec float64 `json:"recentRatePerSec"`
}

// Trend is recent rate divided by window rate; >1 means traffic is growing.
func (e Edge) Trend() float64 {
	if e.RatePerSec <= 0 {
		if e.RecentRatePerSec > 0 {
			return 2
		}
		return 1
	}
	return e.RecentRatePerSec / e.RatePerSec
}

// Snapshot is one collection run.
type Snapshot struct {
	CollectedAt time.Time        `json:"collectedAt"`
	Window      time.Duration    `json:"window"`
	Workloads   []*WorkloadUsage `json:"workloads"`
	Edges       []Edge           `json:"edges"`
	// Warnings lists optional data sources that were unavailable (e.g. no GPU metrics).
	Warnings []string `json:"warnings,omitempty"`
}

// Config controls collection.
type Config struct {
	Window        time.Duration // analysis look-back, e.g. 24h
	RecentWindow  time.Duration // for traffic trend, e.g. 1h
	Namespaces    []string      // empty = all
	ExcludeSystem bool          // skip kube-system etc.
}

// Collector queries Prometheus.
type Collector struct {
	Q   prom.Querier
	Cfg Config
}

func promDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

// Queries returns the PromQL used, keyed by purpose. Exposed for docs and tests.
func Queries(window, recent time.Duration, nsMatcher string) map[string]string {
	w, r := promDuration(window), promDuration(recent)
	c := `container!="",container!="POD"` + nsMatcher
	return map[string]string{
		"cpu_p95":   fmt.Sprintf(`sum by (namespace, pod) (quantile_over_time(0.95, rate(container_cpu_usage_seconds_total{%s}[5m])[%s:5m]))`, c, w),
		"mem_p95":   fmt.Sprintf(`sum by (namespace, pod) (quantile_over_time(0.95, container_memory_working_set_bytes{%s}[%s]))`, c, w),
		"cpu_req":   fmt.Sprintf(`sum by (namespace, pod) (kube_pod_container_resource_requests{resource="cpu"%s})`, nsMatcher),
		"mem_req":   fmt.Sprintf(`sum by (namespace, pod) (kube_pod_container_resource_requests{resource="memory"%s})`, nsMatcher),
		"gpu_req":   fmt.Sprintf(`sum by (namespace, pod) (kube_pod_container_resource_requests{resource="nvidia_com_gpu"%s})`, nsMatcher),
		"gpu_avg":   fmt.Sprintf(`avg by (namespace, pod) (avg_over_time(DCGM_FI_DEV_GPU_UTIL[%s]))`, w),
		"gpu_p95":   fmt.Sprintf(`avg by (namespace, pod) (quantile_over_time(0.95, DCGM_FI_DEV_GPU_UTIL[%s]))`, w),
		"flows":     fmt.Sprintf(`sum by (source_namespace, source_workload, destination_namespace, destination_workload) (rate(hubble_flows_processed_total{source_workload!="",destination_workload!=""}[%s]))`, w),
		"flows_now": fmt.Sprintf(`sum by (source_namespace, source_workload, destination_namespace, destination_workload) (rate(hubble_flows_processed_total{source_workload!="",destination_workload!=""}[%s]))`, r),
	}
}

var systemNamespaces = map[string]bool{
	"kube-system": true, "kube-public": true, "kube-node-lease": true,
	"monitoring": true, "cilium": true, "apva": true,
}

// Collect runs all queries and assembles a Snapshot. CPU/memory queries are required;
// GPU and Hubble queries are optional and only produce warnings if they fail or are empty.
func (c *Collector) Collect(ctx context.Context) (*Snapshot, error) {
	nsMatcher := ""
	if len(c.Cfg.Namespaces) > 0 {
		nsMatcher = fmt.Sprintf(`,namespace=~"%s"`, strings.Join(c.Cfg.Namespaces, "|"))
	}
	qs := Queries(c.Cfg.Window, c.Cfg.RecentWindow, nsMatcher)
	snap := &Snapshot{CollectedAt: time.Now().UTC(), Window: c.Cfg.Window}

	type podKey struct{ ns, pod string }
	pods := map[podKey]map[string]float64{}
	add := func(metric string, samples []prom.Sample) {
		for _, s := range samples {
			ns, pod := label(s.Labels, "namespace", "exported_namespace"), label(s.Labels, "pod", "exported_pod")
			if ns == "" || pod == "" {
				continue
			}
			k := podKey{ns, pod}
			if pods[k] == nil {
				pods[k] = map[string]float64{}
			}
			pods[k][metric] = s.Value
		}
	}

	for _, m := range []string{"cpu_p95", "mem_p95", "cpu_req", "mem_req"} {
		s, err := c.Q.Query(ctx, qs[m])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		add(m, s)
	}
	gpuFound := false
	for _, m := range []string{"gpu_req", "gpu_avg", "gpu_p95"} {
		s, err := c.Q.Query(ctx, qs[m])
		if err != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("GPU metrics unavailable (%s): %v", m, err))
			continue
		}
		if m != "gpu_req" && len(s) > 0 {
			gpuFound = true
		}
		add(m, s)
	}
	if !gpuFound {
		snap.Warnings = append(snap.Warnings, "no DCGM GPU utilisation metrics found; GPU analysis skipped")
	}

	// Aggregate pods into workloads. Per-pod values are combined conservatively:
	// usage takes the max across pods, requests take the max (they are normally equal).
	wl := map[WorkloadKey]*WorkloadUsage{}
	for k, m := range pods {
		if c.Cfg.ExcludeSystem && systemNamespaces[k.ns] {
			continue
		}
		key := WorkloadKey{Namespace: k.ns, Name: WorkloadFromPod(k.pod)}
		w := wl[key]
		if w == nil {
			w = &WorkloadUsage{Key: key}
			wl[key] = w
		}
		w.Replicas++
		w.CPUP95Cores = max(w.CPUP95Cores, m["cpu_p95"])
		w.MemP95Bytes = max(w.MemP95Bytes, m["mem_p95"])
		w.CPURequestCores = max(w.CPURequestCores, m["cpu_req"])
		w.MemRequestBytes = max(w.MemRequestBytes, m["mem_req"])
		w.GPURequested = max(w.GPURequested, m["gpu_req"])
		if _, ok := m["cpu_p95"]; ok {
			w.HasCPUMetrics = true
		}
		if _, ok := m["mem_p95"]; ok {
			w.HasMemMetrics = true
		}
		if _, ok := m["gpu_avg"]; ok {
			w.HasGPUMetrics = true
			w.GPUAvgUtilPct = max(w.GPUAvgUtilPct, m["gpu_avg"])
			w.GPUP95UtilPct = max(w.GPUP95UtilPct, m["gpu_p95"])
		}
	}
	for _, w := range wl {
		snap.Workloads = append(snap.Workloads, w)
	}
	sort.Slice(snap.Workloads, func(i, j int) bool {
		return snap.Workloads[i].Key.String() < snap.Workloads[j].Key.String()
	})

	// Traffic graph (optional).
	flows, err := c.Q.Query(ctx, qs["flows"])
	if err != nil {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("Hubble flow metrics unavailable: %v", err))
	} else if len(flows) == 0 {
		snap.Warnings = append(snap.Warnings, "no Hubble flow metrics found; service graph skipped")
	} else {
		recent := map[[2]WorkloadKey]float64{}
		if now, err := c.Q.Query(ctx, qs["flows_now"]); err == nil {
			for _, s := range now {
				recent[edgeKey(s.Labels)] = s.Value
			}
		}
		for _, s := range flows {
			ek := edgeKey(s.Labels)
			if c.Cfg.ExcludeSystem && (systemNamespaces[ek[0].Namespace] || systemNamespaces[ek[1].Namespace]) {
				continue
			}
			snap.Edges = append(snap.Edges, Edge{From: ek[0], To: ek[1], RatePerSec: s.Value, RecentRatePerSec: recent[ek]})
		}
		sort.Slice(snap.Edges, func(i, j int) bool {
			a, b := snap.Edges[i], snap.Edges[j]
			if a.From != b.From {
				return a.From.String() < b.From.String()
			}
			return a.To.String() < b.To.String()
		})
	}
	return snap, nil
}

func edgeKey(l map[string]string) [2]WorkloadKey {
	return [2]WorkloadKey{
		{Namespace: l["source_namespace"], Name: l["source_workload"]},
		{Namespace: l["destination_namespace"], Name: l["destination_workload"]},
	}
}

func label(l map[string]string, names ...string) string {
	for _, n := range names {
		if v := l[n]; v != "" {
			return v
		}
	}
	return ""
}

var (
	// Kubernetes generates hashes and suffixes from a vowel-free alphabet
	// (k8s.io/apimachinery/pkg/util/rand.SafeEncodeString), which avoids matching words.
	// Deployment pods: <name>-<pod-template-hash (6-10 chars)>-<5 char suffix>
	deployPod = regexp.MustCompile(`^(.+)-[bcdfghjklmnpqrstvwxz2456789]{6,10}-[bcdfghjklmnpqrstvwxz2456789]{5}$`)
	// DaemonSet / Job pods: <name>-<5 char suffix>
	randomSuffix = regexp.MustCompile(`^(.+)-[bcdfghjklmnpqrstvwxz2456789]{5}$`)
	// StatefulSet pods: <name>-<ordinal>
	ordinal = regexp.MustCompile(`^(.+)-[0-9]+$`)
)

// WorkloadFromPod derives the owning workload name from a pod name using Kubernetes'
// naming conventions. v0.2 will replace this heuristic with owner references
// (kube_pod_owner / kube_replicaset_owner).
func WorkloadFromPod(pod string) string {
	if m := deployPod.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	if m := ordinal.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	if m := randomSuffix.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	return pod
}
