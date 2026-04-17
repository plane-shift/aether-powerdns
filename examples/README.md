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

Pre-reqs vary per example — see comments at the top of each file.
