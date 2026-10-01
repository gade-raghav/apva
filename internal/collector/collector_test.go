// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package collector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/demo"
	"github.com/gade-raghav/apva/internal/prom"
)

func TestWorkloadFromPod(t *testing.T) {
	cases := map[string]string{
		"frontend-7d9f8b6c4d-x2k9p": "frontend",
		"my-api-5c8d7f9b6-bcd42":    "my-api",
		"postgres-0":                "postgres",
		"kafka-broker-12":           "kafka-broker",
		"node-exporter-7xk2p":       "node-exporter",
		"web-proxy":                 "web-proxy",
		"standalone":                "standalone",
	}
	for pod, want := range cases {
		if got := collector.WorkloadFromPod(pod); got != want {
			t.Errorf("WorkloadFromPod(%q) = %q, want %q", pod, got, want)
		}
	}
}

func TestQueriesUseWindow(t *testing.T) {
	q := collector.Queries(6*time.Hour, 30*time.Minute, "")
	if !strings.Contains(q["mem_p95"], "[6h]") || !strings.Contains(q["flows_now"], "[30m]") {
		t.Fatalf("windows not applied: %v", q)
	}
}

func newDemoCollector() *collector.Collector {
	return &collector.Collector{Q: demo.Prometheus{Window: "24h"}, Cfg: collector.Config{
		Window: 24 * time.Hour, RecentWindow: time.Hour, ExcludeSystem: true}}
}

func TestCollectDemo(t *testing.T) {
	s, err := newDemoCollector().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*collector.WorkloadUsage{}
	for _, w := range s.Workloads {
		byName[w.Key.String()] = w
	}
	fe := byName["shop/frontend"]
	if fe == nil || fe.Replicas != 2 {
		t.Fatalf("frontend should aggregate 2 pods: %+v", fe)
	}
	if fe.CPUP95Cores != 0.25 { // max across pods
		t.Errorf("frontend cpu p95 = %v, want 0.25", fe.CPUP95Cores)
	}
	if e := byName["ml/batch-embedder"]; e == nil || !e.HasGPUMetrics || e.GPURequested != 1 {
		t.Errorf("batch-embedder GPU not collected: %+v", e)
	}
	if len(s.Edges) != 9 {
		t.Errorf("edges = %d, want 9", len(s.Edges))
	}
	for _, e := range s.Edges {
		if e.To.Name == "search" && e.Trend() < 1.5 {
			t.Errorf("search trend = %v, want > 1.5", e.Trend())
		}
	}
}

type failing struct {
	prom.Querier
	match string
}

func (f failing) Query(ctx context.Context, q string) ([]prom.Sample, error) {
	if strings.Contains(q, f.match) {
		return nil, errors.New("boom")
	}
	return f.Querier.Query(ctx, q)
}

func TestOptionalSourcesDegradeGracefully(t *testing.T) {
	c := newDemoCollector()
	c.Q = failing{Querier: c.Q, match: "hubble"}
	s, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("hubble failure should not be fatal: %v", err)
	}
	if len(s.Edges) != 0 || len(s.Warnings) == 0 {
		t.Errorf("expected no edges and a warning, got %d edges, warnings %v", len(s.Edges), s.Warnings)
	}
}

func TestRequiredSourceFailureIsFatal(t *testing.T) {
	c := newDemoCollector()
	c.Q = failing{Querier: c.Q, match: "container_cpu_usage_seconds_total"}
	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("expected error when CPU metrics fail")
	}
}

// replaced adds a pod that a rollout replaced: it still has usage in the window but is no
// longer running, so it must not count as a replica.
type replaced struct{ prom.Querier }

func (r replaced) Query(ctx context.Context, q string) ([]prom.Sample, error) {
	s, err := r.Querier.Query(ctx, q)
	old := map[string]string{"namespace": "shop", "pod": "frontend-6b8c7d5f4c-bx7zq"}
	switch {
	case strings.Contains(q, "kube_pod_status_phase"):
		s = append(s, prom.Sample{Labels: map[string]string{"namespace": "shop", "pod": "frontend-7d9f8b6c4d-x2k9p"}, Value: 1},
			prom.Sample{Labels: map[string]string{"namespace": "shop", "pod": "frontend-7d9f8b6c4d-q8w7f"}, Value: 1})
	case strings.Contains(q, "container_cpu_usage_seconds_total"):
		s = append(s, prom.Sample{Labels: old, Value: 0.9})
	}
	return s, err
}

func TestReplacedPodsInformUsageButAreNotReplicas(t *testing.T) {
	c := newDemoCollector()
	c.Q = replaced{c.Q}
	s, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range s.Workloads {
		if w.Key.String() == "shop/frontend" {
			if w.Replicas != 2 || w.CPUP95Cores != 0.9 {
				t.Errorf("frontend replicas=%d p95=%v, want 2 and 0.9", w.Replicas, w.CPUP95Cores)
			}
			return
		}
	}
	t.Fatal("frontend missing")
}
