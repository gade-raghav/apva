// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gade-raghav/apva/internal/api"
	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/demo"
	"github.com/gade-raghav/apva/internal/engine"
	"github.com/gade-raghav/apva/internal/recommender"
)

func newServer(t *testing.T, analyse bool) *httptest.Server {
	eng := &engine.Engine{
		C: &collector.Collector{Q: demo.Prometheus{Window: "24h"}, Cfg: collector.Config{
			Window: 24 * time.Hour, RecentWindow: time.Hour, ExcludeSystem: true}},
		Cfg: recommender.DefaultConfig(),
	}
	if analyse {
		if _, err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(api.Handler(eng, "test"))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, string) {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestNotReady(t *testing.T) {
	srv := newServer(t, false)
	if code, _ := get(t, srv.URL+"/readyz"); code != 503 {
		t.Errorf("readyz = %d, want 503", code)
	}
	if code, _ := get(t, srv.URL+"/api/v1/recommendations"); code != 503 {
		t.Errorf("recommendations = %d, want 503", code)
	}
}

func TestEndpoints(t *testing.T) {
	srv := newServer(t, true)
	for _, p := range []string{"/healthz", "/readyz", "/api/v1/result", "/api/v1/graph", "/api/v1/summary", "/metrics", "/", "/app.js"} {
		if code, _ := get(t, srv.URL+p); code != 200 {
			t.Errorf("%s = %d, want 200", p, code)
		}
	}
	_, body := get(t, srv.URL+"/api/v1/recommendations?action=idle")
	var out struct {
		Items []recommender.Recommendation `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Workload.Name != "batch-embedder" {
		t.Errorf("idle filter returned %+v", out.Items)
	}
	_, metrics := get(t, srv.URL+"/metrics")
	if !strings.Contains(metrics, `apva_potential_savings{resource="gpu"} 1`) {
		t.Errorf("metrics missing gpu savings:\n%s", metrics)
	}
}
