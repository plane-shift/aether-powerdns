# DNSDist CRD + HA/PDB Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new `DNSDist` CRD that deploys dnsdist frontends (health-checked backends, packet cache, rate limiting, DoT/DoH) over one or more PowerDNSServers, plus configurable PDB `minAvailable` on both kinds and a real DNS readiness probe on pdns pods.

**Architecture:** `DNSDist` is a separate namespaced kind with `backendRefs[]` to same-namespace PowerDNSServers. A new `internal/manifests/dnsdist.go` renders a deterministic Lua `dnsdist.conf` ConfigMap, Deployment (config-hash rolling restarts), DNS Service, PDB, and — via refactored source-agnostic route builders — TCP/UDP routes that **backend the dnsdist Service** (gateway → dnsdist → pdns). `DNSDistReconciler` is a single-pass converger (zone-controller style) with a backendRefs field index. No finalizer: a DNSDist owns no external state — Kubernetes GC via owner refs covers deletion.

**Tech Stack:** Go 1.26, controller-runtime v0.23.3, gateway-api v1.3.0 (existing). Hand-maintained CRD YAML + deepcopy. Images: `powerdns/dnsdist-19` (default, floating), existing `powerdns/pdns-auth-51`.

**Spec:** `docs/superpowers/specs/2026-06-11-dnsdist-ha-design.md` — read it first. Branch: `feat/dnsdist` (checked out; spec committed).

**Repo facts (verified):**
- `internal/manifests/manifests.go`: `labels(s)` returns `{app.kubernetes.io/name: powerdns, component: auth, managed-by: aether-powerdns, instance: <name>}` via consts `labelApp/labelComponent/labelManagedBy/labelInstance` + `managedBy`. `baseDNSService(s, name)` builds the ClusterIP DNS svc; `applyLBPrimary(svc, lb)` applies LB type/IP/annotations/ETP; `PodDisruptionBudget(s)` returns nil for replicas<=1 else `minAvailable: replicas-1`; pdns container probes are TCP on the API port; pod spec has anti-affinity + topology spread when replicas>1; `dnsTCPPort`/`dnsUDPPort` consts = 53.
- `internal/manifests/routes.go`: `TCPRoute(s)/UDPRoute(s)` use `gatewayParents(s, proto)`; `HTTPRoute(s)` builds parents inline ("keep in sync" comment). `Names` has TCPRoute/UDPRoute/HTTPRoute.
- `internal/controller/`: `reconcileRoutes` (CreateOrUpdate + `deleteIfExists` with NoMatch tolerance), `updateService` (annotation-merge/NodePort-preserve/no-op), `updateConfigMap`, `updateDeployment`, `refreshDNSEndpoint`, `setCondOn`, `refKey`, `requeueShort/Long`. Controller tests: `routesTestScheme`/`fixesTestScheme` patterns, `interceptor.Funcs`, `WithStatusSubresource`.
- `validateSpec` controller-side pattern; CEL cost-budget rule: bound every string/array, immutability via field-level `self == oldSelf` only (none needed here — everything mutable).
- Existing phase consts `ZonePhasePending/Ready/Failed` are generic strings — reuse for DNSDist. Conditions: add `ConditionBackendsReady`.

---

## File map

| File | Action | Responsibility |
|---|---|---|
| `api/v1alpha1/dnsdist_types.go` | Create | DNSDist kinds + sub-specs + `PDBSpec` |
| `api/v1alpha1/types.go` | Modify | `PowerDNSServerSpec.PodDisruptionBudget *PDBSpec` |
| `api/v1alpha1/groupversion_info.go` | Modify | register DNSDist kinds |
| `api/v1alpha1/zz_generated.deepcopy.go` | Modify | deepcopy for all new types |
| `config/crd/dns.aetherplatform.cloud_dnsdists.yaml` | Create | DNSDist CRD (bounded) |
| `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml` | Modify | `spec.podDisruptionBudget` block |
| `config/rbac/rbac.yaml`, `config/kustomization.yaml` | Modify | dnsdists triple; CRD file |
| `internal/manifests/manifests.go` | Modify | PDB override via shared `pdbFor`; DNS readiness probe |
| `internal/manifests/routes.go` | Modify | source-agnostic parents/route builders |
| `internal/manifests/dnsdist.go` | Create | all dnsdist renderers incl. `DNSDistConfig` |
| `internal/manifests/dnsdist_test.go` | Create | render tests |
| `internal/manifests/manifests_test.go`, `routes_test.go` | Modify | PDB/probe/refactor tests |
| `internal/controller/dnsdist_controller.go` | Create | DNSDistReconciler |
| `internal/controller/dnsdist_controller_test.go` | Create | reconciler tests |
| `internal/controller/powerdnsserver_controller.go` | Modify | validateSpec PDB check; RBAC marker |
| `cmd/operator/main.go` | Modify | wire DNSDistReconciler |
| `examples/dnsdist-frontend.yaml`, `examples/README.md`, `README.md`, `docs/configuration.md`, `CLAUDE.md` | Create/Modify | docs |

---

### Task 1: API types, deepcopy, scheme

**Files:**
- Create: `api/v1alpha1/dnsdist_types.go`
- Modify: `api/v1alpha1/types.go`, `api/v1alpha1/groupversion_info.go`, `api/v1alpha1/zz_generated.deepcopy.go`

- [ ] **Step 1: Create `api/v1alpha1/dnsdist_types.go`**

```go
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionBackendsReady reports whether every backendRef resolves to a
// Ready PowerDNSServer.
const ConditionBackendsReady = "BackendsReady"

// PDBSpec configures the PodDisruptionBudget rendered for a workload.
// Shared by PowerDNSServer and DNSDist.
type PDBSpec struct {
	// MinAvailable overrides the default `replicas - 1`. Must be between
	// 1 and replicas-1 — minAvailable == replicas would block ALL
	// voluntary disruptions and is rejected. Ignored when replicas <= 1
	// (no PDB is rendered then).
	// +optional
	MinAvailable *int32 `json:"minAvailable,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="DNS",type=string,JSONPath=`.status.dnsEndpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DNSDist deploys a dnsdist frontend tier (DNS-aware load balancer with
// active backend health checks, packet cache, rate limiting and optional
// DoT/DoH) over one or more PowerDNSServers in the same namespace. With
// a DNSDist in front, point gateway/LB exposure HERE and run the backing
// servers with dns.exposure=none.
type DNSDist struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSDistSpec   `json:"spec,omitempty"`
	Status DNSDistStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DNSDistList contains a list of DNSDist.
type DNSDistList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSDist `json:"items"`
}

// DefaultDNSDistImage is used when spec.image is empty. Floating tag —
// pin a digest in spec.image for reproducibility.
const DefaultDNSDistImage = "powerdns/dnsdist-19"

// DNSDistSpec defines the desired dnsdist tier.
type DNSDistSpec struct {
	// Replicas for the dnsdist Deployment.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image overrides the default `powerdns/dnsdist-19` image.
	// +optional
	Image string `json:"image,omitempty"`

	// Resources for the dnsdist container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling pins pods to nodes/zones/priority classes.
	// +optional
	Scheduling SchedulingSpec `json:"scheduling,omitempty"`

	// BackendRefs lists the PowerDNSServers this tier load-balances.
	// Same-namespace only (the namespace field must be empty). Each
	// becomes a health-checked dnsdist backend.
	// +kubebuilder:validation:MinItems=1
	BackendRefs []ObjectRef `json:"backendRefs"`

	// DNS configures how the dnsdist DNS port is exposed — identical
	// semantics to PowerDNSServer.spec.dns (none/loadBalancer/gateway,
	// multi-parent gateways, additional LB Services). Routes/Services
	// target the DNSDIST pods, fronting the backends.
	// +optional
	DNS DNSSpec `json:"dns,omitempty"`

	// Cache configures the dnsdist packet cache.
	// +optional
	Cache DNSDistCacheSpec `json:"cache,omitempty"`

	// RateLimit configures per-client query-rate protection.
	// +optional
	RateLimit DNSDistRateLimitSpec `json:"rateLimit,omitempty"`

	// TLS configures DNS-over-TLS / DNS-over-HTTPS listeners. v1 exposes
	// their ports on the ClusterIP/LoadBalancer Service only (not via
	// gateway routes).
	// +optional
	TLS DNSDistTLSSpec `json:"tls,omitempty"`

	// PodDisruptionBudget overrides the default minAvailable (replicas-1).
	// +optional
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`
}

// DNSDistCacheSpec controls the packet cache.
type DNSDistCacheSpec struct {
	// Enabled toggles the packet cache. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MaxEntries bounds the cache size. Defaults to 100000.
	// +kubebuilder:validation:Minimum=1024
	// +optional
	MaxEntries *int32 `json:"maxEntries,omitempty"`
}

// DNSDistRateLimitSpec renders a dnsdist dynamic-block rule.
type DNSDistRateLimitSpec struct {
	// QPSPerClient blocks clients exceeding this query rate (10s window,
	// 60s block). 0 (default) disables rate limiting.
	// +kubebuilder:validation:Minimum=0
	// +optional
	QPSPerClient int32 `json:"qpsPerClient,omitempty"`
}

// DNSDistTLSSpec groups the TLS-terminating listeners.
type DNSDistTLSSpec struct {
	// DoT serves DNS-over-TLS on port 853.
	// +optional
	DoT DNSDistTLSListener `json:"dot,omitempty"`

	// DoH serves DNS-over-HTTPS on port 443 at /dns-query.
	// +optional
	DoH DNSDistTLSListener `json:"doh,omitempty"`
}

// DNSDistTLSListener configures one TLS listener.
type DNSDistTLSListener struct {
	// Enabled toggles the listener. Requires CertificateSecretRef.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// CertificateSecretRef names a kubernetes.io/tls Secret (tls.crt +
	// tls.key) in the DNSDist's namespace, e.g. cert-manager issued.
	// +optional
	CertificateSecretRef corev1.LocalObjectReference `json:"certificateSecretRef,omitempty"`
}

// DNSDistStatus reflects the observed tier state.
type DNSDistStatus struct {
	// Phase is Pending, Ready or Failed. Informational — single-pass
	// reconciler, not a phase machine.
	// +optional
	Phase string `json:"phase,omitempty"`

	// DNSEndpoint is the externally reachable DNS address, derived from
	// live Service/exposure state each pass.
	// +optional
	DNSEndpoint string `json:"dnsEndpoint,omitempty"`

	// DesiredReplicas mirrors the Deployment's replica count.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// ReadyReplicas is the number of dnsdist pods currently Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready, BackendsReady, Available.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// FailureMessage details the cause when reconciliation cannot proceed.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`
}
```

- [ ] **Step 2: `api/v1alpha1/types.go` — add to `PowerDNSServerSpec` after the `ZoneManagement` field:**

```go
	// PodDisruptionBudget overrides the default minAvailable (replicas-1)
	// of the PDB rendered when replicas > 1.
	// +optional
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`
```

