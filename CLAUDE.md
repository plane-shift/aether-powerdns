# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Scope

Operator that **only owns the PowerDNS server lifecycle** — provisioning,
config, exposure, API key. It deliberately does not model zones, records or
DNSSEC keys. Users manage those via the PowerDNS HTTP API (with the operator-
generated key) or `pdnsutil` (`kubectl exec`). Don't add per-record CRDs
without revisiting that decision with the user — it would explode the CRD
surface (~50 record types) and reinvent what the PowerDNS API already does.

## Commands

```bash
make tidy           # go mod tidy
make build          # go build ./cmd/operator
make vet            # go vet ./...
make test           # go test ./...
make image TAG=dev  # local docker build (current arch)
make image-push     # multi-arch buildx --no-cache --push
make crd            # kubectl apply CRD
make deploy         # kubectl apply -k config/
```

Run a single test: `go test ./internal/controller/... -run TestName`.

## Architecture

Single CRD `PowerDNSServer` (group `dns.aetherplatform.cloud/v1alpha1`)
drives a phase-based reconciler in `internal/controller`:

```
Pending → ProvisioningBackend → InitializingSchema → DeployingServer → ExposingDNS → Ready
```

Each phase is one method. `setPhase` writes status and requeues — never
chain phases inside one call. `setFailed` is terminal.

`internal/manifests` is **pure**: every function takes a `*PowerDNSServer`
and returns a Kubernetes object. No client calls. Controller wraps each in
`ensureOwned` (sets owner ref + create-if-missing) so cascading delete
works through Kubernetes GC.

`internal/cnpg` builds the CloudNativePG `Cluster` as **`unstructured`** on
purpose — we don't import CNPG's Go types so a CNPG version bump doesn't
force us to bump too. CNPG writes the `<cluster>-app` Secret; the
controller reads it and re-publishes the credentials into the operator's
own backend Secret (`<server>-backend`) with stable keys
(`host`/`port`/`username`/`password`/`database`). Deployment env vars and
the schema-init Job both read from that single normalized Secret, so BYO
and managed modes share the same downstream code path.

PowerDNS reads gpgsql credentials via env vars: `PDNS_GPGSQL_HOST`,
`PDNS_GPGSQL_PORT`, etc. (the `PDNS_<SETTING>` convention). We map them
from `PG*` so the same Secret feeds both `psql` (schema Job) and PowerDNS.

## Image

Default: `powerdns/pdns-auth-51` (floating tag — rolls forward on patch
releases). Pin a digest in `spec.image` if you need reproducibility.

The schema-init Job uses an init container running the pdns image to
**copy the bundled schema** from `/usr/share/doc/pdns/schema.pgsql.sql`,
then a `postgres:16-alpine` container runs `psql -f`. We deliberately do
not vendor the schema — letting the image own it means the schema always
matches the running PowerDNS version.

## Pod lifecycle

`spec.replicas` is enforced by the underlying Deployment, but the operator
**also** owns the workload lifecycle:

- `Owns(&appsv1.Deployment{} | corev1.{Secret,ConfigMap} | batchv1.Job |
  policyv1.PodDisruptionBudget{})` — the controller wakes within seconds
  on any owned-resource change (manual scale, secret rotation, pod loss),
  not just on the 30s requeue.
- `reconcileDrift` re-renders the Deployment + Services + PDB on every
  Ready-loop and overwrites the live spec, so manual `kubectl scale` and
  config edits get reverted.
- `refreshReplicaStatus` mirrors `Deployment.status.{availableReplicas,
  readyReplicas}` onto `PowerDNSServer.status.{readyReplicas,
  desiredReplicas}` and recomputes `status.ready` each pass — a crashed
  pod surfaces in `kubectl get pdns`.
- A pod-template annotation
  `dns.aetherplatform.cloud/config-hash` (sha256 of `pdns.conf` + API
  key + backend secret data) drives a **rolling restart on any config
  change**. PowerDNS doesn't reload `pdns.conf` at runtime, so we
  deliberately roll the pods.
- When `replicas > 1`: a PodDisruptionBudget (`minAvailable: replicas-1`)
  blocks node-drain take-everything-down, plus soft pod anti-affinity +
  topology spread (hostname) to keep replicas off the same node. Both
  go away when scaled back to 1 (a 1-replica PDB would block all drains).

## DNS exposure

`spec.dns.exposure`:
- `none` — ClusterIP only
- `loadBalancer` — Service `type=LoadBalancer`, `ExternalTrafficPolicy=Local`
  by default so PowerDNS sees real client source IPs (important for ACL/logging)
