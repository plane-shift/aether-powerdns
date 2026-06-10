# Design: Zone & RRSet CRDs for aether-powerdns

**Status: APPROVED**
Date: 2026-06-11

Revision points explicitly offered and declined by the user (decisions stand
as designed): RRSet name/type immutability; `deletionPolicy: Delete` default;
`spec.nameservers` authoritative post-create; DNSSEC disable deletes keys;
conflict rule favors the older RRSet.

## Decision context

This revisits the deliberate scope decision in CLAUDE.md ("the operator only
owns the PowerDNS server lifecycle; zones/records are managed via the HTTP
API or `pdnsutil`"). The user has requested declarative zone and record
management via cluster resources. The original concern — a ~50-CRD explosion
of per-record-type kinds — is honored by using **one generic RRSet kind**,
mirroring the PowerDNS API's rrset model.

Decisions already made with the user:

| Question | Decision |
|---|---|
| CRD shape | `Zone` + one generic `RRSet` CRD (record type is a spec field) |
| Drift model | Patch-only / coexist — only CR-declared rrsets are reconciled; out-of-band records untouched |
| Deletion | Finalizer deletes from PowerDNS by default; `deletionPolicy: Orphan` opts out |
| v1 zone scope | Zone kinds (Native/Primary/Secondary), SOA/NS bootstrap, DNSSEC enable/secure, cross-namespace refs |
| API client | Thin in-repo client (`internal/pdnsclient`), no third-party PowerDNS library |
| Cross-ns authorization | Allow-list on the server: `spec.zoneManagement.allowedNamespaces` |

## 1. Shape

Two new namespaced CRDs in `dns.aetherplatform.cloud/v1alpha1`, each with its
own reconciler, plus a thin HTTP client:

- **`Zone`** — zone lifecycle: create/delete, kind, replication masters,
  SOA/NS bootstrap, DNSSEC.
- **`RRSet`** — one record set (name + type + TTL + content strings), the
  PowerDNS API's native unit of change.
- **`internal/pdnsclient`** — ~300-line client wrapping exactly the endpoints
  used: `GetZone`, `CreateZone`, `PutZoneMetadata`, `DeleteZone`,
  `PatchRRSets`, `ListCryptokeys`, `CreateCryptokey`, `DeleteCryptokey`.
  Targets `http://<server>-api.<ns>.svc:<apiPort>/api/v1` with `X-API-Key`
  read from the server's existing API-key Secret. Returns a typed
  `ErrNotFound` so reconcilers distinguish "create it" from real failures.

`internal/manifests` stays pure and untouched. The new controllers render no
Kubernetes objects — they only call the DNS API — so the existing
`ensureOwned`/GC story is unaffected.

## 2. Zone CRD

```yaml
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: Zone
metadata:
  name: example-com
  namespace: dns-team
spec:
  serverRef:                    # immutable
    name: my-pdns
    namespace: dns-system       # optional, defaults to the Zone's namespace
  zoneName: example.com.        # immutable, CEL-enforced trailing dot
  kind: Native                  # Native | Primary | Secondary; default Native; mutable
  masters: []                   # CEL: required iff kind=Secondary, forbidden otherwise
  nameservers:                  # seeds apex NS+SOA at creation; afterwards the
    - ns1.example.com.          # apex NS rrset is operator-declared and kept in sync
  soa:                          # optional; PowerDNS defaults apply otherwise
    hostmaster: hostmaster.example.com.
    ttl: 3600
  dnssec:
    enabled: true               # secures via /cryptokeys with PowerDNS defaults;
                                # disabling unsecures (deletes keys)
  deletionPolicy: Delete        # Delete | Orphan; default Delete
status:
  phase: Ready
  conditions: []                # Ready, Registered, DNSSECReady
  serial: 2026061101
  dsRecords: []                 # populated when DNSSEC enabled
  observedGeneration: 3
  failureMessage: ""
```

Reconcile flow: resolve server (must exist, be `Ready`, and authorize the
Zone's namespace) → ensure finalizer → `GET` zone, create if missing →
correct drift on kind/masters → reconcile apex NS from `spec.nameservers` →
ensure/remove DNSSEC keys, surface DS records in status → write serial +
conditions. Periodic 5m resync corrects out-of-band drift on declared fields
only.

## 3. RRSet CRD

```yaml
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: RRSet
metadata:
  name: www-example-com
  namespace: app-team
spec:
  zoneRef:                      # immutable; references the Zone CR
    name: example-com
    namespace: dns-team         # optional, defaults to the RRSet's namespace
  name: www.example.com.        # immutable; controller validates it's within the zone
  type: A                       # immutable; free-form uppercase string (CEL pattern), no per-type enum
  ttl: 300                      # default 3600; mutable
  records:                      # content passthrough, mutable
    - "203.0.113.10"
status:
  conditions: []                # Ready
  observedGeneration: 1
  failureMessage: ""
```

`name`/`type`/`zoneRef` immutability (CEL) is deliberate: reconcile becomes a
pure idempotent `PATCH changetype=REPLACE` with no "track what I used to be
called and clean it up" state. Renaming = delete + recreate the CR; the
finalizer handles cleanup.

Guards:
- Target zone must be `Ready` and must not be `Secondary` (its content is
  replicated; writes are rejected).
- Two RRSets declaring the same (zone, name, type): the newer one (by
  creationTimestamp, UID tiebreak) goes `Ready=False` reason `Conflict`
  rather than fighting over the rrset.

## 4. Coexistence, deletion, cross-namespace

- **Patch-only drift model**: the operator only ever PATCHes rrsets declared
  in CRs (plus the apex NS when `spec.nameservers` is set). Records created
  via the HTTP API or `pdnsutil` are never touched — the existing workflow
  survives per zone, even alongside CRs.
- **Deletion**: finalizers remove the rrset (`changetype=DELETE`) / zone (API
  delete) unless `deletionPolicy: Orphan`. **Wedge-proofing**: if the
  referenced `PowerDNSServer` (or `Zone`) is already gone or itself being
  deleted, the finalizer releases *without* an API call — the data dies with
  the server's Postgres anyway, and this prevents stuck teardowns.
- **Cross-namespace**: `PowerDNSServer.spec.zoneManagement.allowedNamespaces:
  ["team-a", "*"]` — the only change to the existing CRD. Same-namespace is
  always allowed; the list governs both Zones referencing the server and
  RRSets whose zoneRef crosses namespaces. Unauthorized refs → `Ready=False`
  reason `NamespaceNotAllowed`.
- **NetworkPolicy interaction**: if `spec.networkPolicy.enabled=true` and the
  operator runs outside the server's namespace, the operator's namespace must
  be listed in `additionalAllowedAPINamespaces` or zone reconciliation times
  out. Documented, and surfaced as an `APIUnreachable` condition rather than
  silent retry.

## 5. Controller wiring, validation, testing

- `ZoneReconciler`: `For(Zone)` + `Watches(PowerDNSServer → zones)` via a
  field index on `spec.serverRef`. `RRSetReconciler`: `For(RRSet)` +
  `Watches(Zone → rrsets)` via an index on `spec.zoneRef`. Both follow the
  existing condition-helper patterns in `conditions.go`. `setFailed`-style
  terminal states only for invalid specs; transient API errors requeue with
  backoff.
- **Validation split per repo convention**: CEL for everything
  single-resource (immutability, trailing dot, Secondary⇔masters, type
  pattern); controller-side for cross-resource checks (server ready, authz,
  name-within-zone, zone-kind).
- **Hand-maintained artifacts** (no controller-gen): two new CRD YAMLs in
  `config/crd/`, Go types in `api/v1alpha1/` (new `zone_types.go` /
  `rrset_types.go`), deepcopy additions in `zz_generated.deepcopy.go`.
- **Testing**: `pdnsclient` against `httptest` fixtures of real PowerDNS API
  responses; reconcilers unit-tested with fake k8s client + an httptest fake
  PowerDNS, matching the existing test style. New examples under `examples/`
  (zone-basic, zone-secondary, zone-dnssec, rrset).
- **Docs**: README, `docs/managing-zones.md`, and CLAUDE.md scope section
  rewritten — zones/records are now in scope as CRDs; the "no per-record-type
  CRDs" rule stays (one generic RRSet kind, not ~50).

## Out of scope for v1

Per-record-type CRDs, TSIG keys, catalog zones, zone comments, AXFR
allow-lists beyond `masters`, MySQL backend, and importing third-party
PowerDNS client types.
