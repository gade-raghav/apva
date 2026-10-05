// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gade-raghav/apva/internal/capacity"
	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
	"github.com/gade-raghav/apva/internal/recommender"
)

const (
	podsPath = "/api/v1/namespaces/shop/pods?labelSelector=app%3Dweb"
	resize1  = "/api/v1/namespaces/shop/pods/web-1/resize"
	resize2  = "/api/v1/namespaces/shop/pods/web-2/resize"
)

func inPlaceDeployment(annotations, requests, limits string) string {
	if annotations == "" {
		annotations = "{}"
	}
	if limits == "" {
		limits = "{}"
	}
	return `{"metadata":{"annotations":` + annotations + `},"spec":{"selector":{"matchLabels":{"app":"web"}},
		"template":{"spec":{"containers":[{"name":"app","resources":{"requests":` + requests + `,"limits":` + limits + `}}]}}}}`
}

func podJSON(name, requests, limits string) string {
	if limits == "" {
		limits = "{}"
	}
	return `{"metadata":{"name":"` + name + `"},"spec":{"nodeName":"n1","containers":[{"name":"app","resources":{"requests":` +
		requests + `,"limits":` + limits + `}}]},"status":{"phase":"Running"}}`
}

func cluster(minor, dep, pods string) *fakeAPI {
	return &fakeAPI{objects: map[string]string{
		"/version": `{"major":"1","minor":"` + minor + `"}`,
		depPath:    dep,
		podsPath:   `{"items":[` + pods + `]}`,
	}}
}

type fitter struct {
	fakeCapacity
	fits bool
}

func (f *fitter) FitsInPlace(context.Context, collector.WorkloadKey, float64, float64) (bool, string) {
	if f.fits {
		return true, ""
	}
	return false, "node n1 has no room to grow its pod(s) in place"
}

func upsize() recommender.Recommendation {
	return rec(0.1, 0.3, recommender.ActionUpsize, 64<<20, 64<<20, recommender.ActionOK)
}

func TestInPlaceResizeLeavesTheTemplateAlone(t *testing.T) {
	f := cluster("35", inPlaceDeployment("", `{"cpu":"100m","memory":"64Mi"}`, `{"cpu":"1"}`),
		podJSON("web-1", `{"cpu":"100m","memory":"64Mi"}`, `{"cpu":"1"}`)+","+podJSON("web-2", `{"cpu":"100m","memory":"64Mi"}`, `{"cpu":"1"}`))
	a := newActuator(f)
	a.Capacity = &fitter{fits: true}
	a.Apply(context.Background(), []recommender.Recommendation{upsize()})

	h := a.History()
	if len(h) != 1 || h[0].Outcome != OutcomeApplied || h[0].Method != ModeInPlace || !strings.Contains(h[0].Reason, "in place") {
		t.Fatalf("history %+v", h)
	}
	for _, p := range []string{resize1, resize2} {
		req := f.patches[p]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
		if req["cpu"] != "300m" || req["memory"] != nil {
			t.Errorf("%s: requests %v, want only cpu 300m", p, req)
		}
	}
	dep := f.patches[depPath]
	if _, ok := dep["spec"]; ok {
		t.Fatalf("the pod template must not change (that would roll the pods): %v", dep)
	}
	ann := dep["metadata"].(map[string]any)["annotations"].(map[string]any)
	if !strings.Contains(ann[AnnoInPlace].(string), `"pods":{"app":{"cpu":"300m","memory":"64Mi"}}`) {
		t.Errorf("in-place state not recorded: %v", ann[AnnoInPlace])
	}
}

