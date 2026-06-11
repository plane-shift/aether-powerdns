# Gateway API Exposure (full parentRefs + API HTTPRoute) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Full Gateway API `ParentReference` support (group/kind/name/namespace/sectionName) on DNS routes, a new opt-in HTTPRoute exposing the PowerDNS HTTP API via `spec.api.gateway`, and route drift correction — verified by render/controller tests plus a live e2e against Envoy Gateway on dev.

**Architecture:** Extend the existing `GatewayParentRef` with `group`/`kind`; add `APIGatewaySpec` (`hostnames` + full `parentRefs`) under `spec.api`. A new `HTTPRoute` renderer in `internal/manifests/routes.go` (gateway-api **v1** GA type) backs the API Service with `PathPrefix /`. A new `reconcileRoutes` controller method (called from `phaseExposingDNS` AND `reconcileDrift`) upserts/deletes all three routes via `controllerutil.CreateOrUpdate`, fixing the existing create-only gap where live `parentRefs` edits changed nothing.

**Tech Stack:** Go 1.26, controller-runtime v0.23.3, sigs.k8s.io/gateway-api v1.3.0 (already in go.mod — `v1alpha2` for TCP/UDP routes, `v1` for HTTPRoute). Hand-maintained CRD YAML + deepcopy (no controller-gen). controller-runtime fake client for tests.

**Spec:** `docs/superpowers/specs/2026-06-11-gateway-api-routes-design.md` — read it first.

**Repo facts (verified):**
- Branch to work on: `feat/gateway-api-routes` (already checked out; spec committed).
- `internal/manifests/routes.go` currently renders TCPRoute/UDPRoute via `gatewayParents(s, proto)`; `NameSet` has `TCPRoute`/`UDPRoute` but no HTTPRoute name; `apiSpecOrDefault(s)` and `labels(s)` exist in `manifests.go`.
- `cmd/operator/main.go` registers ONLY `gatewayv1alpha2` — `gatewayv1` must be added for HTTPRoute.
- `phaseExposingDNS` (powerdnsserver_controller.go) ensures TCP/UDP routes create-only via `ensureOwned`; `reconcileDrift` re-renders Deployment/Services/PDB but NOT routes; `validateSpec` validates `dns.gateway`.
- Controller tests use `testScheme(t)` from `zone_controller_test.go` (clientgoscheme + dnsv1alpha1 only — route tests build their own scheme).
- Per the CEL cost-budget lesson (memory `crd-cel-cost-budget`): every new CRD string gets `maxLength`, arrays get `maxItems`. No CEL rules are added by this feature.
- Dev e2e target: Envoy Gateway, Gateway `eg` in `envoy-gateway-system`; mgmt kubeconfig `~/.kube/config-aether`. Ephemeral test manifests go in project-local `.tmp/` (delete when done).

---

## File map

| File | Action | Responsibility |
|---|---|---|
| `api/v1alpha1/types.go` | Modify | `GatewayParentRef` += Group/Kind; `APISpec` += `Gateway *APIGatewaySpec`; new `APIGatewaySpec`, `APIGatewayParentRef` |
| `api/v1alpha1/zz_generated.deepcopy.go` | Modify | deepcopy for the new/changed types |
| `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml` | Modify | parentRefs items += group/kind (+ bounds); `api.gateway` block |
| `internal/manifests/manifests.go` | Modify | `Names` += `HTTPRoute` |
| `internal/manifests/routes.go` | Modify | `gatewayParents` emits Group/Kind; new `HTTPRoute()` renderer |
| `internal/manifests/routes_test.go` | Create | render tests for all three routes |
| `internal/controller/powerdnsserver_controller.go` | Modify | `validateSpec` api.gateway checks; `reconcileRoutes` + 3 upsert helpers; wire into `phaseExposingDNS` + `reconcileDrift`; RBAC marker += httproutes |
| `internal/controller/routes_controller_test.go` | Create | reconcileRoutes lifecycle tests |
| `cmd/operator/main.go` | Modify | register `gatewayv1` scheme |
| `config/rbac/rbac.yaml` | Modify | += httproutes |
| `examples/api-via-gateway.yaml` | Create | usage example |
| `examples/README.md`, `README.md`, `docs/managing-zones.md` (no), `docs/configuration.md`, `CLAUDE.md` | Modify | document the new surface |

---

### Task 1: API types, deepcopy, CRD YAML

**Files:**
- Modify: `api/v1alpha1/types.go`
- Modify: `api/v1alpha1/zz_generated.deepcopy.go`
- Modify: `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml`

Types have no logic; `make build && make vet` is the check (vet catches deepcopy gaps).

- [ ] **Step 1: Extend `GatewayParentRef` in `api/v1alpha1/types.go`**

The type currently starts:

```go
// GatewayParentRef is a minimal subset of gateway.networking.k8s.io
// ParentReference, with optional per-parent listener section names.
type GatewayParentRef struct {
	// Name of the Gateway.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
```

Replace the doc comment and add Group/Kind as the FIRST fields so the struct reads:

