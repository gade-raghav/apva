// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
	"github.com/gade-raghav/apva/internal/recommender"
)

// In-place pod resize (KEP-1287; beta and on by default in Kubernetes 1.33, GA in 1.35)
// changes a running pod's requests through the pods/resize subresource, usually without
// restarting its containers. APVA uses it when it can and falls back to a rolling update
// when it can't (like the VPA's InPlaceOrRecreate mode).
//
// A pod resized in place no longer matches its workload's pod template, and changing the
// template would roll the pods. So APVA records the size it chose in an annotation on the
// workload and brings any new pod (scale-out, eviction, restart) to that size as well. If
// someone changes the template's requests, the annotation is stale and is ignored: the
// template wins.

// Resize modes.
const (
	ModeAuto    = "auto"     // in place when the cluster supports it, otherwise rollout
	ModeInPlace = "in-place" // only in place; skip what can't be done in place
	ModeRollout = "rollout"  // always a rolling update of the pod template
)

// AnnoInPlace records an in-place resize on the workload:
// {"template": <template requests when resized>, "pods": <requests the pods should run with>}.
const AnnoInPlace = "apva.io/in-place-requests"

type requests map[string]map[string]string // container -> resource -> quantity

type inPlaceState struct {
	Template requests `json:"template"`
	Pods     requests `json:"pods"`
}

func templateRequests(cs []container) requests {
	out := requests{}
	for _, c := range cs {
		m := map[string]string{}
		for _, r := range []string{"cpu", "memory"} {
			if v, ok := c.Resources.Requests[r]; ok {
				m[r] = v
			}
		}
		out[c.Name] = m
	}
	return out
}