func TestInPlaceFallsBackToRollout(t *testing.T) {
	cases := []struct {
		name, minor, limits, why string
		fits                     bool
		fail                     error
	}{
		{"old cluster", "32", `{"cpu":"1"}`, "", true, nil},
		{"guaranteed QoS", "35", "guaranteed", "Guaranteed", true, nil},
		{"no room on the node", "35", `{"cpu":"1"}`, "no room to grow", false, nil},
		{"resize unsupported", "35", `{"cpu":"1"}`, "doesn't support in-place", true,
			&kube.StatusError{Code: http.StatusUnprocessableEntity, Message: "spec: Forbidden: Pod running on node without support for resize"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, r := `{"cpu":"100m","memory":"64Mi"}`, upsize()
			if c.limits == "guaranteed" { // requests = limits; shrink it
				req, c.limits = `{"cpu":"1","memory":"64Mi"}`, `{"cpu":"1","memory":"64Mi"}`
				r = rec(1, 0.5, recommender.ActionDownsize, 64<<20, 64<<20, recommender.ActionOK)
			}
			f := cluster(c.minor, inPlaceDeployment("", req, c.limits), podJSON("web-1", req, c.limits))
			if c.fail != nil {
				f.fail = map[string]error{resize1: c.fail}
			}
			a := newActuator(f)
			a.Capacity = &fitter{fakeCapacity: fakeCapacity{d: capacity.Fits}, fits: c.fits}
			a.Apply(context.Background(), []recommender.Recommendation{r})
			h := a.History()
			if len(h) != 1 || h[0].Outcome != OutcomeApplied || h[0].Method != ModeRollout || !strings.Contains(h[0].Reason, c.why) {
				t.Fatalf("history %+v", h)
			}
			if _, ok := f.patches[depPath]["spec"]; !ok {
				t.Fatalf("expected a template patch (rolling update): %v", f.patches)
			}
		})
	}
}

func TestStrictInPlaceNeverRolls(t *testing.T) {
	f := cluster("35", inPlaceDeployment("", `{"cpu":"1","memory":"64Mi"}`, `{"cpu":"1","memory":"64Mi"}`),
		podJSON("web-1", `{"cpu":"1","memory":"64Mi"}`, `{"cpu":"1","memory":"64Mi"}`))
	a := newActuator(f)
	a.Cfg.ResizeMode = ModeInPlace
	a.Apply(context.Background(), []recommender.Recommendation{rec(1, 0.5, recommender.ActionDownsize, 64<<20, 64<<20, recommender.ActionOK)})
	if h := a.History(); len(h) != 1 || h[0].Outcome != OutcomeSkipped || !strings.Contains(h[0].Reason, "can't resize in place") || len(f.patches) != 0 {
		t.Fatalf("history %+v patches %v", h, f.patches)
	}
}

func TestConvergeBringsNewPodsToTheInPlaceSize(t *testing.T) {
	state := `{"apva.io/in-place-requests":"{\"template\":{\"app\":{\"cpu\":\"100m\",\"memory\":\"64Mi\"}},\"pods\":{\"app\":{\"cpu\":\"300m\",\"memory\":\"64Mi\"}}}"}`
	f := cluster("35", inPlaceDeployment(state, `{"cpu":"100m","memory":"64Mi"}`, `{"cpu":"1"}`),
		podJSON("web-1", `{"cpu":"300m","memory":"64Mi"}`, `{"cpu":"1"}`)+","+podJSON("web-2", `{"cpu":"100m","memory":"64Mi"}`, `{"cpu":"1"}`))
	a := newActuator(f)
	ok := rec(0.3, 0.3, recommender.ActionOK, 64<<20, 64<<20, recommender.ActionOK)
	a.track(ok.Workload) // resized in place earlier
	a.Apply(context.Background(), []recommender.Recommendation{ok})
	h := a.History()
	if len(h) != 1 || h[0].Outcome != OutcomeApplied || !strings.Contains(h[0].Reason, "1 new pod(s)") {
		t.Fatalf("history %+v", h)
	}
	if _, ok := f.patches[resize1]; ok {
		t.Error("web-1 already has the in-place size")
	}
	if _, ok := f.patches[resize2]; !ok {
		t.Error("web-2 (created from the template) should be resized")
	}

	// The template changed since (someone deployed new requests): the template wins.
	f2 := cluster("35", inPlaceDeployment(state, `{"cpu":"200m","memory":"64Mi"}`, `{"cpu":"1"}`),
		podJSON("web-1", `{"cpu":"200m","memory":"64Mi"}`, `{"cpu":"1"}`))
	a2 := newActuator(f2)
	a2.track(ok.Workload)
	a2.Apply(context.Background(), []recommender.Recommendation{ok})
	if len(f2.patches) != 0 {
		t.Fatalf("a stale in-place state must be ignored: %v", f2.patches)
	}
}
