// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package recommender turns observed usage and traffic into right-sizing recommendations.
package recommender

import (
	"fmt"
	"math"
	"sort"

	"github.com/gade-raghav/apva/internal/collector"
)

// Action is what APVA suggests for one resource.
type Action string

const (
	ActionOK       Action = "ok"
	ActionDownsize Action = "downsize"
	ActionUpsize   Action = "upsize"
	ActionSet      Action = "set-request" // no request configured
	ActionHold     Action = "hold"        // would downsize, but upstream traffic is growing
	ActionIdle     Action = "idle"        // GPU requested but essentially unused
	ActionShare    Action = "share"       // GPU underused: consider MIG / time-slicing
	ActionNoData   Action = "no-data"
)

// Config tunes the recommender.
type Config struct {
	Headroom        float64 // fraction added on top of p95, e.g. 0.15
	Tolerance       float64 // ignore changes smaller than this fraction, e.g. 0.10
	MinCPUCores     float64 // floor for CPU recommendations
	MinMemBytes     float64 // floor for memory recommendations
	GrowthThreshold float64 // upstream trend above this blocks downsizing, e.g. 1.25
	GPUIdlePct      float64 // p95 GPU util below this => idle
	GPUSharePct     float64 // p95 GPU util below this => share
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		Headroom:        0.15,
		Tolerance:       0.10,
		MinCPUCores:     0.010,
		MinMemBytes:     32 << 20,
		GrowthThreshold: 1.25,
		GPUIdlePct:      5,
		GPUSharePct:     40,
	}
}

// ResourceRec is a recommendation for CPU or memory, per pod.
type ResourceRec struct {
	Current     float64 `json:"current"`
	P95         float64 `json:"p95"`
	Recommended float64 `json:"recommended"`
	ChangePct   float64 `json:"changePct"`
	Action      Action  `json:"action"`
}

// GPURec is the GPU assessment, per pod.
type GPURec struct {
	Requested  float64 `json:"requested"`
	AvgUtilPct float64 `json:"avgUtilPct"`
	P95UtilPct float64 `json:"p95UtilPct"`
	Action     Action  `json:"action"`
}

// Upstream describes a caller of this workload.
type Upstream struct {
	From       collector.WorkloadKey `json:"from"`
	RatePerSec float64               `json:"ratePerSec"`
	Trend      float64               `json:"trend"`
}

// Recommendation is the full result for one workload.
type Recommendation struct {
	Workload collector.WorkloadKey `json:"workload"`
	Replicas int                   `json:"replicas"`
	CPU      ResourceRec           `json:"cpu"`
	Memory   ResourceRec           `json:"memory"`
	GPU      *GPURec               `json:"gpu,omitempty"`
	Upstream []Upstream            `json:"upstream,omitempty"`

	// Savings across all replicas if the recommendation is applied (negative = needs more).
	CPUSavingsCores float64 `json:"cpuSavingsCores"`
	MemSavingsBytes float64 `json:"memSavingsBytes"`
	GPUSavings      float64 `json:"gpuSavings"`

	Confidence string   `json:"confidence"` // high | medium | low
	Reasons    []string `json:"reasons"`
}

// Recommend produces recommendations for every workload in the snapshot.
func Recommend(s *collector.Snapshot, cfg Config) []Recommendation {
	callers := map[collector.WorkloadKey][]Upstream{}
	for _, e := range s.Edges {
		callers[e.To] = append(callers[e.To], Upstream{From: e.From, RatePerSec: e.RatePerSec, Trend: e.Trend()})
	}
	out := make([]Recommendation, 0, len(s.Workloads))
	for _, w := range s.Workloads {
		up := callers[w.Key]
		sort.Slice(up, func(i, j int) bool { return up[i].RatePerSec > up[j].RatePerSec })
		out = append(out, recommendOne(w, up, s.Window.Hours(), cfg))
	}
	return out
}