func sameRequests(a, b requests) bool {
	x, _ := json.Marshal(a) // map keys are marshalled sorted
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// inPlace returns the recorded in-place state, or nil if there is none or the template has
// changed since (then the template wins).
func inPlace(w *workload) *inPlaceState {
	raw := w.Metadata.Annotations[AnnoInPlace]
	if raw == "" {
		return nil
	}
	var st inPlaceState
	if json.Unmarshal([]byte(raw), &st) != nil || !sameRequests(st.Template, templateRequests(w.Spec.Template.Spec.Containers)) {
		return nil
	}
	return &st
}

// effective is the template with the in-place sizes applied: what the pods run with.
func effective(w *workload, st *inPlaceState) []container {
	out := make([]container, len(w.Spec.Template.Spec.Containers))
	for i, c := range w.Spec.Template.Spec.Containers {
		cc := c
		cc.Resources.Requests = map[string]string{}
		for k, v := range c.Resources.Requests {
			cc.Resources.Requests[k] = v
		}
		if st != nil {
			for k, v := range st.Pods[c.Name] {
				cc.Resources.Requests[k] = v
			}
		}
		out[i] = cc
	}
	return out
}

// pod is the part of a pod the in-place path needs.
type pod struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName   string      `json:"nodeName"`
		Containers []container `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// guaranteed reports whether the pod is in the Guaranteed QoS class; changing only its
// requests would change the class, which the API server refuses for a resize.
func (p *pod) guaranteed() bool {
	for _, c := range p.Spec.Containers {
		for _, r := range []string{"cpu", "memory"} {
			l, ok := c.Resources.Limits[r]
			if !ok {
				return false
			}
			if q, ok := c.Resources.Requests[r]; ok && q != l {
				lq, _ := kube.ParseQuantity(l)
				rq, _ := kube.ParseQuantity(q)
				if lq != rq {
					return false
				}
			}
		}
	}
	return len(p.Spec.Containers) > 0
}

// needs returns the container requests p must change to match want (only differences).
func (p *pod) needs(want requests) []map[string]any {
	var out []map[string]any
	for _, c := range p.Spec.Containers {
		diff := map[string]string{}
		for r, v := range want[c.Name] {
			cur, ok := c.Resources.Requests[r]
			if ok {
				a, _ := kube.ParseQuantity(cur)
				b, _ := kube.ParseQuantity(v)
				if a == b {
					continue
				}
			}
			diff[r] = v
		}
		if len(diff) > 0 {
			out = append(out, map[string]any{"name": c.Name, "resources": map[string]any{"requests": diff}})
		}
	}
	return out
}

// workloadPods lists the workload's live pods by its selector's matchLabels.
func (a *Actuator) workloadPods(ctx context.Context, ns string, w *workload) ([]pod, error) {
	if len(w.Spec.Selector.MatchExpressions) > 0 || len(w.Spec.Selector.MatchLabels) == 0 {
		return nil, fmt.Errorf("selector uses matchExpressions")
	}
	keys := make([]string, 0, len(w.Spec.Selector.MatchLabels))
	for k := range w.Spec.Selector.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sel []string
	for _, k := range keys {
		sel = append(sel, k+"="+w.Spec.Selector.MatchLabels[k])
	}
	var list struct {
		Items []pod `json:"items"`
	}
	if err := a.K.Get(ctx, "/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape(strings.Join(sel, ",")), &list); err != nil {
		return nil, err
	}
	var out []pod
	for _, p := range list.Items {
		if p.Metadata.DeletionTimestamp == nil && p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed" {
			out = append(out, p)
		}
	}
	return out, nil
}

// inPlaceOn reports whether this round should try in-place resizes.
func (a *Actuator) inPlaceOn(ctx context.Context) bool {
	switch a.Cfg.ResizeMode {
	case ModeRollout:
		return false
	case ModeInPlace:
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inPlaceKnown {
		return a.inPlaceOK
	}
	var v struct {
		Major, Minor string
	}
	if err := a.K.Get(ctx, "/version", &v); err != nil {
		return false // try again next round
	}
	minor, _ := strconv.Atoi(strings.TrimRight(v.Minor, "+"))
	a.inPlaceKnown, a.inPlaceOK = true, v.Major == "1" && minor >= 33
	return a.inPlaceOK
}

func (a *Actuator) disableInPlace(why string) {
	if a.Cfg.ResizeMode != ModeAuto {
		return
	}
	a.mu.Lock()
	a.inPlaceKnown, a.inPlaceOK = true, false
	a.mu.Unlock()
	if a.Log != nil {
		a.Log.Warn("in-place resize unavailable; using rolling updates", "why", why)
	}
}

func unsupported(err error) bool {
	return kube.IsStatus(err, http.StatusNotFound) || kube.IsStatus(err, http.StatusMethodNotAllowed) ||
		(kube.IsStatus(err, http.StatusUnprocessableEntity) && strings.Contains(err.Error(), "without support for resize"))
}

// InPlaceFitter is implemented by *capacity.Manager: can every pod of w grow to the given
// per-pod totals on the node it already runs on?
type InPlaceFitter interface {
	FitsInPlace(ctx context.Context, w collector.WorkloadKey, cpu, mem float64) (bool, string)
}

// converge brings pods that don't run with the recorded in-place size (created from the
// template after the resize) to that size. It reports whether it did something.
func (a *Actuator) converge(ctx context.Context, ev Event, w *workload, st *inPlaceState) (Event, bool) {
	pods, err := a.workloadPods(ctx, ev.Workload.Namespace, w)
	if err != nil {
		return ev, false
	}
	fixed := 0
	for _, p := range pods {
		change := p.needs(st.Pods)
		if len(change) == 0 || p.guaranteed() {
			continue
		}
		if a.Cfg.DryRun {
			fixed++
			continue
		}
		if err := a.resizePod(ctx, ev.Workload.Namespace, p.Metadata.Name, change); err != nil {
			if unsupported(err) {
				a.disableInPlace(err.Error())
			}
			ev.Outcome, ev.Reason = OutcomeFailed, fmt.Sprintf("bringing pod %s to its in-place size: %v", p.Metadata.Name, err)
			return ev, true
		}
		fixed++
	}
	if fixed == 0 {
		return ev, false
	}
	ev.Outcome, ev.Method = OutcomeApplied, ModeInPlace
	if a.Cfg.DryRun {
		ev.Outcome = OutcomeDryRun
	}
	ev.Reason = fmt.Sprintf("%d new pod(s) brought to the in-place size APVA chose (pods created from the template start at its older size)", fixed)
	return ev, true
}

func (a *Actuator) resizePod(ctx context.Context, ns, name string, containers []map[string]any) error {
	return a.K.StrategicMergePatch(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/resize", ns, name),
		map[string]any{"spec": map[string]any{"containers": containers}})
}

// applyInPlace resizes the workload's pods in place. handled=false means "use a rolling
// update instead", with why saying why.
func (a *Actuator) applyInPlace(ctx context.Context, r recommender.Recommendation, w *workload, eff []container,
	newReq []map[string]string, ev Event) (out Event, handled bool, why string) {
	ns := r.Workload.Namespace
	pods, err := a.workloadPods(ctx, ns, w)
	if err != nil {
		return ev, false, "can't list its pods (" + err.Error() + ")"
	}
	if len(pods) == 0 {
		return ev, false, "no pods to resize in place"
	}
	for _, p := range pods {
		if p.guaranteed() {
			return ev, false, "its pods are Guaranteed QoS (requests = limits), which an in-place change of requests would break"
		}
	}
	// The pods' new requests: the effective requests with this round's changes.
	want := requests{}
	for i, c := range eff {
		m := map[string]string{}
		for _, res := range []string{"cpu", "memory"} {
			if v, ok := c.Resources.Requests[res]; ok {
				m[res] = v
			}
			if v, ok := newReq[i][res]; ok {
				m[res] = v
			}
		}
		want[c.Name] = m
	}
	if f, ok := a.Capacity.(InPlaceFitter); ok && a.Capacity != nil {
		cpu, mem := r.CPU.Current, r.Memory.Current
		if ev.CPU != nil {
			cpu = ev.CPU.To
		}
		if ev.Memory != nil {
			mem = ev.Memory.To
		}
		if fits, reason := f.FitsInPlace(ctx, r.Workload, cpu, mem); !fits {
			return ev, false, reason
		}
	}
	ev.Method = ModeInPlace
	ev.Reason += ", in place (no pod restart)"
	if a.Cfg.DryRun {
		ev.Outcome = OutcomeDryRun
		return ev, true, ""
	}
	for i, p := range pods {
		change := p.needs(want)
		if len(change) == 0 {
			continue
		}
		if err := a.resizePod(ctx, ns, p.Metadata.Name, change); err != nil {
			if unsupported(err) && i == 0 {
				a.disableInPlace(err.Error())
				return ev, false, "the cluster doesn't support in-place resize (" + err.Error() + ")"
			}
			ev.Outcome, ev.Reason = OutcomeFailed, fmt.Sprintf("in-place resize of pod %s: %v", p.Metadata.Name, err)
			return ev, true, ""
		}
	}
	prev, _ := json.Marshal(templateRequestsOf(eff))
	st, _ := json.Marshal(inPlaceState{Template: templateRequests(w.Spec.Template.Spec.Containers), Pods: want})
	// Annotations only: the pod template is untouched, so nothing rolls.
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		AnnoLastResized:  a.now().UTC().Format(time.RFC3339),
		AnnoPrevRequests: string(prev),
		AnnoChange:       ev.Reason,
		AnnoInPlace:      string(st),
	}}}
	if err := a.K.StrategicMergePatch(ctx, fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s", ns, ev.Kind, r.Workload.Name), patch); err != nil {
		ev.Outcome, ev.Reason = OutcomeFailed, "pods resized in place, but recording it on the workload failed: "+err.Error()
		return ev, true, ""
	}
	ev.Outcome = OutcomeApplied
	return ev, true, ""
}

func templateRequestsOf(cs []container) requests { return templateRequests(cs) }
