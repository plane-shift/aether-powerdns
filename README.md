# aether-powerdns

Kubernetes operator that runs the [PowerDNS authoritative server](https://doc.powerdns.com/authoritative/)
backed by a Postgres database, with the HTTP API protected by a generated key.

The operator manages the **server lifecycle** and provides declarative
`Zone` and `RRSet` resources for GitOps-style DNS management. The PowerDNS
HTTP API and `pdnsutil` still work alongside the CRDs — records you manage
imperatively are never overwritten.

## Documentation

- [Getting started](docs/getting-started.md) — install, deploy, smoke-test
- [Configuration reference](docs/configuration.md) — every spec field
- [Managing zones and records](docs/managing-zones.md) — declarative Zone/RRSet CRDs, HTTP API + `pdnsutil` recipes
- [Operations](docs/operations.md) — scaling, upgrades, key rotation, troubleshooting
- [Architecture](docs/architecture.md) — owned resources, reconciler design, trade-offs
- [Examples index](examples/README.md) — ready-to-apply scenarios

## DNSDist frontend tier

A `DNSDist` resource deploys a [dnsdist](https://dnsdist.org) load-balancer
tier in front of one or more `PowerDNSServer` backends in the same namespace.
dnsdist health-checks each backend and stops routing to unhealthy replicas —
combined with a `replicas: 2` `PowerDNSServer` this gives automatic failover
within seconds during a single-pod failure (with `checkInterval=2,
maxCheckFailures=2` a dead backend is marked down in ~4 s; in-flight queries
during that window may be lost). The tier also provides a
configurable packet cache (reduces backend load for repeated queries), per-
client rate limiting (drops abusive clients before queries reach PowerDNS),
and optional DNS-over-TLS / DNS-over-HTTPS termination. When a `DNSDist` is
in use, expose the gateway or LoadBalancer on the `DNSDist` and run the
`PowerDNSServer` with `dns.exposure: none` — the servers become internal
infrastructure unreachable from outside the cluster.

See [`examples/dnsdist-frontend.yaml`](examples/dnsdist-frontend.yaml) for a
ready-to-apply two-document manifest, and the
[`DNSDist` section of the configuration reference](docs/configuration.md#dnsdist)
for the full field table and v1 limitations.

## CRDs

Four namespaced CRDs in group `dns.aetherplatform.cloud`:

- `PowerDNSServer` (short name `pdns`) — server lifecycle
- `DNSDist` (short name `ddist`) — dnsdist frontend tier (load balancer, cache, rate limit, optional DoT/DoH)
- `Zone` — declarative zone management
- `RRSet` — declarative record management (type is a spec field; covers all RFC types)

```yaml
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: PowerDNSServer
metadata:
  name: demo
  namespace: aether-system
spec:
  replicas: 1
  backend:
    type: postgres                # mysql reserved, not yet implemented
    postgres:
      instances: 1                # operator-managed CloudNativePG cluster
      storageSize: 5Gi
  dns:
    exposure: loadBalancer        # none | loadBalancer | gateway
    loadBalancer:
      annotations:
        metallb.io/address-pool: aether-public
```

`spec.backend.postgres.byo.{host,port,database,credentialsSecretRef}` skips
CNPG provisioning and points the server at an existing Postgres.

`spec.dns.exposure: gateway` creates `TCPRoute` + `UDPRoute` resources
attached to a Gateway you supply via `dns.gateway.parentRefs`. Optionally,
`spec.api.gateway` additionally exposes the HTTP API via an `HTTPRoute`
(`<server>-api-http`) — useful for reaching the API through a TLS listener
on the same Gateway. Independent of `dns.exposure`.

## Zones and records

`Zone` and `RRSet` resources let you manage DNS declaratively alongside the
rest of your Kubernetes configuration. The operator reconciles only the
rrsets you declare (patch-only/coexist model) — everything else stays
untouched. Secondary zones, DNSSEC, and cross-namespace record ownership
are all supported. See [docs/managing-zones.md](docs/managing-zones.md) and
the [zone examples](examples/zone-basic.yaml) for details.

## Image

Defaults to `powerdns/pdns-auth-51`. Override per server with `spec.image`.

## API key

Auto-generated into a Secret named `<server>-api-key` (key: `api-key`) on
first reconcile. Reference an existing Secret via
`spec.api.apiKeySecretRef.name` to BYO.

Rotation: delete the Secret and the operator regenerates one on next
reconcile, then restart the pod.

## Phases

`Pending → ProvisioningBackend → InitializingSchema → DeployingServer → ExposingDNS → Ready`

`Failed` is terminal and surfaces the cause via `status.failureMessage`.

## Try it

```bash
make crd                                # install CRD
kubectl apply -f config/rbac/rbac.yaml  # ServiceAccount + ClusterRole
kubectl apply -f config/manager/        # operator Deployment
kubectl apply -f examples/managed-postgres.yaml
kubectl get pdns -n aether-system -w
```

## Local dev

```bash
make tidy build vet test
make image TAG=dev
```

## Layout

```
api/v1alpha1/             CRD types + deepcopy
cmd/operator/             manager entrypoint
internal/cnpg/            CloudNativePG Cluster manifest (unstructured)
internal/controller/      reconcilers (PowerDNSServer phase machine; Zone/RRSet convergers)
internal/manifests/       Deployment, Services, ConfigMap, Job, TCP/UDP routes
internal/pdnsclient/      thin PowerDNS HTTP API client
config/                   CRD, RBAC, manager Deployment, kustomize
examples/                 sample manifests (PowerDNSServer, Zone, RRSet)
```