func recommendOne(w *collector.WorkloadUsage, up []Upstream, windowHours float64, cfg Config) Recommendation {
	r := Recommendation{Workload: w.Key, Replicas: w.Replicas, Upstream: up}

	r.CPU = sizeResource(w.CPURequestCores, w.CPUP95Cores, w.HasCPUMetrics, cfg.MinCPUCores, cfg, roundCPU)
	r.Memory = sizeResource(w.MemRequestBytes, w.MemP95Bytes, w.HasMemMetrics, cfg.MinMemBytes, cfg, roundMem)

	// Traffic-aware guard: don't shrink a workload whose callers are ramping up.
	growing := 0.0
	var growingFrom collector.WorkloadKey
	for _, u := range up {
		if u.Trend > growing {
			growing, growingFrom = u.Trend, u.From
		}
	}
	if growing >= cfg.GrowthThreshold {
		for _, rr := range []*ResourceRec{&r.CPU, &r.Memory} {
			if rr.Action == ActionDownsize {
				rr.Action = ActionHold
				rr.Recommended = rr.Current
				rr.ChangePct = 0
			}
		}
		r.Reasons = append(r.Reasons, fmt.Sprintf("upstream %s traffic is up %.0f%% vs window average; holding any downsizing", growingFrom, (growing-1)*100))
	}

	if w.GPURequested > 0 || w.HasGPUMetrics {
		g := &GPURec{Requested: w.GPURequested, AvgUtilPct: w.GPUAvgUtilPct, P95UtilPct: w.GPUP95UtilPct}
		switch {
		case !w.HasGPUMetrics:
			g.Action = ActionNoData
			r.Reasons = append(r.Reasons, "GPU requested but no DCGM utilisation metrics found")
		case w.GPUP95UtilPct < cfg.GPUIdlePct && growing < cfg.GrowthThreshold:
			g.Action = ActionIdle
			r.GPUSavings = w.GPURequested * float64(w.Replicas)
			r.Reasons = append(r.Reasons, fmt.Sprintf("GPU p95 utilisation %.1f%% — GPU is essentially idle", w.GPUP95UtilPct))
		case w.GPUP95UtilPct < cfg.GPUSharePct:
			g.Action = ActionShare
			r.Reasons = append(r.Reasons, fmt.Sprintf("GPU p95 utilisation %.1f%% — consider MIG or time-slicing to share it", w.GPUP95UtilPct))
		default:
			g.Action = ActionOK
		}
		r.GPU = g
	}

	n := float64(w.Replicas)
	if r.CPU.Action == ActionDownsize || r.CPU.Action == ActionUpsize {
		r.CPUSavingsCores = (r.CPU.Current - r.CPU.Recommended) * n
		r.Reasons = append(r.Reasons, fmt.Sprintf("CPU p95 %.3f cores vs request %.3f", r.CPU.P95, r.CPU.Current))
	}
	if r.Memory.Action == ActionDownsize || r.Memory.Action == ActionUpsize {
		r.MemSavingsBytes = (r.Memory.Current - r.Memory.Recommended) * n
		r.Reasons = append(r.Reasons, fmt.Sprintf("memory p95 %.0fMi vs request %.0fMi", r.Memory.P95/(1<<20), r.Memory.Current/(1<<20)))
	}
	if r.CPU.Action == ActionSet || r.Memory.Action == ActionSet {
		r.Reasons = append(r.Reasons, "no resource requests set; the scheduler cannot place this workload reliably")
	}

	switch {
	case w.Replicas == 0 || (!w.HasCPUMetrics && !w.HasMemMetrics):
		r.Confidence = "low"
	case windowHours < 24 || growing >= cfg.GrowthThreshold:
		r.Confidence = "medium"
	default:
		r.Confidence = "high"
	}
	return r
}

func sizeResource(current, p95 float64, hasData bool, floor float64, cfg Config, round func(float64) float64) ResourceRec {
	rr := ResourceRec{Current: current, P95: p95}
	if !hasData {
		rr.Action = ActionNoData
		rr.Recommended = current
		return rr
	}
	need := p95 * (1 + cfg.Headroom) // what the workload actually needs
	rr.Recommended = round(math.Max(need, floor))
	if current <= 0 {
		rr.Action = ActionSet
		return rr
	}
	switch {
	case p95 > current || need > current*(1+cfg.Tolerance): // genuinely short
		rr.Action = ActionUpsize
	case rr.Recommended < current*(1-cfg.Tolerance):
		rr.Action = ActionDownsize
	default:
		// Includes the case where only the floor exceeds a (tiny) request: that is
		// not a real shortage, so leave it alone.
		rr.Action = ActionOK
		rr.Recommended = current
	}
	if rr.Action != ActionOK {
		rr.ChangePct = (rr.Recommended - current) / current * 100
	}
	return rr
}

// roundCPU rounds up to the nearest 5 millicores.
func roundCPU(c float64) float64 { return math.Ceil(c*200) / 200 }

// roundMem rounds up to the nearest MiB.
func roundMem(b float64) float64 { return math.Ceil(b/(1<<20)) * (1 << 20) }
