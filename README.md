# aether-powerdns

Kubernetes operator that runs the [PowerDNS authoritative server](https://doc.powerdns.com/authoritative/)
backed by a Postgres database, with the HTTP API protected by a generated key.

The operator only manages the **server lifecycle**. Zones, records and DNSSEC
keys are managed out of band — call the PowerDNS HTTP API with the generated
key, or run `pdnsutil` via `kubectl exec`. This keeps the CRD surface tiny and
lets users use every PowerDNS record type without operator changes.

## CRD

A single namespaced CRD: `PowerDNSServer` (group `dns.aetherplatform.cloud`,
short name `pdns`).

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
attached to a Gateway you supply via `dns.gateway.parentRef`.

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
internal/controller/      reconciler (phase-based state machine)
internal/manifests/       Deployment, Services, ConfigMap, Job, TCP/UDP routes
config/                   CRD, RBAC, manager Deployment, kustomize
examples/                 sample PowerDNSServer resources
```
