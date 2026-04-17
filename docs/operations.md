# Operations

Day-2 tasks: scaling, upgrades, rotating credentials, and reading the
operator's signals when things go wrong.

## Inspect state

`kubectl get pdns` shows phase + replica health at a glance:

```text
NAME   PHASE   BACKEND    READY   DESIRED   DNS              API                                    AGE
demo   Ready   postgres   3       3         10.1.0.241:53    http://demo-api.aether-system.svc:8081 4h
```

For the full story, look at conditions + events:

```bash
kubectl describe pdns -n aether-system demo
```

Key conditions:

| Type | Meaning when `True` |
|---|---|
| `BackendProvisioned` | DB credentials published to the backend Secret |
| `SchemaApplied` | Schema-init Job succeeded |
| `Available` | All Deployment replicas Ready |
| `Ready` | Above conditions met; server is serving |

## Scale

Edit `spec.replicas`. The operator overwrites the live `Deployment.spec.replicas`
on every reconcile, so `kubectl scale deploy <name>` reverts within 30s.

Scaling **up** past 1 also creates the `PodDisruptionBudget` and adds
soft anti-affinity / topology spread on hostname (and zone, if
`spec.scheduling.spreadAcrossZones=true`).

Scaling **down** to 1 deletes the PDB so node drains aren't blocked.

## Roll on config change

The operator stamps `dns.aetherplatform.cloud/config-hash: <sha256-prefix>`
onto the pod template. The hash covers:

- The rendered `pdns.conf`
- Every key in the API-key Secret
- Every key in the backend Secret (host/port/db/user/password)

Any change to those bumps the annotation, which forces the Deployment to
rollout. PowerDNS doesn't reload `pdns.conf` at runtime, so this is the
only safe way.

## Rotate the API key

```bash
kubectl delete secret -n aether-system demo-api-key
```

The operator regenerates a fresh key on next reconcile (~5s), the
config-hash flips, the Deployment rolls. Apps that cached the old key
will start getting `401`. There's no zero-downtime rotation today —
intentionally; if you need that, BYO via `spec.api.apiKeySecretRef.name`
and orchestrate it externally.

## Rotate Postgres credentials

**Operator-managed CNPG:** edit the CNPG `Cluster` (or trigger CNPG's
own rotation). The operator polls the CNPG `<cluster>-app` Secret and
re-publishes into `<server>-backend`; the config-hash flips, the
Deployment rolls automatically.

**BYO:** update the Secret referenced by
`spec.backend.postgres.byo.credentialsSecretRef`. Same propagation.

## Upgrade PowerDNS

Set `spec.image` to a pinned digest:

```yaml
spec:
  image: powerdns/pdns-auth-51@sha256:...
```

The operator overwrites the Deployment's pod template on next reconcile;
RollingUpdate strategy replaces pods without downtime (provided
`replicas > 1`).

## Upgrade the operator

The operator and the data plane are decoupled — `kubectl apply` the new
operator Deployment image:

```bash
kubectl set image -n aether-system deployment/aether-powerdns \
  operator=ghcr.io/plane-shift/aether-powerdns:v0.2.0
```

Existing PowerDNSServer resources keep running; the operator just
resumes reconciling them. Apply any new CRD revision **first**.

## Recovering from `Failed`

```bash
kubectl get pdns -n aether-system demo -o jsonpath='{.status.failureMessage}'
```

Common causes:

| Message | Fix |
|---|---|
| `backend type "mysql" not supported` | Set `backend.type: postgres`. |
| `backend.postgres.byo.host is required` | Add the missing BYO field. |
| `dns.exposure=gateway requires dns.gateway` | Add a `gateway` block. |
| `schema init Job exhausted retries` | `kubectl logs -n aether-system job/demo-schema-init` — usually a Postgres connectivity / auth issue. After the fix, delete the Job: `kubectl delete job demo-schema-init` and the operator re-runs it. |

`Failed` is terminal — the reconciler stops touching the resource. To
retry after fixing the cause, edit `status.phase` to `""` (use
`kubectl edit --subresource=status pdns demo`) or delete and recreate.

## Watching events

```bash
kubectl get events -n aether-system --field-selector involvedObject.name=demo --sort-by=.lastTimestamp
```

Lifecycle events the operator emits: `Provisioning`, `BackendProvisioned`,
`SchemaApplied`, `Ready`, `Degraded`, `Failed`, `PodMonitorUnavailable`.

## Metrics

With `spec.observability.podMonitor.enabled=true` and Prometheus
Operator installed, scrape series include
`pdns_auth_*` (queries, cache hit, errors). The operator's own
controller-runtime metrics are on `:8080/metrics` of the operator pod.
