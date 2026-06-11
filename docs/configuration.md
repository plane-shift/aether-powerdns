# Configuration reference

Every `PowerDNSServer.spec` field, what it controls, and when to set it.
Defaults are listed in the CRD schema (`config/crd/...yaml`); only
non-obvious behaviour is called out here.

## Top-level

| Field | Default | Notes |
|---|---|---|
| `replicas` | `1` | Reads/writes hit the same backend DB, so all replicas serve identical zones. Bump for HA, not throughput. |
| `image` | `powerdns/pdns-auth-51` | Floating tag — pin a digest if you need reproducible rollouts. |
| `resources` | requests `50m`/`128Mi`, limits `512Mi` | Standard `corev1.ResourceRequirements`. |

## `backend`

Only `type: postgres` is implemented. `mysql` is reserved.

```yaml
backend:
  type: postgres
  postgres:
    instances: 1                # CNPG instance count (operator-managed mode)
    storageSize: 5Gi
    storageClass: aether-block  # omit to use cluster default
```

### Bring-your-own Postgres

Skips CNPG; the operator only writes the schema and consumes the credentials.

```yaml
backend:
  type: postgres
  postgres:
    byo:
      host: postgres.example.internal
      port: 5432
      database: powerdns
      sslMode: require
      credentialsSecretRef:
        name: pdns-byo-creds   # must contain `username` + `password`
```

The schema-init Job runs against the BYO database too; PowerDNS uses
`CREATE TABLE IF NOT EXISTS`, so re-runs are idempotent on a configured DB.

## `dns.exposure`

| Value | Effect |
|---|---|
| `none` (default) | DNS Service stays `ClusterIP`. Use when fronting with your own ingress / external LB. |
| `loadBalancer` | Service flips to `type: LoadBalancer`. `externalTrafficPolicy: Local` by default so PowerDNS sees real client IPs. |
| `gateway` | Operator creates `TCPRoute` + `UDPRoute` referencing your Gateway. |

```yaml
dns:
  exposure: loadBalancer
  loadBalancer:
    ip: 10.1.0.241                  # MetalLB-pinned IP, optional
    annotations:
      metallb.io/address-pool: aether-public
    externalTrafficPolicy: Local    # or Cluster
    additionalServices:             # extra LB Services targeting the same pods
      - nameSuffix: "-2"            # -> Service `<server>-dns-2`
        ip: 10.1.0.242
        annotations: { metallb.io/address-pool: aether-public }
      - nameSuffix: "-3"
        ip: 10.1.0.243
```

For "N IPs from one pool with no per-IP customisation" prefer the
MetalLB-native `metallb.io/loadBalancerIPs: "ip1,ip2,ip3"` annotation on
the primary Service — no `additionalServices` needed.

```yaml
dns:
  exposure: gateway
  gateway:
    parentRefs:                     # one or more Gateways; same TCP/UDPRoute attaches to all
      - group: gateway.networking.k8s.io  # optional; defaults to gateway.networking.k8s.io
        kind: Gateway                     # optional; defaults to Gateway
        name: aether-edge-1
        namespace: gateway-system
        tcpSectionName: dns-tcp     # listener on this Gateway, optional
        udpSectionName: dns-udp
      - name: aether-edge-2
        namespace: gateway-system
        tcpSectionName: dns-tcp
        udpSectionName: dns-udp
```

`parentRefs` accepts full Gateway API references. `group` and `kind` are
optional and default to `gateway.networking.k8s.io` and `Gateway`
respectively — omit them for the common case. Changes to `parentRefs`
(adding/removing parents, updating section names) propagate to the live
TCPRoute and UDPRoute within the Ready loop (≤30 s).

## `api`

```yaml
api:
  port: 8081                        # PowerDNS HTTP API port
  apiKeySecretRef:
    name: my-existing-key           # BYO key Secret (must have `api-key`)
```

When `apiKeySecretRef` is unset the operator generates a 64-char hex key
into `<server>-api-key`. The API stays on ClusterIP by default — a
leaked key gives full zone control.

### `api.gateway` — optional HTTPRoute exposure

Exposes the HTTP API via a Gateway API `HTTPRoute` named
`<server>-api-http`. Independent of `spec.dns.exposure` — you can use
`dns.exposure: loadBalancer` for DNS and still attach the API to a
Gateway, or vice versa.