```go
// GatewayParentRef is a Gateway API ParentReference with optional
// per-protocol listener section names for the DNS routes.
type GatewayParentRef struct {
	// Group of the parent. Defaults to gateway.networking.k8s.io.
	// +optional
	Group string `json:"group,omitempty"`

	// Kind of the parent. Defaults to Gateway.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the Gateway.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
```

(keep the existing Namespace/TCPSectionName/UDPSectionName fields unchanged below).

- [ ] **Step 2: Add the API gateway types in `api/v1alpha1/types.go`**

(a) In `APISpec`, after the `APIKeySecretRef` field, add:

```go
	// Gateway optionally exposes the HTTP API through Gateway API
	// HTTPRoutes. Unset (default) keeps the API ClusterIP-only. The API
	// is an admin surface — a leaked key gives full zone control; attach
	// only to TLS listeners and scope with Hostnames.
	// +optional
	Gateway *APIGatewaySpec `json:"gateway,omitempty"`
```

(b) After the `APISpec` type, add:

```go
// APIGatewaySpec attaches an HTTPRoute for the PowerDNS HTTP API to one
// or more Gateways.
type APIGatewaySpec struct {
	// Hostnames the HTTPRoute matches. Empty matches every hostname on
	// the attached listener (Gateway API default) — set these on shared
	// gateways.
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`

	// ParentRefs lists the Gateways to attach to. At least one required.
	// +kubebuilder:validation:MinItems=1
	ParentRefs []APIGatewayParentRef `json:"parentRefs"`
}

// APIGatewayParentRef is a Gateway API ParentReference for the API
// HTTPRoute (single sectionName — HTTP has one protocol).
type APIGatewayParentRef struct {
	// Group of the parent. Defaults to gateway.networking.k8s.io.
	// +optional
	Group string `json:"group,omitempty"`

	// Kind of the parent. Defaults to Gateway.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the Gateway.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Gateway. Defaults to the PowerDNSServer's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// SectionName picks a listener on this Gateway. Optional — when
	// unset, the route attaches to the Gateway as a whole.
	// +optional
	SectionName string `json:"sectionName,omitempty"`
}
```

- [ ] **Step 3: deepcopy in `api/v1alpha1/zz_generated.deepcopy.go`**

(a) Find `APISpec.DeepCopyInto`. It currently handles only the `APIKeySecretRef` pointer. Add Gateway handling so the body contains:

```go
	if in.APIKeySecretRef != nil {
		out.APIKeySecretRef = new(corev1.LocalObjectReference)
		*out.APIKeySecretRef = *in.APIKeySecretRef
	}
	if in.Gateway != nil {
		out.Gateway = new(APIGatewaySpec)
		in.Gateway.DeepCopyInto(out.Gateway)
	}
```

(read the existing body first — keep whatever the APIKeySecretRef copy looks like, only ADD the Gateway block).

(b) Append at the end of the file (matches existing style; the file imports metav1 as `v1` but these types need no Condition handling):

```go
func (in *APIGatewaySpec) DeepCopyInto(out *APIGatewaySpec) {
	*out = *in
	if in.Hostnames != nil {
		out.Hostnames = make([]string, len(in.Hostnames))
		copy(out.Hostnames, in.Hostnames)
	}
	if in.ParentRefs != nil {
		out.ParentRefs = make([]APIGatewayParentRef, len(in.ParentRefs))
		copy(out.ParentRefs, in.ParentRefs)
	}
}

func (in *APIGatewaySpec) DeepCopy() *APIGatewaySpec {
	if in == nil {
		return nil
	}
	out := new(APIGatewaySpec)
	in.DeepCopyInto(out)
	return out
}
```

`APIGatewayParentRef` and the extended `GatewayParentRef` are flat string structs — `copy()` on the slice is a deep copy; no per-type methods needed (this matches how `DNSGatewaySpec.DeepCopyInto` already copies `[]GatewayParentRef`; verify that existing copy uses make+copy and leave it).

- [ ] **Step 4: CRD YAML — `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml`**

(a) The `dns.gateway.parentRefs.items.properties` block currently reads:

```yaml
                            properties:
                              name: { type: string }
                              namespace: { type: string }
                              tcpSectionName: { type: string }
                              udpSectionName: { type: string }
```

Replace with (bounds per the CEL-cost lesson — cheap insurance even without rules):

```yaml
                            properties:
                              group: { type: string, maxLength: 253 }
                              kind: { type: string, maxLength: 63 }
                              name: { type: string, minLength: 1, maxLength: 253 }
                              namespace: { type: string, maxLength: 63 }
                              tcpSectionName: { type: string, maxLength: 253 }
                              udpSectionName: { type: string, maxLength: 253 }
```

(b) In the `api:` block's `properties` (currently `port` and `apiKeySecretRef`), add after `apiKeySecretRef`:

```yaml
                    gateway:
                      type: object
                      required: [parentRefs]
                      properties:
                        hostnames:
                          type: array
                          maxItems: 16
                          items:
                            type: string
                            minLength: 1
                            maxLength: 253
                        parentRefs:
                          type: array
                          minItems: 1
                          maxItems: 8
                          items:
                            type: object
                            required: [name]
                            properties:
                              group: { type: string, maxLength: 253 }
                              kind: { type: string, maxLength: 63 }
                              name: { type: string, minLength: 1, maxLength: 253 }
                              namespace: { type: string, maxLength: 63 }
                              sectionName: { type: string, maxLength: 253 }
