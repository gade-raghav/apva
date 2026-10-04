// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/gade-raghav/apva/internal/capacity"
)

// NodegroupLabel is set by EKS on every node of a managed node group.
const NodegroupLabel = "eks.amazonaws.com/nodegroup"

// NodeProvider manages Amazon EKS managed node groups for capacity.Manager.
type NodeProvider struct {
	Client  *Client
	Cluster string // EKS cluster name
}

var _ capacity.Provider = (*NodeProvider)(nil)

func (p *NodeProvider) Name() string       { return "aws" }
func (p *NodeProvider) GroupLabel() string { return NodegroupLabel }

func (p *NodeProvider) DescribeGroup(ctx context.Context, group string) (capacity.Group, error) {
	ng, err := p.Client.DescribeNodegroup(ctx, p.Cluster, group)
	if err != nil {
		return capacity.Group{}, err
	}
	return capacity.Group{Name: ng.Name, Status: ng.Status,
		MinSize: ng.Scaling.MinSize, MaxSize: ng.Scaling.MaxSize, DesiredSize: ng.Scaling.DesiredSize}, nil
}

func (p *NodeProvider) SetDesiredSize(ctx context.Context, g capacity.Group, desired int) error {
	ng := Nodegroup{Name: g.Name}
	ng.Scaling.MinSize, ng.Scaling.MaxSize = g.MinSize, g.MaxSize
	return p.Client.SetDesiredSize(ctx, p.Cluster, ng, desired)
}

// RemoveNode terminates the node's EC2 instance through its Auto Scaling group with
// ShouldDecrementDesiredCapacity, so exactly this instance goes and the group shrinks by one.
func (p *NodeProvider) RemoveNode(ctx context.Context, n *capacity.Node) error {
	id, err := InstanceID(n.ProviderID)
	if err != nil {
		return err
	}
	return p.Client.TerminateInstance(ctx, id)
}

// InstanceID extracts the EC2 instance ID from a node's providerID
// ("aws:///us-east-1a/i-0123456789abcdef0").
func InstanceID(providerID string) (string, error) {
	i := strings.LastIndex(providerID, "/")
	if !strings.HasPrefix(providerID, "aws://") || i < 0 || !strings.HasPrefix(providerID[i+1:], "i-") {
		return "", fmt.Errorf("not an EC2 node (providerID %q)", providerID)
	}
	return providerID[i+1:], nil
}
