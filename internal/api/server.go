// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Package api serves the HTTP API and the built-in visualiser.
package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gade-raghav/apva/internal/engine"
)

//go:embed ui
var uiFS embed.FS

// Latest is implemented by *engine.Engine.
type Latest interface {
	Latest() (*engine.Result, error)
}

// Handler returns the HTTP handler for APVA.
func Handler(src Latest, version string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if res, _ := src.Latest(); res == nil {
			http.Error(w, "no analysis yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"version": version})
	})
	mux.HandleFunc("GET /api/v1/result", func(w http.ResponseWriter, _ *http.Request) {
		if res, ok := latest(w, src); ok {
			writeJSON(w, res)
		}
	})
	mux.HandleFunc("GET /api/v1/recommendations", func(w http.ResponseWriter, r *http.Request) {
		res, ok := latest(w, src)
		if !ok {
			return
		}
		ns := r.URL.Query().Get("namespace")
		action := r.URL.Query().Get("action")
		out := res.Recommendations[:0:0]
		for _, rec := range res.Recommendations {
			if ns != "" && rec.Workload.Namespace != ns {
				continue
			}
			if action != "" && string(rec.CPU.Action) != action && string(rec.Memory.Action) != action &&
				(rec.GPU == nil || string(rec.GPU.Action) != action) {
				continue
			}
			out = append(out, rec)
		}
		writeJSON(w, map[string]any{"generatedAt": res.GeneratedAt, "items": out})
	})
	mux.HandleFunc("GET /api/v1/graph", func(w http.ResponseWriter, _ *http.Request) {
		if res, ok := latest(w, src); ok {
			writeJSON(w, res.Graph)
		}
	})
	mux.HandleFunc("GET /api/v1/summary", func(w http.ResponseWriter, _ *http.Request) {
		if res, ok := latest(w, src); ok {
			writeJSON(w, res.Summary)
		}
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		res, _ := src.Latest()
		if res == nil {
			return
		}
		s := res.Summary
		fmt.Fprintf(w, "# HELP apva_workloads Workloads analysed.\n# TYPE apva_workloads gauge\napva_workloads %d\n", s.Workloads)
		fmt.Fprintf(w, "# HELP apva_recommendations Workloads by recommended action.\n# TYPE apva_recommendations gauge\n")
		fmt.Fprintf(w, "apva_recommendations{action=\"downsize\"} %d\napva_recommendations{action=\"upsize\"} %d\napva_recommendations{action=\"hold\"} %d\n", s.Downsize, s.Upsize, s.Held)
		fmt.Fprintf(w, "# HELP apva_potential_savings Resources reclaimable if recommendations are applied.\n# TYPE apva_potential_savings gauge\n")
		fmt.Fprintf(w, "apva_potential_savings{resource=\"cpu_cores\"} %g\napva_potential_savings{resource=\"memory_bytes\"} %g\napva_potential_savings{resource=\"gpu\"} %g\n", s.CPUSavingsCores, s.MemSavingsBytes, s.GPUSavings)
	})

	ui, _ := fs.Sub(uiFS, "ui")
	files := http.FileServer(http.FS(ui))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}))
	return securityHeaders(mux)
}

func latest(w http.ResponseWriter, src Latest) (*engine.Result, bool) {
	res, err := src.Latest()
	if res == nil {
		msg := "analysis not ready yet"
		if err != nil {
			msg = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return nil, false
	}
	return res, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
		h.ServeHTTP(w, r)
	})
}
