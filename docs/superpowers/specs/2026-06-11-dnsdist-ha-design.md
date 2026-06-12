# Design: DNSDist CRD + HA/PDB hardening

**Status: APPROVED**
Date: 2026-06-11

## Decision context

The DNS tier needs (a) a DNS-aware frontend for health-checked failover,
caching, and abuse protection, (b) confidence in the existing multi-pod
HA story, and (c) PDB guarantees that at least one DNS pod always
answers. Multi-replica support, anti-affinity, topology spread, and a
`replicas-1` PDB already exist on `PowerDNSServer` — the new work is the
dnsdist subsystem plus targeted hardening.

Decisions made with the user:

| Question | Decision |
|---|---|
| Modeling | Separate `DNSDist` CRD with `backendRefs[]` to PowerDNSServers — composable (one frontend tier over many servers), servers stay usable without it |
| v1 surface | Backends + active DNS health checks + packet cache; per-client rate limiting; DoT/DoH listeners. **Proxy-protocol deliberately omitted** — pdns sees dnsdist pod IPs as clients (future one-flag addition) |
| PDB | `spec.podDisruptionBudget.minAvailable` added to BOTH kinds; default stays `replicas-1`; replicas=1 still renders no PDB |
| HA hardening | Real DNS responsiveness readiness probe on pdns pods (replacing the TCP-socket probe); no speculative features |
| Topology (user-stated) | With a DNSDist in front, Gateway API TCP/UDP routes target the **dnsdist Service :53**; the PowerDNSServer runs `dns.exposure: none` (ClusterIP), reached by dnsdist internally |

## 1. DNSDist CRD (`dns.aetherplatform.cloud/v1alpha1`, namespaced)

```yaml
spec:
  replicas: 2                        # default 1
  image: powerdns/dnsdist-19         # default, floating; digest-pinnable
  resources: {}                      # corev1.ResourceRequirements
  scheduling: {}                     # reuses SchedulingSpec
  backendRefs:                       # >=1, PowerDNSServers in the SAME namespace
    - name: aether-dns               # (reuses ObjectRef; namespace must be empty in v1)
  dns:                               # reuses the existing DNSSpec type verbatim:
    exposure: none|loadBalancer|gateway   # multi-parent gateways, additionalServices,
    gateway: {parentRefs: [...]}          # LB IPs/annotations — all existing machinery
  cache:
    enabled: true                    # default true
    maxEntries: 100000               # default 100000, min 1024
  rateLimit:
    qpsPerClient: 0                  # default 0 = disabled; >0 renders a dynamic-block rule
  tls:
    dot: {enabled: false, certificateSecretRef: {name: ...}}  # TLS Secret, listener :853
    doh: {enabled: false, certificateSecretRef: {name: ...}}  # listener :443, path /dns-query
  podDisruptionBudget:
    minAvailable: 1                  # optional int; default replicas-1
status:
  phase (Pending|Ready|Failed — informational), readyReplicas, desiredReplicas,
  dnsEndpoint, observedGeneration, failureMessage,
  conditions: Ready, BackendsReady, Available
```

Validation: CEL bounds on every string/array (cost-budget rule);
controller-side: backendRefs non-empty with names, namespace field empty
(same-ns v1), DoT/DoH enabled ⇒ certificateSecretRef set, referenced
backends must exist and be Ready before the tier reports Ready.

## 2. Rendered resources (collision-proof `<name>-dnsdist*` names)

- **ConfigMap `<name>-dnsdist-config`** — `dnsdist.conf` (Lua), rendered
  deterministically (sorted backends):
  - `setACL({"0.0.0.0/0", "::/0"})` — dnsdist's DEFAULT ACL allows only
    RFC1918; without this a public frontend silently refuses everything.
  - `setLocal("0.0.0.0:53", {reusePort=true})`.
  - One `newServer({address="<server>-dns.<ns>.svc.cluster.local:53",
    name="<server>", checkInterval=2, maxCheckFailures=2, rise=1})` per
    backendRef — active DNS health checks with auto up/down. v1
    limitation (documented): backends address the server's ClusterIP DNS
    Service, not individual pods; per-pod discovery is v2.
  - Packet cache: `pc = newPacketCache(<maxEntries>, {maxTTL=86400})`;
    `getPool(""):setCache(pc)` when enabled.
  - Rate limit (when qpsPerClient>0): `dynBlockRulesGroup` with
    `setQueryRate(<qps>, 10, "rate-limited", 60)` applied from a
    `maintenance()` hook.
  - DoT: `addTLSLocal("0.0.0.0:853", "/tls/dot/tls.crt", "/tls/dot/tls.key")`;
    DoH: `addDOHLocal("0.0.0.0:443", "/tls/doh/...", "/dns-query")` —
    cert Secrets mounted as volumes; no secret material in the ConfigMap.
