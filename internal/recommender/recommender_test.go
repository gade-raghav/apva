// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package recommender

import (
	"math"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
)

const mi = 1 << 20

func key(n string) collector.WorkloadKey { return collector.WorkloadKey{Namespace: "ns", Name: n} }

func snap(ws []*collector.WorkloadUsage, edges ...collector.Edge) *collector.Snapshot {
	return &collector.Snapshot{Window: 24 * time.Hour, Workloads: ws, Edges: edges}
}

func TestSizeResource(t *testing.T) {
	cfg := DefaultConfig()
	tests := []struct {
		name       string
		cur, p95   float64
		wantAction Action
		wantRec    float64
	}{
		{"over-provisioned", 1, 0.2, ActionDownsize, 0.23},
		{"right-sized within tolerance", 0.25, 0.22, ActionOK, 0.25},
		{"using more than requested", 0.25, 0.4, ActionUpsize, 0.46},
		{"no request", 0, 0.1, ActionSet, 0.115},
		{"no data", 1, 0, ActionNoData, 1},
		{"floor applied", 1, 0.001, ActionDownsize, 0.01},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sizeResource(tt.cur, tt.p95, cfg.MinCPUCores, cfg, roundCPU)
			if got.Action != tt.wantAction {
				t.Errorf("action = %s, want %s", got.Action, tt.wantAction)
			}
			if math.Abs(got.Recommended-tt.wantRec) > 0.0051 {
				t.Errorf("recommended = %v, want ~%v", got.Recommended, tt.wantRec)
			}
		})
	}
}

func TestRoundMem(t *testing.T) {
	if got := roundMem(100*mi + 1); got != 101*mi {
		t.Fatalf("roundMem = %v, want %v", got, 101*mi)
	}
}

func TestDownsizeAndSavings(t *testing.T) {
	w := &collector.WorkloadUsage{Key: key("api"), Replicas: 3,
		CPURequestCores: 1, CPUP95Cores: 0.2, MemRequestBytes: 1024 * mi, MemP95Bytes: 300 * mi}
	r := Recommend(snap([]*collector.WorkloadUsage{w}), DefaultConfig())[0]
	if r.CPU.Action != ActionDownsize || r.Memory.Action != ActionDownsize {
		t.Fatalf("want downsize, got cpu=%s mem=%s", r.CPU.Action, r.Memory.Action)
	}
	if want := (1 - r.CPU.Recommended) * 3; math.Abs(r.CPUSavingsCores-want) > 1e-9 {
		t.Errorf("cpu savings = %v, want %v", r.CPUSavingsCores, want)
	}
	if r.Confidence != "high" {
		t.Errorf("confidence = %s, want high", r.Confidence)
	}
}

func TestTrafficGrowthHoldsDownsize(t *testing.T) {
	w := &collector.WorkloadUsage{Key: key("search"), Replicas: 1,
		CPURequestCores: 2, CPUP95Cores: 0.4, MemRequestBytes: 2048 * mi, MemP95Bytes: 700 * mi}
	e := collector.Edge{From: key("frontend"), To: key("search"), RatePerSec: 30, RecentRatePerSec: 55}
	r := Recommend(snap([]*collector.WorkloadUsage{w}, e), DefaultConfig())[0]
	if r.CPU.Action != ActionHold || r.Memory.Action != ActionHold {
		t.Fatalf("want hold, got cpu=%s mem=%s", r.CPU.Action, r.Memory.Action)
	}
	if r.CPU.Recommended != r.CPU.Current {
		t.Errorf("held recommendation should keep current request")
	}
	if len(r.Upstream) != 1 || r.Upstream[0].From != key("frontend") {
		t.Errorf("upstream not attached: %+v", r.Upstream)
	}
	if r.Confidence != "medium" {
		t.Errorf("confidence = %s, want medium", r.Confidence)
	}
}

func TestTrafficGrowthDoesNotBlockUpsize(t *testing.T) {
	w := &collector.WorkloadUsage{Key: key("pay"), Replicas: 1,
		CPURequestCores: 0.25, CPUP95Cores: 0.4, MemRequestBytes: 256 * mi, MemP95Bytes: 240 * mi}
	e := collector.Edge{From: key("checkout"), To: key("pay"), RatePerSec: 10, RecentRatePerSec: 30}
	r := Recommend(snap([]*collector.WorkloadUsage{w}, e), DefaultConfig())[0]
	if r.CPU.Action != ActionUpsize {
		t.Fatalf("cpu action = %s, want upsize", r.CPU.Action)
	}
}

func TestGPU(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name string
		w    collector.WorkloadUsage
		want Action
	}{
		{"idle", collector.WorkloadUsage{GPURequested: 1, HasGPUMetrics: true, GPUP95UtilPct: 1}, ActionIdle},
		{"share", collector.WorkloadUsage{GPURequested: 1, HasGPUMetrics: true, GPUP95UtilPct: 30}, ActionShare},
		{"busy", collector.WorkloadUsage{GPURequested: 1, HasGPUMetrics: true, GPUP95UtilPct: 90}, ActionOK},
		{"no metrics", collector.WorkloadUsage{GPURequested: 1}, ActionNoData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := c.w
			w.Key, w.Replicas = key("gpu"), 2
			r := Recommend(snap([]*collector.WorkloadUsage{&w}), cfg)[0]
			if r.GPU == nil || r.GPU.Action != c.want {
				t.Fatalf("gpu = %+v, want %s", r.GPU, c.want)
			}
			if c.want == ActionIdle && r.GPUSavings != 2 {
				t.Errorf("gpu savings = %v, want 2", r.GPUSavings)
			}
		})
	}
}

func TestNoGPUWhenNotRequested(t *testing.T) {
	w := &collector.WorkloadUsage{Key: key("web"), Replicas: 1, CPURequestCores: 1, CPUP95Cores: 0.9}
	if r := Recommend(snap([]*collector.WorkloadUsage{w}), DefaultConfig())[0]; r.GPU != nil {
		t.Fatalf("unexpected GPU rec: %+v", r.GPU)
	}
}
