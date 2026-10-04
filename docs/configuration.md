# Configuration reference

APVA is configured with command-line flags. The Helm chart turns its values into these
flags. A few flags can also be set through environment variables.

## Flags

### Analysis

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--prometheus-url` | `APVA_PROMETHEUS_URL` | `http://localhost:9090` | Prometheus-compatible query endpoint (Prometheus, Thanos, Mimir, VictoriaMetrics) |
| `--window` | | `24h` | look-back window for p95 usage. Under 24h, confidence is at most `medium` |
| `--recent-window` | | `1h` | window for the traffic trend (the traffic-aware hold) |
| `--refresh` | | `5m` | how often to re-analyse (and act) |
| `--namespaces` | | all | comma-separated namespaces to analyse |
| `--include-system` | | `false` | also analyse `kube-system`, `kube-public`, `kube-node-lease`, `monitoring`, `cilium`, `apva` |
| `--headroom` | | `0.15` | fraction added on top of p95 |
| `--listen` | `APVA_LISTEN` | `:8080` | HTTP address for the dashboard, API and probes |
| `--demo` | | `false` | use the built-in sample cluster instead of Prometheus (can't be combined with `--auto-resize`) |
| `--once` | | `false` | analyse once, print the result as JSON and exit |
| `--version` | | | print the version and exit |

### Auto-resize

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--auto-resize` | `APVA_AUTO_RESIZE=true` | `false` | patch Deployment/StatefulSet requests automatically |
| `--auto-resize-dry-run` | | `false` | decide and show, never patch (also stops node changes) |
| `--auto-resize-min-confidence` | | `high` | lowest confidence to act on: `high`, `medium`, `low` |
| `--auto-resize-cooldown` | | `30m` | minimum time between two resizes of one workload |
| `--auto-resize-max-down` | | `0.5` | the most a request may shrink in one step, as a fraction (0, 1] |
| `--node-autoscaler-present` | | `false` | a Cluster Autoscaler adds nodes for Pending pods, so resizes that don't fit yet may go ahead. Karpenter nodes are detected without this flag |
| `--kube-api` | `APVA_KUBE_API` | in-cluster | Kubernetes API URL when running outside the cluster, e.g. `http://127.0.0.1:8001` from `kubectl proxy` |

### Node groups: Amazon EKS provider (needs `--auto-resize`)

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--aws-cluster` | `APVA_AWS_CLUSTER` | | EKS cluster name. Setting it turns the provider on |
| `--aws-region` | `AWS_REGION`, `AWS_DEFAULT_REGION` | | region of the cluster |
| `--aws-nodegroups` | `APVA_AWS_NODEGROUPS` | | **required**: comma-separated allowlist of managed node groups APVA may scale |
| `--aws-consolidate` | | `true` | drain and remove under-used nodes |
| `--aws-consolidate-below` | | `0.5` | a node is a candidate when CPU, memory and GPU requests are all below this fraction |
| `--aws-scale-up-timeout` | | `15m` | how long to wait for new nodes to become Ready |
| `--aws-drain-timeout` | | `10m` | give up a drain (and uncordon the node) after this long |
| `--aws-nodegroup-cooldown` | | `10m` | minimum time between two changes to one node group |

**AWS credentials** are looked up in this order:
1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`;
2. EKS Pod Identity (`AWS_CONTAINER_CREDENTIALS_FULL_URI` +
   `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`, injected by EKS);
3. IRSA (`AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE`, injected by EKS).

`APVA_AWS_EKS_ENDPOINT` and `APVA_AWS_AUTOSCALING_ENDPOINT` override the AWS endpoints,
for LocalStack or the simulated end-to-end test.

## Helm values

| Value | Default | Flag |
|---|---|---|
| `image.repository` / `image.tag` / `image.pullPolicy` | `ghcr.io/gade-raghav/apva` / chart appVersion / `IfNotPresent` | |
| `prometheus.url` | `http://prometheus-server.monitoring.svc:80` | `--prometheus-url` |
| `analysis.window` · `recentWindow` · `refresh` · `headroom` | `24h` · `1h` · `5m` · `0.15` | `--window` … |
| `analysis.namespaces` · `includeSystem` | `""` · `false` | `--namespaces` · `--include-system` |
| `autoResize.enabled` | `false` | `--auto-resize`; also mounts the service account token and creates the RBAC |
| `autoResize.dryRun` · `minConfidence` · `cooldown` · `maxDownStep` | `false` · `high` · `30m` · `0.5` | `--auto-resize-*` |
| `autoResize.nodeAutoscalerPresent` | `false` | `--node-autoscaler-present` |
| `aws.enabled` | `false` | turns on the EKS provider (only together with `autoResize.enabled`) and adds node/eviction RBAC |
| `aws.cluster` · `region` · `nodegroups` | required when enabled | `--aws-cluster` · `--aws-region` · `--aws-nodegroups` |
| `aws.consolidate` · `consolidateBelow` · `scaleUpTimeout` · `drainTimeout` · `cooldown` | `true` · `0.5` · `15m` · `10m` · `10m` | `--aws-*` |
| `serviceAccount.annotations` | `{}` | e.g. `eks.amazonaws.com/role-arn` for IRSA |
| `demo` | `false` | `--demo` |
| `serviceMonitor.enabled` | `false` | creates a ServiceMonitor for `/metrics` (Prometheus Operator) |
| `replicaCount`, `resources`, `service.*`, `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` | | standard |

## Annotations

APVA reads these on **your** objects:

| Annotation | On | Effect |
|---|---|---|
| `apva.io/auto-resize: "off"` (or `"false"`) | Deployment / StatefulSet | never resize this workload |
| `apva.io/safe-to-evict: "false"` | Pod | never drain a node running this pod (overrides the one below) |
| `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` / `"true"` | Pod | honoured the same way; `"true"` also allows draining bare, `kube-system` or `emptyDir` pods |

APVA writes these, and uses them to resume after a restart:

| Annotation | On | Contents |
|---|---|---|
| `apva.io/last-resized` | workload | time of APVA's last resize (drives the cooldown) |
| `apva.io/previous-requests` | workload | JSON of the requests before that resize, per container (for rollback) |
| `apva.io/last-resize-summary` | workload | e.g. `CPU 300m → 150m, memory 192Mi → 96Mi per pod` |
| `apva.io/draining` | node | start time of an in-flight drain |

Node labels APVA understands: `eks.amazonaws.com/nodegroup` (the EKS provider's groups) and
`karpenter.sh/nodepool` (Karpenter, detected automatically).
