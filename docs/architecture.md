# Architecture

What the operator deploys, how the reconciler is wired, and the design
decisions worth knowing before changing anything.

## Resources owned per `PowerDNSServer`

```text
PowerDNSServer (CR)
├─ Secret           <name>-api-key             # generated, key: api-key
├─ Secret           <name>-backend             # normalized PG host/port/user/pass/db
├─ ConfigMap        <name>-config              # pdns.conf
├─ Job              <name>-schema-init         # idempotent, copies schema from pdns image, runs psql
├─ CNPG Cluster     <name>-pg                  # only in operator-managed mode (unstructured)
├─ Deployment       <name>                     # pdns-auth, replicas from spec
├─ Service          <name>-api                 # ClusterIP, port 8081
├─ Service          <name>-dns                 # ClusterIP|LoadBalancer, ports 53/tcp + 53/udp
├─ PodDisruptionBudget <name>-pdb              # only when replicas > 1
├─ NetworkPolicy    <name>-netpol              # only when spec.networkPolicy.enabled
├─ PodMonitor       <name>-metrics             # only when spec.observability.podMonitor.enabled
└─ TCPRoute / UDPRoute  <name>-dns-{tcp,udp}   # only when dns.exposure=gateway
```

All children carry an owner reference to the `PowerDNSServer` so
`kubectl delete pdns` cascades cleanly.

## Reconciler

Phase-based state machine in `internal/controller/powerdnsserver_controller.go`:

```text
Pending ─► ProvisioningBackend ─► InitializingSchema ─► DeployingServer ─► ExposingDNS ─► Ready
                                                                               │
                                                                               ▼
                                                                          (drift loop)
```

- Every phase is one method. `setPhase` writes status and requeues —
  phases never chain inside a single Reconcile call.
- `setFailed` is terminal; the reconciler stops touching the resource
  until the spec or status is mutated.
- The `Ready` loop (`reconcileDrift` + `refreshReplicaStatus`) re-renders
  every owned manifest, reverts manual edits, and mirrors live replica
  counts to status. Runs every 30s plus on every owned-resource change
  (Owns watches).

## Manifests are pure

`internal/manifests/*.go` functions take a `*PowerDNSServer` and return a
Kubernetes object. No client calls, no I/O. Unit-testable in isolation;
the controller wraps each with `ensureOwned` (set owner ref +
create-if-missing).

## Why CNPG via `unstructured`?

`internal/cnpg/cnpg.go` builds the CNPG `Cluster` object via
`unstructured.Unstructured` rather than importing CNPG's Go types. This
means a CNPG version bump doesn't force a corresponding operator
release. Trade-off: no compile-time validation against CNPG's schema —
mistakes surface at apply-time. We accept that for the small surface
area we use (`spec.{instances, storage, bootstrap.initdb}`).

## Backend secret normalization

Both managed and BYO modes converge on a single Secret
(`<name>-backend`) with stable keys:

```text
host / port / username / password / database
```

The Deployment env vars and the schema-init Job both read from that
Secret. So:

- **Managed:** controller polls CNPG's `<cluster>-app` Secret, copies
  the relevant fields across.
- **BYO:** controller reads the user-supplied credentials Secret and
  copies into the same shape.

This isolates downstream code from "where did the credentials come
from?" and lets the schema Job + Deployment share env wiring.

## Configuration rollout

PowerDNS doesn't reload `pdns.conf` at runtime. We force a Deployment
rollout on any config-relevant change via a pod-template annotation:

```text
dns.aetherplatform.cloud/config-hash = sha256(pdns.conf || api-key-secret-data || backend-secret-data)[:16]
```

When the hash changes, the pod template's hash changes, the
ReplicaSet's hash changes, and the Deployment rolls per its
`RollingUpdate` strategy. No external reload sidecar needed.

## Validation layers

1. **CRD schema** — types, enums, defaults, ranges.
2. **CEL `x-kubernetes-validations`** on the CRD — immutability rules
   (`backend.type` can't change after create). Fires at admission, so
   `kubectl apply` rejects bad transitions.
3. **Operator `validateSpec`** in `phasePending` — cross-field +
   semantic checks CEL can't express cleanly. Failures land on
   `status.failureMessage` with `phase=Failed`.

We don't run a webhook — webhooks add cert-management complexity, and
the rules we have today fit in CEL + controller checks.

## Watches

```go
For(&PowerDNSServer{}).
Owns(&Deployment{}).
Owns(&Service{}).
Owns(&Secret{}).
Owns(&ConfigMap{}).
Owns(&Job{}).
Owns(&PodDisruptionBudget{}).
Owns(&NetworkPolicy{})
```

Owns watches reconcile us within seconds when an owned child changes —
manual scale, secret rotation, pod loss, ConfigMap edit. Periodic
30s requeue is the safety net, not the primary trigger.

## Out of scope (deliberate)

- **Zone / Record / DNSSEC-key CRDs** — would require modelling ~50
  RFC record types and would reinvent the PowerDNS HTTP API. Use
  `pdnsutil` or the API directly (see `docs/managing-zones.md`).
- **MySQL backend** — reserved enum value, but not implemented. CNPG
  Postgres matches the rest of the Aether stack.
- **DoH / DoT** — PowerDNS Auth doesn't serve those; that needs `dnsdist`
  and is a separate operator concern.
- **HPA** — DNS auth load is predictable; capacity planning beats
  autoscaling for this workload.
- **Backups** — CNPG's Barman config is exposed by CNPG itself; for BYO
  Postgres, backups are the database owner's responsibility.