```

Mind indentation: `gateway:` sits at the same depth as `port:` (20 spaces) inside `api.properties`.

- [ ] **Step 5: Verify**

Run: `make build && make vet && python3 -c "import yaml; list(yaml.safe_load_all(open('config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml'))); print('ok')" && kubectl kustomize config/ > /dev/null && echo kustomize-ok`
Expected: all succeed.

- [ ] **Step 6: Commit**

```bash
git add api/v1alpha1/ config/crd/
git commit -m "feat: full Gateway API parentRefs (group/kind) + spec.api.gateway types"
```

---

### Task 2: Route renderers (TDD)

**Files:**
- Modify: `internal/manifests/manifests.go` (Names struct + NameSet)
- Modify: `internal/manifests/routes.go`
- Create: `internal/manifests/routes_test.go`

- [ ] **Step 1: Write the failing tests — create `internal/manifests/routes_test.go`**

```go
package manifests

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func gatewayServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			DNS: dnsv1alpha1.DNSSpec{
				Exposure: dnsv1alpha1.DNSExposureGateway,
				Gateway: &dnsv1alpha1.DNSGatewaySpec{
					ParentRefs: []dnsv1alpha1.GatewayParentRef{{
						Group:          "gateway.networking.k8s.io",
						Kind:           "Gateway",
						Name:           "eg",
						Namespace:      "envoy-gateway-system",
						TCPSectionName: "dns-tcp",
						UDPSectionName: "dns-udp",
					}},
				},
			},
		},
	}
}

func TestTCPRouteCarriesFullParentRef(t *testing.T) {
	route := TCPRoute(gatewayServer())
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("want 1 parentRef, got %d", len(route.Spec.ParentRefs))
	}
	p := route.Spec.ParentRefs[0]
	if p.Group == nil || string(*p.Group) != "gateway.networking.k8s.io" {
		t.Errorf("group not propagated: %v", p.Group)
	}
	if p.Kind == nil || string(*p.Kind) != "Gateway" {
		t.Errorf("kind not propagated: %v", p.Kind)
	}
	if string(p.Name) != "eg" {
		t.Errorf("name = %q", p.Name)
	}
	if p.Namespace == nil || string(*p.Namespace) != "envoy-gateway-system" {
		t.Errorf("namespace not propagated: %v", p.Namespace)
	}
	if p.SectionName == nil || string(*p.SectionName) != "dns-tcp" {
		t.Errorf("tcp sectionName = %v", p.SectionName)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatal("want exactly one rule with one backend")
	}
	b := route.Spec.Rules[0].BackendRefs[0]
	if string(b.Name) != "test-dns" || b.Port == nil || int32(*b.Port) != 53 {
		t.Errorf("backend = %s:%v, want test-dns:53", b.Name, b.Port)
	}
}

func TestUDPRouteUsesUDPSectionName(t *testing.T) {
	route := UDPRoute(gatewayServer())
	p := route.Spec.ParentRefs[0]
	if p.SectionName == nil || string(*p.SectionName) != "dns-udp" {
		t.Errorf("udp sectionName = %v, want dns-udp", p.SectionName)
	}
}

func TestParentRefOmitsOptionalFieldsWhenUnset(t *testing.T) {
	s := gatewayServer()
	s.Spec.DNS.Gateway.ParentRefs = []dnsv1alpha1.GatewayParentRef{{Name: "local-gw"}}
	route := TCPRoute(s)
	p := route.Spec.ParentRefs[0]
	if p.Group != nil || p.Kind != nil {
		t.Errorf("unset group/kind must stay nil (Gateway API defaults them): %v/%v", p.Group, p.Kind)
	}
	if p.Namespace != nil {
		t.Errorf("same-namespace parent must omit namespace, got %v", *p.Namespace)
	}
	if p.SectionName != nil {
		t.Errorf("unset sectionName must stay nil, got %v", *p.SectionName)
	}
}

func apiGatewayServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			API: dnsv1alpha1.APISpec{
				Port: 8081,
				Gateway: &dnsv1alpha1.APIGatewaySpec{
					Hostnames: []string{"pdns-api.internal.example.com"},
					ParentRefs: []dnsv1alpha1.APIGatewayParentRef{{
						Group:       "gateway.networking.k8s.io",
						Kind:        "Gateway",
						Name:        "eg",
						Namespace:   "envoy-gateway-system",
						SectionName: "https",
					}},
				},
			},
		},
	}
}

