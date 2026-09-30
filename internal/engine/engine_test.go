// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/demo"
	"github.com/gade-raghav/apva/internal/engine"
	"github.com/gade-raghav/apva/internal/recommender"
)

// TestDemoClusterEndToEnd runs the full pipeline on the sample cluster and checks the
// headline findings a user would see.
func TestDemoClusterEndToEnd(t *testing.T) {
	c := &collector.Collector{Q: demo.Prometheus{Window: "24h"}, Cfg: collector.Config{
		Window: 24 * time.Hour, RecentWindow: time.Hour, ExcludeSystem: true}}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := engine.Analyse(snap, recommender.DefaultConfig())

	status := map[string]string{}
	for _, n := range res.Graph.Nodes {
		status[n.ID] = n.Status
	}
	want := map[string]string{
		"shop/frontend":     "over",     // 1 core requested, ~0.25 used
		"shop/search":       "hold",     // over-provisioned, but frontend→search traffic is rising
		"shop/payments":     "under",    // using more CPU than requested
		"ml/batch-embedder": "idle-gpu", // GPU requested, ~1% used
		"shop/checkout":     "ok",
	}
	for id, s := range want {
		if status[id] != s {
			t.Errorf("%s status = %q, want %q", id, status[id], s)
		}
	}
	if res.Summary.GPUSavings != 1 || res.Summary.IdleGPUWorkloads != 1 {
		t.Errorf("gpu summary = %+v", res.Summary)
	}
	if res.Summary.CPUSavingsCores <= 0 || res.Summary.MemSavingsBytes <= 0 {
		t.Errorf("expected positive savings: %+v", res.Summary)
	}
	if len(res.Graph.Links) != 9 || len(res.Graph.Nodes) != res.Summary.Workloads {
		t.Errorf("graph nodes=%d links=%d workloads=%d", len(res.Graph.Nodes), len(res.Graph.Links), res.Summary.Workloads)
	}
}
