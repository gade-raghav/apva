// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package vpa

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
	"github.com/gade-raghav/apva/internal/recommender"
)

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

func (f *fakeAPI) MergePatch(_ context.Context, path string, patch any) error {
	if f.patches == nil {
		f.patches = map[string]map[string]any{}
	}
	b, _ := json.Marshal(patch)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.patches[path] = m
	return nil
}

const (
	listPath   = "/apis/autoscaling.k8s.io/v1/namespaces/shop/verticalpodautoscalers"
	depPath    = "/apis/apps/v1/namespaces/shop/deployments/web"
	statusPath = "/apis/autoscaling.k8s.io/v1/namespaces/shop/verticalpodautoscalers/web-vpa/status"
)

func vpaJSON(recommenders, extra string) string {
	return `{"metadata":{"name":"web-vpa","namespace":"shop"},"spec":{"targetRef":{"apiVersion":"apps/v1","kind":"Deployment","name":"web"},` +
		`"recommenders":` + recommenders + extra + `}}`
}

const twoContainers = `{"spec":{"template":{"spec":{"containers":[
	{"name":"app","resources":{"requests":{"cpu":"300m","memory":"256Mi"}}},
	{"name":"proxy","resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}}}}`

func rec(cpuAct recommender.Action, cpuCur, cpuRec float64) recommender.Recommendation {
	return recommender.Recommendation{
		Workload:   collector.WorkloadKey{Namespace: "shop", Name: "web"},
		Replicas:   2,
		CPU:        recommender.ResourceRec{Current: cpuCur, Recommended: cpuRec, Action: cpuAct},
		Memory:     recommender.ResourceRec{Current: 320 << 20, Recommended: 320 << 20, Action: recommender.ActionOK},
		Confidence: "high",
	}
}

func writer(f *fakeAPI) *Writer {
	return &Writer{K: f, Name: "apva", Namespaces: []string{"shop"}, Tolerance: 0.1,
		Now: func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }}
}

func containerRecs(t *testing.T, f *fakeAPI) map[string]map[string]any {
	t.Helper()
	st := f.patches[statusPath]["status"].(map[string]any)
	out := map[string]map[string]any{}
	for _, c := range st["recommendation"].(map[string]any)["containerRecommendations"].([]any) {
		m := c.(map[string]any)
		out[m["containerName"].(string)] = m
	}
	return out
}

func TestWritesProportionalRecommendation(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{
		listPath: `{"items":[` + vpaJSON(`[{"name":"apva"}]`, "") + `]}`,
		depPath:  twoContainers,
	}}
	w := writer(f)
	w.Write(context.Background(), []recommender.Recommendation{rec(recommender.ActionDownsize, 0.4, 0.2)})

	cr := containerRecs(t, f)
	if got := cr["app"]["target"].(map[string]any)["cpu"]; got != "150m" { // 300/400 of 200m
		t.Errorf("app cpu target = %v, want 150m", got)
	}
	if got := cr["proxy"]["target"].(map[string]any)["cpu"]; got != "50m" {
		t.Errorf("proxy cpu target = %v, want 50m", got)
	}
	if got := cr["app"]["target"].(map[string]any)["memory"]; got != "256Mi" { // memory ok: unchanged
		t.Errorf("app memory target = %v, want 256Mi", got)
	}
	if lo := cr["app"]["lowerBound"].(map[string]any)["cpu"]; lo != "135m" {
		t.Errorf("lowerBound = %v, want 135m", lo)
	}
	conds := f.patches[statusPath]["status"].(map[string]any)["conditions"].([]any)
	if c := conds[0].(map[string]any); c["type"] != "RecommendationProvided" || c["status"] != "True" {
		t.Errorf("conditions = %v", conds)
	}
	if h := w.History(); len(h) != 1 || h[0].Outcome != "written" || w.Managed() != 1 {
		t.Errorf("history %+v managed %d", h, w.Managed())
	}
}

func TestHoldKeepsTheCurrentSize(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{listPath: `{"items":[` + vpaJSON(`[{"name":"apva"}]`, "") + `]}`, depPath: twoContainers}}
	writer(f).Write(context.Background(), []recommender.Recommendation{rec(recommender.ActionHold, 0.4, 0.4)})
	cr := containerRecs(t, f)
	if cr["app"]["target"].(map[string]any)["cpu"] != "300m" || cr["app"]["lowerBound"].(map[string]any)["cpu"] != "300m" {
		t.Fatalf("hold must keep target and lower bound at the current request: %v", cr["app"])
	}
}

