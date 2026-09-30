// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package engine runs periodic collection and analysis and holds the latest results.
package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/recommender"
)

// Summary is a cluster-level roll-up.
type Summary struct {
	Workloads        int     `json:"workloads"`
	Downsize         int     `json:"downsize"`
	Upsize           int     `json:"upsize"`
	Held             int     `json:"held"`
	IdleGPUWorkloads int     `json:"idleGpuWorkloads"`
	CPUSavingsCores  float64 `json:"cpuSavingsCores"`
	MemSavingsBytes  float64 `json:"memSavingsBytes"`
	GPUSavings       float64 `json:"gpuSavings"`
}

// Node and Link form the service graph returned to the visualiser.
type Node struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"` // ok | over | under | hold | idle-gpu | unknown
	Replicas int     `json:"replicas"`
	HasGPU   bool    `json:"hasGpu"`
	CPUUtil  float64 `json:"cpuUtil"` // p95 / request, 0 if unknown
}

type Link struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	RatePerSec float64 `json:"ratePerSec"`
	Trend      float64 `json:"trend"`
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Links []Link `json:"links"`
}

// Result is one complete analysis.
type Result struct {
	GeneratedAt     time.Time                    `json:"generatedAt"`
	Window          string                       `json:"window"`
	Summary         Summary                      `json:"summary"`
	Recommendations []recommender.Recommendation `json:"recommendations"`
	Graph           Graph                        `json:"graph"`
	Warnings        []string                     `json:"warnings,omitempty"`
}

// Collector is what the engine needs from a data source.
type Collector interface {
	Collect(ctx context.Context) (*collector.Snapshot, error)
}

// Engine is safe for concurrent use.
type Engine struct {
	C   Collector
	Cfg recommender.Config
	Log *slog.Logger

	mu      sync.RWMutex
	last    *Result
	lastErr error
}

// Latest returns the most recent result and error.
func (e *Engine) Latest() (*Result, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.last, e.lastErr
}

// RunOnce collects and analyses once.
func (e *Engine) RunOnce(ctx context.Context) (*Result, error) {
	snap, err := e.C.Collect(ctx)
	if err != nil {
		e.mu.Lock()
		e.lastErr = err
		e.mu.Unlock()
		return nil, err
	}
	res := Analyse(snap, e.Cfg)
	e.mu.Lock()
	e.last, e.lastErr = res, nil
	e.mu.Unlock()
	return res, nil
}

// Run refreshes every interval until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	log := e.Log
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if res, err := e.RunOnce(ctx); err != nil {
			log.Error("analysis failed", "err", err)
		} else {
			log.Info("analysis complete", "workloads", res.Summary.Workloads,
				"downsize", res.Summary.Downsize, "upsize", res.Summary.Upsize,
				"cpuSavingsCores", res.Summary.CPUSavingsCores)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Analyse is the pure analysis step: snapshot in, result out.
func Analyse(snap *collector.Snapshot, cfg recommender.Config) *Result {
	recs := recommender.Recommend(snap, cfg)
	res := &Result{
		GeneratedAt:     snap.CollectedAt,
		Window:          snap.Window.String(),
		Recommendations: recs,
		Warnings:        snap.Warnings,
	}
	s := &res.Summary
	s.Workloads = len(recs)
	nodes := map[string]*Node{}
	for _, r := range recs {
		n := &Node{ID: r.Workload.String(), Replicas: r.Replicas, Status: "ok", HasGPU: r.GPU != nil}
		if r.CPU.Current > 0 {
			n.CPUUtil = r.CPU.P95 / r.CPU.Current
		}
		switch {
		case r.CPU.Action == recommender.ActionUpsize || r.Memory.Action == recommender.ActionUpsize:
			s.Upsize++
			n.Status = "under"
		case r.CPU.Action == recommender.ActionHold || r.Memory.Action == recommender.ActionHold:
			s.Held++
			n.Status = "hold"
		case r.CPU.Action == recommender.ActionDownsize || r.Memory.Action == recommender.ActionDownsize:
			s.Downsize++
			n.Status = "over"
		case r.CPU.Action == recommender.ActionNoData && r.Memory.Action == recommender.ActionNoData:
			n.Status = "unknown"
		}
		if r.GPU != nil && r.GPU.Action == recommender.ActionIdle {
			s.IdleGPUWorkloads++
			n.Status = "idle-gpu"
		}
		if r.CPUSavingsCores > 0 {
			s.CPUSavingsCores += r.CPUSavingsCores
		}
		if r.MemSavingsBytes > 0 {
			s.MemSavingsBytes += r.MemSavingsBytes
		}
		s.GPUSavings += r.GPUSavings
		nodes[n.ID] = n
	}
	for _, e := range snap.Edges {
		src, dst := e.From.String(), e.To.String()
		for _, id := range []string{src, dst} {
			if nodes[id] == nil { // e.g. external callers or workloads without metrics
				nodes[id] = &Node{ID: id, Status: "unknown"}
			}
		}
		res.Graph.Links = append(res.Graph.Links, Link{Source: src, Target: dst, RatePerSec: e.RatePerSec, Trend: e.Trend()})
	}
	for _, r := range recs { // keep deterministic order
		res.Graph.Nodes = append(res.Graph.Nodes, *nodes[r.Workload.String()])
		delete(nodes, r.Workload.String())
	}
	for _, e := range snap.Edges {
		for _, id := range []string{e.From.String(), e.To.String()} {
			if n := nodes[id]; n != nil {
				res.Graph.Nodes = append(res.Graph.Nodes, *n)
				delete(nodes, id)
			}
		}
	}
	return res
}
