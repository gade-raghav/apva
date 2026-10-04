// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/capacity"
	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
	"github.com/gade-raghav/apva/internal/recommender"
)

// fakeAPI serves objects from a map of path -> JSON and records patches.
type fakeAPI struct {
	objects map[string]string
	patches map[string]map[string]any
}

func (f *fakeAPI) Get(_ context.Context, path string, out any) error {
	s, ok := f.objects[path]
	if !ok {
		return kube.ErrNotFound
	}
	return json.Unmarshal([]byte(s), out)
}

func (f *fakeAPI) StrategicMergePatch(_ context.Context, path string, patch any) error {
	if f.patches == nil {
		f.patches = map[string]map[string]any{}
	}
	b, _ := json.Marshal(patch)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.patches[path] = m
	return nil
}

const depPath = "/apis/apps/v1/namespaces/shop/deployments/web"

func deployment(annotations, requests, limits string) string {
	if annotations == "" {
		annotations = "{}"
	}
	if limits == "" {
		limits = "{}"
	}
	return `{"metadata":{"annotations":` + annotations + `},"spec":{"template":{"spec":{"containers":[
		{"name":"app","resources":{"requests":` + requests + `,"limits":` + limits + `}}]}}}}`
}

func rec(cpuCur, cpuRec float64, cpuAct recommender.Action, memCur, memRec float64, memAct recommender.Action) recommender.Recommendation {
	return recommender.Recommendation{
		Workload:   collector.WorkloadKey{Namespace: "shop", Name: "web"},
		Replicas:   2,
		CPU:        recommender.ResourceRec{Current: cpuCur, Recommended: cpuRec, Action: cpuAct},
		Memory:     recommender.ResourceRec{Current: memCur, Recommended: memRec, Action: memAct},
		Confidence: "high",
	}
}

func newActuator(f *fakeAPI) *Actuator {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return &Actuator{K: f, Cfg: DefaultConfig(), Now: func() time.Time { return now }}
}

func requestsIn(t *testing.T, patch map[string]any) map[string]any {
	t.Helper()
	cs := patch["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	return cs[0].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
}

func TestDownsizeIsLimitedPerStep(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{depPath: deployment("", `{"cpu":"1","memory":"512Mi"}`, "")}}
	a := newActuator(f)
	a.Apply(context.Background(), []recommender.Recommendation{
		rec(1, 0.05, recommender.ActionDownsize, 512<<20, 512<<20, recommender.ActionOK),
	})
	h := a.History()
	if len(h) != 1 || h[0].Outcome != OutcomeApplied {
		t.Fatalf("history = %+v", h)
	}
	req := requestsIn(t, f.patches[depPath])
	if req["cpu"] != "500m" { // 1 core can drop at most 50% in one step
		t.Errorf("cpu = %v, want 500m", req["cpu"])
	}
	if _, ok := req["memory"]; ok {
		t.Errorf("memory should be untouched: %v", req)
	}
	ann := f.patches[depPath]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if !strings.Contains(ann[AnnoPrevRequests].(string), `"cpu":"1"`) {
		t.Errorf("previous requests not recorded: %v", ann)
	}
}

func TestUpsizeCappedAtLimit(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{depPath: deployment("", `{"cpu":"20m","memory":"16Mi"}`, `{"cpu":"300m"}`)}}
	a := newActuator(f)
	a.Apply(context.Background(), []recommender.Recommendation{
		rec(0.02, 0.345, recommender.ActionUpsize, 16<<20, 16<<20, recommender.ActionOK),
	})
	req := requestsIn(t, f.patches[depPath])
	if req["cpu"] != "300m" {
		t.Errorf("cpu = %v, want 300m (limit)", req["cpu"])
	}
	if h := a.History(); !strings.Contains(h[0].Reason, "capped") {
		t.Errorf("reason should mention cap: %q", h[0].Reason)
	}
}

