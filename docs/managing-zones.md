# Managing zones and records

The operator deliberately doesn't model zones, records, or DNSSEC keys —
PowerDNS already has a complete HTTP API and `pdnsutil` CLI for that.
Two equivalent paths:

1. **HTTP API** — script-friendly, works from anywhere, uses the
   operator-generated API key
2. **`pdnsutil` via `kubectl exec`** — interactive, no key needed (runs
   inside the pod with full access)

All examples assume:

```bash
SERVER=demo
NS=aether-system
KEY=$(kubectl get secret -n $NS ${SERVER}-api-key -o jsonpath='{.data.api-key}' | base64 -d)
API=$(kubectl get pdns -n $NS $SERVER -o jsonpath='{.status.apiEndpoint}')
```

If you're outside the cluster, port-forward the API service:

```bash
kubectl port-forward -n $NS svc/${SERVER}-api 8081:8081 &
API=http://127.0.0.1:8081
```

## HTTP API

PowerDNS uses `localhost` as its server ID by default — that's the
literal string in the URL, not your hostname.

### Create a zone

```bash
curl -sf -X POST -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  $API/api/v1/servers/localhost/zones \
  -d '{
    "name": "example.com.",
    "kind": "Native",
    "nameservers": ["ns1.example.com.", "ns2.example.com."]
  }' | jq .
```

`kind` is one of `Native` (single-server), `Master` (you serve, others
slave), or `Slave` (you slave from a master). `Native` is the default
choice for in-cluster auth.

### List zones

```bash
curl -sf -H "X-API-Key: $KEY" $API/api/v1/servers/localhost/zones | jq '.[].name'
```

### Add records (RRset)

PowerDNS groups records by name + type. Replace the entire RRset in one
call (`changetype: REPLACE`):

```bash
curl -sf -X PATCH -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  $API/api/v1/servers/localhost/zones/example.com. \
  -d '{
    "rrsets": [{
      "name": "www.example.com.",
      "type": "A",
      "ttl": 300,
      "changetype": "REPLACE",
      "records": [
        {"content": "192.0.2.10", "disabled": false},
        {"content": "192.0.2.11", "disabled": false}
      ]
    }]
  }'
```

### Delete an RRset

```bash
curl -sf -X PATCH -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  $API/api/v1/servers/localhost/zones/example.com. \
  -d '{"rrsets": [{"name": "www.example.com.", "type": "A", "changetype": "DELETE"}]}'
```

### Every supported record type

PowerDNS supports the full set of RFC types — `A`, `AAAA`, `ALIAS`, `CAA`,
`CDNSKEY`, `CDS`, `CNAME`, `DNAME`, `DNSKEY`, `DS`, `HTTPS`, `LOC`,
`MX`, `NAPTR`, `NS`, `OPENPGPKEY`, `PTR`, `RP`, `SOA`, `SPF`, `SRV`,
`SSHFP`, `SVCB`, `TLSA`, `TXT`, `URI`, `ZONEMD`, … plus more. `content`
is always the RFC presentation format. Examples:

```json
{ "type": "MX",  "records": [{"content": "10 mail.example.com.", "disabled": false}] }
{ "type": "TXT", "records": [{"content": "\"v=spf1 mx -all\"",   "disabled": false}] }
{ "type": "SRV", "records": [{"content": "0 5 5060 sip.example.com.", "disabled": false}] }
{ "type": "CAA", "records": [{"content": "0 issue \"letsencrypt.org\"",  "disabled": false}] }
```

## pdnsutil via kubectl exec

```bash
POD=$(kubectl get pod -n $NS -l app.kubernetes.io/instance=$SERVER -o jsonpath='{.items[0].metadata.name}')
EXEC="kubectl exec -n $NS $POD -- pdnsutil"

$EXEC create-zone example.com ns1.example.com
$EXEC add-record example.com www A 300 192.0.2.10 192.0.2.11
$EXEC list-zone example.com
$EXEC delete-rrset example.com www A
```

`pdnsutil` is convenient for ops; the API is convenient for automation
(e.g. external-dns, CI pipelines).

## DNSSEC

DNSSEC is fully supported by PowerDNS but not modelled by the operator
(out of scope, like zones). Sign zones with `pdnsutil`:

```bash
$EXEC secure-zone example.com
$EXEC show-zone example.com    # prints DNSKEY / DS records to publish at the parent
```

## Backups

Zone data lives in Postgres. Use CNPG's built-in Barman backup config
(see CNPG docs) or your own pg_dump pipeline. The operator does not
manage backups for the BYO case either — that's your DB's responsibility.
