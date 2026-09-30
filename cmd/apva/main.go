// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

// Command apva runs the APVA analytics engine, API and visualiser.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gade-raghav/apva/internal/api"
	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/demo"
	"github.com/gade-raghav/apva/internal/engine"
	"github.com/gade-raghav/apva/internal/prom"
	"github.com/gade-raghav/apva/internal/recommender"
)

// version is set at build time with -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "apva:", err)
		os.Exit(1)
	}
}

func run() error {
	def := recommender.DefaultConfig()
	var (
		promURL    = flag.String("prometheus-url", envOr("APVA_PROMETHEUS_URL", "http://localhost:9090"), "Prometheus-compatible query endpoint")
		listen     = flag.String("listen", envOr("APVA_LISTEN", ":8080"), "HTTP listen address")
		window     = flag.Duration("window", 24*time.Hour, "analysis look-back window")
		recent     = flag.Duration("recent-window", time.Hour, "window for traffic trend")
		refresh    = flag.Duration("refresh", 5*time.Minute, "how often to re-analyse")
		namespaces = flag.String("namespaces", "", "comma-separated namespaces to analyse (default all)")
		inclSystem = flag.Bool("include-system", false, "include kube-system, monitoring, etc.")
		headroom   = flag.Float64("headroom", def.Headroom, "fraction added above p95 usage")
		demoMode   = flag.Bool("demo", false, "use a built-in sample cluster instead of Prometheus")
		once       = flag.Bool("once", false, "analyse once, print JSON to stdout and exit")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var q prom.Querier = prom.NewClient(*promURL)
	if *demoMode {
		q = demo.Prometheus{Window: promWindow(*window)}
		log.Info("demo mode: using built-in sample cluster")
	}
	var ns []string
	if *namespaces != "" {
		ns = strings.Split(*namespaces, ",")
	}
	cfg := def
	cfg.Headroom = *headroom
	eng := &engine.Engine{
		C: &collector.Collector{Q: q, Cfg: collector.Config{
			Window: *window, RecentWindow: *recent, Namespaces: ns, ExcludeSystem: !*inclSystem,
		}},
		Cfg: cfg,
		Log: log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		res, err := eng.RunOnce(ctx)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	go eng.Run(ctx, *refresh)
	srv := &http.Server{Addr: *listen, Handler: api.Handler(eng, version), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("apva listening", "addr", *listen, "version", version, "prometheus", *promURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// promWindow formats a duration the way the collector writes it into PromQL.
func promWindow(d time.Duration) string {
	q := collector.Queries(d, time.Hour, "")["mem_p95"]
	i, j := strings.LastIndex(q, "["), strings.LastIndex(q, "]")
	return q[i+1 : j]
}
