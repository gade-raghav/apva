// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package vpa lets APVA act as a VerticalPodAutoscaler custom recommender
// (AEP-3919; see https://github.com/kubernetes/autoscaler/issues/10395).
//
// A VPA object selects APVA by naming it in spec.recommenders:
//
//	spec:
//	  recommenders: [{name: apva}]
//	  targetRef: {apiVersion: apps/v1, kind: Deployment, name: web}
//	  updatePolicy: {updateMode: InPlaceOrRecreate}
//
// The default VPA recommender then ignores that object, and APVA writes its own
// recommendation (with its traffic-aware hold) into status.recommendation. VPA's updater
// and admission controller apply it as usual, honouring the VPA's resourcePolicy and
// updatePolicy. APVA never patches those workloads itself (see Targets).
package vpa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
	"github.com/gade-raghav/apva/internal/recommender"
)

// DefaultName is the recommender name VPA objects use to select APVA.
const DefaultName = "apva"

// Getter reads from the Kubernetes API.
type Getter interface {
	Get(ctx context.Context, path string, out any) error
}

// API is the subset of the Kubernetes client the Writer uses.
type API interface {
	Getter
	MergePatch(ctx context.Context, path string, patch any) error
}

// Object is the part of a VerticalPodAutoscaler APVA reads.
type Object struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		TargetRef struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
		} `json:"targetRef"`
		UpdatePolicy *struct {
			UpdateMode *string `json:"updateMode"`
		} `json:"updatePolicy"`
		ResourcePolicy *struct {
			ContainerPolicies []ContainerPolicy `json:"containerPolicies"`
		} `json:"resourcePolicy"`
		Recommenders []struct {
			Name string `json:"name"`
		} `json:"recommenders"`
	} `json:"spec"`
}

// ContainerPolicy is a VPA container resource policy.
type ContainerPolicy struct {
	ContainerName       string            `json:"containerName"`
	Mode                *string           `json:"mode"`
	MinAllowed          map[string]string `json:"minAllowed"`
	MaxAllowed          map[string]string `json:"maxAllowed"`
	ControlledResources *[]string         `json:"controlledResources"`
}

func (o *Object) key() string { return o.Metadata.Namespace + "/" + o.Metadata.Name }

// selects reports whether the VPA names the given recommender.
func (o *Object) selects(name string) bool {
	for _, r := range o.Spec.Recommenders {
		if r.Name == name {
			return true
		}
	}
	return false
}

func (o *Object) updateMode() string {
	if o.Spec.UpdatePolicy != nil && o.Spec.UpdatePolicy.UpdateMode != nil {
		return *o.Spec.UpdatePolicy.UpdateMode
	}
	return "Recreate" // VPA's default
}

func (o *Object) policy(container string) *ContainerPolicy {
	var def *ContainerPolicy
	if o.Spec.ResourcePolicy == nil {
		return nil
	}
	for i := range o.Spec.ResourcePolicy.ContainerPolicies {
		p := &o.Spec.ResourcePolicy.ContainerPolicies[i]
		if p.ContainerName == container {
			return p
		}
		if p.ContainerName == "*" {
			def = p
		}
	}
	return def
}

const vpaPath = "/apis/autoscaling.k8s.io/v1"

// List returns the VPA objects in the given namespaces (all namespaces when empty). A
// cluster without the VPA CRD has none.
func List(ctx context.Context, k Getter, namespaces []string) ([]Object, error) {
	paths := []string{vpaPath + "/verticalpodautoscalers"}
	if len(namespaces) > 0 {
		paths = paths[:0]
		for _, ns := range namespaces {
			paths = append(paths, vpaPath+"/namespaces/"+ns+"/verticalpodautoscalers")
		}
	}
	var out []Object
	for _, p := range paths {
		var list struct {
			Items []Object `json:"items"`
		}
		if err := k.Get(ctx, p, &list); err != nil {
			if errors.Is(err, kube.ErrNotFound) {
				continue // CRD not installed
			}
			return nil, err
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

// Event is one decision about a VPA object, shown in the dashboard.
type Event struct {
	Time    time.Time `json:"time"`
	VPA     string    `json:"vpa"` // namespace/name
	Target  string    `json:"target,omitempty"`
	Outcome string    `json:"outcome"` // written | skipped | failed
	Reason  string    `json:"reason"`
}

// Writer writes APVA's recommendations into the VPA objects that select it.
type Writer struct {
	K          API
	Name       string   // recommender name, default "apva"
	Namespaces []string // empty: all
	Tolerance  float64  // half-width of the [lowerBound, upperBound] band, e.g. 0.10
	Log        *slog.Logger
	Now        func() time.Time

	mu      sync.Mutex
	history []Event
	last    map[string]Event
	managed int
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// History returns recent decisions, newest first.
func (w *Writer) History() []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Event(nil), w.history...)
}

// Managed is the number of VPA objects that selected APVA in the last round.
func (w *Writer) Managed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.managed
}

func (w *Writer) record(ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[string]Event{}
	}
	prev, ok := w.last[ev.VPA]
	w.last[ev.VPA] = ev
	if ok && prev.Outcome == ev.Outcome && prev.Reason == ev.Reason {
		return
	}
	w.history = append([]Event{ev}, w.history...)
	if len(w.history) > 100 {
		w.history = w.history[:100]
	}
	if w.Log != nil {
		w.Log.Info("vpa recommender", "vpa", ev.VPA, "outcome", ev.Outcome, "reason", ev.Reason)
	}
}

