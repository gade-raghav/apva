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

	"github.com/gade-raghav/apva/internal/actuator"
	"github.com/gade-raghav/apva/internal/api"
	awsapi "github.com/gade-raghav/apva/internal/aws"
	"github.com/gade-raghav/apva/internal/capacity"
	"github.com/gade-raghav/apva/internal/collector"
	"github.com/gade-raghav/apva/internal/demo"
	"github.com/gade-raghav/apva/internal/engine"
	"github.com/gade-raghav/apva/internal/kube"
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
	actDef := actuator.DefaultConfig()
	capDef := capacity.DefaultConfig()
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

		autoResize     = flag.Bool("auto-resize", envOr("APVA_AUTO_RESIZE", "") == "true", "apply recommendations automatically by patching Deployment/StatefulSet requests")
		arDryRun       = flag.Bool("auto-resize-dry-run", false, "with --auto-resize: decide and log, but never patch")
		arMinConf      = flag.String("auto-resize-min-confidence", actDef.MinConfidence, "with --auto-resize: lowest confidence to act on (high|medium|low)")
		arCooldown     = flag.Duration("auto-resize-cooldown", actDef.Cooldown, "with --auto-resize: minimum time between resizes of one workload")
		arMaxDown      = flag.Float64("auto-resize-max-down", actDef.MaxDownStep, "with --auto-resize: max fraction a request may shrink in one step")
		awsCluster     = flag.String("aws-cluster", envOr("APVA_AWS_CLUSTER", ""), "EKS cluster name: with --auto-resize, manage the EKS managed node groups in --aws-nodegroups")
		awsRegion      = flag.String("aws-region", envOr("AWS_REGION", os.Getenv("AWS_DEFAULT_REGION")), "AWS region of the EKS cluster")
		awsNodegroups  = flag.String("aws-nodegroups", envOr("APVA_AWS_NODEGROUPS", ""), "comma-separated managed node groups APVA may scale (allowlist)")
		awsConsol      = flag.Bool("aws-consolidate", capDef.Consolidate, "drain and remove under-used nodes in managed node groups")
		awsConsolBelow = flag.Float64("aws-consolidate-below", capDef.ConsolidateBelow, "a node is a removal candidate when CPU, memory and GPU requests are all below this fraction")
		awsUpTimeout   = flag.Duration("aws-scale-up-timeout", capDef.ScaleUpTimeout, "how long to wait for new nodes to become Ready")
		awsDrainTO     = flag.Duration("aws-drain-timeout", capDef.DrainTimeout, "give up draining a node (and uncordon it) after this long")
		awsCooldown    = flag.Duration("aws-nodegroup-cooldown", capDef.Cooldown, "minimum time between two changes to one node group")
		extAutoscaler  = flag.Bool("node-autoscaler-present", false, "a Cluster Autoscaler adds nodes for Pending pods: let upsizes that do not fit go ahead (Karpenter nodes are detected automatically)")
		kubeAPI        = flag.String("kube-api", envOr("APVA_KUBE_API", ""), "Kubernetes API URL for --auto-resize outside a cluster, e.g. http://127.0.0.1:8001 from `kubectl proxy` (default: in-cluster service account)")
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

	if *awsCluster != "" && !*autoResize {
		return errors.New("--aws-cluster requires --auto-resize")
	}
	if *autoResize {
		if *demoMode {
			return errors.New("--auto-resize needs a real cluster; it cannot be combined with --demo")
		}
		switch *arMinConf {
		case "high", "medium", "low":
		default:
			return fmt.Errorf("--auto-resize-min-confidence must be high, medium or low, got %q", *arMinConf)
		}
		if *arMaxDown <= 0 || *arMaxDown > 1 {
			return fmt.Errorf("--auto-resize-max-down must be in (0, 1], got %g", *arMaxDown)
		}
		var k *kube.Client
		if *kubeAPI != "" {
			k = kube.New(*kubeAPI)
		} else {
			var err error
			if k, err = kube.InCluster(); err != nil {
				return fmt.Errorf("--auto-resize: %w", err)
			}
		}
		eng.ActCfg = actuator.Config{DryRun: *arDryRun, MinConfidence: *arMinConf, Cooldown: *arCooldown, MaxDownStep: *arMaxDown}
		act := &actuator.Actuator{K: k, Cfg: eng.ActCfg, Log: log}
		mgr := &capacity.Manager{K: k, Log: log, Cfg: capDef}
		mgr.Cfg.DryRun, mgr.Cfg.ExternalAutoscaler = *arDryRun, *extAutoscaler
		if *awsCluster != "" {
			if *awsRegion == "" {
				return errors.New("--aws-cluster needs --aws-region (or AWS_REGION)")
			}
			groups := map[string]bool{}
			for _, g := range strings.Split(*awsNodegroups, ",") {
				if g = strings.TrimSpace(g); g != "" {
					groups[g] = true
				}
			}
			if len(groups) == 0 {
				return errors.New("--aws-cluster needs --aws-nodegroups: list the managed node groups APVA may scale")
			}
			if *awsConsolBelow <= 0 || *awsConsolBelow > 1 {
				return fmt.Errorf("--aws-consolidate-below must be in (0, 1], got %g", *awsConsolBelow)
			}
			client := awsapi.NewClient(*awsRegion)
			// For LocalStack and the e2e test's fake AWS.
			client.EKSEndpoint, client.AutoscalingEndpoint = os.Getenv("APVA_AWS_EKS_ENDPOINT"), os.Getenv("APVA_AWS_AUTOSCALING_ENDPOINT")
			if client.Creds.Source() == "" {
				return errors.New("--aws-cluster: no AWS credentials found (use EKS Pod Identity or IRSA, see docs/aws.md)")
			}
			mgr.Provider = &awsapi.NodeProvider{Client: client, Cluster: *awsCluster}
			mgr.Cfg.Groups = groups
			mgr.Cfg.Consolidate, mgr.Cfg.ConsolidateBelow = *awsConsol, *awsConsolBelow
			mgr.Cfg.ScaleUpTimeout, mgr.Cfg.DrainTimeout, mgr.Cfg.Cooldown = *awsUpTimeout, *awsDrainTO, *awsCooldown
			eng.NodesProvider, eng.NodesCluster = "aws", *awsCluster
			log.Info("EKS node group management enabled", "cluster", *awsCluster, "region", *awsRegion,
				"nodegroups", *awsNodegroups, "consolidate", *awsConsol, "credentials", client.Creds.Source())
		}
		eng.Nodes, eng.NodesCfg = mgr, mgr.Cfg
		act.Capacity = mgr
		eng.Act = act
		log.Info("auto-resize enabled", "dryRun", *arDryRun, "minConfidence", *arMinConf, "cooldown", arCooldown.String(), "maxDown", *arMaxDown)
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