```yaml
api:
  gateway:
    hostnames:                        # optional; empty list matches ALL hostnames on the listener
      - pdns-api.internal.example.com
    parentRefs:                       # required; at least one entry with name set
      - group: gateway.networking.k8s.io  # optional; defaults to gateway.networking.k8s.io
        kind: Gateway                     # optional; defaults to Gateway
        name: eg
        namespace: envoy-gateway-system
        sectionName: https            # target a specific listener, optional
```

The HTTPRoute uses a single `PathPrefix /` rule forwarding to the
`<server>-api` ClusterIP Service on the API port. `status.apiEndpoint`
always reports the in-cluster ClusterIP URL — Zone and RRSet controllers
consume it directly and are not affected by gateway exposure.

When `api.gateway` is unset (the default), the API is ClusterIP-only.
Omitting `hostnames` (or setting it to an empty list — equivalent) matches **all**
hostnames on the listener; set hostnames on shared gateways to prevent
accidental matches.

**Security:** attach `api.gateway` only to TLS listeners (port 443 /
HTTPS). On a shared gateway, always set `hostnames` to prevent other
routes from accidentally matching API traffic. With
`networkPolicy.enabled=true`, the gateway's namespace (e.g.
`envoy-gateway-system`) must be listed in
`networkPolicy.additionalAllowedAPINamespaces` or the gateway pods
cannot reach the API port:

```yaml
networkPolicy:
  enabled: true
  additionalAllowedAPINamespaces:
    - envoy-gateway-system
```

An `APIExposed` event fires when the HTTPRoute is first created. Route
changes (adding/removing parentRefs or hostnames) propagate within the
Ready loop (≤30 s); removing `api.gateway` entirely deletes the
HTTPRoute.

## `scheduling`

```yaml
scheduling:
  nodeSelector:
    node-role.aetherplatform.cloud/dns: ""
  tolerations:
    - key: aetherplatform.cloud/dedicated
      operator: Equal
      value: dns
      effect: NoSchedule
  priorityClassName: system-cluster-critical
  spreadAcrossZones: true           # adds zone topology spread on top of hostname spread (replicas > 1 only)
```

`priorityClassName` matters under cluster pressure — DNS is critical infra.

## `observability.podMonitor`

Creates a Prometheus `PodMonitor` (group `monitoring.coreos.com`) scraping
PowerDNS's built-in `/metrics` endpoint on the API port. Requires the
Prometheus Operator's CRDs to be installed; otherwise the operator logs
a warning and emits a `PodMonitorUnavailable` event without failing.

```yaml
observability:
  podMonitor:
    enabled: true
    interval: 30s
    labels:
      release: kube-prometheus-stack   # match Prometheus Operator's selector
```

## `networkPolicy`

Default-deny ingress + selective allow. DNS (53/tcp + 53/udp) is open to
the world (DNS is fundamentally public). The API port is locked to the
pdns pod's namespace plus any explicitly listed namespaces.

```yaml
networkPolicy:
  enabled: true
  additionalAllowedAPINamespaces:
    - external-dns                    # if you run external-dns and it talks to the API
```

Egress is intentionally not restricted — the pod must reach Postgres,
which the operator can't enumerate generically. Keep disabled until you
have confirmed every API consumer is in an allowlisted namespace.

## CEL validations enforced at admission

- `backend.type` is immutable after creation. To swap backends, delete
  the server and re-create it (you will lose zones unless the BYO DB
  persists them).
- `dns.exposure=gateway` requires `dns.gateway` to be set (also enforced
  by the operator as a safety net).

## Status fields

| Field | Meaning |
|---|---|
| `phase` | Lifecycle phase (`Pending` → `Ready`, or `Failed`). |
| `ready` | True iff `availableReplicas == desiredReplicas`. |
| `desiredReplicas` / `readyReplicas` | Mirrored from the Deployment. |
| `dnsEndpoint` | External DNS address (`ip:53` or `gateway:ns/name` or ClusterIP). |
| `apiEndpoint` | In-cluster URL of the HTTP API. |
| `apiKeySecretName` / `backendSecretName` | Where credentials live. |
| `schemaApplied` | True after the schema-init Job succeeds. |
| `configHash` | sha256 prefix over pdns.conf + secrets. Change → rolling restart. |
| `conditions` | Standard k8s conditions: `Ready`, `BackendProvisioned`, `SchemaApplied`, `Available`. |
| `failureMessage` | Set when `phase=Failed`. |
