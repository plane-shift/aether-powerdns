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
```

```yaml
dns:
  exposure: gateway
  gateway:
    parentRef:
      name: aether-edge
      namespace: gateway-system
    tcpSectionName: dns-tcp         # listener name on the Gateway, optional
    udpSectionName: dns-udp
```

## `api`

```yaml
api:
  port: 8081                        # PowerDNS HTTP API port
  apiKeySecretRef:
    name: my-existing-key           # BYO key Secret (must have `api-key`)
```

When `apiKeySecretRef` is unset the operator generates a 64-char hex key
into `<server>-api-key`. The API stays on ClusterIP — never exposes
externally — because a leaked key gives full zone control.

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
