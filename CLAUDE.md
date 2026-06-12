# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Development Workflow

Always use Superpowers skills for this project. Begin every task by invoking
/using-superpowers before doing anything else.

## Scope

Operator owns the PowerDNS server lifecycle (provisioning, config,
exposure, API key) **plus declarative zone/record management** via two
CRDs: `Zone` and one generic `RRSet` (the record type is a spec field),
**plus a `DNSDist` frontend tier** (active-health-checked DNS load
balancer with packet cache, rate limiting, and optional DoT/DoH).
Do NOT add per-record-type CRDs — ~50 kinds would reinvent the PowerDNS
API; that decision stands. Reconciliation is patch-only/coexist: only
CR-declared rrsets are ever written, so managing records via the
PowerDNS HTTP API or `pdnsutil` still works alongside the CRDs.

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

## Releasing & dev deployment

`make deploy` is for ad-hoc clusters only. The dev mgmt cluster is
ArgoCD-managed: the `powerdns-operator` app (aether-infra
`clusters/mgmt/apps/powerdns-operator.yaml`) pins a git TAG as
`targetRevision` AND a kustomize image override — release = tag `vX.Y.Z`
(build workflow publishes `ghcr.io/...:vX.Y.Z`) → bump BOTH fields in one
aether-infra PR → hard-refresh `aether-apps` (the app-of-apps owns the
Application spec) THEN `powerdns-operator`. selfHeal reverts any manual
`kubectl set image`/`scale`/RBAC apply. Dev pins the pdns image to a
digest via `spec.image` in aether-infra's `powerdns-server` manifests.

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

Zone/RRSet reconcilers (`zone_controller.go`, `rrset_controller.go`) are
single-pass, not phase machines — every reconcile converges from scratch;
`status.phase` is informational. They talk to PowerDNS through
`internal/pdnsclient` (thin in-repo client; same no-third-party-types
reasoning as the unstructured CNPG builder) using the server's
`status.apiEndpoint` + API-key Secret. Key semantics: `spec.nameservers`
and `spec.soa` seed ONCE at zone creation (SOA-seed failure rolls the
zone back and retries); DNSSEC disable deactivates keys but never
deletes them (registrar DS stays valid); rrset conflicts reject BOTH
claimants; apply is compare-before-patch so unchanged records don't bump
the zone serial; deletion is via finalizer with `deletionPolicy: Orphan`
opt-out and wedge-proof release when the server/zone CR is already gone
or Failed. Cross-namespace refs are gated by
`PowerDNSServer.spec.zoneManagement.allowedNamespaces`. The
`refKey`-based field indexes in SetupWithManager and any test
`WithIndex` must stay identical or watches silently miss.

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
  TCP/UDP listener via `tcpSectionName` / `udpSectionName`. `parentRefs`
  items accept optional `group` (default `gateway.networking.k8s.io`) and
  `kind` (default `Gateway`) for non-standard gateway implementations.

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

The HTTP API stays ClusterIP by default. `spec.api.gateway` (optional)
additionally exposes it via a Gateway API HTTPRoute (`<server>-api-http`,
PathPrefix `/`, optional hostnames, full parentRefs incl. group/kind) —
independent of `dns.exposure`. It's an admin surface: attach only to TLS
listeners, set hostnames on shared gateways, and with
networkPolicy.enabled add the gateway's namespace to
`additionalAllowedAPINamespaces`. `status.apiEndpoint` stays the
ClusterIP URL (Zone/RRSet controllers consume it). Routes (TCP/UDP/HTTP)
are drift-corrected by `reconcileRoutes` (CreateOrUpdate from
phaseExposingDNS + reconcileDrift; disabled routes deleted).

Gateway route types are deliberately NOT in `Owns()` — their informers would
hard-require the Gateway API CRDs at manager start; drift heals on the
30s requeue instead.

When a `DNSDist` fronts the server, gateway/LB exposure moves entirely to the
`DNSDist`; the `PowerDNSServer` runs `dns.exposure: none` (see `## DNSDist`).

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

CEL cost budget (burnt in v0.1.1): spec-level rules comparing unbounded
strings get REJECTED at admission ("cost exceeds budget"). Put
immutability rules on the narrowest field/object as `self == oldSelf`,
give every string/array a CEL rule touches `maxLength`/`maxItems`, and
apply new CRDs to a real apiserver before tagging — no local check
catches this.

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
- Tests: controller-runtime fake client (`WithStatusSubresource` for any
  status writer; `WithIndex` extractors MUST mirror SetupWithManager's or
  watches silently miss), `pdnsfake_test.go` in-memory PowerDNS (fault
  injection via `failNextPatch`), `interceptor.Funcs` to simulate
  conflicts and missing-CRD `NoKindMatchError`.