func TestGuardrails(t *testing.T) {
	cases := []struct {
		name, obj, hpa, want string
		r                    recommender.Recommendation
		dryRun               bool
	}{
		{name: "opt-out", obj: deployment(`{"apva.io/auto-resize":"off"}`, `{"cpu":"1"}`, ""), want: "opted out"},
		{name: "cooldown", obj: deployment(`{"apva.io/last-resized":"2026-10-01T11:50:00Z"}`, `{"cpu":"1"}`, ""), want: "cooldown"},
		{name: "hpa", obj: deployment("", `{"cpu":"1"}`, ""), hpa: `{"items":[{"spec":{"scaleTargetRef":{"kind":"Deployment","name":"web"}}}]}`, want: "HorizontalPodAutoscaler"},
		{name: "rollout", obj: deployment("", `{"cpu":"2"}`, ""), want: "rollout in progress"},
		{name: "dry-run", obj: deployment("", `{"cpu":"1"}`, ""), want: "CPU 1000m → 500m", dryRun: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAPI{objects: map[string]string{depPath: c.obj}}
			if c.hpa != "" {
				f.objects["/apis/autoscaling/v2/namespaces/shop/horizontalpodautoscalers"] = c.hpa
			}
			a := newActuator(f)
			a.Cfg.DryRun = c.dryRun
			a.Apply(context.Background(), []recommender.Recommendation{
				rec(1, 0.05, recommender.ActionDownsize, 0, 0, recommender.ActionOK),
			})
			h := a.History()
			if len(h) != 1 || !strings.Contains(h[0].Reason, c.want) {
				t.Fatalf("history = %+v, want reason containing %q", h, c.want)
			}
			if len(f.patches) != 0 {
				t.Errorf("should not patch, got %v", f.patches)
			}
		})
	}
}

func TestLowConfidenceAndNoOpSkipped(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{depPath: deployment("", `{"cpu":"1"}`, "")}}
	a := newActuator(f)
	low := rec(1, 0.05, recommender.ActionDownsize, 0, 0, recommender.ActionOK)
	low.Confidence = "medium"
	ok := rec(1, 1, recommender.ActionOK, 0, 0, recommender.ActionOK)
	ok.Workload.Name = "other"
	a.Apply(context.Background(), []recommender.Recommendation{low, ok})
	a.Apply(context.Background(), []recommender.Recommendation{low, ok}) // repeated decision is logged once
	h := a.History()
	if len(h) != 1 || !strings.Contains(h[0].Reason, "confidence medium") {
		t.Fatalf("history = %+v", h)
	}
}

type fakeCapacity struct {
	d   capacity.Decision
	why string
	got []float64
}

func (f *fakeCapacity) Ensure(_ context.Context, _ collector.WorkloadKey, cpu, mem float64, _ int) (capacity.Decision, string) {
	f.got = append(f.got, cpu, mem)
	return f.d, f.why
}

func TestCapacityGatesEveryResize(t *testing.T) {
	up := rec(0.1, 0.3, recommender.ActionUpsize, 64<<20, 64<<20, recommender.ActionOK)
	down := rec(1, 0.05, recommender.ActionDownsize, 512<<20, 512<<20, recommender.ActionOK)
	for _, c := range []struct {
		name    string
		r       recommender.Recommendation
		d       capacity.Decision
		outcome string
		called  bool
	}{
		{"upsize waits for nodes", up, capacity.Waiting, OutcomeWaiting, true},
		{"upsize blocked", up, capacity.Blocked, OutcomeSkipped, true},
		{"upsize fits", up, capacity.Fits, OutcomeApplied, true},
		{"downsize asks too (surge pod)", down, capacity.Blocked, OutcomeSkipped, true},
		{"downsize fits", down, capacity.Fits, OutcomeApplied, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAPI{objects: map[string]string{depPath: deployment("", `{"cpu":"`+map[bool]string{true: "100m", false: "1"}[c.r.CPU.Action == recommender.ActionUpsize]+`","memory":"64Mi"}`, "")}}
			fc := &fakeCapacity{d: c.d, why: "because"}
			a := newActuator(f)
			a.Capacity = fc
			a.Apply(context.Background(), []recommender.Recommendation{c.r})
			h := a.History()
			if len(h) != 1 || h[0].Outcome != c.outcome {
				t.Fatalf("history %+v, want %s", h, c.outcome)
			}
			if (len(fc.got) > 0) != c.called {
				t.Fatalf("capacity called = %v, want %v", len(fc.got) > 0, c.called)
			}
			if c.r.CPU.Action == recommender.ActionUpsize && (fc.got[0] != 0.3 || fc.got[1] != 64<<20) {
				t.Errorf("capacity asked about cpu=%v mem=%v", fc.got[0], fc.got[1])
			}
			if c.outcome != OutcomeApplied && len(f.patches) != 0 {
				t.Errorf("must not patch when capacity says %v", c.d)
			}
		})
	}
}
