// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFrom(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestClientCalls(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
			t.Errorf("unsigned request: %v", r.Header)
		}
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"nodegroup":{"nodegroupName":"gpu","status":"ACTIVE","instanceTypes":["g5.xlarge"],"scalingConfig":{"minSize":1,"maxSize":4,"desiredSize":2}}}`)
		case strings.Contains(string(b), "i-bad"):
			w.WriteHeader(400)
			io.WriteString(w, `<ErrorResponse><Error><Code>ValidationError</Code><Message>below min</Message></Error></ErrorResponse>`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	c := &Client{Region: "us-east-1", HTTP: srv.Client(), EKSEndpoint: srv.URL, AutoscalingEndpoint: srv.URL + "/",
		Creds: &Provider{Getenv: envFrom(map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"})}}
	ctx := context.Background()

	ng, err := c.DescribeNodegroup(ctx, "prod", "gpu")
	if err != nil || ng.Scaling.DesiredSize != 2 || ng.Scaling.MaxSize != 4 || ng.InstanceTypes[0] != "g5.xlarge" {
		t.Fatalf("describe = %+v, %v", ng, err)
	}
	if err := c.SetDesiredSize(ctx, "prod", ng, 3); err != nil {
		t.Fatal(err)
	}
	if err := c.TerminateInstance(ctx, "i-0abc"); err != nil {
		t.Fatal(err)
	}
	if err := c.TerminateInstance(ctx, "i-bad"); err == nil || !strings.Contains(err.Error(), "ValidationError: below min") {
		t.Errorf("want ASG error surfaced, got %v", err)
	}
	want := []string{
		"GET /clusters/prod/node-groups/gpu ",
		`POST /clusters/prod/node-groups/gpu/update-config {"scalingConfig":{"desiredSize":3,"maxSize":4,"minSize":1}}`,
		"POST / Action=TerminateInstanceInAutoScalingGroup&InstanceId=i-0abc&ShouldDecrementDesiredCapacity=true&Version=2011-01-01",
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestWebIdentityCredentials(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	os.WriteFile(tokFile, []byte("jwt-token\n"), 0o600)
	calls := 0
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.Form.Get("WebIdentityToken") != "jwt-token" || r.Form.Get("RoleArn") != "arn:aws:iam::1:role/apva" {
			t.Errorf("bad STS form: %v", r.Form)
		}
		io.WriteString(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
<AccessKeyId>ASIA1</AccessKeyId><SecretAccessKey>sec</SecretAccessKey><SessionToken>tok</SessionToken>
<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
	}))
	defer sts.Close()
	p := &Provider{Region: "us-east-1", HTTP: sts.Client(), STSURL: sts.URL,
		Getenv: envFrom(map[string]string{"AWS_ROLE_ARN": "arn:aws:iam::1:role/apva", "AWS_WEB_IDENTITY_TOKEN_FILE": tokFile})}
	for i := 0; i < 2; i++ {
		c, err := p.Retrieve(context.Background())
		if err != nil || c.AccessKeyID != "ASIA1" || c.SessionToken != "tok" {
			t.Fatalf("creds = %+v, %v", c, err)
		}
	}
	if calls != 1 {
		t.Errorf("credentials should be cached, STS called %d times", calls)
	}
}

func TestPodIdentityCredentials(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	os.WriteFile(tokFile, []byte("pod-token"), 0o600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "pod-token" {
			t.Errorf("missing pod identity token")
		}
		io.WriteString(w, `{"AccessKeyId":"ASIA2","SecretAccessKey":"s","Token":"t","Expiration":"2099-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()
	p := &Provider{HTTP: srv.Client(), Getenv: envFrom(map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": tokFile})}
	c, err := p.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "ASIA2" || p.Source() != "eks-pod-identity" {
		t.Fatalf("creds = %+v, %v", c, err)
	}
}

func TestNoCredentials(t *testing.T) {
	p := &Provider{Getenv: envFrom(nil)}
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("expected an error without credentials")
	}
}
