# Node-aware resizing (Amazon EKS provider)

Resizing pods is only half the job. A bigger pod that no node can hold sits in `Pending`;
smaller pods only save money once the nodes they free up are gone. With `aws.enabled`, APVA
manages the **EKS managed node groups you list** so that pods and nodes change in the
right order:

| Direction | Order | What APVA does |
|---|---|---|
| **Up** (pods need more) | nodes first, then pods | Checks the larger pods fit on the node group. If not, raises the group's desired size, waits until the new nodes are `Ready` and the pods fit, then resizes the pods. |
| **Down** (pods need less) | pods first, then nodes | Resizes the pods. Then picks the least-used node, checks every pod on it fits on the other nodes, cordons it, evicts its pods through the Eviction API, and terminates **exactly that instance** while lowering the desired size by one. |

Every resize is a rolling update, so even a *downsize* needs room for the rollout's first
new pod. APVA checks that before every resize, on every cluster:

| Where the workload's nodes come from | What APVA does when the new pods won't fit |
|---|---|
| A node group APVA manages (`aws.nodegroups`) | adds nodes first, as above |
| **Karpenter** (nodes labelled `karpenter.sh/nodepool`, detected automatically) | resizes anyway: Karpenter provisions nodes for the Pending pods |
| A Cluster Autoscaler (`autoResize.nodeAutoscalerPresent=true`) | resizes anyway: the autoscaler adds nodes |
| Anything else | doesn't resize, and says why in the activity log |

Node management sits behind a small provider interface (`capacity.Provider`: describe a
group, set its size, remove one node). Amazon EKS managed node groups are the first
provider; other clouds implement the same four calls.

## Guardrails

- **Allowlist.** Only node groups in `aws.nodegroups` are ever touched.
- **Within min/max.** Never scales above `maxSize` (it tells you to raise it) or below `minSize`.
- **One change at a time** per node group, with a cooldown (`aws.cooldown`, default 10m) between changes.
- **Waits instead of racing.** A pod resize that needs new nodes is held (`waiting`) until the nodes are `Ready` and the pods fit; if that takes longer than `aws.scaleUpTimeout` (15m) it gives up and says so.
- **Instance types are not changed.** If a single pod no longer fits on any node of its group, APVA reports that it needs a larger instance type instead of acting.
- **Safe drains only.** A node is not drained if any pod on it has no controller, uses an on-disk `emptyDir`, runs in `kube-system`, or is annotated `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` (or `apva.io/safe-to-evict: "false"`). Evictions respect PodDisruptionBudgets; a drain that can't finish within `aws.drainTimeout` (10m) is abandoned and the node uncordoned.
- **Exact termination.** The removed instance is the drained one (`TerminateInstanceInAutoScalingGroup` with `ShouldDecrementDesiredCapacity=true`, the same call the Cluster Autoscaler uses), never a random member of the group.
- **Restart-safe.** An in-flight drain is marked on the node (`apva.io/draining`) and is resumed or rolled back after an APVA restart.
- **Dry run.** `autoResize.dryRun=true` shows every node group decision without changing anything.

**Don't list node groups that Karpenter or the Cluster Autoscaler also manage**: two
controllers resizing the same group will fight. On Karpenter clusters, use APVA's pod
resizing alone; Karpenter already provisions and consolidates nodes.

## Setup

### 1. IAM policy

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["eks:DescribeNodegroup", "eks:UpdateNodegroupConfig"],
      "Resource": "arn:aws:eks:<region>:<account-id>:nodegroup/<cluster>/*/*"
    },
    {
      "Effect": "Allow",
      "Action": "autoscaling:TerminateInstanceInAutoScalingGroup",
      "Resource": "*",
      "Condition": {"StringEquals": {"aws:ResourceTag/eks:cluster-name": "<cluster>"}}
    }
  ]
}
```

### 2. Give APVA's service account the role

EKS Pod Identity (recommended):

```bash
aws eks create-pod-identity-association --cluster-name <cluster> \
  --namespace apva --service-account apva --role-arn arn:aws:iam::<account-id>:role/apva
```

or IRSA: add `--set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::<account-id>:role/apva`.

APVA also accepts `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (for running outside the cluster).

### 3. Install

```bash
helm upgrade --install apva ./charts/apva -n apva --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring.svc:80 \
  --set autoResize.enabled=true --set autoResize.dryRun=true \
  --set aws.enabled=true --set aws.cluster=prod --set aws.region=us-east-1 \
  --set aws.nodegroups="gpu-a10g\,general"
```

Watch the dashboard's activity log in dry-run first; then set `autoResize.dryRun=false`.

Kubernetes permissions added by `aws.enabled`: `patch` on nodes (cordon) and `create` on
`pods/eviction`. Auto-resize itself now also reads nodes and pods for the capacity check.

## Flags

| Flag | Default | |
|---|---|---|
| `--aws-cluster` | | EKS cluster name; enables node group management (needs `--auto-resize`) |
| `--aws-region` | `$AWS_REGION` | |
| `--aws-nodegroups` | | comma-separated allowlist |
| `--aws-consolidate` | `true` | drain and remove under-used nodes |
| `--aws-consolidate-below` | `0.5` | candidate when CPU, memory and GPU requests are all below this fraction |
| `--aws-scale-up-timeout` | `15m` | |
| `--aws-drain-timeout` | `10m` | |
| `--aws-nodegroup-cooldown` | `10m` | |

## Testing without an AWS account

`.github/workflows/e2e-aws.yml` runs on every pull request: a real Kubernetes control
plane (k3s, no kubelet), [KWOK](https://kwok.sigs.k8s.io) simulating the nodes and pods, and
`test/e2e-aws/fake.py` playing EKS, Auto Scaling and Prometheus. The fake node group creates
and deletes KWOK nodes, so APVA's whole cycle runs against real scheduling, rollouts and
evictions: scale the group up before a resize, roll the pods onto the new node, remove the
node left empty, then shrink the pods and drain the group down to its minimum. Run it
locally with `make e2e-aws` against any cluster with KWOK.

## Not yet

- Changing a node group's instance type (e.g. A10G → H100) through a new launch template version.
- Packing pods across several node groups at once; each workload is planned within the node group its pods run on.
- Self-managed node groups and plain Auto Scaling groups.
