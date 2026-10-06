# APVA as a VPA recommender

APVA can run as a **custom recommender for the Kubernetes Vertical Pod Autoscaler**. A VPA
names APVA in `spec.recommenders`. APVA writes the recommendation into that VPA's status.
The VPA's own updater and admission controller then apply it, as they would for the
default recommender.

You keep the VPA as the single thing that changes pod requests, with its update modes,
container policies, eviction rules and tooling. APVA supplies the numbers, and adds what
it knows:

- **Traffic-aware hold.** When a workload's callers are ramping up (seen in the service
  graph), APVA doesn't shrink it, even if its own usage still looks low.
- **Explanations.** Every recommendation carries its reasons and callers in the APVA
  dashboard and API.
- **Confidence.** A recommendation from too little data is flagged with the VPA
  `LowConfidence` condition.

This uses the custom recommender support VPA already has
([AEP-3919](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler/enhancements/3919-customized-recommender-vpa)),
so no change to the VPA is needed. Discussion upstream:
[kubernetes/autoscaler#10395](https://github.com/kubernetes/autoscaler/issues/10395).

## Turn it on

You need the VPA CRDs, and for the VPA to act on recommendations, its updater and
admission controller. The VPA's default recommender can keep running: it ignores VPAs that
name another recommender.

```sh
helm upgrade --install apva ./charts/apva -n apva --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring.svc \
  --set vpaRecommender.enabled=true
```

Then point a VPA at APVA:

```yaml
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata: {name: web, namespace: shop}
spec:
  targetRef: {apiVersion: apps/v1, kind: Deployment, name: web}
  recommenders:
    - name: apva # must match vpaRecommender.name / --vpa-recommender-name
  updatePolicy:
    updateMode: InPlaceOrRecreate # or Off to only see the recommendation
  resourcePolicy:
    containerPolicies:
      - containerName: "*"
        minAllowed: {cpu: 50m, memory: 64Mi}
```

After the next refresh (`analysis.refresh`, 5m by default), check the result:

```sh
kubectl -n shop get vpa web -o jsonpath='{.status.recommendation}' | jq
```

Flags: `--vpa-recommender` (env `APVA_VPA_RECOMMENDER=true`) and `--vpa-recommender-name`
(default `apva`). The name can't be `default`.

## How it works

On every refresh, after the analysis:

1. APVA lists the VPAs in `analysis.namespaces`, or in all namespaces when that is empty.
2. It handles only the VPAs whose `spec.recommenders` includes its name. A VPA that also
   lists `default` is skipped, so that two recommenders don't fight over its status.
3. For each, it reads the target's pod template and turns APVA's pod-level
   recommendation into per-container recommendations:

   | VPA status field | What APVA writes |
   |---|---|
   | `target` | APVA's pod size, split across containers in proportion to their current requests. For `ok` and `hold`, the current request. |
   | `lowerBound` / `upperBound` | `target` ± 10% (the recommender's tolerance). On a `hold`, `lowerBound` = `target`, so the VPA never evicts a pod to shrink it while its callers ramp up. |
   | `uncappedTarget` | `target` before `minAllowed` / `maxAllowed`. |
   | conditions | `RecommendationProvided` (with APVA's reasons) and `LowConfidence`. When APVA can't recommend, `RecommendationProvided` is `False` with reason `NoMetrics` (no data yet) or `ConfigUnsupported` (a target kind it can't size). |

4. The VPA's **container policies apply**. A container with `mode: Off` gets no
   recommendation. `controlledResources` limits the resources. `minAllowed` and
   `maxAllowed` cap the target and both bounds.
5. APVA writes only the VPA's `status` subresource, with a JSON merge patch.

The VPA does the rest: `updateMode` decides whether and how pods are resized (`Off`,
`Initial`, `Recreate`, `InPlaceOrRecreate`), and the VPA's eviction rules and
PodDisruptionBudgets apply.

### APVA never resizes a VPA-managed workload itself

With `--auto-resize` on as well, APVA's own actuator skips **every workload targeted by any
VPA**, whichever recommender that VPA uses. The activity log shows:

```
skipped  web  targeted by VerticalPodAutoscaler shop/web (updateMode Recreate); the VPA owns its requests
```

So you can run both: VPA-managed workloads get APVA's numbers through the VPA, and the
rest are resized by APVA directly. If APVA can't list the VPAs in a namespace, it leaves
that namespace's workloads alone for that round.

## Permissions

`vpaRecommender.enabled` adds one role (see [security.md](security.md)):

| Resource | Verbs | Why |
|---|---|---|
| `verticalpodautoscalers` (`autoscaling.k8s.io`) | `list` | find the VPAs that name APVA |
| `verticalpodautoscalers/status` | `patch` | write the recommendation, **status only** |
| `deployments`, `statefulsets` | `get` | read the pod template to split the recommendation per container |

APVA can't change a VPA's spec (update mode, policies, target, recommenders), create or
delete VPAs, or touch pods. With `analysis.namespaces` set, the role is bound only in
those namespaces. With `autoResize.enabled`, the resizer role also gets `list`
on `verticalpodautoscalers`, so that the actuator can see which workloads to leave alone.

## Limitations

- **Deployments and StatefulSets only**, the workloads APVA analyses. Other target kinds
  get `RecommendationProvided: False` with reason `ConfigUnsupported`.
- **CPU and memory only.** GPU recommendations stay in APVA's dashboard; the VPA doesn't
  size extended resources.
- **Per-container split.** APVA measures whole pods. It splits a pod's recommendation in
  proportion to current requests, so a sidecar whose share of the usage changes over time
  is not sized on its own.
- **No checkpoints or histograms.** APVA's state is its Prometheus look-back window, so a
  restart loses nothing. It doesn't write `VerticalPodAutoscalerCheckpoint` objects.

## Test

`test/e2e-vpa/run.sh` runs on every PR. It uses a real Kubernetes 1.35 API, KWOK nodes, the
real VPA CRD (`vertical-pod-autoscaler-1.8.0`) and a fake Prometheus, with APVA as its own
least-privilege ServiceAccount. It checks that:

- the VPA naming `apva` gets a per-container recommendation that respects `minAllowed`,
  with bounds and conditions;
- a VPA using the default recommender is left untouched;
- APVA doesn't resize either VPA-targeted workload itself, but still resizes one with no VPA;
- APVA's identity can't change a VPA's spec or write status outside its namespaces.