type container struct {
	Name      string `json:"name"`
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}

// Write publishes one round of recommendations.
func (w *Writer) Write(ctx context.Context, recs []recommender.Recommendation) {
	objs, err := List(ctx, w.K, w.Namespaces)
	if err != nil {
		w.record(Event{Time: w.now().UTC(), VPA: "*", Outcome: "failed", Reason: "listing VerticalPodAutoscalers: " + err.Error()})
		return
	}
	byKey := map[collector.WorkloadKey]recommender.Recommendation{}
	for _, r := range recs {
		byKey[r.Workload] = r
	}
	managed := 0
	for i := range objs {
		o := &objs[i]
		if !o.selects(w.Name) {
			continue
		}
		managed++
		ev := Event{Time: w.now().UTC(), VPA: o.key(), Target: o.Spec.TargetRef.Kind + "/" + o.Spec.TargetRef.Name, Outcome: "skipped"}
		ev.Outcome, ev.Reason = w.writeOne(ctx, o, byKey)
		w.record(ev)
	}
	w.mu.Lock()
	w.managed = managed
	w.mu.Unlock()
}

func (w *Writer) writeOne(ctx context.Context, o *Object, recs map[collector.WorkloadKey]recommender.Recommendation) (string, string) {
	if o.selects("default") {
		return "skipped", "also selects the default recommender; two recommenders would overwrite each other"
	}
	kinds := map[string]string{"Deployment": "deployments", "StatefulSet": "statefulsets"}
	res, ok := kinds[o.Spec.TargetRef.Kind]
	if !ok {
		return w.condition(ctx, o, "ConfigUnsupported", fmt.Sprintf("APVA supports Deployment and StatefulSet targets, not %s", o.Spec.TargetRef.Kind))
	}
	ns := o.Metadata.Namespace
	r, ok := recs[collector.WorkloadKey{Namespace: ns, Name: o.Spec.TargetRef.Name}]
	if !ok || (r.CPU.Action == recommender.ActionNoData && r.Memory.Action == recommender.ActionNoData) {
		return w.condition(ctx, o, "NoMetrics", "no usage metrics for the target yet (or its namespace isn't analysed)")
	}
	var wl struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []container `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := w.K.Get(ctx, fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s", ns, res, o.Spec.TargetRef.Name), &wl); err != nil {
		return "failed", "reading target: " + err.Error()
	}
	rec, reason := Recommendation(o, wl.Spec.Template.Spec.Containers, r, w.Tolerance)
	now := w.now().UTC().Format(time.RFC3339)
	conditions := []map[string]string{
		{"type": "RecommendationProvided", "status": "True", "lastTransitionTime": now, "reason": "APVA", "message": reason},
		{"type": "LowConfidence", "status": boolStatus(r.Confidence == "low"), "lastTransitionTime": now,
			"reason": "APVA", "message": "confidence " + r.Confidence},
	}
	if err := w.K.MergePatch(ctx, w.statusPath(o), map[string]any{"status": map[string]any{
		"recommendation": rec, "conditions": conditions,
	}}); err != nil {
		return "failed", "writing status: " + err.Error()
	}
	return "written", reason
}

func boolStatus(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func (w *Writer) statusPath(o *Object) string {
	return fmt.Sprintf("%s/namespaces/%s/verticalpodautoscalers/%s/status", vpaPath, o.Metadata.Namespace, o.Metadata.Name)
}

func (w *Writer) condition(ctx context.Context, o *Object, reason, msg string) (string, string) {
	err := w.K.MergePatch(ctx, w.statusPath(o), map[string]any{"status": map[string]any{
		"conditions": []map[string]string{{"type": "RecommendationProvided", "status": "False",
			"lastTransitionTime": w.now().UTC().Format(time.RFC3339), "reason": reason, "message": msg}},
	}})
	if err != nil {
		return "failed", "writing status: " + err.Error()
	}
	return "skipped", msg
}

// Recommendation builds a VPA status.recommendation from an APVA recommendation. APVA
// sizes whole pods; each container gets its share in proportion to its current request.
// A "hold" (callers' traffic rising) or "ok" keeps the current request as the target. The
// VPA's container policies apply: mode Off skips a container, controlledResources limits
// the resources, minAllowed/maxAllowed clamp the target and bounds (uncappedTarget keeps
// APVA's number).
func Recommendation(o *Object, cs []container, r recommender.Recommendation, tol float64) (map[string]any, string) {
	type res struct {
		name   string
		rr     recommender.ResourceRec
		format func(float64) string
	}
	all := []res{{"cpu", r.CPU, kube.FormatCPU}, {"memory", r.Memory, kube.FormatMemory}}
	sums := map[string]float64{}
	for _, c := range cs {
		for _, x := range all {
			if q, ok := c.Resources.Requests[x.name]; ok {
				v, _ := kube.ParseQuantity(q)
				sums[x.name] += v
			}
		}
	}
	var out []map[string]any
	for _, c := range cs {
		p := o.policy(c.Name)
		if p != nil && p.Mode != nil && *p.Mode == "Off" {
			continue
		}
		target, lower, upper, uncapped := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
		for _, x := range all {
			if p != nil && p.ControlledResources != nil && !contains(*p.ControlledResources, x.name) {
				continue
			}
			cur, ok := c.Resources.Requests[x.name]
			if !ok || sums[x.name] <= 0 || x.rr.Action == recommender.ActionNoData {
				continue
			}
			curV, _ := kube.ParseQuantity(cur)
			v := curV // ok, hold: keep what it has
			if x.rr.Action == recommender.ActionUpsize || x.rr.Action == recommender.ActionDownsize || x.rr.Action == recommender.ActionSet {
				v = x.rr.Recommended * curV / sums[x.name]
			}
			uncapped[x.name] = x.format(v)
			lo, hi := v*(1-tol), v*(1+tol)
			if x.rr.Action == recommender.ActionHold {
				lo = v // never below the current size while callers ramp up
			}
			clamp := func(f float64) float64 { return f }
			if p != nil {
				minV, maxV := 0.0, math.Inf(1)
				if q, ok := p.MinAllowed[x.name]; ok {
					if m, err := kube.ParseQuantity(q); err == nil {
						minV = m
					}
				}
				if q, ok := p.MaxAllowed[x.name]; ok {
					if m, err := kube.ParseQuantity(q); err == nil {
						maxV = m
					}
				}
				clamp = func(f float64) float64 { return math.Min(math.Max(f, minV), maxV) }
			}
			// Like the default recommender, the container policy caps the target and both
			// bounds; uncappedTarget keeps APVA's own number.
			target[x.name] = x.format(clamp(v))
			lower[x.name] = x.format(clamp(lo))
			upper[x.name] = x.format(clamp(hi))
		}
		if len(target) == 0 {
			continue
		}
		out = append(out, map[string]any{
			"containerName": c.Name, "target": target, "lowerBound": lower, "upperBound": upper, "uncappedTarget": uncapped,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["containerName"].(string) < out[j]["containerName"].(string) })
	reason := fmt.Sprintf("CPU %s, memory %s (p95 + headroom; confidence %s)", r.CPU.Action, r.Memory.Action, r.Confidence)
	if r.CPU.Action == recommender.ActionHold || r.Memory.Action == recommender.ActionHold {
		reason = "holding the current size: callers' traffic is rising (" + r.Confidence + " confidence)"
	}
	return map[string]any{"containerRecommendations": out}, reason
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Targets maps each workload a VPA object targets (any recommender, any mode) to that
// VPA's namespace/name, for the given namespace. APVA's own auto-resize leaves these
// alone: the VPA owns their requests.
func Targets(ctx context.Context, k Getter, ns string) (map[collector.WorkloadKey]string, error) {
	objs, err := List(ctx, k, []string{ns})
	if err != nil {
		return nil, err
	}
	out := map[collector.WorkloadKey]string{}
	for i := range objs {
		o := &objs[i]
		if o.Spec.TargetRef.Kind == "Deployment" || o.Spec.TargetRef.Kind == "StatefulSet" {
			out[collector.WorkloadKey{Namespace: ns, Name: o.Spec.TargetRef.Name}] = o.key() + " (updateMode " + o.updateMode() + ")"
		}
	}
	return out, nil
}