func TestHTTPRouteRendersParentsHostnamesAndBackend(t *testing.T) {
	route := HTTPRoute(apiGatewayServer())
	if route.Name != "test-api-http" {
		t.Errorf("route name = %q, want test-api-http", route.Name)
	}
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("want 1 parentRef, got %d", len(route.Spec.ParentRefs))
	}
	p := route.Spec.ParentRefs[0]
	if p.Namespace == nil || string(*p.Namespace) != "envoy-gateway-system" ||
		p.SectionName == nil || string(*p.SectionName) != "https" {
		t.Errorf("parentRef not fully propagated: %+v", p)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "pdns-api.internal.example.com" {
		t.Errorf("hostnames = %v", route.Spec.Hostnames)
	}
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(route.Spec.Rules))
	}
	rule := route.Spec.Rules[0]
	if len(rule.Matches) != 1 || rule.Matches[0].Path == nil ||
		rule.Matches[0].Path.Value == nil || *rule.Matches[0].Path.Value != "/" {
		t.Errorf("want a single PathPrefix / match, got %+v", rule.Matches)
	}
	if len(rule.BackendRefs) != 1 {
		t.Fatalf("want 1 backend, got %d", len(rule.BackendRefs))
	}
	b := rule.BackendRefs[0]
	if string(b.Name) != "test-api" || b.Port == nil || int32(*b.Port) != 8081 {
		t.Errorf("backend = %s:%v, want test-api:8081", b.Name, b.Port)
	}
}

func TestHTTPRouteNoHostnamesMeansNil(t *testing.T) {
	s := apiGatewayServer()
	s.Spec.API.Gateway.Hostnames = nil
	route := HTTPRoute(s)
	if route.Spec.Hostnames != nil {
		t.Errorf("empty hostnames must render nil (match-all), got %v", route.Spec.Hostnames)
	}
}

func TestHTTPRouteDefaultAPIPort(t *testing.T) {
	s := apiGatewayServer()
	s.Spec.API.Port = 0 // operator default is 8081
	route := HTTPRoute(s)
	b := route.Spec.Rules[0].BackendRefs[0]
	if b.Port == nil || int32(*b.Port) != 8081 {
		t.Errorf("default api port = %v, want 8081", b.Port)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/manifests/ -run 'TestTCPRoute|TestUDPRoute|TestParentRef|TestHTTPRoute'`
Expected: compile error — `undefined: HTTPRoute`, unknown fields `Group`/`Kind` (until Task 1 is merged this also fails; Task 1 must land first).

- [ ] **Step 3: Implement**

(a) `internal/manifests/manifests.go` — in the `Names` struct change the line `TCPRoute, UDPRoute string` to:

```go
	TCPRoute, UDPRoute, HTTPRoute string
```

and in `NameSet`, after the `UDPRoute:` entry add:

```go
		HTTPRoute:     n + "-api-http",
```

(b) `internal/manifests/routes.go` — in `gatewayParents`, after `ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p.Name)}`, add:

```go
		if p.Group != "" {
			g := gatewayv1.Group(p.Group)
			ref.Group = &g
		}
		if p.Kind != "" {
			k := gatewayv1.Kind(p.Kind)
			ref.Kind = &k
		}
```

(c) `internal/manifests/routes.go` — append the HTTPRoute renderer:

```go
// HTTPRoute exposes the PowerDNS HTTP API through the Gateways listed in
// spec.api.gateway.parentRefs. Opt-in only — the API is an admin surface
// (a leaked key gives full zone control); pair with TLS listeners and
// hostnames. Returns nil when spec.api.gateway is unset.
func HTTPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1.HTTPRoute {
	gw := s.Spec.API.Gateway
	if gw == nil {
		return nil
	}
	names := NameSet(s)
	api := apiSpecOrDefault(s)

	parents := make([]gatewayv1.ParentReference, 0, len(gw.ParentRefs))
	for _, p := range gw.ParentRefs {
		ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p.Name)}
		if p.Group != "" {
			g := gatewayv1.Group(p.Group)
			ref.Group = &g
		}
		if p.Kind != "" {
			k := gatewayv1.Kind(p.Kind)
			ref.Kind = &k
		}
		if p.Namespace != "" && p.Namespace != s.Namespace {
			ns := gatewayv1.Namespace(p.Namespace)
			ref.Namespace = &ns
		}
		if p.SectionName != "" {
			sn := gatewayv1.SectionName(p.SectionName)
			ref.SectionName = &sn
		}
		parents = append(parents, ref)
	}

	var hostnames []gatewayv1.Hostname
	for _, h := range gw.Hostnames {
		hostnames = append(hostnames, gatewayv1.Hostname(h))
	}

	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/"
	port := gatewayv1.PortNumber(api.Port)
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.HTTPRoute,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Hostnames:       hostnames,
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &pathValue},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(names.APIService),
							Port: &port,
						},
					},
				}},
			}},
		},
	}
}
```

Note: `apiSpecOrDefault` already defaults Port to 8081 — confirm by reading it in `manifests.go`; if it doesn't, default the port locally (`if api.Port == 0 { api.Port = 8081 }`) to make `TestHTTPRouteDefaultAPIPort` pass.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/manifests/ -v`
Expected: ALL manifests tests PASS (the 3 existing deploy-bug tests + 6 new route tests).