- [ ] **Step 3: `groupversion_info.go` — extend `init()` registration:**

```go
func init() {
	SchemeBuilder.Register(
		&PowerDNSServer{}, &PowerDNSServerList{},
		&Zone{}, &ZoneList{},
		&RRSet{}, &RRSetList{},
		&DNSDist{}, &DNSDistList{},
	)
}
```

- [ ] **Step 4: deepcopy in `zz_generated.deepcopy.go`** (file imports metav1 as `v1`; List items use the local-staging `l := make(...)` pattern):

(a) In `PowerDNSServerSpec.DeepCopyInto`, after the `in.ZoneManagement.DeepCopyInto(&out.ZoneManagement)` line:

```go
	if in.PodDisruptionBudget != nil {
		out.PodDisruptionBudget = new(PDBSpec)
		in.PodDisruptionBudget.DeepCopyInto(out.PodDisruptionBudget)
	}
```

(b) Append at end of file:

```go
func (in *PDBSpec) DeepCopyInto(out *PDBSpec) {
	*out = *in
	if in.MinAvailable != nil {
		v := *in.MinAvailable
		out.MinAvailable = &v
	}
}

func (in *PDBSpec) DeepCopy() *PDBSpec {
	if in == nil {
		return nil
	}
	out := new(PDBSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSDist) DeepCopyInto(out *DNSDist) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *DNSDist) DeepCopy() *DNSDist {
	if in == nil {
		return nil
	}
	out := new(DNSDist)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSDist) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *DNSDistList) DeepCopyInto(out *DNSDistList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		l := make([]DNSDist, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&l[i])
		}
		out.Items = l
	}
}

func (in *DNSDistList) DeepCopy() *DNSDistList {
	if in == nil {
		return nil
	}
	out := new(DNSDistList)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSDistList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *DNSDistSpec) DeepCopyInto(out *DNSDistSpec) {
	*out = *in
	if in.Replicas != nil {
		v := *in.Replicas
		out.Replicas = &v
	}
	in.Resources.DeepCopyInto(&out.Resources)
	in.Scheduling.DeepCopyInto(&out.Scheduling)
	if in.BackendRefs != nil {
		l := make([]ObjectRef, len(in.BackendRefs))
		copy(l, in.BackendRefs)
		out.BackendRefs = l
	}
	in.DNS.DeepCopyInto(&out.DNS)
	in.Cache.DeepCopyInto(&out.Cache)
	out.RateLimit = in.RateLimit
	out.TLS = in.TLS
	if in.PodDisruptionBudget != nil {
		out.PodDisruptionBudget = new(PDBSpec)
		in.PodDisruptionBudget.DeepCopyInto(out.PodDisruptionBudget)
	}
}

func (in *DNSDistSpec) DeepCopy() *DNSDistSpec {
	if in == nil {
		return nil
	}
	out := new(DNSDistSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSDistCacheSpec) DeepCopyInto(out *DNSDistCacheSpec) {
	*out = *in
	if in.Enabled != nil {
		v := *in.Enabled
		out.Enabled = &v
	}
	if in.MaxEntries != nil {
		v := *in.MaxEntries
		out.MaxEntries = &v
	}
}

func (in *DNSDistCacheSpec) DeepCopy() *DNSDistCacheSpec {
	if in == nil {
		return nil
	}
	out := new(DNSDistCacheSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSDistStatus) DeepCopyInto(out *DNSDistStatus) {
	*out = *in
	if in.Conditions != nil {
		l := make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&l[i])
		}
		out.Conditions = l
	}
}

func (in *DNSDistStatus) DeepCopy() *DNSDistStatus {
	if in == nil {
		return nil
	}
	out := new(DNSDistStatus)
	in.DeepCopyInto(out)
	return out
}
```

Notes: `DNSDistRateLimitSpec`, `DNSDistTLSSpec`, `DNSDistTLSListener` are flat (value copies via `*out = *in` in their parents) — no methods needed. `DNSDistCacheSpec` has pointers → methods above.

- [ ] **Step 5: Verify** — `make build && make vet` (vet is the deepcopy-gap check). Expected: clean.

- [ ] **Step 6: Commit** — `git add api/v1alpha1/ && git commit -m "feat: DNSDist API types + shared PDBSpec"`

---

### Task 2: CRD YAMLs, RBAC, kustomization

**Files:**
- Create: `config/crd/dns.aetherplatform.cloud_dnsdists.yaml`
- Modify: `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml`, `config/rbac/rbac.yaml`, `config/kustomization.yaml`

- [ ] **Step 1: Create `config/crd/dns.aetherplatform.cloud_dnsdists.yaml`**

The `dns:` property block must MIRROR the powerdnsservers CRD's `spec.properties.dns` schema exactly (same exposure enum, loadBalancer, gateway/parentRefs with group/kind + bounds, additionalServices) — copy it verbatim from that file and keep indentation consistent. Full file:

```yaml
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: dnsdists.dns.aetherplatform.cloud
spec:
  group: dns.aetherplatform.cloud
  names:
    kind: DNSDist
    listKind: DNSDistList
    plural: dnsdists
    singular: dnsdist
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      additionalPrinterColumns:
        - jsonPath: .status.phase
          name: Phase
          type: string
        - jsonPath: .status.readyReplicas
          name: Ready
          type: integer
        - jsonPath: .status.desiredReplicas
          name: Desired
          type: integer
        - jsonPath: .status.dnsEndpoint
          name: DNS
          type: string
        - jsonPath: .metadata.creationTimestamp
          name: Age
          type: date
      schema:
        openAPIV3Schema:
          type: object
          properties:
            apiVersion: { type: string }
            kind: { type: string }
            metadata: { type: object }
            spec:
              type: object
              required: [backendRefs]
              properties:
                replicas:
                  type: integer
                  format: int32
                  minimum: 1
                  default: 1
                image:
                  type: string
                  maxLength: 512
                resources:
                  type: object
                  x-kubernetes-preserve-unknown-fields: true
                scheduling:
                  type: object
                  x-kubernetes-preserve-unknown-fields: true
                backendRefs:
                  type: array
                  minItems: 1
                  maxItems: 16
                  items:
                    type: object
                    required: [name]
                    properties:
                      name: { type: string, minLength: 1, maxLength: 253 }
                      namespace: { type: string, maxLength: 63 }
                dns:
                  type: object
                  properties:
                    # >>> copy the FULL `dns` properties schema verbatim from
                    # config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml
                    # (exposure enum+default, loadBalancer incl.
                    # additionalServices, gateway incl. bounded parentRefs
                    # with group/kind/tcpSectionName/udpSectionName) <<<
                    exposure:
                      type: string
                      enum: [none, loadBalancer, gateway]
                      default: none
                cache:
                  type: object
                  properties:
                    enabled:
                      type: boolean
                      default: true
                    maxEntries:
                      type: integer
                      format: int32
                      minimum: 1024
                      default: 100000
                rateLimit:
                  type: object
                  properties:
                    qpsPerClient:
                      type: integer
                      format: int32
                      minimum: 0
                      default: 0
                tls:
                  type: object
                  properties:
                    dot:
                      type: object
                      properties:
                        enabled: { type: boolean, default: false }
                        certificateSecretRef:
                          type: object
                          required: [name]
                          properties:
                            name: { type: string, minLength: 1, maxLength: 253 }
                    doh:
                      type: object
                      properties:
                        enabled: { type: boolean, default: false }
                        certificateSecretRef:
                          type: object
                          required: [name]
                          properties:
                            name: { type: string, minLength: 1, maxLength: 253 }
                podDisruptionBudget:
                  type: object
                  properties:
                    minAvailable:
                      type: integer
                      format: int32
                      minimum: 1
            status:
              type: object
              properties:
                phase: { type: string }
                dnsEndpoint: { type: string }
                desiredReplicas: { type: integer, format: int32 }
                readyReplicas: { type: integer, format: int32 }
                observedGeneration: { type: integer, format: int64 }
                failureMessage: { type: string }
                conditions:
                  type: array
                  items:
                    type: object
                    required: [type, status, reason, message, lastTransitionTime]
                    properties:
                      type: { type: string }
                      status: { type: string, enum: ["True", "False", "Unknown"] }
                      reason: { type: string }
                      message: { type: string }
                      observedGeneration: { type: integer, format: int64 }
                      lastTransitionTime: { type: string, format: date-time }
```