- Converge-helper rules (issues #9/#13): steady state must be a WRITE
  no-op (resourceVersion churn races LB controllers); MERGE annotations,
  never replace (MetalLB/Cilium annotate Services too); preserve
  apiserver-assigned NodePorts. Job health: with `RestartPolicy:
  OnFailure` the `status.failed` counter is unreliable — gate on the
  `JobFailed` condition.
- Gateway exposure needs the Gateway API EXPERIMENTAL channel
  (TCPRoute/UDPRoute aren't in standard); the operator tolerates missing
  route CRDs (IsNoMatchError → skip deletes, clear error on ensure).

## DNSDist

`DNSDist` CRD (`dns.aetherplatform.cloud/v1alpha1`, short name `ddist`).
Single-pass reconciler — no phase machine, no finalizer (owned resources are
GC'd by owner-reference cascade). All generated resource names are prefixed
`<name>-dnsdist*`.

**Lua config** is deterministically rendered: backends are sorted
alphabetically by PowerDNSServer name before emitting `newServer()` calls.
This ensures the rendered ConfigMap is identical across reconcile passes so
the config-hash annotation is stable and rolling restarts are not triggered
spuriously. `setACL({"0.0.0.0/0", "::/0"})` is mandatory — dnsdist's default
ACL is RFC1918-only and would silently drop public queries.

**BackendRefs**: same-namespace only; the `namespace` field on each ref must
be empty. Duplicate `name` values are rejected by `validateDNSDist` — the
entire CR is failed at the first duplicate found. The operator resolves each
ref to the backing server's ClusterIP Service FQDN + port 53 (service-level
addressing, not per-pod). Children (ConfigMap, Deployment, Service, PDB,
routes) are only created once every backendRef resolves to a `PowerDNSServer`
in `phase=Ready`; a missing or non-Ready backend sets `BackendsReady=False`
and requeues without creating any child resources. After all backends are
Ready, runtime health is delegated to dnsdist's own active health checks
(`checkInterval=2, maxCheckFailures=2`).

**Exposure**: `spec.dns` mirrors `PowerDNSServer.spec.dns` exactly
(`none`/`loadBalancer`/`gateway`). Gateway routes and `additionalServices`
target the `<name>-dnsdist-dns` Service (the dnsdist pod selector), NOT the
backing pdns Services. The backing `PowerDNSServer` resources MUST run
`dns.exposure: none` — exposing both sides results in clients potentially
bypassing dnsdist.

**PDB**: rendered by `pdbFor` (shared helper). Default `minAvailable =
replicas - 1`. No PDB when `replicas <= 1`. `spec.podDisruptionBudget.
minAvailable` is validated: must be ≥ 1 and < replicas (equal-to-replicas
blocks all voluntary disruptions and is rejected).

**PowerDNS readiness probe** (added alongside DNSDist): pdns pods now use an
`exec` probe (`pdns_control rping`) instead of a plain TCP check. Confirms
the control socket is live, not just the port. Liveness stays TCP.

## Out of scope (do not add without asking)

- Per-record-type CRDs (A/AAAA/MX/… as separate kinds) — the generic
  RRSet covers all types. Also out: TSIG keys, catalog zones, zone
  comments, AXFR allow-lists beyond `masters`.
- MySQL backend — the field is reserved (`backend.type=mysql`) but the
  reconciler rejects it. Don't wire it in without confirming the user
  still wants it; Postgres-via-CNPG matches the rest of the Aether stack.
- **DNSDist is now IN scope** (see `## DNSDist` above). The following
  dnsdist capabilities are deferred — do not add without asking:
  - **Proxy-protocol client IP preservation** — dnsdist can forward the
    original client IP to PowerDNS via PROXY protocol, but that requires
    PowerDNS to be configured to accept it; deferred to v2.
  - **Per-pod backend discovery** — current addressing is service-level
    (ClusterIP), not per-pod. Pod-level discovery (e.g. via Endpoints
    watch) is deferred.
  - **Cross-namespace backends** — `backendRefs[].namespace` is rejected;
    same-namespace only in v1.
  - **Recursor support** — dnsdist can front a PowerDNS Recursor too, but
    the operator only manages authoritative servers; recursor is a
    different concern.
  - **dnsdist console / controlSocket** — exposing the dnsdist Lua console
    or control socket via the operator is not planned.
  - **DoT/DoH via gateway listeners** — TLS ports are on the Service only
    in v1; route-based exposure via dedicated Gateway TCPRoute listeners
    (port 853) requires Gateway-side listener changes and is deferred.