- [ ] **Step 5: Commit**

```bash
git add internal/manifests/
git commit -m "feat: HTTPRoute renderer + group/kind on DNS route parents"
```

---

### Task 3: Controller — validation, reconcileRoutes, wiring (TDD)

**Files:**
- Modify: `internal/controller/powerdnsserver_controller.go`
- Create: `internal/controller/routes_controller_test.go`
- Modify: `cmd/operator/main.go`
- Modify: `config/rbac/rbac.yaml`

- [ ] **Step 1: Write the failing tests — create `internal/controller/routes_controller_test.go`**

```go
package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func routesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, dnsv1alpha1.AddToScheme,
		gatewayv1alpha2.Install, gatewayv1.Install,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func newRoutesReconciler(t *testing.T, objs ...client.Object) (*PowerDNSServerReconciler, client.Client) {
	t.Helper()
	scheme := routesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PowerDNSServerReconciler{Client: c, Scheme: scheme}, c
}

func gatewayExposedServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			DNS: dnsv1alpha1.DNSSpec{
				Exposure: dnsv1alpha1.DNSExposureGateway,
				Gateway: &dnsv1alpha1.DNSGatewaySpec{
					ParentRefs: []dnsv1alpha1.GatewayParentRef{{Name: "eg", Namespace: "envoy-gateway-system"}},
				},
			},
			API: dnsv1alpha1.APISpec{
				Gateway: &dnsv1alpha1.APIGatewaySpec{
					ParentRefs: []dnsv1alpha1.APIGatewayParentRef{{Name: "eg", Namespace: "envoy-gateway-system", SectionName: "https"}},
				},
			},
		},
	}
}

func TestReconcileRoutesCreatesAllThree(t *testing.T) {
	s := gatewayExposedServer()
	r, c := newRoutesReconciler(t, s)

	if err := r.reconcileRoutes(context.Background(), s); err != nil {
		t.Fatalf("reconcileRoutes: %v", err)
	}

	ctx := context.Background()
	tcp := &gatewayv1alpha2.TCPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns-tcp", Namespace: "default"}, tcp); err != nil {
		t.Errorf("TCPRoute missing: %v", err)
	}
	udp := &gatewayv1alpha2.UDPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns-udp", Namespace: "default"}, udp); err != nil {
		t.Errorf("UDPRoute missing: %v", err)
	}
	http := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-api-http", Namespace: "default"}, http); err != nil {
		t.Fatalf("HTTPRoute missing: %v", err)
	}
	if len(http.OwnerReferences) == 0 {
		t.Error("HTTPRoute must carry an owner reference for GC")
	}
}

func TestReconcileRoutesUpdatesParentRefDrift(t *testing.T) {
	s := gatewayExposedServer()
	r, c := newRoutesReconciler(t, s)
	ctx := context.Background()
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	// live edit: spec moves to a different gateway
	s.Spec.API.Gateway.ParentRefs[0].Name = "internal-gw"
	s.Spec.DNS.Gateway.ParentRefs[0].Name = "internal-gw"
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	http := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-api-http", Namespace: "default"}, http); err != nil {
		t.Fatal(err)
	}
	if string(http.Spec.ParentRefs[0].Name) != "internal-gw" {
		t.Errorf("HTTPRoute parent not updated: %s", http.Spec.ParentRefs[0].Name)
	}
	tcp := &gatewayv1alpha2.TCPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns-tcp", Namespace: "default"}, tcp); err != nil {
		t.Fatal(err)
	}
	if string(tcp.Spec.ParentRefs[0].Name) != "internal-gw" {
		t.Errorf("TCPRoute parent not updated: %s", tcp.Spec.ParentRefs[0].Name)
	}
}

func TestReconcileRoutesDeletesWhenDisabled(t *testing.T) {
	s := gatewayExposedServer()
	r, c := newRoutesReconciler(t, s)
	ctx := context.Background()
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	// API gateway switched off, DNS exposure moves to loadBalancer
	s.Spec.API.Gateway = nil
	s.Spec.DNS.Exposure = dnsv1alpha1.DNSExposureLoadBalancer
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	for name, obj := range map[string]client.Object{
		"test-dns-tcp":  &gatewayv1alpha2.TCPRoute{},
		"test-dns-udp":  &gatewayv1alpha2.UDPRoute{},
		"test-api-http": &gatewayv1.HTTPRoute{},
	} {
		err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, obj)
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s should be deleted, got err=%v", name, err)
		}
	}
}

func TestValidateSpecAPIGateway(t *testing.T) {
	s := gatewayExposedServer()
	s.Spec.Backend.Type = dnsv1alpha1.BackendPostgres

	s.Spec.API.Gateway.ParentRefs = nil
	if msg := validateSpec(s); !strings.Contains(msg, "api.gateway") {
		t.Errorf("empty parentRefs must fail validation, got %q", msg)
	}

	s.Spec.API.Gateway.ParentRefs = []dnsv1alpha1.APIGatewayParentRef{{}}
	if msg := validateSpec(s); !strings.Contains(msg, "name is required") {
		t.Errorf("nameless parentRef must fail validation, got %q", msg)
	}

	s.Spec.API.Gateway.ParentRefs = []dnsv1alpha1.APIGatewayParentRef{{Name: "eg"}}
	if msg := validateSpec(s); msg != "" {
		t.Errorf("valid api.gateway rejected: %q", msg)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run 'TestReconcileRoutes|TestValidateSpecAPIGateway'`
