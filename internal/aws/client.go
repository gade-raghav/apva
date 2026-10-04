// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls Amazon EKS and EC2 Auto Scaling.
type Client struct {
	Region string
	Creds  *Provider
	HTTP   *http.Client
	Now    func() time.Time

	// Endpoint overrides, for tests. Defaults: https://eks.<region>.amazonaws.com and
	// https://autoscaling.<region>.amazonaws.com.
	EKSEndpoint, AutoscalingEndpoint string
}

// NewClient returns a client using the default credential chain.
func NewClient(region string) *Client {
	h := &http.Client{Timeout: 20 * time.Second}
	return &Client{Region: region, HTTP: h, Creds: &Provider{Region: region, HTTP: h}}
}

// Nodegroup is the part of an EKS managed node group APVA uses.
type Nodegroup struct {
	Name          string   `json:"nodegroupName"`
	Status        string   `json:"status"`
	InstanceTypes []string `json:"instanceTypes"`
	Scaling       struct {
		MinSize     int `json:"minSize"`
		MaxSize     int `json:"maxSize"`
		DesiredSize int `json:"desiredSize"`
	} `json:"scalingConfig"`
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) send(ctx context.Context, service, method, rawURL, ctype string, body []byte, out any) error {
	creds, err := c.Creds.Retrieve(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("User-Agent", "apva")
	Sign(req, body, creds, c.Region, service, c.now())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		msg := xmlError(data)
		var j struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &j) == nil && j.Message != "" {
			msg = j.Message
		}
		return fmt.Errorf("%s %s: %s: %s", service, method, resp.Status, msg)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) eksURL(path string) string {
	base := c.EKSEndpoint
	if base == "" {
		base = "https://eks." + c.Region + ".amazonaws.com"
	}
	return strings.TrimRight(base, "/") + path
}

// DescribeNodegroup returns an EKS managed node group.
func (c *Client) DescribeNodegroup(ctx context.Context, cluster, nodegroup string) (Nodegroup, error) {
	var out struct {
		Nodegroup Nodegroup `json:"nodegroup"`
	}
	err := c.send(ctx, "eks", http.MethodGet,
		c.eksURL("/clusters/"+url.PathEscape(cluster)+"/node-groups/"+url.PathEscape(nodegroup)), "", nil, &out)
	return out.Nodegroup, err
}

// SetDesiredSize changes a managed node group's desired size, keeping min and max.
func (c *Client) SetDesiredSize(ctx context.Context, cluster string, ng Nodegroup, desired int) error {
	body, _ := json.Marshal(map[string]any{"scalingConfig": map[string]int{
		"minSize": ng.Scaling.MinSize, "maxSize": ng.Scaling.MaxSize, "desiredSize": desired,
	}})
	return c.send(ctx, "eks", http.MethodPost,
		c.eksURL("/clusters/"+url.PathEscape(cluster)+"/node-groups/"+url.PathEscape(ng.Name)+"/update-config"),
		"application/json", body, nil)
}

// TerminateInstance terminates one instance in its Auto Scaling group and lowers the
// group's desired capacity by one, so the group shrinks by exactly that instance (the
// same call the Kubernetes Cluster Autoscaler uses for managed node groups).
func (c *Client) TerminateInstance(ctx context.Context, instanceID string) error {
	base := c.AutoscalingEndpoint
	if base == "" {
		base = "https://autoscaling." + c.Region + ".amazonaws.com/"
	}
	form := url.Values{
		"Action": {"TerminateInstanceInAutoScalingGroup"}, "Version": {"2011-01-01"},
		"InstanceId": {instanceID}, "ShouldDecrementDesiredCapacity": {"true"},
	}
	return c.send(ctx, "autoscaling", http.MethodPost, base,
		"application/x-www-form-urlencoded; charset=utf-8", []byte(form.Encode()), nil)
}