IMPORTANT (marked `>>> <<<` above): replace the placeholder `dns` block with the verbatim `dns` schema from the powerdnsservers CRD — the implementer copies it; the spec reviewer diff-checks the two blocks for equality. The `scheduling`/`resources` use `x-kubernetes-preserve-unknown-fields: true` (corev1-typed; matches how the server CRD handles `resources` — check and mirror its exact style for `scheduling`: if the server CRD enumerates scheduling fields, copy that enumeration instead).

- [ ] **Step 2: powerdnsservers CRD — add as a sibling of `zoneManagement` in spec properties:**

```yaml
                podDisruptionBudget:
                  type: object
                  properties:
                    minAvailable:
                      type: integer
                      format: int32
                      minimum: 1
```

- [ ] **Step 3: RBAC — `config/rbac/rbac.yaml`, after the zones/rrsets finalizers rule:**

```yaml
  - apiGroups: ["dns.aetherplatform.cloud"]
    resources: ["dnsdists"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["dns.aetherplatform.cloud"]
    resources: ["dnsdists/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["dns.aetherplatform.cloud"]
    resources: ["dnsdists/finalizers"]
    verbs: ["update"]
```

(No create/delete — user-owned resource, least privilege per the zones precedent.)

- [ ] **Step 4: `config/kustomization.yaml`** — add `crd/dns.aetherplatform.cloud_dnsdists.yaml` to resources after the rrsets line.

- [ ] **Step 5: Verify** — `python3 -c "import yaml,sys; [list(yaml.safe_load_all(open(f))) for f in sys.argv[1:]]; print('ok')" config/crd/*.yaml config/rbac/rbac.yaml && kubectl kustomize config/ > /dev/null && echo kustomize-ok`. ALSO apply the new CRD to the dev apiserver (`KUBECONFIG=~/.kube/config-aether kubectl apply -f config/crd/dns.aetherplatform.cloud_dnsdists.yaml`) — the CEL cost-budget lesson: only a real apiserver validates schema cost; confirm Established, then this stays installed for the e2e.

- [ ] **Step 6: Commit** — `git add config/ && git commit -m "feat: DNSDist CRD + PDB schema + RBAC"`

---

### Task 3: Shared manifests refactors — PDB override, route builders, DNS readiness probe (TDD)

**Files:**
- Modify: `internal/manifests/manifests.go`, `internal/manifests/routes.go`
- Test: `internal/manifests/manifests_test.go`, `internal/manifests/routes_test.go`

- [ ] **Step 1: Write the failing tests** — append to `internal/manifests/manifests_test.go`:

```go
func TestPDBMinAvailableOverride(t *testing.T) {
	s := testServer()
	three := int32(3)
	s.Spec.Replicas = &three

	pdb := PodDisruptionBudget(s)
	if pdb == nil || pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Fatalf("default must stay replicas-1, got %v", pdb)
	}

	one := int32(1)
	s.Spec.PodDisruptionBudget = &dnsv1alpha1.PDBSpec{MinAvailable: &one}
	pdb = PodDisruptionBudget(s)
	if pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("override minAvailable=1 not honored, got %v", pdb.Spec.MinAvailable)
	}

	single := int32(1)
	s.Spec.Replicas = &single
	if PodDisruptionBudget(s) != nil {
		t.Error("replicas<=1 must render no PDB even with an override set")
	}
}

func TestDeploymentReadinessProbeIsDNSCheck(t *testing.T) {
	dep := Deployment(testServer(), "h")
	rp := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
	if rp == nil || rp.Exec == nil {
		t.Fatalf("readiness must be an exec DNS check, got %+v", rp)
	}
	joined := strings.Join(rp.Exec.Command, " ")
	if !strings.Contains(joined, "pdns_control") && !strings.Contains(joined, "sdig") {
		t.Errorf("readiness command should use in-image DNS tooling, got %q", joined)
	}
	lp := dep.Spec.Template.Spec.Containers[0].LivenessProbe
	if lp == nil || lp.TCPSocket == nil {
		t.Errorf("liveness stays TCP, got %+v", lp)
	}
}
```

- [ ] **Step 2: Run to confirm failure** — `go test ./internal/manifests/ -run 'TestPDBMinAvailable|TestDeploymentReadinessProbe'` → FAIL (PDBSpec unused / probe is TCPSocket).

- [ ] **Step 3: Verify the probe command on the LIVE pdns pod before wiring it:**

```bash
export KUBECONFIG=~/.kube/config-aether
POD=$(kubectl -n powerdns get pods -l app.kubernetes.io/instance=aether-dns -o name | head -1)
kubectl -n powerdns exec $POD -- pdns_control --socket-dir=/var/run rping; echo "rping-exit=$?"
kubectl -n powerdns exec $POD -- sh -c 'sdig 127.0.0.1 53 . SOA 2>&1 | head -2'; echo "sdig-exit=$?"
```

Pick the command that (a) exits 0 when the server answers and (b) exists in the image. Preference order: `pdns_control --socket-dir=/var/run rping` (control-socket liveness of the DNS engine), else `sh -c "sdig 127.0.0.1 53 <anything> SOA >/dev/null"`. Record the chosen command in the implementation and in the commit message.

- [ ] **Step 4: Implement in `internal/manifests/manifests.go`:**

(a) Generalize the PDB via a shared helper (DNSDist reuses it in Task 4):

```go
// pdbFor renders a PodDisruptionBudget for any operator-managed workload.
// nil when replicas <= 1 (a single-replica PDB would block every drain).
// Default minAvailable is replicas-1; the override is clamped into
// [1, replicas-1] by validateSpec before it gets here.
func pdbFor(name, namespace string, lbls map[string]string, replicas int32, override *int32) *policyv1.PodDisruptionBudget {
	if replicas <= 1 {
		return nil
	}
	min := replicas - 1
	if override != nil {
		min = *override
	}
	minAvail := intstr.FromInt(int(min))
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: lbls},
		},
	}
}
```

Rewrite `PodDisruptionBudget(s)` to delegate:

```go
func PodDisruptionBudget(s *dnsv1alpha1.PowerDNSServer) *policyv1.PodDisruptionBudget {
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	var override *int32
	if s.Spec.PodDisruptionBudget != nil {
		override = s.Spec.PodDisruptionBudget.MinAvailable
	}
	return pdbFor(NameSet(s).PDB, s.Namespace, labels(s), replicas, override)
}
```

(b) Replace the pdns container's ReadinessProbe (keep liveness TCP as-is) with the verified command — assuming `rping` won (adjust if Step 3 said otherwise):

```go
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						// Real DNS-engine responsiveness, not just an open
						// socket: an up-but-wedged pdns drops from
						// endpoints (HA hardening; command verified live).
						Exec: &corev1.ExecAction{
							Command: []string{"pdns_control", "--socket-dir=/var/run", "rping"},
						},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
				},
```

