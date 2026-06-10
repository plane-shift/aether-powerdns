# Examples

Each file is a self-contained `kubectl apply -f`-able manifest.

| File | Scenario |
|---|---|
| `managed-postgres.yaml` | Operator-provisioned CNPG Postgres + MetalLB LoadBalancer exposure. Smallest viable config. |
| `byo-postgres.yaml` | Bring-your-own Postgres + Gateway API `TCPRoute` + `UDPRoute` exposure. |
| `ha-multi-zone.yaml` | 3 replicas with PDB, hostname + zone topology spread, priorityClassName. |
| `with-observability.yaml` | Same as managed-postgres + Prometheus `PodMonitor`. |
| `with-network-policy.yaml` | NetworkPolicy enabled + extra namespace allowlisted for the API. |
| `scheduling.yaml` | Pin pods to a dedicated DNS node pool with nodeSelector + tolerations. |
| `multi-ip-loadbalancer.yaml` | One Deployment, three public IPs — `additionalServices` for per-IP pools / annotations. |
| `multi-gateway.yaml` | One Deployment fronted by three Gateway API Gateways via TCP/UDPRoute parentRefs. |
| `zone-basic.yaml` | Native zone with apex NS seed and three RRSet records (A, A multi-value, MX). |
| `zone-secondary.yaml` | Secondary zone replicating from external primaries via AXFR/IXFR; RRSet resources rejected. |
| `zone-dnssec.yaml` | DNSSEC-signed Native zone; DS records appear in `status.dsRecords` for registrar upload. |
| `rrset-cross-namespace.yaml` | App-team namespace managing a record in a platform-owned zone via cross-namespace `zoneRef`. |

Pre-reqs vary per example — see comments at the top of each file.