Expected: compile error `r.reconcileRoutes undefined`.

- [ ] **Step 3: Implement in `internal/controller/powerdnsserver_controller.go`**

(a) Imports: add `gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"` and `gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"` (v1alpha2 may already be imported — check; if not, add it).

(b) RBAC marker: change the existing line

```go
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes;udproutes,verbs=get;list;watch;create;update;patch;delete
```

to:

```go
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes;udproutes;httproutes,verbs=get;list;watch;create;update;patch;delete
```

(c) `validateSpec`: after the existing `dns.exposure=gateway` block, add:

```go
	if gw := s.Spec.API.Gateway; gw != nil {
		if len(gw.ParentRefs) == 0 {
			return "api.gateway requires parentRefs (at least one)"
		}
		for i, p := range gw.ParentRefs {
			if p.Name == "" {
				return fmt.Sprintf("api.gateway.parentRefs[%d].name is required", i)
			}
		}
	}
```

(d) Add `reconcileRoutes` + helpers (place near `reconcileNetworkPolicy`):

```go
// reconcileRoutes converges the Gateway API routes: TCP/UDP routes exist
// iff dns.exposure==gateway, the API HTTPRoute iff spec.api.gateway is
// set. CreateOrUpdate (not create-only ensureOwned) so live parentRef /
// hostname / sectionName edits propagate; disabled routes are deleted.
func (r *PowerDNSServerReconciler) reconcileRoutes(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	names := manifests.NameSet(s)

	if s.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureGateway {
		desiredTCP := manifests.TCPRoute(s)
		tcp := &gatewayv1alpha2.TCPRoute{ObjectMeta: metav1.ObjectMeta{Name: desiredTCP.Name, Namespace: desiredTCP.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, tcp, func() error {
			tcp.Labels = desiredTCP.Labels
			tcp.Spec = desiredTCP.Spec
			return ctrl.SetControllerReference(s, tcp, r.Scheme)
		}); err != nil {
			return fmt.Errorf("ensure tcproute: %w", err)
		}

		desiredUDP := manifests.UDPRoute(s)
		udp := &gatewayv1alpha2.UDPRoute{ObjectMeta: metav1.ObjectMeta{Name: desiredUDP.Name, Namespace: desiredUDP.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, udp, func() error {
			udp.Labels = desiredUDP.Labels
			udp.Spec = desiredUDP.Spec
			return ctrl.SetControllerReference(s, udp, r.Scheme)
		}); err != nil {
			return fmt.Errorf("ensure udproute: %w", err)
		}
	} else {
		if err := client.IgnoreNotFound(r.Delete(ctx, &gatewayv1alpha2.TCPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: names.TCPRoute, Namespace: s.Namespace},
		})); err != nil {
			return fmt.Errorf("delete tcproute: %w", err)
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, &gatewayv1alpha2.UDPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: names.UDPRoute, Namespace: s.Namespace},
		})); err != nil {
			return fmt.Errorf("delete udproute: %w", err)
		}
	}

	if desired := manifests.HTTPRoute(s); desired != nil {
		http := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, http, func() error {
			http.Labels = desired.Labels
			http.Spec = desired.Spec
			return ctrl.SetControllerReference(s, http, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("ensure httproute: %w", err)
		}
		if op == controllerutil.OperationResultCreated {
			r.event(s, corev1.EventTypeNormal, "APIExposed",
				"HTTP API exposed via Gateway API HTTPRoute "+desired.Name+" — admin surface, ensure TLS at the listener")
		}
	} else {
		if err := client.IgnoreNotFound(r.Delete(ctx, &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: names.HTTPRoute, Namespace: s.Namespace},
		})); err != nil {
			return fmt.Errorf("delete httproute: %w", err)
		}
	}
	return nil
}
```

(e) `phaseExposingDNS`, the `DNSExposureGateway` case: REPLACE the two `ensureOwned` calls

```go
		tcp := manifests.TCPRoute(s)
		if err := r.ensureOwned(ctx, s, tcp); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure tcproute: %w", err)
		}
		udp := manifests.UDPRoute(s)
		if err := r.ensureOwned(ctx, s, udp); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure udproute: %w", err)
		}
```

with:

```go
		if err := r.reconcileRoutes(ctx, s); err != nil {
			return ctrl.Result{}, err
		}
```

AND, so api.gateway works with non-gateway DNS exposure, add this right BEFORE the `switch exposure {` line in the same function:

```go
	// The API HTTPRoute is independent of the DNS exposure mode; the
	// gateway case below also re-runs this (idempotent).
	if s.Spec.API.Gateway != nil || s.Spec.DNS.Exposure != dnsv1alpha1.DNSExposureGateway {
		if err := r.reconcileRoutes(ctx, s); err != nil {
			return ctrl.Result{}, err
		}
	}
```

