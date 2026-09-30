// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package prom is a minimal, dependency-free client for the Prometheus HTTP query API.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sample is one element of an instant-vector result.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Querier runs an instant PromQL query. It is implemented by Client and by test fakes.
type Querier interface {
	Query(ctx context.Context, promql string) ([]Sample, error)
}

// Client talks to a Prometheus-compatible server (Prometheus, Thanos, Mimir, VictoriaMetrics).
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	// BearerToken is optional, for servers behind authentication.
	BearerToken string
}

// NewClient returns a Client with a sensible timeout.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query executes an instant query and returns the vector result.
func (c *Client) Query(ctx context.Context, promql string) ([]Sample, error) {
	u := c.BaseURL + "/api/v1/query?" + url.Values{"query": {promql}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query: %w", err)
	}
	defer resp.Body.Close()

	var r apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("prometheus query: decode (HTTP %d): %w", resp.StatusCode, err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s: %s", r.ErrorType, r.Error)
	}
	if r.Data.ResultType != "vector" {
		return nil, fmt.Errorf("prometheus query: expected vector, got %q", r.Data.ResultType)
	}
	out := make([]Sample, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s, ok := res.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out = append(out, Sample{Labels: res.Metric, Value: v})
	}
	return out, nil
}
