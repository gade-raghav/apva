// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package actuator applies APVA's right-sizing recommendations to workloads automatically.
//
// It is opt-in (apva --auto-resize) and deliberately conservative. A workload is resized
// only when all of these hold:
//
//   - it is a Deployment or StatefulSet and is not annotated apva.io/auto-resize: "off"
//   - no HorizontalPodAutoscaler targets it (changing requests would change HPA maths)
//   - the recommendation's confidence is at least the configured minimum
//   - its running pods already match its spec (no rollout in progress)
//   - it was not resized within the cooldown period
//
// Downsizing is limited per step (default: at most 50% at once), requests are never
// raised above a container's limit, and the previous requests are stored in the
// apva.io/previous-requests annotation so a change can be reverted.
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/gade-raghav/apva/internal/capacity"
	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
	"github.com/gade-raghav/apva/internal/recommender"
)

// Annotations APVA reads and writes on workloads.
const (
	AnnoOptOut       = "apva.io/auto-resize"         // "off" or "false" disables auto-resize
	AnnoLastResized  = "apva.io/last-resized"        // RFC3339 time of APVA's last change
	AnnoPrevRequests = "apva.io/previous-requests"   // JSON of the requests before that change
	AnnoChange       = "apva.io/last-resize-summary" // human-readable summary of the change
)

// Outcomes recorded in Events.
const (
	OutcomeApplied = "applied"
	OutcomeDryRun  = "dry-run"
	OutcomeSkipped = "skipped"
	OutcomeFailed  = "failed"
	OutcomeWaiting = "waiting" // waiting for node capacity before resizing
)

// Capacity is consulted before a workload's requests grow; implemented by
// *capacity.Manager. cpu and mem are the new per-pod totals.
type Capacity interface {
	Ensure(ctx context.Context, w collector.WorkloadKey, cpu, mem float64, replicas int) (capacity.Decision, string)
}

// Config tunes the actuator.
type Config struct {
	DryRun        bool          // decide and log, but never patch
	MinConfidence string        // high | medium | low
	Cooldown      time.Duration // minimum time between two resizes of one workload
	MaxDownStep   float64       // max fraction a request may shrink in one step, e.g. 0.5
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{MinConfidence: "high", Cooldown: 30 * time.Minute, MaxDownStep: 0.5}
}

// Change is a per-pod request change for one resource.
type Change struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

// Event is one auto-resize decision.
type Event struct {
	Time     time.Time             `json:"time"`
	Workload collector.WorkloadKey `json:"workload"`
	Kind     string                `json:"kind,omitempty"`
	Outcome  string                `json:"outcome"`
	Reason   string                `json:"reason"`
	CPU      *Change               `json:"cpu,omitempty"`
	Memory   *Change               `json:"memory,omitempty"`
}

// API is the subset of the Kubernetes client the actuator uses.
type API interface {
	Get(ctx context.Context, path string, out any) error
	StrategicMergePatch(ctx context.Context, path string, patch any) error
}

// Actuator applies recommendations. It is safe for concurrent use.
type Actuator struct {
	K   API
	Cfg Config
	// Capacity, if set, must agree that larger pods will fit (and may add nodes first).
	Capacity Capacity
	Log      *slog.Logger
	Now      func() time.Time // for tests

	mu      sync.Mutex
	history []Event // newest first
	last    map[collector.WorkloadKey]Event
}

const maxHistory = 100

// History returns recent decisions, newest first. Repeated identical decisions for a
// workload are recorded once.
func (a *Actuator) History() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.history...)
}

// Apply acts on one round of recommendations.
func (a *Actuator) Apply(ctx context.Context, recs []recommender.Recommendation) {
	hpas := map[string]map[string]bool{} // namespace -> "kind/name" targeted by an HPA
	for _, r := range recs {
		if ev, ok := a.decide(ctx, r, hpas); ok {
			a.record(ev)
		}
	}
}

func (a *Actuator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Actuator) record(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		a.last = map[collector.WorkloadKey]Event{}
	}
	prev, seen := a.last[ev.Workload]
	a.last[ev.Workload] = ev
	if seen && prev.Outcome == ev.Outcome && prev.Reason == ev.Reason && ev.Outcome != OutcomeApplied {
		return // same decision as last round; don't flood the log
	}
	a.history = append([]Event{ev}, a.history...)
	if len(a.history) > maxHistory {
		a.history = a.history[:maxHistory]
	}
	if a.Log != nil {
		a.Log.Info("auto-resize", "workload", ev.Workload.String(), "outcome", ev.Outcome, "reason", ev.Reason)
	}
}

var confidenceRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// workload is the part of a Deployment/StatefulSet APVA needs.
type workload struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []container `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type container struct {
	Name      string `json:"name"`
	Resources struct {
		Requests map[string]string `json:"requests"`
		Limits   map[string]string `json:"limits"`
	} `json:"resources"`
}

func (a *Actuator) decide(ctx context.Context, r recommender.Recommendation, hpas map[string]map[string]bool) (Event, bool) {
	ev := Event{Time: a.now().UTC(), Workload: r.Workload, Outcome: OutcomeSkipped}
	cpuWanted := r.CPU.Action == recommender.ActionUpsize || r.CPU.Action == recommender.ActionDownsize
	memWanted := r.Memory.Action == recommender.ActionUpsize || r.Memory.Action == recommender.ActionDownsize
	if !cpuWanted && !memWanted {
		return ev, false // nothing to do: right-sized, held, or no data
	}
	if confidenceRank[r.Confidence] < confidenceRank[a.Cfg.MinConfidence] {
		ev.Reason = fmt.Sprintf("confidence %s is below the required %s", r.Confidence, a.Cfg.MinConfidence)
		return ev, true
	}

	ns, name := r.Workload.Namespace, r.Workload.Name
	var w workload
	for _, kind := range []string{"deployments", "statefulsets"} {
		err := a.K.Get(ctx, fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s", ns, kind, name), &w)
		if err == nil {
			ev.Kind = kind
			break
		}
		if !errors.Is(err, kube.ErrNotFound) {
			ev.Outcome, ev.Reason = OutcomeFailed, "reading workload: "+err.Error()
			return ev, true
		}
	}
	if ev.Kind == "" {
		ev.Reason = "not a Deployment or StatefulSet"
		return ev, true
	}
	if v := w.Metadata.Annotations[AnnoOptOut]; v == "off" || v == "false" {
		ev.Reason = "opted out with " + AnnoOptOut + "=" + v
		return ev, true
	}
	targeted, err := a.hpaTargets(ctx, ns, hpas)
	if err != nil {
		ev.Outcome, ev.Reason = OutcomeFailed, "listing HPAs: "+err.Error()
		return ev, true
	}
	kindName := map[string]string{"deployments": "Deployment", "statefulsets": "StatefulSet"}[ev.Kind] + "/" + name
	if targeted[kindName] {
		ev.Reason = "managed by a HorizontalPodAutoscaler; HPA target utilisation already provides headroom"
		return ev, true
	}
	if ts := w.Metadata.Annotations[AnnoLastResized]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil && a.now().Sub(t) < a.Cfg.Cooldown {
			ev.Reason = fmt.Sprintf("cooldown: resized %s ago (cooldown %s)", a.now().Sub(t).Round(time.Second), a.Cfg.Cooldown)
			return ev, true
		}
	}

	containers := w.Spec.Template.Spec.Containers
	newReq := make([]map[string]string, len(containers))
	for i := range newReq {
		newReq[i] = map[string]string{}
	}
	capped := map[string]bool{}
	plan := func(res string, rr recommender.ResourceRec, format func(float64) string) (*Change, string) {
		spec := make([]float64, len(containers))
		sum := 0.0
		for i, c := range containers {
			if q, ok := c.Resources.Requests[res]; ok {
				v, err := kube.ParseQuantity(q)
				if err != nil {
					return nil, fmt.Sprintf("container %s has unparseable %s request %q", c.Name, res, q)
				}
				spec[i] = v
				sum += v
			}
		}
		if sum <= 0 {
			return nil, "no " + res + " requests in the workload spec"
		}
		// Pods report what they run with; if that differs from the spec a rollout is still
		// in progress (possibly our own), so wait rather than compound changes.
		if math.Abs(sum-rr.Current) > 0.02*math.Max(sum, rr.Current) {
			return nil, "rollout in progress: running pods don't match the spec yet"
		}
		target := rr.Recommended
		if target < sum {
			target = math.Max(target, sum*(1-a.Cfg.MaxDownStep))
		}
		total := 0.0
		changed := false
		for i, c := range containers {
			if spec[i] == 0 {
				continue
			}
			v := spec[i] * target / sum
			if l, ok := c.Resources.Limits[res]; ok {
				if lim, err := kube.ParseQuantity(l); err == nil && lim > 0 && v > lim {
					v, capped[res] = lim, true
				}
			}
			s := format(v)
			parsed, _ := kube.ParseQuantity(s)
			total += parsed
			if old := c.Resources.Requests[res]; s != old && math.Abs(parsed-spec[i]) > 1e-9 {
				newReq[i][res] = s
				changed = true
			}
		}
		if !changed {
			return nil, ""
		}
		return &Change{From: sum, To: total}, ""
	}

	var skipReasons []string
	if cpuWanted {
		ch, why := plan("cpu", r.CPU, kube.FormatCPU)
		ev.CPU = ch
		if why != "" {
			skipReasons = append(skipReasons, why)
		}
	}
	if memWanted {
		ch, why := plan("memory", r.Memory, kube.FormatMemory)
		ev.Memory = ch
		if why != "" {
			skipReasons = append(skipReasons, why)
		}
	}
	if ev.CPU == nil && ev.Memory == nil {
		switch {
		case len(skipReasons) > 0:
			ev.Reason = skipReasons[0]
		case len(capped) > 0:
			ev.Reason = "already at the container limit; raise the limit to let it grow"
		default:
			ev.Reason = "change is too small after rounding"
		}
		return ev, true
	}

	if a.Capacity != nil && ((ev.CPU != nil && ev.CPU.To > ev.CPU.From) || (ev.Memory != nil && ev.Memory.To > ev.Memory.From)) {
		cpu, mem := r.CPU.Current, r.Memory.Current
		if ev.CPU != nil {
			cpu = ev.CPU.To
		}
		if ev.Memory != nil {
			mem = ev.Memory.To
		}
		switch d, why := a.Capacity.Ensure(ctx, r.Workload, cpu, mem, r.Replicas); d {
		case capacity.Waiting:
			ev.Outcome, ev.Reason = OutcomeWaiting, why
			return ev, true
		case capacity.Blocked:
			ev.Reason = why
			return ev, true
		}
	}

	ev.Reason = describe(ev, capped)
	if a.Cfg.DryRun {
		ev.Outcome = OutcomeDryRun
		return ev, true
	}

	prev := map[string]map[string]string{}
	var patchContainers []map[string]any
	for i, c := range containers {
		if len(newReq[i]) == 0 {
			continue
		}
		prev[c.Name] = c.Resources.Requests
		patchContainers = append(patchContainers, map[string]any{
			"name":      c.Name,
			"resources": map[string]any{"requests": newReq[i]},
		})
	}
	prevJSON, _ := json.Marshal(prev)
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnoLastResized:  a.now().UTC().Format(time.RFC3339),
			AnnoPrevRequests: string(prevJSON),
			AnnoChange:       ev.Reason,
		}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": patchContainers}}},
	}
	if err := a.K.StrategicMergePatch(ctx, fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s", ns, ev.Kind, name), patch); err != nil {
		ev.Outcome, ev.Reason = OutcomeFailed, "patching: "+err.Error()
		return ev, true
	}
	ev.Outcome = OutcomeApplied
	return ev, true
}

func describe(ev Event, capped map[string]bool) string {
	s := ""
	if ev.CPU != nil {
		s += fmt.Sprintf("CPU %s → %s", kube.FormatCPU(ev.CPU.From), kube.FormatCPU(ev.CPU.To))
		if capped["cpu"] {
			s += " (capped at limit)"
		}
	}
	if ev.Memory != nil {
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("memory %s → %s", kube.FormatMemory(ev.Memory.From), kube.FormatMemory(ev.Memory.To))
		if capped["memory"] {
			s += " (capped at limit)"
		}
	}
	return s + " per pod"
}

func (a *Actuator) hpaTargets(ctx context.Context, ns string, cache map[string]map[string]bool) (map[string]bool, error) {
	if t, ok := cache[ns]; ok {
		return t, nil
	}
	var list struct {
		Items []struct {
			Spec struct {
				ScaleTargetRef struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"scaleTargetRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := a.K.Get(ctx, "/apis/autoscaling/v2/namespaces/"+ns+"/horizontalpodautoscalers", &list); err != nil && !errors.Is(err, kube.ErrNotFound) {
		return nil, err
	}
	t := map[string]bool{}
	for _, h := range list.Items {
		t[h.Spec.ScaleTargetRef.Kind+"/"+h.Spec.ScaleTargetRef.Name] = true
	}
	cache[ns] = t
	return t, nil
}