(Simplification is allowed: calling `r.reconcileRoutes` ONCE unconditionally before the switch and removing the in-case call entirely is equivalent and preferred — do that, and in the gateway case keep only the parentRefs validation + endpoint string building.)

(f) `reconcileDrift`: after the `reconcileAdditionalDNSServices` call, add:

```go
	if err := r.reconcileRoutes(ctx, s); err != nil {
		return err
	}
```

(g) `cmd/operator/main.go`: add import `gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"` and in `init()` after the v1alpha2 line:

```go
	utilruntime.Must(gatewayv1.Install(scheme))
```

(h) `config/rbac/rbac.yaml`: change

```yaml
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["tcproutes", "udproutes"]
```

to:

```yaml
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["tcproutes", "udproutes", "httproutes"]
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/controller/ -run 'TestReconcileRoutes|TestValidateSpecAPIGateway' -v`
Expected: 4/4 PASS. Then the full suite: `make test && make vet && make build` — all green (zone/rrset tests unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/ cmd/operator/main.go config/rbac/rbac.yaml
git commit -m "feat: reconcileRoutes — API HTTPRoute lifecycle + route drift correction"
```

---

### Task 4: Example + docs

**Files:**
- Create: `examples/api-via-gateway.yaml`
- Modify: `examples/README.md`, `examples/multi-gateway.yaml` (read it; update ONLY if it contradicts the new fields), `README.md`, `docs/configuration.md`, `CLAUDE.md`

- [ ] **Step 1: Create `examples/api-via-gateway.yaml`**

```yaml
# DNS via Gateway API TCP/UDP listeners AND the HTTP API via an HTTPRoute
# on a TLS listener of the same Envoy Gateway. The API is an admin
# surface — a leaked key gives full zone control: attach it only to TLS
# listeners and always set hostnames on shared gateways.
#
# Prereq: a Gateway named `eg` in `envoy-gateway-system` with listeners
# `dns-tcp` (TCP/53), `dns-udp` (UDP/53) and `https` (HTTPS/443).
# With networkPolicy.enabled=true, add "envoy-gateway-system" to
# networkPolicy.additionalAllowedAPINamespaces or the gateway cannot
# reach the API port.
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: PowerDNSServer
metadata:
  name: gw-demo
  namespace: aether-system
spec:
  backend:
    type: postgres
    postgres:
      instances: 1
      storageSize: 5Gi
  dns:
    exposure: gateway
    gateway:
      parentRefs:
        - group: gateway.networking.k8s.io
          kind: Gateway
          name: eg
          namespace: envoy-gateway-system
          tcpSectionName: dns-tcp
          udpSectionName: dns-udp
  api:
    gateway:
      hostnames:
        - pdns-api.internal.example.com
      parentRefs:
        - group: gateway.networking.k8s.io
          kind: Gateway
          name: eg
          namespace: envoy-gateway-system
          sectionName: https
```

- [ ] **Step 2: `examples/README.md`** — add a row for `api-via-gateway.yaml` in the existing table format: "DNS via Gateway API TCP/UDP routes plus the HTTP API via HTTPRoute on a TLS listener".

- [ ] **Step 3: `docs/configuration.md`** — read it first; in the API section (or create one matching the doc's structure) document `api.gateway` (hostnames optional/match-all caveat, full parentRefs incl. group/kind defaults, PathPrefix /, the TLS + NetworkPolicy security notes, and that `status.apiEndpoint` remains the in-cluster ClusterIP URL). In the DNS exposure section note that `dns.gateway.parentRefs` now accepts `group`/`kind` and that parentRef edits propagate to live routes.

- [ ] **Step 4: `README.md`** — read it; extend the gateway exposure feature bullet to mention the API HTTPRoute option.

- [ ] **Step 5: `CLAUDE.md`** — in the "DNS exposure" section, update the `gateway` bullet to mention full parentRefs (group/kind) and append:

```markdown
The HTTP API can additionally be exposed via `spec.api.gateway`
(HTTPRoute, PathPrefix `/`, optional hostnames, full parentRefs) — opt-in
only; `status.apiEndpoint` stays the ClusterIP URL (in-cluster consumers
like the Zone/RRSet controllers depend on it). Routes are drift-corrected
by `reconcileRoutes` (CreateOrUpdate, called from phaseExposingDNS +
reconcileDrift) — parentRef edits propagate; disabled routes are deleted.
```

Also update the line in the "DNS exposure" section that says the HTTP API "stays on ClusterIP" to say it stays ClusterIP by default with opt-in HTTPRoute exposure.

- [ ] **Step 6: Verify + commit**

```bash
python3 -c "import yaml; list(yaml.safe_load_all(open('examples/api-via-gateway.yaml'))); print('ok')"
git add examples/ README.md docs/configuration.md CLAUDE.md
git commit -m "docs: api-via-gateway example + Gateway API exposure docs"
```

---

### Task 5: Full verification + live e2e on dev

- [ ] **Step 1: Full local suite**

Run: `make tidy && make build && make vet && go test ./... -count=1 && kubectl kustomize config/ > /dev/null && echo ok`
Expected: tidy makes no changes; everything green.

- [ ] **Step 2: Inspect the dev Gateway (read-only)**

```bash
export KUBECONFIG=~/.kube/config-aether
kubectl -n envoy-gateway-system get gateway eg -o jsonpath='{range .spec.listeners[*]}{.name} {.protocol}/{.port}{"\n"}{end}'
kubectl get gatewayclass
```

Record which listeners exist. The e2e needs TCP/53, UDP/53 and an HTTPS (or HTTP) listener. If `eg` lacks the DNS listeners, create a dedicated test Gateway in `.tmp/e2e-gateway.yaml` (same gatewayClass) with listeners `dns-tcp` TCP/53, `dns-udp` UDP/53, `http` HTTP/8080 — apply it, and target it from the test server instead. If the Gateway implementation reports TCPRoute/UDPRoute unsupported (route `Accepted=False`, reason like `UnsupportedKind`), capture the condition text and report it — do NOT silently pass.

- [ ] **Step 3: Apply the CRD + a test server**

```bash
kubectl apply -f config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml
```

Write `.tmp/gw-e2e.yaml` — a copy of `examples/api-via-gateway.yaml` with `metadata.name: gw-e2e`, `namespace: powerdns`, and parentRefs/sectionNames matching what Step 2 found (real listener names). Apply it; wait for `status.phase=Ready`.

NOTE: the operator deployment on dev runs the pinned release image, which does NOT include this branch. For the e2e either (a) run the operator locally against the cluster (`go run ./cmd/operator` with KUBECONFIG set — the mgmt kubeconfig has the RBAC of your user, fine for a test) after scaling the in-cluster operator to 0 **via the ArgoCD app pause patch — do NOT fight selfHeal manually**, or (b) build+push a dev image and `kustomize` it in. Option (a) is faster and reversible: ask the user before touching the in-cluster operator if anything is unclear. Capture which option was used in the report.

- [ ] **Step 4: Assert route acceptance + traffic**

```bash
# Route status: every parent should report Accepted=True
kubectl -n powerdns get tcproute gw-e2e-dns-tcp -o jsonpath='{.status.parents[*].conditions[?(@.type=="Accepted")].status}'
kubectl -n powerdns get udproute gw-e2e-dns-udp -o jsonpath='{.status.parents[*].conditions[?(@.type=="Accepted")].status}'
kubectl -n powerdns get httproute gw-e2e-api-http -o jsonpath='{.status.parents[*].conditions[?(@.type=="Accepted")].status}'

# Traffic through the gateway address (GWADDR from the Gateway status):
GWADDR=$(kubectl -n envoy-gateway-system get gateway eg -o jsonpath='{.status.addresses[0].value}')
dig +short @"$GWADDR" example.com SOA          # any zone the test server carries; create one via the API first if empty
curl -sk -o /dev/null -w '%{http_code}\n' -H "X-API-Key: $KEY" --resolve pdns-api.internal.example.com:443:"$GWADDR" https://pdns-api.internal.example.com/api/v1/servers/localhost
```

Expected: Accepted=True for all parents; dig answers; curl 200. Record actual outputs.

- [ ] **Step 5: Drift check (the new behavior)**

Edit the live server's `api.gateway.parentRefs[0].sectionName` to a different listener; within one Ready-loop (≤30s) the HTTPRoute's parentRef must reflect it (`kubectl -n powerdns get httproute gw-e2e-api-http -o jsonpath='{.spec.parentRefs[0].sectionName}'`). Then unset `spec.api.gateway` entirely and verify the HTTPRoute is DELETED.

- [ ] **Step 6: Clean up**

Delete the test server CR (cascades routes via owner refs), any test Gateway, and `.tmp/` files. If the in-cluster operator was paused, restore it.

- [ ] **Step 7: Finish the branch**

Use superpowers:finishing-a-development-branch — push, PR against `plane-shift/aether-powerdns` main, squash-merge after CodeQL + CodeRabbit.

---

## Self-review notes (already applied)

- Names cross-checked: `Names.HTTPRoute` = `<name>-api-http` (Task 2 NameSet, Task 3 tests `test-api-http`, e2e `gw-e2e-api-http`); `reconcileRoutes`/`HTTPRoute()`/`APIGatewaySpec`/`APIGatewayParentRef` used consistently across tasks.
- `HTTPRoute()` returns nil when `api.gateway` unset — controller relies on that for the delete branch.
- `controllerutil` and `client` are already imported in the controller; `corev1` (for the event) too. `metav1` already imported. Verify imports compile rather than assuming.
- The fake client supports `controllerutil.CreateOrUpdate` natively; no status subresource needed for route objects.
- `phaseExposingDNS` simplification (single unconditional `reconcileRoutes` before the switch) is the preferred shape — the (e) step says so explicitly.
- Task 1 must land before Task 2's tests compile (route tests reference the new fields) — tasks are ordered accordingly.