func TestContainerPolicies(t *testing.T) {
	policy := `,"resourcePolicy":{"containerPolicies":[
		{"containerName":"proxy","mode":"Off"},
		{"containerName":"*","minAllowed":{"cpu":"200m"},"controlledResources":["cpu"]}]}`
	f := &fakeAPI{objects: map[string]string{listPath: `{"items":[` + vpaJSON(`[{"name":"apva"}]`, policy) + `]}`, depPath: twoContainers}}
	writer(f).Write(context.Background(), []recommender.Recommendation{rec(recommender.ActionDownsize, 0.4, 0.2)})
	cr := containerRecs(t, f)
	if _, ok := cr["proxy"]; ok {
		t.Error("mode Off must skip the container")
	}
	app := cr["app"]
	if app["target"].(map[string]any)["cpu"] != "200m" || app["uncappedTarget"].(map[string]any)["cpu"] != "150m" {
		t.Errorf("minAllowed should clamp target but not uncappedTarget: %v", app)
	}
	if app["lowerBound"].(map[string]any)["cpu"] != "200m" {
		t.Errorf("minAllowed should clamp the lower bound too: %v", app)
	}
	if _, ok := app["target"].(map[string]any)["memory"]; ok {
		t.Error("controlledResources [cpu] must leave memory out")
	}
}

func TestOnlyVPAsSelectingAPVA(t *testing.T) {
	items := []string{
		vpaJSON(`[]`, ""), // default recommender: not ours
		strings.Replace(vpaJSON(`[{"name":"other"}]`, ""), "web-vpa", "other-vpa", 1), // someone else's
	}
	f := &fakeAPI{objects: map[string]string{listPath: `{"items":[` + strings.Join(items, ",") + `]}`, depPath: twoContainers}}
	w := writer(f)
	w.Write(context.Background(), []recommender.Recommendation{rec(recommender.ActionDownsize, 0.4, 0.2)})
	if len(f.patches) != 0 || w.Managed() != 0 {
		t.Fatalf("must not touch VPAs that don't select apva: %v", f.patches)
	}
}

func TestSkipsAndConditions(t *testing.T) {
	cases := []struct {
		name, vpa string
		recs      []recommender.Recommendation
		outcome   string
		why       string
	}{
		{"also selects default", vpaJSON(`[{"name":"apva"},{"name":"default"}]`, ""), []recommender.Recommendation{rec(recommender.ActionDownsize, 0.4, 0.2)}, "skipped", "default recommender"},
		{"unsupported kind", strings.Replace(vpaJSON(`[{"name":"apva"}]`, ""), "Deployment", "DaemonSet", 1), nil, "skipped", "not DaemonSet"},
		{"no metrics", vpaJSON(`[{"name":"apva"}]`, ""), nil, "skipped", "no usage metrics"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAPI{objects: map[string]string{listPath: `{"items":[` + c.vpa + `]}`, depPath: twoContainers}}
			w := writer(f)
			w.Write(context.Background(), c.recs)
			h := w.History()
			if len(h) != 1 || h[0].Outcome != c.outcome || !strings.Contains(h[0].Reason, c.why) {
				t.Fatalf("history %+v", h)
			}
			if p, ok := f.patches[statusPath]; ok {
				if _, has := p["status"].(map[string]any)["recommendation"]; has {
					t.Errorf("no recommendation should be written: %v", p)
				}
			}
		})
	}
}

func TestTargets(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{listPath: `{"items":[` + vpaJSON(`[]`, `,"updatePolicy":{"updateMode":"Off"}`) + `]}`}}
	got, err := Targets(context.Background(), f, "shop")
	if err != nil || got[collector.WorkloadKey{Namespace: "shop", Name: "web"}] != "shop/web-vpa (updateMode Off)" {
		t.Fatalf("targets = %v, %v", got, err)
	}
	none, err := Targets(context.Background(), &fakeAPI{objects: map[string]string{}}, "shop") // no CRD
	if err != nil || len(none) != 0 {
		t.Fatalf("no CRD should mean no targets: %v, %v", none, err)
	}
}
