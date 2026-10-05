# Security

APVA changes running workloads, so its permissions matter as much as its maths. Three rules
shape the design:
- **least privilege**: each permission exists for one reason, documented below;
- **defence in depth**: RBAC decides *which* objects APVA may touch, admission policies
  decide *what* it may change in them;
- **read-only by default**: APVA writes nothing until you opt in.

## What APVA can do, by mode

| Mode | Kubernetes permissions | AWS permissions |
|---|---|---|
| **Recommend-only** (default) | **none**; no service account token is mounted | none |
| `autoResize.enabled` | see the tables below | none |
| `+ aws.enabled` | + cordon nodes, evict pods | `eks:DescribeNodegroup`, `eks:UpdateNodegroupConfig`, `autoscaling:TerminateInstanceInAutoScalingGroup` (scoped by tag) |

### RBAC with `autoResize.enabled`

| Role | Rules | Why | Scope |
|---|---|---|---|
| `apva-capacity-reader` | `list` pods and nodes | the capacity check needs every pod's requests and every node's allocatable to know whether a resized pod fits | cluster |
| `apva-resizer` | `get, patch` deployments and statefulsets | change requests and record decisions in `apva.io/*` annotations | **namespaced** to `analysis.namespaces` when set (one RoleBinding per namespace), otherwise cluster |
| | `list` horizontalpodautoscalers | skip HPA-managed workloads | same |
| | `patch pods/resize` | in-place resize. The subresource can only change resources. Omitted with `resizeMode: rollout` | same |
| `apva-node-manager` (`aws.enabled`) | `patch` nodes | cordon or uncordon a node being drained | cluster |
| | `create pods/eviction` | drain through the Eviction API, so PodDisruptionBudgets are respected | cluster |

**APVA cannot:**
- read Secrets or ConfigMaps;
- create or delete anything in Kubernetes;
- `update` (replace) objects;
- list workloads;
- touch Services, Ingresses, RBAC or CRDs;
- patch pods directly (only their `resize` subresource).

**Know this about pod reads:** reading pods cluster-wide also means reading their specs,
including any environment variables written inline. Keep secrets in Secrets (APVA can't
read those), not in plain `env` values.

### Admission policies (Kubernetes 1.30+)

RBAC's `patch` on a Deployment is broad: on its own, it would allow changing the image. So
the chart also installs **ValidatingAdmissionPolicies** (`security.admissionPolicy.enabled`,
on by default). They match only APVA's service account. Even with stolen credentials,
APVA's identity can only:

| On | Allowed | Denied (examples) |
|---|---|---|
| Deployments, StatefulSets | container **resource requests**; annotations starting `apva.io/` | images, commands, args, env, envFrom, volumes, volume mounts, ports, probes/lifecycle, limits, resize policies, security contexts, service account, host namespaces, scheduling (nodeSelector, affinity, tolerations, priority, runtime class), init/ephemeral containers, replicas, selectors, labels, pod template metadata, other annotations; any create or delete; anything in `kube-system`, `kube-public`, `kube-node-lease` |
| Nodes (`aws.enabled`) | `spec.unschedulable` (cordon); the `apva.io/draining` annotation | labels, other annotations, taints, providerID, pod CIDRs; create, delete |

The end-to-end test runs APVA as its own service account and checks both halves: all of
APVA's real work succeeds, and these changes are denied
([testing.md](testing.md#end-to-end-simulated-amazon-eks-no-aws-account)).

On clusters older than 1.30 the chart refuses to install with the policy on. Set
`security.admissionPolicy.enabled=false` to rely on RBAC alone.

### AWS

- **Credentials:** EKS Pod Identity or IRSA, so there are no long-lived keys. APVA also
  accepts environment keys for running outside a cluster.
- **Scope:** the IAM policy in [aws.md](aws.md#1-iam-policy) limits node group calls to one
  cluster's node groups, and instance termination to Auto Scaling groups tagged with that
  cluster.
- **Allowlist:** APVA only changes node groups listed in `aws.nodegroups`, within their
  min/max size.

## The running pod

The chart's pod:
- runs as non-root with a read-only root filesystem;
- drops all Linux capabilities and uses the `RuntimeDefault` seccomp profile;
- disallows privilege escalation;
- mounts the service account token only when auto-resize is on.

APVA has no third-party Go dependencies, so the supply chain is the Go standard library and
a distroless base image.

## The dashboard and API

The dashboard and API **have no authentication** and show workload names, namespaces,
resource usage and decisions. The Service is `ClusterIP`, so it isn't exposed outside the
cluster. Reach it with `kubectl port-forward`, or put it behind your ingress with
authentication. Don't expose it publicly. The API is read-only: nothing on it can change
the cluster.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