- **Deployment `<name>-dnsdist`** — dnsdist container (`--disable-syslog`,
  conf from the ConfigMap mount), ports 53/tcp+udp (+853/+443 when
  enabled), `NET_BIND_SERVICE`-style securityContext mirroring the pdns
  container's approach, config-hash pod annotation (sha256 of the conf +
  mounted cert names) for rolling restarts, replicas/anti-affinity/
  topology-spread/scheduling identical to the server Deployment's
  pattern, readiness probe TCP:53 (the *backend* DNS-health gating lives
  in dnsdist's own health checks; an exec DNS self-probe is v2).
- **Service `<name>-dnsdist-dns`** — 53/tcp+udp (+853 dot, +443 doh),
  type per `dns.exposure` (the same Service/additionalServices/route
  machinery as servers). **Gateway exposure renders TCPRoute/UDPRoute
  `<name>-dnsdist-{tcp,udp}` backending THIS Service** — the user-stated
  topology.
- **PDB `<name>-dnsdist-pdb`** — same semantics as servers (below).

## 3. Shared machinery refactors

- `gatewayParents` + the TCP/UDP route renderers become source-agnostic:
  parametrized on (DNSSpec, namespace, names, backend service/port,
  labels) so server and dnsdist exposure share one implementation —
  no copy-paste drift of the parentRef construction rules.
- `reconcileRoutes`-equivalent logic for DNSDist uses the same
  CreateOrUpdate/deleteIfExists/no-match-tolerance patterns (all v0.2.x
  lessons apply).

## 4. PDB + probes (both kinds)

- New shared `PDBSpec{ MinAvailable *int32 }` under
  `spec.podDisruptionBudget` on PowerDNSServer AND DNSDist. Default
  (unset) keeps today's `replicas-1`. Explicit values are clamped
  validated 1..replicas-1 by the controller (a PDB with
  minAvailable==replicas blocks ALL drains — rejected in validateSpec).
  replicas<=1 renders no PDB (documented).
- pdns readiness probe: replaced with a real DNS responsiveness exec
  probe using in-image tooling. The implementation plan verifies the
  exact command inside `powerdns/pdns-auth-51` (candidates:
  `pdns_control rping`, `sdig 127.0.0.1 53 <zone> SOA`) via a container
  run before wiring it; liveness stays TCP (cheap, restart-deciding).

## 5. Reconciler

`DNSDistReconciler` — single-pass converge (zone-controller style, not a
phase machine): validate spec → resolve backends (exist + phase Ready;
else `BackendsReady=False` + requeue) → converge ConfigMap → compute
config hash → Deployment → Services → PDB → routes → status
(readyReplicas mirror, dnsEndpoint per exposure — live-derived like the
server's v0.2.3 behavior). Field index on backendRefs +
`Watches(PowerDNSServer→dnsdists)` so backend lifecycle wakes the tier.
Owns() the core workload kinds (Deployment/Service/ConfigMap/PDB); route
kinds excluded from Owns() as established.

## 6. Testing & verification

- Render tests: dnsdist.conf golden assertions (ACL line, newServer
  per backend with sorted order, cache/rate-limit/DoT/DoH toggles),
  determinism (N renders DeepEqual), Deployment/Service/PDB shape,
  minAvailable override on both kinds, new readiness probe.
- Controller tests (fake client): backend-not-ready gating, conf-hash
  roll on backend list change, PDB render/removal across replica counts,
  route lifecycle reusing the shared machinery, deletion via owner refs.
- Live e2e on dev: scratch PowerDNSServer (replicas 2, exposure none) +
  DNSDist (replicas 2, cache on, gateway exposure on a dedicated
  throwaway Gateway) → dig TCP+UDP through gateway→dnsdist→pdns; kill a
  pdns pod and confirm dnsdist health-check failover keeps answering;
  PDB blocks a drain below minAvailable; full teardown.

## Out of scope (v1)

Proxy-protocol client IPs, cross-namespace backendRefs, per-pod backend
discovery (headless service), dnsdist metrics PodMonitor, DoT/DoH via
gateway listeners (v1 exposes 853/443 only on the LB/ClusterIP Service),
dnsdist console/controlSocket, recursor support.