(c) `validateSpec` in `internal/controller/powerdnsserver_controller.go` — after the api.gateway block, add (and the same check appears in Task 5's `validateDNSDist`):

```go
	if pdb := s.Spec.PodDisruptionBudget; pdb != nil && pdb.MinAvailable != nil {
		replicas := int32(1)
		if s.Spec.Replicas != nil {
			replicas = *s.Spec.Replicas
		}
		if replicas > 1 && *pdb.MinAvailable >= replicas {
			return fmt.Sprintf("podDisruptionBudget.minAvailable (%d) must be < replicas (%d) — equal would block all drains", *pdb.MinAvailable, replicas)
		}
	}
```

- [ ] **Step 5: Route-builder refactor in `internal/manifests/routes.go`** — make parents + routes source-agnostic so Task 4 reuses them. Replace `gatewayParents(s *dnsv1alpha1.PowerDNSServer, proto gatewayProto)` with:

```go
// dnsRouteParents builds ParentReferences from a DNSSpec's gateway block —
// shared by PowerDNSServer and DNSDist exposure (same DNSSpec type).
func dnsRouteParents(dns *dnsv1alpha1.DNSSpec, localNS string, proto gatewayProto) []gatewayv1.ParentReference {
	if dns.Gateway == nil {
		return nil
	}
	out := make([]gatewayv1.ParentReference, 0, len(dns.Gateway.ParentRefs))
	for _, p := range dns.Gateway.ParentRefs {
		ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p.Name)}
		if p.Group != "" {
			g := gatewayv1.Group(p.Group)
			ref.Group = &g
		}
		if p.Kind != "" {
			k := gatewayv1.Kind(p.Kind)
			ref.Kind = &k
		}
		if p.Namespace != "" && p.Namespace != localNS {
			nsRef := gatewayv1.Namespace(p.Namespace)
			ref.Namespace = &nsRef
		}
		var sn string
		switch proto {
		case gatewayProtoTCP:
			sn = p.TCPSectionName
		case gatewayProtoUDP:
			sn = p.UDPSectionName
		}
		if sn != "" {
			snRef := gatewayv1.SectionName(sn)
			ref.SectionName = &snRef
		}
		out = append(out, ref)
	}
	return out
}

// buildTCPRoute / buildUDPRoute render one route for any owner.
func buildTCPRoute(name, namespace string, lbls map[string]string, parents []gatewayv1.ParentReference, backendSvc string, port int32) *gatewayv1alpha2.TCPRoute {
	p := gatewayv1.PortNumber(port)
	return &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: gatewayv1alpha2.TCPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Rules: []gatewayv1alpha2.TCPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backendSvc), Port: &p,
					},
				}},
			}},
		},
	}
}

func buildUDPRoute(name, namespace string, lbls map[string]string, parents []gatewayv1.ParentReference, backendSvc string, port int32) *gatewayv1alpha2.UDPRoute {
	p := gatewayv1.PortNumber(port)
	return &gatewayv1alpha2.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: gatewayv1alpha2.UDPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Rules: []gatewayv1alpha2.UDPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backendSvc), Port: &p,
					},
				}},
			}},
		},
	}
}
```

Rewrite the public server renderers as thin wrappers (PUBLIC API unchanged — existing controller/tests keep working):

```go
func TCPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.TCPRoute {
	names := NameSet(s)
	return buildTCPRoute(names.TCPRoute, s.Namespace, labels(s),
		dnsRouteParents(&s.Spec.DNS, s.Namespace, gatewayProtoTCP), names.DNSService, dnsTCPPort)
}

func UDPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.UDPRoute {
	names := NameSet(s)
	return buildUDPRoute(names.UDPRoute, s.Namespace, labels(s),
		dnsRouteParents(&s.Spec.DNS, s.Namespace, gatewayProtoUDP), names.DNSService, dnsUDPPort)
}
```

Delete the old `gatewayParents`. `HTTPRoute` stays as-is (its parents differ structurally). The existing routes_test.go suite is the regression net for this refactor — zero behavior change expected.

- [ ] **Step 6: Run all manifests tests** — `go test ./internal/manifests/ -count=1` → ALL PASS (new + the 10+ existing, proving the refactor is behavior-neutral). Then `make build && make vet && make test`.

- [ ] **Step 7: Commit** — `git add internal/ && git commit -m "feat: configurable PDB minAvailable, DNS readiness probe, source-agnostic route builders"`

---

### Task 4: dnsdist manifests (TDD)

**Files:**
- Create: `internal/manifests/dnsdist.go`
- Test: `internal/manifests/dnsdist_test.go`

- [ ] **Step 1: Write the failing tests** — create `internal/manifests/dnsdist_test.go`:

```go
package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func testDNSDist() *dnsv1alpha1.DNSDist {
	two := int32(2)
	return &dnsv1alpha1.DNSDist{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default"},
		Spec: dnsv1alpha1.DNSDistSpec{
			Replicas: &two,
			BackendRefs: []dnsv1alpha1.ObjectRef{
				{Name: "srv-b"}, {Name: "srv-a"},
			},
		},
	}
}

func TestDNSDistConfigBackendsSortedAndHealthChecked(t *testing.T) {
	conf := DNSDistConfig(testDNSDist())
	ia := strings.Index(conf, `newServer({address="srv-a-dns.default.svc.cluster.local:53"`)
	ib := strings.Index(conf, `newServer({address="srv-b-dns.default.svc.cluster.local:53"`)
	if ia < 0 || ib < 0 {
		t.Fatalf("backends missing:\n%s", conf)
	}
	if ia > ib {
		t.Error("backends must render in sorted order (deterministic conf = stable config hash)")
	}
	if !strings.Contains(conf, "checkInterval=2") || !strings.Contains(conf, "maxCheckFailures=2") {
		t.Error("active health-check parameters missing")
	}
}

func TestDNSDistConfigACLOpensPublicQueries(t *testing.T) {
	conf := DNSDistConfig(testDNSDist())
	if !strings.Contains(conf, `setACL({"0.0.0.0/0", "::/0"})`) {
		t.Error("dnsdist's default ACL allows only RFC1918 — a public frontend MUST setACL wide open")
	}
}

func TestDNSDistConfigCacheDefaultsOnAndTogglesOff(t *testing.T) {
	d := testDNSDist()
	conf := DNSDistConfig(d)
	if !strings.Contains(conf, "newPacketCache(100000") {
		t.Errorf("cache must default on with 100000 entries:\n%s", conf)
	}
	off := false
	d.Spec.Cache.Enabled = &off
	if strings.Contains(DNSDistConfig(d), "newPacketCache") {
		t.Error("cache.enabled=false must omit the packet cache")
	}
}

func TestDNSDistConfigRateLimitAndTLSToggles(t *testing.T) {
	d := testDNSDist()
	conf := DNSDistConfig(d)
	if strings.Contains(conf, "setQueryRate") || strings.Contains(conf, "addTLSLocal") || strings.Contains(conf, "addDOHLocal") {
		t.Error("rate limit and TLS listeners must be off by default")
	}
	d.Spec.RateLimit.QPSPerClient = 50
	d.Spec.TLS.DoT = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "dot-cert"}}
	d.Spec.TLS.DoH = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "doh-cert"}}
	conf = DNSDistConfig(d)
	if !strings.Contains(conf, "setQueryRate(50, 10") {
		t.Errorf("qpsPerClient=50 must render a dynamic-block rule:\n%s", conf)
	}
	if !strings.Contains(conf, `addTLSLocal("0.0.0.0:853", "/tls/dot/tls.crt", "/tls/dot/tls.key")`) {
		t.Errorf("DoT listener missing:\n%s", conf)
	}
	if !strings.Contains(conf, `addDOHLocal("0.0.0.0:443", "/tls/doh/tls.crt", "/tls/doh/tls.key", { "/dns-query" })`) {
		t.Errorf("DoH listener missing:\n%s", conf)
	}
}

func TestDNSDistDeploymentShape(t *testing.T) {
	d := testDNSDist()
	d.Spec.TLS.DoT = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "dot-cert"}}
	dep := DNSDistDeployment(d, "hash123")
	if dep.Name != "edge-dnsdist" {
		t.Errorf("deployment name = %q, want edge-dnsdist", dep.Name)
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d", *dep.Spec.Replicas)
	}
	if dep.Spec.Template.Annotations[ConfigHashAnnotation] != "hash123" {
		t.Error("config-hash pod annotation missing — conf changes must roll pods")
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != dnsv1alpha1.DefaultDNSDistImage {
		t.Errorf("default image = %q", c.Image)
	}
	var ports []string
	for _, p := range c.Ports {
		ports = append(ports, p.Name)
	}
	joined := strings.Join(ports, ",")
	for _, want := range []string{"dns-tcp", "dns-udp", "dot"} {
		if !strings.Contains(joined, want) {
			t.Errorf("port %q missing (have %s)", want, joined)
		}
	}
	if dep.Spec.Template.Spec.Affinity == nil || dep.Spec.Template.Spec.Affinity.PodAntiAffinity == nil {
		t.Error("replicas>1 must set pod anti-affinity (same HA posture as pdns)")
	}
	// cert volume mounted, conf volume mounted
	mounts := c.VolumeMounts
	var hasConf, hasDot bool
	for _, m := range mounts {
		if m.MountPath == "/etc/dnsdist" {
			hasConf = true
		}
		if m.MountPath == "/tls/dot" {
			hasDot = true
		}
	}
	if !hasConf || !hasDot {
		t.Errorf("expected conf + DoT cert mounts, got %+v", mounts)
	}
}

func TestDNSDistServiceAndPDBAndRoutes(t *testing.T) {
	d := testDNSDist()
	svc := DNSDistDNSService(d)
	if svc.Name != "edge-dnsdist-dns" || svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service = %s/%s", svc.Name, svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Errorf("default ports tcp+udp 53, got %+v", svc.Spec.Ports)
	}

	pdb := DNSDistPDB(d)
	if pdb == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("replicas=2 default PDB minAvailable=1, got %v", pdb)
	}

	d.Spec.DNS = dnsv1alpha1.DNSSpec{
		Exposure: dnsv1alpha1.DNSExposureGateway,
		Gateway: &dnsv1alpha1.DNSGatewaySpec{
			ParentRefs: []dnsv1alpha1.GatewayParentRef{{Name: "gw", TCPSectionName: "dns-tcp", UDPSectionName: "dns-udp"}},
		},
	}
	tcp := DNSDistTCPRoute(d)
	if tcp.Name != "edge-dnsdist-tcp" || string(tcp.Spec.Rules[0].BackendRefs[0].Name) != "edge-dnsdist-dns" {
		t.Errorf("TCP route must backend the DNSDIST service (gateway→dnsdist→pdns): %+v", tcp)
	}
	udp := DNSDistUDPRoute(d)
	if string(*udp.Spec.ParentRefs[0].SectionName) != "dns-udp" {
		t.Errorf("UDP sectionName: %+v", udp.Spec.ParentRefs[0])
	}
}

func TestDNSDistConfigDeterministic(t *testing.T) {
	d := testDNSDist()
	first := DNSDistConfig(d)
	for i := 0; i < 20; i++ {
		if DNSDistConfig(d) != first {
			t.Fatal("conf must render identically every time (config hash stability)")
		}
	}
}
```

- [ ] **Step 2: Confirm failure** — `go test ./internal/manifests/ -run TestDNSDist` → compile errors (undefined renderers).

- [ ] **Step 3: Create `internal/manifests/dnsdist.go`:**

```go
package manifests

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

const (
	dotPort = 853
	dohPort = 443
)

// DNSDistNames groups the resource names derived from a DNSDist. All are
// suffixed `-dnsdist*` so a DNSDist and a PowerDNSServer sharing a
// metadata.name can't collide.
type DNSDistNames struct {
	Deployment, ConfigMap, DNSService string
	PDB, TCPRoute, UDPRoute           string
}

// DNSDistNameSet returns the canonical names for all owned resources.
func DNSDistNameSet(d *dnsv1alpha1.DNSDist) DNSDistNames {
	n := d.Name
	return DNSDistNames{
		Deployment: n + "-dnsdist",
		ConfigMap:  n + "-dnsdist-config",
		DNSService: n + "-dnsdist-dns",
		PDB:        n + "-dnsdist-pdb",
		TCPRoute:   n + "-dnsdist-tcp",
		UDPRoute:   n + "-dnsdist-udp",
	}
}

// dnsdistLabels parallel the server labels with component=frontend.
func dnsdistLabels(d *dnsv1alpha1.DNSDist) map[string]string {
	return map[string]string{
		labelApp:       "dnsdist",
		labelComponent: "frontend",
		labelManagedBy: managedBy,
		labelInstance:  d.Name,
	}
}

func dnsdistReplicas(d *dnsv1alpha1.DNSDist) int32 {
	if d.Spec.Replicas != nil {
		return *d.Spec.Replicas
	}
	return 1
}

// DNSDistConfig renders dnsdist.conf (Lua). Deterministic: backends are
// sorted so the config hash is stable across reconciles.
func DNSDistConfig(d *dnsv1alpha1.DNSDist) string {
	var b strings.Builder
	b.WriteString("-- managed by aether-powerdns\n")
	// dnsdist's DEFAULT ACL allows only RFC1918 — a public frontend
	// would silently refuse everything without this.
	b.WriteString(`setACL({"0.0.0.0/0", "::/0"})` + "\n")
	b.WriteString(`setLocal("0.0.0.0:53", {reusePort=true})` + "\n")

	names := make([]string, 0, len(d.Spec.BackendRefs))
	for _, ref := range d.Spec.BackendRefs {
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	for _, n := range names {
		// Backend = the server's DNS Service FQDN (v1: service-level
		// addressing; per-pod discovery is v2). Active health checks
		// with fast up/down.
		fmt.Fprintf(&b, `newServer({address="%s-dns.%s.svc.cluster.local:53", name="%s", checkInterval=2, maxCheckFailures=2, rise=1})`+"\n",
			n, d.Namespace, n)
	}

	if d.Spec.Cache.Enabled == nil || *d.Spec.Cache.Enabled {
		max := int32(100000)
		if d.Spec.Cache.MaxEntries != nil {
			max = *d.Spec.Cache.MaxEntries
		}
		fmt.Fprintf(&b, "pc = newPacketCache(%d, {maxTTL=86400})\n", max)
		b.WriteString(`getPool(""):setCache(pc)` + "\n")
	}

	if qps := d.Spec.RateLimit.QPSPerClient; qps > 0 {
		b.WriteString("local dbr = dynBlockRulesGroup()\n")
		fmt.Fprintf(&b, `dbr:setQueryRate(%d, 10, "rate-limited", 60)`+"\n", qps)
		b.WriteString("function maintenance() dbr:apply() end\n")
	}

	if d.Spec.TLS.DoT.Enabled {
		b.WriteString(`addTLSLocal("0.0.0.0:853", "/tls/dot/tls.crt", "/tls/dot/tls.key")` + "\n")
	}
	if d.Spec.TLS.DoH.Enabled {
		b.WriteString(`addDOHLocal("0.0.0.0:443", "/tls/doh/tls.crt", "/tls/doh/tls.key", { "/dns-query" })` + "\n")
	}
	return b.String()
}

// DNSDistConfigMap wraps the rendered conf.
func DNSDistConfigMap(d *dnsv1alpha1.DNSDist) *corev1.ConfigMap {
	names := DNSDistNameSet(d)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap, Namespace: d.Namespace, Labels: dnsdistLabels(d)},
		Data:       map[string]string{"dnsdist.conf": DNSDistConfig(d)},
	}
}

// DNSDistDeployment renders the dnsdist workload with the same HA posture
// as the pdns Deployment (anti-affinity + hostname topology spread when
// replicas > 1, config-hash rolling restarts).
func DNSDistDeployment(d *dnsv1alpha1.DNSDist, configHash string) *appsv1.Deployment {
	names := DNSDistNameSet(d)
	lbls := dnsdistLabels(d)
	replicas := dnsdistReplicas(d)
	image := d.Spec.Image
	if image == "" {
		image = dnsv1alpha1.DefaultDNSDistImage
	}

	ports := []corev1.ContainerPort{
		{Name: "dns-tcp", ContainerPort: dnsTCPPort, Protocol: corev1.ProtocolTCP},
		{Name: "dns-udp", ContainerPort: dnsUDPPort, Protocol: corev1.ProtocolUDP},
	}
	svcPorts := []corev1.VolumeMount{{Name: "config", MountPath: "/etc/dnsdist", ReadOnly: true}}
	volumes := []corev1.Volume{{
		Name: "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: names.ConfigMap},
		}},
	}}
	if d.Spec.TLS.DoT.Enabled {
		ports = append(ports, corev1.ContainerPort{Name: "dot", ContainerPort: dotPort, Protocol: corev1.ProtocolTCP})
		svcPorts = append(svcPorts, corev1.VolumeMount{Name: "dot-cert", MountPath: "/tls/dot", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "dot-cert",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: d.Spec.TLS.DoT.CertificateSecretRef.Name,
			}},
		})
	}
	if d.Spec.TLS.DoH.Enabled {
		ports = append(ports, corev1.ContainerPort{Name: "doh", ContainerPort: dohPort, Protocol: corev1.ProtocolTCP})
		svcPorts = append(svcPorts, corev1.VolumeMount{Name: "doh-cert", MountPath: "/tls/doh", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "doh-cert",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: d.Spec.TLS.DoH.CertificateSecretRef.Name,
			}},
		})
	}

	podAnnotations := map[string]string{}
	if configHash != "" {
		podAnnotations[ConfigHashAnnotation] = configHash
	}

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "dnsdist",
			Image: image,
			Args:  []string{"--supervised", "--disable-syslog", "-C", "/etc/dnsdist/dnsdist.conf"},
			Ports: ports,
			Resources: d.Spec.Resources,
			VolumeMounts: svcPorts,
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(dnsTCPPort)},
				},
				InitialDelaySeconds: 2,
				PeriodSeconds:       5,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(dnsTCPPort)},
				},
				InitialDelaySeconds: 10,
				PeriodSeconds:       10,
			},
		}},
		Volumes:           volumes,
		NodeSelector:      d.Spec.Scheduling.NodeSelector,
		Tolerations:       d.Spec.Scheduling.Tolerations,
		PriorityClassName: d.Spec.Scheduling.PriorityClassName,
	}

	if replicas > 1 {
		podSpec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: lbls},
						TopologyKey:   "kubernetes.io/hostname",
					},
				}},
			},
		}
		podSpec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: lbls},
		}}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: names.Deployment, Namespace: d.Namespace, Labels: lbls},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: podAnnotations},
				Spec:       podSpec,
			},
		},
	}
}

// DNSDistDNSService renders the primary DNS Service for the tier — the
// backend target for gateway routes (gateway → dnsdist → pdns) and the
// LoadBalancer when exposure=loadBalancer.
func DNSDistDNSService(d *dnsv1alpha1.DNSDist) *corev1.Service {
	names := DNSDistNameSet(d)
	lbls := dnsdistLabels(d)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: names.DNSService, Namespace: d.Namespace, Labels: lbls},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: lbls,
			Ports: []corev1.ServicePort{
				{Name: "dns-tcp", Port: dnsTCPPort, TargetPort: intstr.FromInt(dnsTCPPort), Protocol: corev1.ProtocolTCP},
				{Name: "dns-udp", Port: dnsUDPPort, TargetPort: intstr.FromInt(dnsUDPPort), Protocol: corev1.ProtocolUDP},
			},
		},
	}
	if d.Spec.TLS.DoT.Enabled {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "dot", Port: dotPort, TargetPort: intstr.FromInt(dotPort), Protocol: corev1.ProtocolTCP})
	}
	if d.Spec.TLS.DoH.Enabled {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "doh", Port: dohPort, TargetPort: intstr.FromInt(dohPort), Protocol: corev1.ProtocolTCP})
	}
	if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureLoadBalancer {
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		applyLBPrimary(svc, d.Spec.DNS.LoadBalancer)
	}
	return svc
}

// DNSDistPDB delegates to the shared pdbFor (same semantics as servers).
func DNSDistPDB(d *dnsv1alpha1.DNSDist) *policyv1.PodDisruptionBudget {
	var override *int32
	if d.Spec.PodDisruptionBudget != nil {
		override = d.Spec.PodDisruptionBudget.MinAvailable
	}
	return pdbFor(DNSDistNameSet(d).PDB, d.Namespace, dnsdistLabels(d), dnsdistReplicas(d), override)
}

// DNSDistTCPRoute / DNSDistUDPRoute expose the tier via Gateway API,
// backending the DNSDIST Service.
func DNSDistTCPRoute(d *dnsv1alpha1.DNSDist) *gatewayv1alpha2.TCPRoute {
	names := DNSDistNameSet(d)
	return buildTCPRoute(names.TCPRoute, d.Namespace, dnsdistLabels(d),
		dnsRouteParents(&d.Spec.DNS, d.Namespace, gatewayProtoTCP), names.DNSService, dnsTCPPort)
}

func DNSDistUDPRoute(d *dnsv1alpha1.DNSDist) *gatewayv1alpha2.UDPRoute {
	names := DNSDistNameSet(d)
	return buildUDPRoute(names.UDPRoute, d.Namespace, dnsdistLabels(d),
		dnsRouteParents(&d.Spec.DNS, d.Namespace, gatewayProtoUDP), names.DNSService, dnsUDPPort)
}
```

NOTE on the variable name `svcPorts` holding VolumeMounts in DNSDistDeployment — rename to `mounts` during implementation (the plan keeps the logic; the name is a known wart, fix it inline).

- [ ] **Step 4: Run** — `go test ./internal/manifests/ -count=1 -run TestDNSDist -v` → 7/7 PASS; then the full package.

- [ ] **Step 5: Commit** — `git add internal/manifests/ && git commit -m "feat: dnsdist manifests — conf renderer, deployment, service, PDB, routes"`

---

### Task 5: DNSDistReconciler + wiring (TDD)

**Files:**
- Create: `internal/controller/dnsdist_controller.go`
- Test: `internal/controller/dnsdist_controller_test.go`
- Modify: `cmd/operator/main.go`

- [ ] **Step 1: Write the failing tests** — create `internal/controller/dnsdist_controller_test.go`:

```go
package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func newDNSDistReconciler(t *testing.T, objs ...client.Object) (*DNSDistReconciler, client.Client) {
	t.Helper()
	scheme := routesTestScheme(t) // clientgo + dns + both gateway schemes
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSDist{}, &dnsv1alpha1.PowerDNSServer{}).
		WithIndex(&dnsv1alpha1.DNSDist{}, dnsdistBackendIndex, func(o client.Object) []string {
			d := o.(*dnsv1alpha1.DNSDist)
			keys := make([]string, 0, len(d.Spec.BackendRefs))
			for _, ref := range d.Spec.BackendRefs {
				keys = append(keys, refKey(ref, d.GetNamespace()))
			}
			return keys
		}).
		WithObjects(objs...).Build()
	return &DNSDistReconciler{Client: c, Scheme: scheme}, c
}

func readyBackend(name string) *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Status:     dnsv1alpha1.PowerDNSServerStatus{Phase: dnsv1alpha1.PhaseReady},
	}
}

func edgeDNSDist() *dnsv1alpha1.DNSDist {
	two := int32(2)
	return &dnsv1alpha1.DNSDist{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default", UID: "edge-uid"},
		Spec: dnsv1alpha1.DNSDistSpec{
			Replicas:    &two,
			BackendRefs: []dnsv1alpha1.ObjectRef{{Name: "srv-a"}},
		},
	}
}

func reconcileDNSDistN(t *testing.T, r *DNSDistReconciler, key types.NamespacedName, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
}

func TestDNSDistCreatesWorkload(t *testing.T) {
	r, c := newDNSDistReconciler(t, edgeDNSDist(), readyBackend("srv-a"))
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	reconcileDNSDistN(t, r, key, 2)
	ctx := context.Background()

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-config", Namespace: "default"}, cm); err != nil {
		t.Fatalf("configmap: %v", err)
	}
	if !strings.Contains(cm.Data["dnsdist.conf"], "srv-a-dns.default.svc") {
		t.Error("conf must carry the backend")
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist", Namespace: "default"}, dep); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	if len(dep.OwnerReferences) == 0 {
		t.Error("owner ref missing — GC relies on it (no finalizer by design)")
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-dns", Namespace: "default"}, svc); err != nil {
		t.Fatalf("service: %v", err)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-pdb", Namespace: "default"}, pdb); err != nil {
		t.Fatalf("pdb: %v", err)
	}
	got := &dnsv1alpha1.DNSDist{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, dnsv1alpha1.ConditionBackendsReady) {
		t.Errorf("BackendsReady should be True: %+v", got.Status.Conditions)
	}
}

func TestDNSDistGatesOnBackendReadiness(t *testing.T) {
	backend := readyBackend("srv-a")
	backend.Status.Phase = dnsv1alpha1.PhaseDeployingServer
	r, c := newDNSDistReconciler(t, edgeDNSDist(), backend)
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	reconcileDNSDistN(t, r, key, 2)

	got := &dnsv1alpha1.DNSDist{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, dnsv1alpha1.ConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("BackendsReady must be False while the backend isn't Ready: %+v", cond)
	}
	if got.Status.Phase == dnsv1alpha1.ZonePhaseReady {
		t.Error("tier must not report Ready with unready backends")
	}
}

func TestDNSDistMissingBackendNotReady(t *testing.T) {
	r, c := newDNSDistReconciler(t, edgeDNSDist()) // backend absent
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	reconcileDNSDistN(t, r, key, 2)
	got := &dnsv1alpha1.DNSDist{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, dnsv1alpha1.ConditionBackendsReady)
	if cond == nil || cond.Reason != "BackendNotFound" {
		t.Errorf("want BackendNotFound, got %+v", cond)
	}
}

func TestDNSDistValidateRejectsCrossNamespaceAndBadPDB(t *testing.T) {
	d := edgeDNSDist()
	d.Spec.BackendRefs = []dnsv1alpha1.ObjectRef{{Name: "x", Namespace: "other"}}
	if msg := validateDNSDist(d); !strings.Contains(msg, "namespace") {
		t.Errorf("cross-namespace backendRef must be rejected, got %q", msg)
	}

	d = edgeDNSDist()
	two := int32(2)
	d.Spec.PodDisruptionBudget = &dnsv1alpha1.PDBSpec{MinAvailable: &two}
	if msg := validateDNSDist(d); !strings.Contains(msg, "minAvailable") {
		t.Errorf("minAvailable >= replicas must be rejected, got %q", msg)
	}

	d = edgeDNSDist()
	d.Spec.TLS.DoT.Enabled = true
	if msg := validateDNSDist(d); !strings.Contains(msg, "certificateSecretRef") {
		t.Errorf("DoT without cert must be rejected, got %q", msg)
	}
}

func TestDNSDistConfChangeUpdatesConfigMap(t *testing.T) {
	r, c := newDNSDistReconciler(t, edgeDNSDist(), readyBackend("srv-a"), readyBackend("srv-b"))
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	ctx := context.Background()
	reconcileDNSDistN(t, r, key, 2)

	d := &dnsv1alpha1.DNSDist{}
	if err := c.Get(ctx, key, d); err != nil {
		t.Fatal(err)
	}
	d.Spec.BackendRefs = append(d.Spec.BackendRefs, dnsv1alpha1.ObjectRef{Name: "srv-b"})
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	reconcileDNSDistN(t, r, key, 2)

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-config", Namespace: "default"}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["dnsdist.conf"], "srv-b-dns.default.svc") {
		t.Error("backend addition must converge into the live conf")
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist", Namespace: "default"}, dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Annotations[manifestsConfigHashAnnotation()] == "" {
		t.Error("config-hash annotation must be set")
	}
}
```

(`manifestsConfigHashAnnotation()` is just `manifests.ConfigHashAnnotation` — use the import directly in the real file; tests in this package already import manifests in other files. Adjust the reference accordingly.)

- [ ] **Step 2: Confirm failure** — `go test ./internal/controller/ -run TestDNSDist` → undefined symbols.

- [ ] **Step 3: Create `internal/controller/dnsdist_controller.go`:**

```go
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/manifests"
)

// dnsdistBackendIndex indexes DNSDists by each backendRef "<ns>/<name>"
// so PowerDNSServer events fan out to dependent tiers.
const dnsdistBackendIndex = "spec.backendRefs"

// DNSDistReconciler converges DNSDist tiers. Single-pass (no phase
// machine) and NO finalizer: a DNSDist owns no external state — every
// child is a Kubernetes object reaped by GC through owner references.
type DNSDistReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=dnsdists,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=dnsdists/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=dnsdists/finalizers,verbs=update

func (r *DNSDistReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	d := &dnsv1alpha1.DNSDist{}
	if err := r.Get(ctx, req.NamespacedName, d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !d.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // owner-ref GC handles everything
	}

	if msg := validateDNSDist(d); msg != "" {
		return r.markDNSDistFailed(ctx, d, msg)
	}

	allReady, reason, msg, err := r.backendsReady(ctx, d)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allReady {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionBackendsReady,
			metav1.ConditionFalse, reason, msg)
		return r.markDNSDistNotReady(ctx, d, reason, msg, requeueShort)
	}
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionBackendsReady,
		metav1.ConditionTrue, "BackendsReady", "all backends Ready")

	// Converge children (CreateOrUpdate everywhere; steady state no-ops).
	cm := manifests.DNSDistConfigMap(d)
	if err := r.upsertOwnedDNSDist(ctx, d, cm, func(live *corev1.ConfigMap) {
		live.Labels = cm.Labels
		live.Data = cm.Data
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure configmap: %w", err)
	}

	hash := sha256.Sum256([]byte(cm.Data["dnsdist.conf"] +
		d.Spec.TLS.DoT.CertificateSecretRef.Name + d.Spec.TLS.DoH.CertificateSecretRef.Name))
	dep := manifests.DNSDistDeployment(d, hex.EncodeToString(hash[:16]))
	if err := r.upsertOwnedDNSDist(ctx, d, dep, func(live *appsv1.Deployment) {
		live.Labels = dep.Labels
		live.Spec = dep.Spec
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure deployment: %w", err)
	}

	if err := r.updateService(ctx, ownedService(r.Scheme, d, manifests.DNSDistDNSService(d))); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure dns service: %w", err)
	}

	names := manifests.DNSDistNameSet(d)
	if pdb := manifests.DNSDistPDB(d); pdb != nil {
		if err := r.upsertOwnedDNSDist(ctx, d, pdb, func(live *policyv1.PodDisruptionBudget) {
			live.Labels = pdb.Labels
			live.Spec.MinAvailable = pdb.Spec.MinAvailable
			live.Spec.Selector = pdb.Spec.Selector
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure pdb: %w", err)
		}
	} else if err := r.deleteIfExists(ctx, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: names.PDB, Namespace: d.Namespace},
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete pdb: %w", err)
	}

	if err := r.reconcileDNSDistRoutes(ctx, d); err != nil {
		return ctrl.Result{}, err
	}

	return r.refreshDNSDistStatus(ctx, d)
}

// validateDNSDist is the controller-side validation pass (cross-field
// checks CEL doesn't cover).
func validateDNSDist(d *dnsv1alpha1.DNSDist) string {
	for i, ref := range d.Spec.BackendRefs {
		if ref.Name == "" {
			return fmt.Sprintf("backendRefs[%d].name is required", i)
		}
		if ref.Namespace != "" {
			return fmt.Sprintf("backendRefs[%d].namespace must be empty — cross-namespace backends are not supported", i)
		}
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	if pdb := d.Spec.PodDisruptionBudget; pdb != nil && pdb.MinAvailable != nil &&
		replicas > 1 && *pdb.MinAvailable >= replicas {
		return fmt.Sprintf("podDisruptionBudget.minAvailable (%d) must be < replicas (%d)", *pdb.MinAvailable, replicas)
	}
	if d.Spec.TLS.DoT.Enabled && d.Spec.TLS.DoT.CertificateSecretRef.Name == "" {
		return "tls.dot.certificateSecretRef.name is required when dot is enabled"
	}
	if d.Spec.TLS.DoH.Enabled && d.Spec.TLS.DoH.CertificateSecretRef.Name == "" {
		return "tls.doh.certificateSecretRef.name is required when doh is enabled"
	}
	if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureGateway &&
		(d.Spec.DNS.Gateway == nil || len(d.Spec.DNS.Gateway.ParentRefs) == 0) {
		return "dns.exposure=gateway requires dns.gateway.parentRefs (at least one)"
	}
	return ""
}

func (r *DNSDistReconciler) backendsReady(ctx context.Context, d *dnsv1alpha1.DNSDist) (bool, string, string, error) {
	for _, ref := range d.Spec.BackendRefs {
		s := &dnsv1alpha1.PowerDNSServer{}
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: d.Namespace}, s)
		if apierrors.IsNotFound(err) {
			return false, "BackendNotFound", fmt.Sprintf("PowerDNSServer %s/%s not found", d.Namespace, ref.Name), nil
		}
		if err != nil {
			return false, "", "", err
		}
		if s.Status.Phase != dnsv1alpha1.PhaseReady {
			return false, "BackendNotReady", fmt.Sprintf("PowerDNSServer %s is %s", ref.Name, s.Status.Phase), nil
		}
	}
	return true, "", "", nil
}

// reconcileDNSDistRoutes mirrors the server-side reconcileRoutes
// semantics for the tier's TCP/UDP routes (no HTTPRoute — dnsdist has no
// admin HTTP API surface here).
func (r *DNSDistReconciler) reconcileDNSDistRoutes(ctx context.Context, d *dnsv1alpha1.DNSDist) error {
	names := manifests.DNSDistNameSet(d)
	if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureGateway {
		desiredTCP := manifests.DNSDistTCPRoute(d)
		tcp := &gatewayv1alpha2.TCPRoute{ObjectMeta: metav1.ObjectMeta{Name: desiredTCP.Name, Namespace: desiredTCP.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, tcp, func() error {
			tcp.Labels = desiredTCP.Labels
			tcp.Spec = desiredTCP.Spec
			return ctrl.SetControllerReference(d, tcp, r.Scheme)
		}); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("ensure tcproute: TCPRoute CRD not installed (Gateway API experimental channel): %w", err)
			}
			return fmt.Errorf("ensure tcproute: %w", err)
		}
		desiredUDP := manifests.DNSDistUDPRoute(d)
		udp := &gatewayv1alpha2.UDPRoute{ObjectMeta: metav1.ObjectMeta{Name: desiredUDP.Name, Namespace: desiredUDP.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, udp, func() error {
			udp.Labels = desiredUDP.Labels
			udp.Spec = desiredUDP.Spec
			return ctrl.SetControllerReference(d, udp, r.Scheme)
		}); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("ensure udproute: UDPRoute CRD not installed (Gateway API experimental channel): %w", err)
			}
			return fmt.Errorf("ensure udproute: %w", err)
		}
		return nil
	}
	if err := r.deleteIfExists(ctx, &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: names.TCPRoute, Namespace: d.Namespace},
	}); err != nil {
		return fmt.Errorf("delete tcproute: %w", err)
	}
	if err := r.deleteIfExists(ctx, &gatewayv1alpha2.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: names.UDPRoute, Namespace: d.Namespace},
	}); err != nil {
		return fmt.Errorf("delete udproute: %w", err)
	}
	return nil
}

func (r *DNSDistReconciler) refreshDNSDistStatus(ctx context.Context, d *dnsv1alpha1.DNSDist) (ctrl.Result, error) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: manifests.DNSDistNameSet(d).Deployment, Namespace: d.Namespace}, dep); err != nil {
		return ctrl.Result{}, err
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	d.Status.DesiredReplicas = desired
	d.Status.ReadyReplicas = dep.Status.ReadyReplicas
	available := dep.Status.AvailableReplicas == desired && desired > 0
	if available {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionAvailable,
			metav1.ConditionTrue, "PodsAvailable", "")
	} else {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionAvailable,
			metav1.ConditionFalse, "PodsUnavailable",
			fmt.Sprintf("%d/%d replicas available", dep.Status.AvailableReplicas, desired))
	}

	// dnsEndpoint mirrors live exposure state (the server's v0.2.3 rule:
	// status reflects reality, never the wish).
	endpoint := ""
	switch d.Spec.DNS.Exposure {
	case dnsv1alpha1.DNSExposureGateway:
		if d.Spec.DNS.Gateway != nil {
			parents := make([]string, 0, len(d.Spec.DNS.Gateway.ParentRefs))
			for _, p := range d.Spec.DNS.Gateway.ParentRefs {
				ns := p.Namespace
				if ns == "" {
					ns = d.Namespace
				}
				parents = append(parents, ns+"/"+p.Name)
			}
			endpoint = "gateway:" + joinComma(parents)
		}
	default:
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: manifests.DNSDistNameSet(d).DNSService, Namespace: d.Namespace}, svc); err == nil {
			if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureLoadBalancer {
				for _, ing := range svc.Status.LoadBalancer.Ingress {
					if ing.IP != "" {
						endpoint = ing.IP + ":53"
						break
					}
					if ing.Hostname != "" {
						endpoint = ing.Hostname + ":53"
						break
					}
				}
			} else {
				endpoint = svc.Spec.ClusterIP + ":53"
			}
		}
	}
	d.Status.DNSEndpoint = endpoint

	d.Status.Phase = dnsv1alpha1.ZonePhaseReady
	d.Status.FailureMessage = ""
	d.Status.ObservedGeneration = d.Generation
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

func (r *DNSDistReconciler) markDNSDistNotReady(ctx context.Context, d *dnsv1alpha1.DNSDist, reason, msg string, after time.Duration) (ctrl.Result, error) {
	d.Status.Phase = dnsv1alpha1.ZonePhasePending
	d.Status.FailureMessage = msg
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *DNSDistReconciler) markDNSDistFailed(ctx context.Context, d *dnsv1alpha1.DNSDist, msg string) (ctrl.Result, error) {
	d.Status.Phase = dnsv1alpha1.ZonePhaseFailed
	d.Status.FailureMessage = msg
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, "InvalidSpec", msg)
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(d, corev1.EventTypeWarning, "InvalidSpec", msg)
	}
	return ctrl.Result{}, nil
}

func (r *DNSDistReconciler) dnsdistsForServer(ctx context.Context, obj client.Object) []reconcile.Request {
	var list dnsv1alpha1.DNSDistList
	if err := r.List(ctx, &list, client.MatchingFields{
		dnsdistBackendIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list dnsdists for server", "server", client.ObjectKeyFromObject(obj))
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

func (r *DNSDistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dnsv1alpha1.DNSDist{}, dnsdistBackendIndex,
		func(o client.Object) []string {
			d := o.(*dnsv1alpha1.DNSDist)
			keys := make([]string, 0, len(d.Spec.BackendRefs))
			for _, ref := range d.Spec.BackendRefs {
				keys = append(keys, refKey(ref, d.GetNamespace()))
			}
			return keys
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSDist{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// Route kinds deliberately NOT in Owns() (Gateway API optional).
		Watches(&dnsv1alpha1.PowerDNSServer{}, handler.EnqueueRequestsFromMapFunc(r.dnsdistsForServer)).
		Complete(r)
}
```

Implementation notes the engineer must resolve (small, mechanical):
- `time` import for `markDNSDistNotReady`.
- `joinComma` = `strings.Join(parents, ",")` — use strings directly, drop the helper.
- `upsertOwnedDNSDist` is a generic CreateOrUpdate wrapper; Go generics:

```go
func upsertOwned[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, desired T, mutate func(live T)) error {
	live := desired.DeepCopyObject().(T)
	// reset to bare identity; CreateOrUpdate fills from the cluster
	live.SetResourceVersion("")
	_, err := controllerutil.CreateOrUpdate(ctx, c, live, func() error {
		mutate(live)
		return controllerutil.SetControllerReference(owner, live, scheme)
	})
	return err
}
```

  Call as `upsertOwned(ctx, r.Client, r.Scheme, d, cm, func(live *corev1.ConfigMap) {...})` — adjust the calls in Reconcile from the `r.upsertOwnedDNSDist` placeholder spelling to this free function. CreateOrUpdate requires the object to have only name/namespace set initially: construct `live` as a NEW empty object with name/namespace from `desired` instead of DeepCopy (cleaner):

```go
func upsertOwned[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, blank T, name, namespace string, mutate func(live T)) error {
	blank.SetName(name)
	blank.SetNamespace(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, c, blank, func() error {
		mutate(blank)
		return controllerutil.SetControllerReference(owner, blank, scheme)
	})
	return err
}
```

  Call: `upsertOwned(ctx, r.Client, r.Scheme, d, &corev1.ConfigMap{}, cm.Name, cm.Namespace, func(live *corev1.ConfigMap) { live.Labels = cm.Labels; live.Data = cm.Data })`.
- `ownedService(...)` placeholder: `updateService` (shared with the server controller) handles create-or-converge but doesn't set owner refs on create. Set the owner ref on the desired Service BEFORE calling: `svc := manifests.DNSDistDNSService(d); if err := ctrl.SetControllerReference(d, svc, r.Scheme); err != nil { return ... }; if err := r.updateService(ctx, svc); err != nil {...}`. (`updateService` is a method on PowerDNSServerReconciler — move it plus `deleteIfExists`, `updateConfigMap`, `servicePortsMatch` to a new shared file `internal/controller/converge.go` as standalone funcs taking `client.Client`, with thin method wrappers kept on PowerDNSServerReconciler so existing call sites/tests don't change. DNSDistReconciler calls the standalone forms.)

- [ ] **Step 4: `cmd/operator/main.go`** — after the RRSetReconciler block:

```go
	if err := (&controller.DNSDistReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("aether-powerdns"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DNSDist")
		os.Exit(1)
	}
```

- [ ] **Step 5: Run** — `go test ./internal/controller/ -run TestDNSDist -count=1 -v` → 5/5 PASS; full suite `make test && make vet && make build` green (all existing server/zone/rrset tests must be untouched by the converge.go extraction).

- [ ] **Step 6: Commit** — `git add internal/controller/ cmd/operator/main.go && git commit -m "feat: DNSDistReconciler — single-pass tier convergence with backend gating"`

---

### Task 6: Docs + example

**Files:**
- Create: `examples/dnsdist-frontend.yaml`
- Modify: `examples/README.md`, `README.md`, `docs/configuration.md`, `CLAUDE.md`

- [ ] **Step 1: Create `examples/dnsdist-frontend.yaml`:**

```yaml
# dnsdist frontend tier over a PowerDNSServer: gateway routes point at
# DNSDIST (which health-checks and load-balances the pdns backends); the
# server itself runs ClusterIP-only. Prereq: a Gateway with dns-tcp/dns-udp
# listeners (see api-via-gateway.yaml's prereq notes).
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: PowerDNSServer
metadata:
  name: demo
  namespace: aether-system
spec:
  replicas: 2
  backend:
    type: postgres
    postgres:
      instances: 1
      storageSize: 5Gi
  dns:
    exposure: none          # dnsdist is the public face
---
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: DNSDist
metadata:
  name: demo-edge
  namespace: aether-system
spec:
  replicas: 2
  backendRefs:
    - name: demo
  cache:
    enabled: true
  rateLimit:
    qpsPerClient: 100
  podDisruptionBudget:
    minAvailable: 1
  dns:
    exposure: gateway
    gateway:
      parentRefs:
        - name: eg
          namespace: envoy-gateway-system
          tcpSectionName: dns-tcp
          udpSectionName: dns-udp
```

- [ ] **Step 2: `examples/README.md`** — add a row: "dnsdist frontend tier (health-checked backends, cache, rate limit) with gateway exposure pointing at dnsdist".

- [ ] **Step 3: `docs/configuration.md`** — new `## DNSDist` section: the CRD fields table, the topology rule (gateway/LB exposure moves to the DNSDist; servers run exposure none), v1 limitations (same-namespace backends, service-level backend addressing, DoT/DoH ports on the Service only, **client IPs appear as dnsdist pod IPs — proxy-protocol deliberately deferred**), PDB minAvailable on both kinds (default replicas-1, rejected when >= replicas, none at 1 replica), and the new DNS readiness probe behavior.

- [ ] **Step 4: `README.md`** — feature list addition (one paragraph in the existing voice).

- [ ] **Step 5: `CLAUDE.md`** — extend the Scope line to include the DNSDist frontier tier; add an `## DNSDist` section (terse, agent-facing): names `<name>-dnsdist*`, conf is deterministic-rendered Lua (sorted backends — hash stability), `setACL` open is mandatory, single-pass reconciler with NO finalizer (owner-ref GC), backendRefs same-ns only, routes backend the dnsdist Service, PDB shared `pdbFor` semantics, readiness probe note. Update the "Out of scope" section: dnsdist is IN scope now; the remaining dnsdist deferrals (proxy-protocol, per-pod backends, recursor, console) listed there.

- [ ] **Step 6: Verify + commit** — YAML parse the example; `git add examples/ README.md docs/configuration.md CLAUDE.md && git commit -m "docs: DNSDist frontend tier documentation + example"`

---

### Task 7: Full verification + live e2e + finish

- [ ] **Step 1: Local suite** — `make tidy && make build && make vet && go test ./... -count=1 && kubectl kustomize config/ > /dev/null && echo ok`. tidy must be a no-op.

- [ ] **Step 2: Spec coverage walk** — every spec section maps to landed code (types/CRD → T1-2, conf/workload → T4, reconciler → T5, PDB/probe → T3, docs → T6); fix gaps before proceeding.

- [ ] **Step 3: Live e2e on dev** (after release+deploy — coordinate with the session owner on the release flow: tag v0.3.0 → infra pin bump → ArgoCD; the operator must run the new code before the e2e). Test sequence, all in `.tmp/` manifests, all cleaned after:
  1. Scratch `PowerDNSServer gw-ha` (replicas 2, exposure none) + `Zone`/`RRSet` test data + `DNSDist gw-ha-edge` (replicas 2, cache on, qpsPerClient 0, gateway exposure on a dedicated throwaway Gateway with dns-tcp/dns-udp listeners — the aether-e2e Gateway pattern from the v0.2.x e2e).
  2. Assert: DNSDist Ready, BackendsReady=True, routes Accepted, conf in the live ConfigMap contains the backend + setACL.
  3. Traffic: `dig` TCP+UDP through the gateway address → answers from the test zone (path: gateway → dnsdist → pdns).
  4. **Failover**: `kubectl delete pod` one pdns backend pod while looping digs (1/s) through the gateway — expect zero or near-zero failed queries (second pdns replica + dnsdist health checks). Record the loss count.
  5. **PDB**: `kubectl drain --dry-run` is insufficient — instead `kubectl get pdb` shows `ALLOWED DISRUPTIONS: 1` for the 2-replica server with default PDB; evict one pod via the eviction API (`kubectl --as=system:admin ... create -f eviction.json` or `kubectl delete pod --grace-period=1` + verify the SECOND eviction is refused while the first replacement is unready: `kubectl get pdb -o jsonpath='{.status.disruptionsAllowed}'` == 0 during the roll).
  6. Teardown: delete DNSDist CR (owner-ref GC: deployment/cm/svc/pdb/routes all vanish — verify), server CR, Gateway, `.tmp` files.

- [ ] **Step 4: Finish the branch** — superpowers:finishing-a-development-branch (push, PR, squash-merge after CodeQL + CodeRabbit).

---

## Self-review notes (already applied)

- Identifier consistency: `DNSDistNameSet`/`DNSDistNames`, `DNSDistConfig(Map)`, `DNSDistDeployment/DNSDistDNSService/DNSDistPDB/DNSDistTCPRoute/DNSDistUDPRoute`, `dnsdistLabels`, `pdbFor`, `dnsRouteParents`, `buildTCPRoute/buildUDPRoute`, `dnsdistBackendIndex`, `validateDNSDist`, `ConditionBackendsReady`, `PDBSpec`, `DefaultDNSDistImage` — defined once each, used consistently across tasks.
- The Task 5 reconciler code intentionally contains two named integration seams (`upsertOwned` generic + the converge.go extraction) with full replacement code given — they're refactoring instructions, not placeholders.
- Spec items covered: CRD shape (T1/T2), conf renderer with ACL/cache/ratelimit/DoT/DoH/sorted-backends (T4), topology routes→dnsdist-service (T4/T5), PDB both kinds + validation (T1/T2/T3/T5), readiness probe with live verification step (T3), reconciler single-pass/no-finalizer/index/watch (T5), docs incl. proxy-protocol omission consequence (T6), e2e incl. failover + PDB (T7).
- `ConditionAvailable` reuses the existing const from types.go (server already defines it) — do NOT redefine.
