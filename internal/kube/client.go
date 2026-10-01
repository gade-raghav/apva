// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package kube is a minimal Kubernetes API client built on net/http. APVA only needs to
// read and patch a handful of objects, so it avoids pulling in client-go.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrNotFound is returned when the API server answers 404.
var ErrNotFound = errors.New("not found")

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Client talks to the Kubernetes API server.
type Client struct {
	Base      string // e.g. https://10.96.0.1:443 or http://127.0.0.1:8001 (kubectl proxy)
	TokenFile string // re-read on every request so rotated tokens are picked up
	HTTP      *http.Client
}

// InCluster builds a client from the pod's service account.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST unset); use --kube-api with `kubectl proxy`")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("reading service account CA: %w (is automountServiceAccountToken enabled?)", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA is not valid PEM")
	}
	return &Client{
		Base:      "https://" + net.JoinHostPort(host, port),
		TokenFile: saDir + "/token",
		HTTP: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// New returns a client for an unauthenticated endpoint such as `kubectl proxy`.
func New(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Get fetches path and decodes the JSON body into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

// StrategicMergePatch applies a strategic merge patch to path.
func (c *Client) StrategicMergePatch(ctx context.Context, path string, patch any) error {
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, path, "application/strategic-merge-patch+json", b, nil)
}

func (c *Client) do(ctx context.Context, method, path, ctype string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "apva")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("reading service account token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, st.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
