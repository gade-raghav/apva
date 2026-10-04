// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The worked example from the AWS Signature Version 4 documentation
// ("Examples of the complete Signature Version 4 signing process", IAM ListUsers).
func TestSignMatchesAWSDocumentedExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	now, _ := time.Parse(amzDate, "20150830T123600Z")
	Sign(req, nil, Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
		"us-east-1", "iam", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n %s\nwant\n %s", got, want)
	}
}

func TestSignAddsSessionToken(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://eks.us-east-1.amazonaws.com/clusters/c/node-groups/g/update-config", nil)
	Sign(req, []byte(`{}`), Credentials{AccessKeyID: "A", SecretAccessKey: "S", SessionToken: "T"}, "us-east-1", "eks", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "T" || !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("session token not signed: %v", req.Header)
	}
}
