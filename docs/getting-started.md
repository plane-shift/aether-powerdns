# Getting started

Install the operator, deploy a PowerDNS server, and resolve a query.

## Prerequisites

- Kubernetes 1.25+ (for CRD CEL `x-kubernetes-validations`)
- [CloudNativePG](https://cloudnative-pg.io/) operator installed (only if you
  want the operator-managed Postgres backend; skip for BYO Postgres)
- A `LoadBalancer` provider (e.g. MetalLB) **or** the Gateway API if you
  plan to expose DNS externally — `none` exposure works with stock K8s

## Install

```bash
git clone https://github.com/plane-shift/aether-powerdns.git
cd aether-powerdns

# CRD + RBAC + operator Deployment
kubectl apply -f config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml
kubectl apply -f config/rbac/rbac.yaml
kubectl apply -f config/manager/deployment.yaml
```

The operator runs in `aether-system` by default; create the namespace first if
you don't have it:

```bash
kubectl create namespace aether-system
```

## Deploy your first server

```bash
kubectl apply -f examples/managed-postgres.yaml
kubectl get pdns -n aether-system -w
```

Expect: `Phase` walks through `ProvisioningBackend → InitializingSchema →
DeployingServer → ExposingDNS → Ready`. First reconcile takes ~60s on a
warm cluster (most of it CNPG initdb).

```text
NAME   PHASE   BACKEND    READY   DESIRED   DNS              API
demo   Ready   postgres   1       1         10.43.7.8:53     http://demo-api.aether-system.svc:8081
```

## Retrieve the API key

The operator generates a 64-character hex key into `<server>-api-key`.

```bash
kubectl get secret -n aether-system demo-api-key -o jsonpath='{.data.api-key}' | base64 -d
```

## Smoke-test DNS

If you used `examples/managed-postgres.yaml` (LoadBalancer exposure), grab
the assigned IP from `status.dnsEndpoint` and:

```bash
dig @<dns-endpoint> ns example.com
```

Expect `REFUSED` — there are no zones yet. That's the "DNS is responding"
signal. See [managing-zones.md](managing-zones.md) for adding your first zone.

## Tear down

```bash
kubectl delete pdns demo -n aether-system
```

This cascades to the Deployment, Services, ConfigMap, Secrets, the schema
Job, the PDB / NetworkPolicy / PodMonitor (if enabled), and the
operator-managed CNPG `Cluster` (which in turn frees its PVC).
