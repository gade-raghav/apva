// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient(t *testing.T) {
	var gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/missing":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/forbidden":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"cannot patch deployments"}`)
		case r.Method == http.MethodPatch:
			gotType = r.Header.Get("Content-Type")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		default:
			io.WriteString(w, `{"metadata":{"name":"web"}}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	ctx := context.Background()

	var obj struct {
		Metadata struct{ Name string } `json:"metadata"`
	}
	if err := c.Get(ctx, "/obj", &obj); err != nil || obj.Metadata.Name != "web" {
		t.Fatalf("Get = %v, %+v", err, obj)
	}
	if err := c.Get(ctx, "/missing", &obj); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if err := c.Get(ctx, "/forbidden", &obj); err == nil || !strings.Contains(err.Error(), "cannot patch deployments") {
		t.Errorf("want API message in error, got %v", err)
	}
	if err := c.StrategicMergePatch(ctx, "/obj", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if gotType != "application/strategic-merge-patch+json" || gotBody != `{"a":1}` {
		t.Errorf("patch sent %q %q", gotType, gotBody)
	}
}
