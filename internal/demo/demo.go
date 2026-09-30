// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package demo provides a fake Prometheus with a realistic sample cluster, so APVA can be
// explored and tested without a real cluster (`apva --demo`).
package demo

import (
	"context"
	"strings"

	"github.com/gade-raghav/apva/internal/prom"
)

type pod struct {
	ns, name               string
	cpuReq, cpuP95         float64
	memReqMi, memP95Mi     float64
	gpuReq, gpuAvg, gpuP95 float64
	hasGPU                 bool
}

type flow struct {
	src, dst     string // workload names, namespace "shop"
	rate, recent float64
}

// Pods and flows of an example e-commerce + AI inference cluster.
var pods = []pod{
	{ns: "shop", name: "frontend-7d9f8b6c4d-x2k9p", cpuReq: 1, cpuP95: 0.22, memReqMi: 1024, memP95Mi: 310},
	{ns: "shop", name: "frontend-7d9f8b6c4d-q8w7f", cpuReq: 1, cpuP95: 0.25, memReqMi: 1024, memP95Mi: 295},
	{ns: "shop", name: "checkout-5c8d7f9b6-bcd42", cpuReq: 0.5, cpuP95: 0.46, memReqMi: 512, memP95Mi: 470},
	{ns: "shop", name: "catalog-6f7c8d9b5-mn4pq", cpuReq: 2, cpuP95: 0.3, memReqMi: 2048, memP95Mi: 600},
	{ns: "shop", name: "recommender-8c9b7c6d5-rs2tv", cpuReq: 1, cpuP95: 0.35, memReqMi: 4096, memP95Mi: 1800,
		gpuReq: 1, gpuAvg: 18, gpuP95: 31, hasGPU: true},
	{ns: "shop", name: "payments-4b5c6d7f8-vw5xz", cpuReq: 0.25, cpuP95: 0.41, memReqMi: 256, memP95Mi: 240},
	{ns: "shop", name: "search-9c8b7c6d5-zz2bb", cpuReq: 2, cpuP95: 0.4, memReqMi: 2048, memP95Mi: 700},
	{ns: "ml", name: "batch-embedder-0", cpuReq: 4, cpuP95: 0.1, memReqMi: 16384, memP95Mi: 900,
		gpuReq: 1, gpuAvg: 0.4, gpuP95: 1.2, hasGPU: true},
	{ns: "shop", name: "redis-0", cpuReq: 0.5, cpuP95: 0.12, memReqMi: 1024, memP95Mi: 400},
	{ns: "shop", name: "postgres-0", cpuReq: 1, cpuP95: 0.8, memReqMi: 2048, memP95Mi: 1900},
}

var flows = []flow{
	{"frontend", "checkout", 40, 42},
	{"frontend", "catalog", 120, 110},
	{"frontend", "recommender", 60, 58},
	{"frontend", "search", 30, 55}, // search traffic is ramping up
	{"checkout", "payments", 38, 41},
	{"checkout", "postgres", 50, 52},
	{"catalog", "redis", 300, 280},
	{"catalog", "postgres", 20, 19},
	{"recommender", "redis", 90, 85},
}

// Prometheus is a fake prom.Querier that recognises APVA's queries.
// Window must match the collector's analysis window (e.g. "24h"); flow queries with any
// other range are treated as the "recent" window.
type Prometheus struct{ Window string }

var _ prom.Querier = Prometheus{}

func (d Prometheus) Query(_ context.Context, q string) ([]prom.Sample, error) {
	var out []prom.Sample
	perPod := func(v func(p pod) (float64, bool)) {
		for _, p := range pods {
			if val, ok := v(p); ok {
				out = append(out, prom.Sample{Labels: map[string]string{"namespace": p.ns, "pod": p.name}, Value: val})
			}
		}
	}
	switch {
	case strings.Contains(q, "container_cpu_usage_seconds_total"):
		perPod(func(p pod) (float64, bool) { return p.cpuP95, true })
	case strings.Contains(q, "container_memory_working_set_bytes"):
		perPod(func(p pod) (float64, bool) { return p.memP95Mi * (1 << 20), true })
	case strings.Contains(q, `resource="cpu"`):
		perPod(func(p pod) (float64, bool) { return p.cpuReq, true })
	case strings.Contains(q, `resource="memory"`):
		perPod(func(p pod) (float64, bool) { return p.memReqMi * (1 << 20), true })
	case strings.Contains(q, `resource="nvidia_com_gpu"`):
		perPod(func(p pod) (float64, bool) { return p.gpuReq, p.gpuReq > 0 })
	case strings.Contains(q, "DCGM_FI_DEV_GPU_UTIL") && strings.Contains(q, "quantile_over_time"):
		perPod(func(p pod) (float64, bool) { return p.gpuP95, p.hasGPU })
	case strings.Contains(q, "DCGM_FI_DEV_GPU_UTIL"):
		perPod(func(p pod) (float64, bool) { return p.gpuAvg, p.hasGPU })
	case strings.Contains(q, "hubble_flows_processed_total"):
		w := d.Window
		if w == "" {
			w = "24h"
		}
		recent := !strings.Contains(q, "["+w+"]")
		for _, f := range flows {
			v := f.rate
			if recent {
				v = f.recent
			}
			out = append(out, prom.Sample{Labels: map[string]string{
				"source_namespace": "shop", "source_workload": f.src,
				"destination_namespace": "shop", "destination_workload": f.dst,
			}, Value: v})
		}
	}
	return out, nil
}
