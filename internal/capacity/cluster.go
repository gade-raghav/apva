// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package capacity makes APVA's resizing node-aware.
//
// Before a workload is resized upwards it checks that the new pods will actually fit on
// the nodes the workload runs on. On Amazon EKS it can grow a managed node group first,
// wait for the nodes to be Ready, and only then resize the pods. After pods are resized
// downwards it can consolidate: drain an under-used node (respecting PodDisruptionBudgets)
// and terminate exactly that instance, shrinking the node group by one.
package capacity

import (
	"context"

	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/kube"
)

// Resource names used in Resources.
const (
	CPU    = "cpu"            // cores
	Memory = "memory"         // bytes
	GPU    = "nvidia.com/gpu" // devices
	Pods   = "pods"           // pod slots
)

// KarpenterLabel is set by Karpenter on the nodes it provisions.
const KarpenterLabel = "karpenter.sh/nodepool"

// Resources maps a resource name to a quantity.
type Resources map[string]float64

func (r Resources) clone() Resources {
	out := Resources{}
	for k, v := range r {
		out[k] = v
	}
	return out
}

func (r Resources) add(o Resources, sign float64) {
	for k, v := range o {
		r[k] += sign * v
	}
}

// fits reports whether req fits into free.
func fits(free, req Resources) bool {
	for k, v := range req {
		if v > 0 && v > free[k]+1e-9 {
			return false
		}
	}
	return true
}

// Pod is the part of a pod the planner needs.
type Pod struct {
	Namespace, Name, Node string
	Workload              collector.WorkloadKey
	Requests              Resources
	DaemonSet, Mirror     bool
	Controlled            bool // has a controller owner (ReplicaSet, StatefulSet, Job, ...)
	LocalStorage          bool // uses an emptyDir on disk
	Annotations           map[string]string
}

// Node is the part of a node the planner needs.
type Node struct {
	Name, Group, ProviderID string
	Karpenter               bool // provisioned by Karpenter (Group is then "karpenter:<nodepool>")
	Ready, Unschedulable    bool
	Allocatable, Requested  Resources
	Annotations             map[string]string
	Pods                    []*Pod
}

// Free is what is left to schedule on the node.
func (n *Node) Free() Resources {
	f := n.Allocatable.clone()
	f.add(n.Requested, -1)
	return f
}

// Schedulable reports whether new pods can land on the node.
func (n *Node) Schedulable() bool { return n.Ready && !n.Unschedulable }

// Cluster is a point-in-time view of nodes and the pods on them.
type Cluster struct {
	Nodes map[string]*Node
	Pods  []*Pod
}

// API is the subset of the Kubernetes client the planner uses.
type API interface {
	Get(ctx context.Context, path string, out any) error
	StrategicMergePatch(ctx context.Context, path string, patch any) error
	Create(ctx context.Context, path string, body any) error
}

type resourceList map[string]string

func parse(rl resourceList) Resources {
	out := Resources{}
	for k, v := range rl {
		if q, err := kube.ParseQuantity(v); err == nil {
			out[k] = q
		}
	}
	return out
}

// Load reads all nodes and active pods. groupLabel is the node label that names a node's
// group (a provider's GroupLabel); nodes Karpenter provisioned are grouped by nodepool.
func Load(ctx context.Context, k API, groupLabel string) (*Cluster, error) {
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool   `json:"unschedulable"`
				ProviderID    string `json:"providerID"`
			} `json:"spec"`
			Status struct {
				Allocatable resourceList `json:"allocatable"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.Get(ctx, "/api/v1/nodes", &nodes); err != nil {
		return nil, err
	}
	cl := &Cluster{Nodes: map[string]*Node{}}
	for _, it := range nodes.Items {
		n := &Node{
			Name: it.Metadata.Name, ProviderID: it.Spec.ProviderID,
			Unschedulable: it.Spec.Unschedulable, Allocatable: parse(it.Status.Allocatable),
			Requested: Resources{}, Annotations: it.Metadata.Annotations,
		}
		if groupLabel != "" {
			n.Group = it.Metadata.Labels[groupLabel]
		}
		if pool := it.Metadata.Labels[KarpenterLabel]; pool != "" {
			n.Group, n.Karpenter = "karpenter:"+pool, true
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		cl.Nodes[n.Name] = n
	}

	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace"`
				Annotations       map[string]string `json:"annotations"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
				OwnerReferences   []struct {
					Kind       string `json:"kind"`
					Controller bool   `json:"controller"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				NodeName   string `json:"nodeName"`
				Containers []struct {
					Resources struct{ Requests resourceList }
				} `json:"containers"`
				InitContainers []struct {
					Resources struct{ Requests resourceList }
				} `json:"initContainers"`
				Overhead resourceList `json:"overhead"`
				Volumes  []struct {
					EmptyDir *struct {
						Medium string `json:"medium"`
					} `json:"emptyDir"`
				} `json:"volumes"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.Get(ctx, "/api/v1/pods?fieldSelector=status.phase!%3DSucceeded,status.phase!%3DFailed", &pods); err != nil {
		return nil, err
	}
	for _, it := range pods.Items {
		if it.Status.Phase == "Succeeded" || it.Status.Phase == "Failed" {
			continue
		}
		p := &Pod{
			Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Node: it.Spec.NodeName,
			Workload:    collector.WorkloadKey{Namespace: it.Metadata.Namespace, Name: collector.WorkloadFromPod(it.Metadata.Name)},
			Requests:    Resources{Pods: 1},
			Annotations: it.Metadata.Annotations,
			Mirror:      it.Metadata.Annotations["kubernetes.io/config.mirror"] != "",
		}
		for _, o := range it.Metadata.OwnerReferences {
			if o.Controller {
				p.Controlled = true
				p.DaemonSet = o.Kind == "DaemonSet"
			}
		}
		for _, v := range it.Spec.Volumes {
			if v.EmptyDir != nil && v.EmptyDir.Medium != "Memory" {
				p.LocalStorage = true
			}
		}
		// Effective request: sum of containers, or the largest init container if bigger, plus overhead.
		sum := Resources{}
		for _, c := range it.Spec.Containers {
			sum.add(parse(c.Resources.Requests), 1)
		}
		for _, c := range it.Spec.InitContainers {
			for k, v := range parse(c.Resources.Requests) {
				if v > sum[k] {
					sum[k] = v
				}
			}
		}
		sum.add(parse(it.Spec.Overhead), 1)
		p.Requests.add(sum, 1)
		cl.Pods = append(cl.Pods, p)
		if n := cl.Nodes[p.Node]; n != nil {
			n.Pods = append(n.Pods, p)
			n.Requested.add(p.Requests, 1)
		}
	}
	return cl, nil
}
