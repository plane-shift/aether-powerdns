# Design: Gateway API exposure — full parentRefs + API HTTPRoute

**Status: APPROVED**
Date: 2026-06-11

## Decision context

`spec.dns.exposure=gateway` already renders a TCPRoute + UDPRoute for port
53, but (a) the parentRefs lack the full Gateway API `ParentReference`
shape (no `group`/`kind`), (b) the HTTP API has no gateway exposure at all
(deliberately ClusterIP-only), (c) routes are create-only — editing
`parentRefs` on a live server changes nothing, and (d) none of it has test
coverage or has been verified against a real gateway implementation.

Decisions made with the user:

| Question | Decision |
|---|---|
| API exposure config | New independent `spec.api.gateway` block with its own parentRefs — not coupled to `dns.exposure` |
| parentRefs shape | Full Gateway API form: `group` (default `gateway.networking.k8s.io`), `kind` (default `Gateway`), `name`, `namespace`, `sectionName`; DNS parents additionally keep per-protocol `tcpSectionName`/`udpSectionName` and gain `group`/`kind` |
| HTTPRoute hostnames | Optional (empty = all hostnames on the listener); docs recommend setting them |
| Path matching | Single `PathPrefix /` rule |
| Verification | Render tests + controller tests + live e2e on dev against Envoy Gateway (`eg` / `envoy-gateway-system`) |

## 1. API types (`api/v1alpha1/types.go`)

`GatewayParentRef` (existing, used by `dns.gateway.parentRefs`) gains:

```go
// Group of the parent. Defaults to gateway.networking.k8s.io.
Group string `json:"group,omitempty"`
// Kind of the parent. Defaults to Gateway.
Kind string `json:"kind,omitempty"`
```

`APISpec` gains:

```go
// Gateway optionally exposes the HTTP API through Gateway API HTTPRoutes.
// Unset (default) keeps the API ClusterIP-only. The API is an admin
// surface — attach only to TLS listeners and scope with hostnames.
Gateway *APIGatewaySpec `json:"gateway,omitempty"`
```

New types:

```go
// APIGatewaySpec attaches an HTTPRoute for the PowerDNS HTTP API to one
// or more Gateways.
type APIGatewaySpec struct {
	// Hostnames the HTTPRoute matches. Empty = all hostnames on the
	// matched listener (Gateway API default) — set these on shared
	// gateways.
	Hostnames []string `json:"hostnames,omitempty"`
	// ParentRefs lists the Gateways (or other parent kinds) to attach
	// to. At least one required.
	ParentRefs []APIGatewayParentRef `json:"parentRefs"`
}

// APIGatewayParentRef is a Gateway API ParentReference for the API
// HTTPRoute (single sectionName — HTTP has one protocol).
type APIGatewayParentRef struct {
	Group       string `json:"group,omitempty"`     // default gateway.networking.k8s.io
	Kind        string `json:"kind,omitempty"`      // default Gateway
	Name        string `json:"name"`                // required
	Namespace   string `json:"namespace,omitempty"` // default: server's namespace
	SectionName string `json:"sectionName,omitempty"`
}
```

Hand-maintained CRD YAML + deepcopy as usual. All new strings carry
`maxLength` (253 names/hostnames, 63 namespaces/sectionName/kind/group…)
and arrays `maxItems` — per the CEL cost-budget lesson, bound everything
even though this block adds no CEL rules.

## 2. Manifests (`internal/manifests/routes.go`)

- `gatewayParents` emits `Group`/`Kind` on the ParentReference when set
  on the DNS parents (shared by TCPRoute + UDPRoute).
- New `HTTPRoute(s *PowerDNSServer) *gatewayv1.HTTPRoute` renderer
  (gateway-api v1 GA type, already vendored): name `Names.HTTPRoute`
  (`<server>-api-http`), labels per `labels(s)`, parents from
  `spec.api.gateway.parentRefs` (with group/kind/namespace defaulting),
  `spec.hostnames` from config, one rule `PathPrefix /` →
  backend `Names.APIService`:`api.Port`.

## 3. Controller

- `validateSpec`: when `spec.api.gateway` is set, `parentRefs` must be
  non-empty and every entry needs `name` (mirrors the DNS gateway
  checks).
- The HTTPRoute is ensured whenever `spec.api.gateway` is set —
  independent of `dns.exposure`. When unset, a previously created
  HTTPRoute is deleted (delete-if-exists by name; owner ref keeps GC
  safe regardless).
- **Route drift correction (fixes an existing gap):** `reconcileDrift`
  re-renders and updates TCPRoute + UDPRoute (when
  `dns.exposure=gateway`) and the HTTPRoute (when `api.gateway` set) the
  same way it updates the Deployment/Services — so live `parentRefs`,
  hostnames, or section-name edits propagate. When `dns.exposure` moves
  away from `gateway`, the TCP/UDP routes are deleted.
- `status.apiEndpoint` is NOT changed — it stays the ClusterIP URL that
  in-cluster consumers (Zone/RRSet controllers) depend on. Gateway
  exposure is surfaced via an Event (`APIExposed`) and docs.
- Wiring: register `gatewayv1` into the manager scheme (main.go installs
  only `v1alpha2` today); add `httproutes` to the RBAC marker +
  `config/rbac/rbac.yaml`.

## 4. Security posture (docs, not code)

API-via-gateway is opt-in, off by default. Docs state: a leaked API key
gives full zone control — attach only to TLS listeners
(`sectionName: https`), set `hostnames`, and with
`networkPolicy.enabled=true` add the gateway's namespace (e.g.
`envoy-gateway-system`) to `additionalAllowedAPINamespaces` or the
gateway cannot reach the API port.

## 5. Testing & verification

- **Render tests** (`internal/manifests/routes_test.go`, new): TCPRoute /
  UDPRoute / HTTPRoute — parent group/kind/namespace defaulting,
  per-protocol sectionNames, backend service/port, hostnames, PathPrefix,
  stable output (determinism).
- **Controller tests**: validation failure for api.gateway without
  parentRefs; HTTPRoute created when api.gateway set, updated on
  parentRef change (drift), deleted when unset; TCP/UDP route drift
  correction on parentRef edits; route deletion when exposure leaves
  `gateway`.
- **Live e2e on dev** against Envoy Gateway (`eg` in
  `envoy-gateway-system`): a test PowerDNSServer with
  `dns.exposure=gateway` + `api.gateway`; assert route `Accepted`
  conditions; real traffic — `dig` through the gateway's TCP/UDP 53
  listeners and `curl` the API through the `https` listener. If `eg`
  lacks the needed listeners, add them to a test Gateway or report
  exactly what's missing rather than skipping silently.

## Out of scope

TLSRoute / GRPCRoute, DNS-over-TLS / DoH (dnsdist territory), changing
`status.apiEndpoint` semantics, HTTPRoute filters/header matching, BackendTLSPolicy.
