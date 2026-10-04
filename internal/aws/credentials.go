// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Provider resolves credentials from, in order:
//
//  1. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
//  2. EKS Pod Identity (AWS_CONTAINER_CREDENTIALS_FULL_URI + AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE)
//  3. IAM Roles for Service Accounts (AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE)
//
// Temporary credentials are cached and refreshed five minutes before they expire.
type Provider struct {
	Region string
	HTTP   *http.Client
	Getenv func(string) string // for tests
	STSURL string              // for tests; default https://sts.<region>.amazonaws.com/

	mu     sync.Mutex
	cached Credentials
}

func (p *Provider) env(k string) string {
	if p.Getenv != nil {
		return p.Getenv(k)
	}
	return os.Getenv(k)
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Source names the credential source that will be used, for logs.
func (p *Provider) Source() string {
	switch {
	case p.env("AWS_ACCESS_KEY_ID") != "":
		return "environment"
	case p.env("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "":
		return "eks-pod-identity"
	case p.env("AWS_ROLE_ARN") != "" && p.env("AWS_WEB_IDENTITY_TOKEN_FILE") != "":
		return "irsa"
	}
	return ""
}

// Retrieve returns valid credentials.
func (p *Provider) Retrieve(ctx context.Context) (Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached.AccessKeyID != "" && (p.cached.Expires.IsZero() || time.Until(p.cached.Expires) > 5*time.Minute) {
		return p.cached, nil
	}
	var (
		c   Credentials
		err error
	)
	switch p.Source() {
	case "environment":
		c = Credentials{AccessKeyID: p.env("AWS_ACCESS_KEY_ID"), SecretAccessKey: p.env("AWS_SECRET_ACCESS_KEY"), SessionToken: p.env("AWS_SESSION_TOKEN")}
		if c.SecretAccessKey == "" {
			err = errors.New("AWS_ACCESS_KEY_ID is set but AWS_SECRET_ACCESS_KEY is not")
		}
	case "eks-pod-identity":
		c, err = p.podIdentity(ctx)
	case "irsa":
		c, err = p.webIdentity(ctx)
	default:
		err = errors.New("no AWS credentials found: use EKS Pod Identity or IRSA in-cluster, or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY")
	}
	if err != nil {
		return Credentials{}, err
	}
	p.cached = c
	return c, nil
}

func (p *Provider) podIdentity(ctx context.Context) (Credentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.env("AWS_CONTAINER_CREDENTIALS_FULL_URI"), nil)
	if err != nil {
		return Credentials{}, err
	}
	if f := p.env("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
		tok, err := os.ReadFile(f)
		if err != nil {
			return Credentials{}, fmt.Errorf("pod identity token: %w", err)
		}
		req.Header.Set("Authorization", strings.TrimSpace(string(tok)))
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("pod identity: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("pod identity: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var r struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      time.Time
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Credentials{}, fmt.Errorf("pod identity: %w", err)
	}
	return Credentials{AccessKeyID: r.AccessKeyID, SecretAccessKey: r.SecretAccessKey, SessionToken: r.Token, Expires: r.Expiration}, nil
}

func (p *Provider) webIdentity(ctx context.Context) (Credentials, error) {
	tok, err := os.ReadFile(p.env("AWS_WEB_IDENTITY_TOKEN_FILE"))
	if err != nil {
		return Credentials{}, fmt.Errorf("web identity token: %w", err)
	}
	endpoint := p.STSURL
	if endpoint == "" {
		endpoint = "https://sts." + p.Region + ".amazonaws.com/"
	}
	form := url.Values{
		"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"},
		"RoleArn": {p.env("AWS_ROLE_ARN")}, "RoleSessionName": {"apva"},
		"WebIdentityToken": {strings.TrimSpace(string(tok))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client().Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sts: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("sts AssumeRoleWithWebIdentity: %s: %s", resp.Status, xmlError(body))
	}
	var r struct {
		Creds struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(body, &r); err != nil {
		return Credentials{}, fmt.Errorf("sts: %w", err)
	}
	c := r.Creds
	return Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Expires: c.Expiration}, nil
}

// xmlError extracts Code: Message from an AWS query-API error body.
func xmlError(body []byte) string {
	var e struct {
		Code    string `xml:"Error>Code"`
		Message string `xml:"Error>Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(body))
}
