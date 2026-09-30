// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "up" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"pod":"a"},"value":[1700000000,"0.5"]},
			{"metric":{"pod":"b"},"value":[1700000000,"NaN"]}]}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL + "/")
	c.BearerToken = "tok"
	got, err := c.Query(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Labels["pod"] != "a" || got[0].Value != 0.5 {
		t.Fatalf("got %+v", got)
	}
}

func TestQueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL).Query(context.Background(), "("); err == nil {
		t.Fatal("expected error")
	}
}