- `gateway` — Gateway API `TCPRoute` + `UDPRoute` attached to one or more
  user-supplied Gateways via `parentRefs[]`. Each parent may pick its own
  TCP/UDP listener via `tcpSectionName` / `udpSectionName`.

For "one Deployment, multiple public IPs" use either:
- `spec.dns.loadBalancer.additionalServices[]` — extra LB Services
  targeting the same pod selector. Each can have its own IP / pool /
  annotations / `externalTrafficPolicy`. Removed entries are GC'd by
  label selector (`dns.aetherplatform.cloud/role=additional-dns`).
- For homogeneous "N IPs from one MetalLB pool", prefer
  `metallb.io/loadBalancerIPs: "ip1,ip2,…"` on the primary Service —
  no extra Services needed.

For "one Deployment, multiple Gateways" use `gateway.parentRefs[]` —
TCPRoute and UDPRoute support multi-parent natively, so still only one
route resource per protocol.

The HTTP API stays on ClusterIP. **Don't** expose it externally by default —
it's an admin surface, and a leaked key gives full zone control.

## Observability

`spec.observability.podMonitor.enabled=true` creates a `PodMonitor`
(monitoring.coreos.com/v1, rendered as unstructured so we don't import
prometheus-operator types) scraping PowerDNS's built-in `/metrics`
endpoint on the API webserver port. If the CRD isn't installed,
the operator logs a warning and emits a `PodMonitorUnavailable` event
instead of going `Failed`.

`status.conditions` reports `Ready`, `BackendProvisioned`,
`SchemaApplied`, `Available` (and `Failed` reason on `Ready=False`).
Pair with `kubectl describe pdns` events for the lifecycle log.

## Validation

Two layers:
- **CEL on the CRD** (`x-kubernetes-validations`): `backend.type` is
  immutable post-create. Catches transitions before the operator wakes.
- **Operator-side `validateSpec`** in `phasePending`: rejects unsupported
  backend types, enforces required BYO fields, and gates
  `dns.exposure=gateway` on `dns.gateway` being set. Runs once per spec
  change and short-circuits to `Failed` with `status.failureMessage`.

When extending validation, prefer CEL — it runs at admission and surfaces
in `kubectl apply` errors. Fall back to the controller only for
cross-resource checks CEL can't express.

## Network policy

`spec.networkPolicy.enabled=true` installs a Kubernetes `NetworkPolicy`:
- DNS (53/tcp + 53/udp): from anywhere (DNS is fundamentally public)
- API (port from `spec.api.port`, default 8081/tcp): from the pdns pod's
  own namespace, plus any in `spec.networkPolicy.additionalAllowedAPINamespaces`
  (matched on the standard `kubernetes.io/metadata.name` label, K8s 1.22+)

Egress is intentionally not restricted — the pod must reach Postgres,
which the operator can't enumerate generically. Default off because it
denies anything not whitelisted; turn on after confirming your
admin/scrape paths.

## Conventions

- Resource names follow `aether-*` and use `app.kubernetes.io/managed-by:
  aether-powerdns` so they're easy to find/grep.
- All owned resources go through `ensureOwned` so they GC with the parent.
- The CNPG `Cluster` is owned by the `PowerDNSServer` (controller ref) so
  deleting the server reaps Postgres + PVC.

## Tooling notes

- No `kubebuilder` / `controller-gen` installed. CRD YAML in
  `config/crd/` and deepcopy in `api/v1alpha1/zz_generated.deepcopy.go`
  are **hand-maintained**. When adding a field, edit both — `go vet ./...`
  catches deepcopy gaps for slice/map/pointer fields, but won't catch
  CRD-vs-types drift.
- Module path `github.com/plane-shift/aether-powerdns`, repo lives at
  `plane-shift/aether-powerdns` on GitHub (per the org convention shared
  with `aether-operator`).

## Out of scope (do not add without asking)

- Zone/Record/DNSSEC-key CRDs — explicitly deferred to the API + `pdnsutil`.
- MySQL backend — the field is reserved (`backend.type=mysql`) but the
  reconciler rejects it. Don't wire it in without confirming the user
  still wants it; Postgres-via-CNPG matches the rest of the Aether stack.
- DNS-over-TLS / DoH — PowerDNS Auth doesn't serve those; that would mean
  bringing in `dnsdist`, which is a separate operator concern.
