# Zone + RRSet CRDs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add declarative zone and record management to aether-powerdns via two new CRDs (`Zone`, generic `RRSet`) reconciled against the PowerDNS HTTP API.

**Architecture:** Two new namespaced CRDs in `dns.aetherplatform.cloud/v1alpha1`, each with its own reconciler in `internal/controller`, talking to PowerDNS through a thin in-repo HTTP client (`internal/pdnsclient`). Patch-only/coexist drift model — only CR-declared rrsets are ever touched. Finalizers delete from PowerDNS by default (`deletionPolicy: Orphan` opts out) with wedge-proofing when the server/zone is already gone. Cross-namespace refs authorized by `PowerDNSServer.spec.zoneManagement.allowedNamespaces`.

**Tech Stack:** Go 1.26, controller-runtime v0.23.3 (fake client + `WithStatusSubresource`/`WithIndex` for tests), `net/http/httptest` for the fake PowerDNS API. No new go.mod dependencies. CRD YAML and deepcopy are **hand-maintained** (no controller-gen in this repo).

**Spec:** `docs/superpowers/specs/2026-06-11-zone-rrset-crds-design.md` — read it before starting. Key revised semantics: `spec.nameservers` is create-only seeding; DNSSEC disable *deactivates* keys (never deletes); rrset conflicts reject *both* claimants.

**Repo facts you need (verified against current code):**
- Module: `github.com/plane-shift/aether-powerdns`. Existing API types: `api/v1alpha1/types.go`; scheme registration: `api/v1alpha1/groupversion_info.go`; hand-maintained deepcopy: `api/v1alpha1/zz_generated.deepcopy.go`.
- The server controller publishes `Status.APIEndpoint` (`http://<name>-api.<ns>.svc:<port>`) and `Status.APIKeySecretName` (Secret key `api-key`). The new controllers consume those — never construct URLs themselves.
- `internal/controller/powerdnsserver_controller.go` defines package-level `finalizer`, `requeueShort = 5s`, `requeueLong = 30s`, `pickKey(secret, keys...)`. Reuse `requeueShort`/`requeueLong`/`pickKey`; do NOT rename the existing `finalizer` const — name the new ones `zoneFinalizer`/`rrsetFinalizer`.
- `internal/controller/conditions.go` has PowerDNSServer-specific condition helpers; Task 4 adds a generic one.
- There are currently **no test files** in the repo. `make test` = `go test ./...`.
- PowerDNS HTTP API: base path `/api/v1`, server id `localhost`. Zone id == canonical dotted name. `PATCH /zones/{id}` body `{"rrsets":[{name,type,ttl,changetype,records:[{content,disabled}]}]}` with changetype `REPLACE`/`DELETE`. Cryptokeys under `/zones/{id}/cryptokeys`; `PUT /cryptokeys/{kid}` toggles `active`. `PUT /zones/{id}/rectify` rectifies. Zone create (`POST /zones`) requires the `nameservers` field to be present (may be `[]`).

---

## File map

| File | Action | Responsibility |
|---|---|---|
| `api/v1alpha1/zone_types.go` | Create | `Zone`/`ZoneList` types, zone consts, `ObjectRef`, `SOASpec`, `ZoneDNSSECSpec` |
| `api/v1alpha1/rrset_types.go` | Create | `RRSet`/`RRSetList` types |
| `api/v1alpha1/types.go` | Modify | add `ZoneManagementSpec` + field on `PowerDNSServerSpec`; update the stale "zones are out of band" doc comment |
| `api/v1alpha1/groupversion_info.go` | Modify | register new types |
| `api/v1alpha1/zz_generated.deepcopy.go` | Modify | hand-written deepcopy for all new types |
| `config/crd/dns.aetherplatform.cloud_zones.yaml` | Create | Zone CRD + CEL |
| `config/crd/dns.aetherplatform.cloud_rrsets.yaml` | Create | RRSet CRD + CEL |
| `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml` | Modify | add `spec.zoneManagement` |
| `config/rbac/rbac.yaml` | Modify | zones/rrsets verbs |
| `config/kustomization.yaml`, `Makefile` | Modify | include new CRDs; `crd` target applies the whole dir |
| `internal/pdnsclient/client.go` (+`client_test.go`) | Create | thin PowerDNS API client |
| `internal/controller/conditions.go` | Modify | generic `setCondOn` helper |
| `internal/controller/pdnsaccess.go` | Create | `pdnsClientFor`, `namespaceAllowed`, `refKey` — shared by both new reconcilers |
| `internal/controller/pdnsfake_test.go` | Create | in-memory fake PowerDNS API for reconciler tests |
| `internal/controller/zone_controller.go` (+`_test.go`) | Create | Zone reconciler |
| `internal/controller/rrset_controller.go` (+`_test.go`) | Create | RRSet reconciler |
| `cmd/operator/main.go` | Modify | wire up both reconcilers |
| `examples/zone-basic.yaml`, `examples/zone-secondary.yaml`, `examples/zone-dnssec.yaml`, `examples/rrset-cross-namespace.yaml` | Create | usage examples |
| `README.md`, `docs/managing-zones.md`, `CLAUDE.md` | Modify | document the new surface |

Work on the existing branch `feat/zone-rrset-crds-design` (the spec is already committed there).

---

### Task 1: API types

**Files:**
- Create: `api/v1alpha1/zone_types.go`
- Create: `api/v1alpha1/rrset_types.go`
- Modify: `api/v1alpha1/types.go`
- Modify: `api/v1alpha1/groupversion_info.go`
- Modify: `api/v1alpha1/zz_generated.deepcopy.go`

Types have no behavior, so no test-first here; `go vet ./...` is the deepcopy-gap check (per CLAUDE.md).

- [ ] **Step 1: Create `api/v1alpha1/zone_types.go`**

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Zone kinds (PowerDNS zone replication modes).
const (
	ZoneKindNative    = "Native"
	ZoneKindPrimary   = "Primary"
	ZoneKindSecondary = "Secondary"
)

// Deletion policies shared by Zone and RRSet.
const (
	DeletionPolicyDelete = "Delete"
	DeletionPolicyOrphan = "Orphan"
)

// Zone phases.
const (
	ZonePhasePending = "Pending"
	ZonePhaseReady   = "Ready"
	ZonePhaseFailed  = "Failed"
)

// Condition types reported on Zone.status.conditions, in addition to
// ConditionReady (shared with PowerDNSServer).
const (
	ConditionRegistered  = "Registered"
	ConditionDNSSECReady = "DNSSECReady"
)

// ObjectRef names a namespaced object. Namespace defaults to the
// referrer's own namespace when empty.
type ObjectRef struct {
	// Name of the referenced object.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the referenced object. Cross-namespace references must
	// be authorized by the target PowerDNSServer's
	// `spec.zoneManagement.allowedNamespaces`.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Zone declares one DNS zone on a PowerDNSServer. The operator owns the
// zone's existence and metadata (kind, masters, DNSSEC) but is
// patch-only about its contents: only rrsets declared via RRSet
// resources (plus a one-time NS/SOA seed at creation) are ever written.
// Records managed via the HTTP API or pdnsutil coexist untouched.
type Zone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZoneSpec   `json:"spec,omitempty"`
	Status ZoneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZoneList contains a list of Zone.
type ZoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Zone `json:"items"`
}

// ZoneSpec defines the desired zone.
type ZoneSpec struct {
	// ServerRef names the PowerDNSServer hosting this zone. Immutable.
	// +kubebuilder:validation:Required
	ServerRef ObjectRef `json:"serverRef"`

	// ZoneName is the canonical zone name with trailing dot
	// (e.g. `example.com.`). Immutable.
	// +kubebuilder:validation:Required
	ZoneName string `json:"zoneName"`

	// Kind is the PowerDNS zone kind. Defaults to Native.
	// +kubebuilder:validation:Enum=Native;Primary;Secondary
	// +kubebuilder:default=Native
	// +optional
	Kind string `json:"kind,omitempty"`

	// Masters lists the primaries a Secondary zone replicates from.
	// Required for Secondary, forbidden otherwise.
	// +optional
	Masters []string `json:"masters,omitempty"`

	// Nameservers seeds the apex NS (and default SOA) ONCE at zone
	// creation. After creation the apex NS is an ordinary record —
	// manage it via RRSet resources or the HTTP API. Create-only by
	// design.
	// +optional
	Nameservers []string `json:"nameservers,omitempty"`

	// SOA overrides the seeded SOA record at zone creation. Create-only,
	// like Nameservers. Ignored for Secondary zones.
	// +optional
	SOA *SOASpec `json:"soa,omitempty"`

	// DNSSEC controls zone signing.
	// +optional
	DNSSEC ZoneDNSSECSpec `json:"dnssec,omitempty"`

	// DeletionPolicy controls whether deleting this resource deletes the
	// zone from PowerDNS (`Delete`, default) or leaves it (`Orphan`).
	// +kubebuilder:validation:Enum=Delete;Orphan
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// SOASpec customizes the SOA record seeded at zone creation.
type SOASpec struct {
	// Hostmaster in dotted form (e.g. `hostmaster.example.com.`).
	// +kubebuilder:validation:Required
	Hostmaster string `json:"hostmaster"`

	// TTL of the SOA record. Defaults to 3600.
	// +kubebuilder:default=3600
	// +optional
	TTL *int32 `json:"ttl,omitempty"`
}

// ZoneDNSSECSpec controls DNSSEC for the zone.
type ZoneDNSSECSpec struct {
	// Enabled secures the zone with a PowerDNS-default CSK. Disabling
	// DEACTIVATES the keys but keeps them, so re-enabling reuses the
	// same key and any DS lodged at the registrar stays valid.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// ZoneStatus reflects the observed zone state.
type ZoneStatus struct {
	// Phase is Pending, Ready or Failed. Informational — the reconciler
	// is single-pass, not a phase machine.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Serial is the zone's SOA serial as last observed.
	// +optional
	Serial int64 `json:"serial,omitempty"`

	// DSRecords lists the DS records of active DNSSEC keys, for lodging
	// at the registrar.
	// +optional
	DSRecords []string `json:"dsRecords,omitempty"`

	// ObservedGeneration is the spec generation last reconciled to Ready.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready, Registered, DNSSECReady.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// FailureMessage details the cause when the zone cannot be reconciled.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`
}
```

- [ ] **Step 2: Create `api/v1alpha1/rrset_types.go`**

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RRSet declares one record set (name + type) in a Zone. One generic
// kind covers every record type — the type is a spec field, mirroring
// the PowerDNS API's rrset model. Do not add per-record-type CRDs.
type RRSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RRSetSpec   `json:"spec,omitempty"`
	Status RRSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RRSetList contains a list of RRSet.
type RRSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RRSet `json:"items"`
}

// RRSetSpec defines one record set. zoneRef, name and type are immutable
// so reconciliation stays a stateless idempotent PATCH — to rename,
// delete and recreate the resource.
type RRSetSpec struct {
	// ZoneRef names the Zone resource this record set belongs to.
	// Immutable.
	// +kubebuilder:validation:Required
	ZoneRef ObjectRef `json:"zoneRef"`

	// Name is the fully qualified record name with trailing dot
	// (e.g. `www.example.com.`). Must equal the zone apex or end in it.
	// Immutable.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type is the record type (A, AAAA, MX, TXT, …). Immutable.
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// TTL in seconds. Defaults to 3600.
	// +kubebuilder:default=3600
	// +optional
	TTL *int32 `json:"ttl,omitempty"`

	// Records holds the content strings, passed to PowerDNS verbatim
	// (TXT content must carry its own quotes; MX is `<prio> <target>`).
	// +kubebuilder:validation:MinItems=1
	Records []string `json:"records"`

	// DeletionPolicy controls whether deleting this resource removes the
	// rrset from PowerDNS (`Delete`, default) or leaves it (`Orphan`).
	// +kubebuilder:validation:Enum=Delete;Orphan
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// RRSetStatus reflects the observed record-set state.
type RRSetStatus struct {
	// ObservedGeneration is the spec generation last applied to PowerDNS.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// FailureMessage details the cause when the rrset cannot be applied.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`
}
```

- [ ] **Step 3: Modify `api/v1alpha1/types.go`**

(a) Add the field at the end of `PowerDNSServerSpec` (after the `NetworkPolicy` field):

```go
	// ZoneManagement authorizes Zone/RRSet resources in OTHER namespaces
	// to target this server. Same-namespace resources are always allowed.
	// +optional
	ZoneManagement ZoneManagementSpec `json:"zoneManagement,omitempty"`
```

(b) Add the type after `NetworkPolicySpec`:

```go
// ZoneManagementSpec gates cross-namespace Zone/RRSet references.
type ZoneManagementSpec struct {
	// AllowedNamespaces lists namespaces whose Zone/RRSet resources may
	// reference this server. `*` allows all namespaces.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}
```

(c) Update the now-stale doc comment on the `PowerDNSServer` type. Replace:

```go
// PowerDNSServer is a PowerDNS authoritative deployment with a database
// backend. Zones and records are managed out of band via the HTTP API or
// `pdnsutil` (e.g. `kubectl exec`); this CRD intentionally only owns the
// server lifecycle.
```

with:

```go
// PowerDNSServer is a PowerDNS authoritative deployment with a database
// backend. Zones and records can be managed declaratively via the Zone
// and RRSet CRDs, or out of band via the HTTP API / `pdnsutil` — the
// operator is patch-only and never touches undeclared records.
```

- [ ] **Step 4: Register the new types in `api/v1alpha1/groupversion_info.go`**

Replace the `init()` body:

```go
func init() {
	SchemeBuilder.Register(
		&PowerDNSServer{}, &PowerDNSServerList{},
		&Zone{}, &ZoneList{},
		&RRSet{}, &RRSetList{},
	)
}
```

- [ ] **Step 5: Add deepcopy to `api/v1alpha1/zz_generated.deepcopy.go`**

(a) In the existing `PowerDNSServerSpec.DeepCopyInto`, after the `in.NetworkPolicy.DeepCopyInto(&out.NetworkPolicy)` line, add:

```go
	in.ZoneManagement.DeepCopyInto(&out.ZoneManagement)
```

(b) Append at the end of the file (matches the file's controller-gen style):

```go
func (in *ZoneManagementSpec) DeepCopyInto(out *ZoneManagementSpec) {
	*out = *in
	if in.AllowedNamespaces != nil {
		out.AllowedNamespaces = make([]string, len(in.AllowedNamespaces))
		copy(out.AllowedNamespaces, in.AllowedNamespaces)
	}
}

func (in *ZoneManagementSpec) DeepCopy() *ZoneManagementSpec {
	if in == nil {
		return nil
	}
	out := new(ZoneManagementSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ObjectRef) DeepCopyInto(out *ObjectRef) {
	*out = *in
}

func (in *ObjectRef) DeepCopy() *ObjectRef {
	if in == nil {
		return nil
	}
	out := new(ObjectRef)
	in.DeepCopyInto(out)
	return out
}

func (in *Zone) DeepCopyInto(out *Zone) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Zone) DeepCopy() *Zone {
	if in == nil {
		return nil
	}
	out := new(Zone)
	in.DeepCopyInto(out)
	return out
}

func (in *Zone) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ZoneList) DeepCopyInto(out *ZoneList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Zone, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ZoneList) DeepCopy() *ZoneList {
	if in == nil {
		return nil
	}
	out := new(ZoneList)
	in.DeepCopyInto(out)
	return out
}

func (in *ZoneList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ZoneSpec) DeepCopyInto(out *ZoneSpec) {
	*out = *in
	out.ServerRef = in.ServerRef
	if in.Masters != nil {
		out.Masters = make([]string, len(in.Masters))
		copy(out.Masters, in.Masters)
	}
	if in.Nameservers != nil {
		out.Nameservers = make([]string, len(in.Nameservers))
		copy(out.Nameservers, in.Nameservers)
	}
	if in.SOA != nil {
		out.SOA = new(SOASpec)
		in.SOA.DeepCopyInto(out.SOA)
	}
	out.DNSSEC = in.DNSSEC
}

func (in *ZoneSpec) DeepCopy() *ZoneSpec {
	if in == nil {
		return nil
	}
	out := new(ZoneSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *SOASpec) DeepCopyInto(out *SOASpec) {
	*out = *in
	if in.TTL != nil {
		v := *in.TTL
		out.TTL = &v
	}
}

func (in *SOASpec) DeepCopy() *SOASpec {
	if in == nil {
		return nil
	}
	out := new(SOASpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ZoneStatus) DeepCopyInto(out *ZoneStatus) {
	*out = *in
	if in.DSRecords != nil {
		out.DSRecords = make([]string, len(in.DSRecords))
		copy(out.DSRecords, in.DSRecords)
	}
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ZoneStatus) DeepCopy() *ZoneStatus {
	if in == nil {
		return nil
	}
	out := new(ZoneStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *RRSet) DeepCopyInto(out *RRSet) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *RRSet) DeepCopy() *RRSet {
	if in == nil {
		return nil
	}
	out := new(RRSet)
	in.DeepCopyInto(out)
	return out
}

func (in *RRSet) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RRSetList) DeepCopyInto(out *RRSetList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]RRSet, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *RRSetList) DeepCopy() *RRSetList {
	if in == nil {
		return nil
	}
	out := new(RRSetList)
	in.DeepCopyInto(out)
	return out
}

func (in *RRSetList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RRSetSpec) DeepCopyInto(out *RRSetSpec) {
	*out = *in
	out.ZoneRef = in.ZoneRef
	if in.TTL != nil {
		v := *in.TTL
		out.TTL = &v
	}
	if in.Records != nil {
		out.Records = make([]string, len(in.Records))
		copy(out.Records, in.Records)
	}
}

func (in *RRSetSpec) DeepCopy() *RRSetSpec {
	if in == nil {
		return nil
	}
	out := new(RRSetSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RRSetStatus) DeepCopyInto(out *RRSetStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *RRSetStatus) DeepCopy() *RRSetStatus {
	if in == nil {
		return nil
	}
	out := new(RRSetStatus)
	in.DeepCopyInto(out)
	return out
}
```

Note: the file imports `metav1` as `v1` — keep `v1.Condition` as written above.

- [ ] **Step 6: Verify**

Run: `make build && make vet`
Expected: both succeed with no output. If vet flags a missing deepcopy for a slice/map/pointer field, fix the deepcopy — that's the check working.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/
git commit -m "feat: add Zone and RRSet API types + zoneManagement on PowerDNSServer"
```

---

### Task 2: CRD YAMLs, RBAC, kustomize, Makefile

**Files:**
- Create: `config/crd/dns.aetherplatform.cloud_zones.yaml`
- Create: `config/crd/dns.aetherplatform.cloud_rrsets.yaml`
- Modify: `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml`
- Modify: `config/rbac/rbac.yaml`
- Modify: `config/kustomization.yaml`
- Modify: `Makefile`

- [ ] **Step 1: Create `config/crd/dns.aetherplatform.cloud_zones.yaml`**

```yaml
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: zones.dns.aetherplatform.cloud
spec:
  group: dns.aetherplatform.cloud
  names:
    kind: Zone
    listKind: ZoneList
    plural: zones
    singular: zone
    shortNames: [pdnszone]
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      additionalPrinterColumns:
        - jsonPath: .spec.zoneName
          name: Zone
          type: string
        - jsonPath: .spec.kind
          name: Kind
          type: string
        - jsonPath: .status.phase
          name: Phase
          type: string
        - jsonPath: .status.serial
          name: Serial
          type: integer
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
              required: [serverRef, zoneName]
              x-kubernetes-validations:
                - rule: "self.serverRef.name == oldSelf.serverRef.name && (has(self.serverRef.namespace) ? self.serverRef.namespace : '') == (has(oldSelf.serverRef.namespace) ? oldSelf.serverRef.namespace : '')"
                  message: "serverRef is immutable after creation"
                - rule: "self.zoneName == oldSelf.zoneName"
                  message: "zoneName is immutable after creation"
                - rule: "self.kind != 'Secondary' || (has(self.masters) && self.masters.size() > 0)"
                  message: "masters is required for Secondary zones"
                - rule: "self.kind == 'Secondary' || !has(self.masters) || self.masters.size() == 0"
                  message: "masters is only allowed on Secondary zones"
              properties:
                serverRef:
                  type: object
                  required: [name]
                  properties:
                    name: { type: string }
                    namespace: { type: string }
                zoneName:
                  type: string
                  pattern: '^[a-z0-9_.\-]+\.$'
                kind:
                  type: string
                  enum: [Native, Primary, Secondary]
                  default: Native
                masters:
                  type: array
                  items: { type: string }
                nameservers:
                  type: array
                  items:
                    type: string
                    pattern: '\.$'
                soa:
                  type: object
                  required: [hostmaster]
                  properties:
                    hostmaster:
                      type: string
                      pattern: '\.$'
                    ttl:
                      type: integer
                      format: int32
                      minimum: 1
                      default: 3600
                dnssec:
                  type: object
                  properties:
                    enabled:
                      type: boolean
                      default: false
                deletionPolicy:
                  type: string
                  enum: [Delete, Orphan]
                  default: Delete
            status:
              type: object
              properties:
                phase: { type: string }
                serial: { type: integer, format: int64 }
                dsRecords:
                  type: array
                  items: { type: string }
                observedGeneration: { type: integer, format: int64 }
                failureMessage: { type: string }
                conditions:
                  type: array
                  items:
                    type: object
                    required: [type, status]
                    properties:
                      type: { type: string }
                      status: { type: string }
                      reason: { type: string }
                      message: { type: string }
                      observedGeneration: { type: integer, format: int64 }
                      lastTransitionTime: { type: string, format: date-time }
```

- [ ] **Step 2: Create `config/crd/dns.aetherplatform.cloud_rrsets.yaml`**

```yaml
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: rrsets.dns.aetherplatform.cloud
spec:
  group: dns.aetherplatform.cloud
  names:
    kind: RRSet
    listKind: RRSetList
    plural: rrsets
    singular: rrset
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      additionalPrinterColumns:
        - jsonPath: .spec.name
          name: Record
          type: string
        - jsonPath: .spec.type
          name: Type
          type: string
        - jsonPath: .spec.zoneRef.name
          name: Zone
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
              required: [zoneRef, name, type, records]
              x-kubernetes-validations:
                - rule: "self.zoneRef.name == oldSelf.zoneRef.name && (has(self.zoneRef.namespace) ? self.zoneRef.namespace : '') == (has(oldSelf.zoneRef.namespace) ? oldSelf.zoneRef.namespace : '')"
                  message: "zoneRef is immutable after creation — delete and recreate to move a record"
                - rule: "self.name == oldSelf.name"
                  message: "name is immutable after creation — delete and recreate to rename"
                - rule: "self.type == oldSelf.type"
                  message: "type is immutable after creation — delete and recreate to retype"
              properties:
                zoneRef:
                  type: object
                  required: [name]
                  properties:
                    name: { type: string }
                    namespace: { type: string }
                name:
                  type: string
                  pattern: '^(\*\.)?[a-z0-9_.\-]+\.$'
                type:
                  type: string
                  pattern: '^[A-Z0-9]+$'
                ttl:
                  type: integer
                  format: int32
                  minimum: 1
                  default: 3600
                records:
                  type: array
                  minItems: 1
                  items: { type: string }
                deletionPolicy:
                  type: string
                  enum: [Delete, Orphan]
                  default: Delete
            status:
              type: object
              properties:
                observedGeneration: { type: integer, format: int64 }
                failureMessage: { type: string }
                conditions:
                  type: array
                  items:
                    type: object
                    required: [type, status]
                    properties:
                      type: { type: string }
                      status: { type: string }
                      reason: { type: string }
                      message: { type: string }
                      observedGeneration: { type: integer, format: int64 }
                      lastTransitionTime: { type: string, format: date-time }
```

- [ ] **Step 3: Add `zoneManagement` to the PowerDNSServer CRD**

In `config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml`, under `spec.properties` (sibling of `networkPolicy`, keep alphabetical-ish placement near the end of the spec properties), add:

```yaml
                zoneManagement:
                  type: object
                  properties:
                    allowedNamespaces:
                      type: array
                      items: { type: string }
```

Mind the indentation — match the sibling properties exactly (the file nests spec properties at 16 spaces).

- [ ] **Step 4: RBAC**

In `config/rbac/rbac.yaml`, after the `powerdnsservers/finalizers` rule, add:

```yaml
  - apiGroups: ["dns.aetherplatform.cloud"]
    resources: ["zones", "rrsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["dns.aetherplatform.cloud"]
    resources: ["zones/status", "rrsets/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["dns.aetherplatform.cloud"]
    resources: ["zones/finalizers", "rrsets/finalizers"]
    verbs: ["update"]
```

- [ ] **Step 5: kustomization + Makefile**

`config/kustomization.yaml` — add the two new CRDs to `resources`:

```yaml
resources:
  - crd/dns.aetherplatform.cloud_powerdnsservers.yaml
  - crd/dns.aetherplatform.cloud_zones.yaml
  - crd/dns.aetherplatform.cloud_rrsets.yaml
  - rbac/rbac.yaml
  - manager/deployment.yaml
```

`Makefile` — replace the `crd` target body:

```makefile
crd:
	kubectl apply -f config/crd/
```

- [ ] **Step 6: Verify YAML parses**

Run: `go run sigs.k8s.io/yaml/... 2>/dev/null || python3 -c "import yaml,sys; [yaml.safe_load_all(open(f).read()) and print(f,'ok') for f in sys.argv[1:]]" config/crd/dns.aetherplatform.cloud_zones.yaml config/crd/dns.aetherplatform.cloud_rrsets.yaml`
Expected: both files print `ok`. (If python3/pyyaml is unavailable, `kubectl apply --dry-run=client -f config/crd/` against any cluster works too. Full CEL validation only happens server-side — flag any apply error during deployment, the rules above are tested syntax.)

- [ ] **Step 7: Commit**

```bash
git add config/ Makefile
git commit -m "feat: add Zone and RRSet CRDs, RBAC and kustomize wiring"
```

---

### Task 3: PowerDNS API client (`internal/pdnsclient`)

**Files:**
- Create: `internal/pdnsclient/client.go`
- Test: `internal/pdnsclient/client_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/pdnsclient/client_test.go`:

```go
package pdnsclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient returns a Client pointed at an httptest server that first
// enforces the API key, then delegates to fn.
func newTestClient(t *testing.T, fn http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "secret")
}

func TestGetZone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/servers/localhost/zones/example.com." {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"example.com.","name":"example.com.","kind":"Native","serial":2026061101,"dnssec":false}`))
	})
	z, err := c.GetZone(context.Background(), "example.com.")
	if err != nil {
		t.Fatalf("GetZone: %v", err)
	}
	if z.Kind != "Native" || z.Serial != 2026061101 {
		t.Errorf("unexpected zone: %+v", z)
	}
}

func TestGetZoneNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.GetZone(context.Background(), "missing.example.")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateZoneSendsNameserversField(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/servers/localhost/zones" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["nameservers"]; !ok {
			t.Error("nameservers field missing — PowerDNS requires it on create")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"example.com.","name":"example.com.","kind":"Native","serial":1}`))
	})
	z, err := c.CreateZone(context.Background(), &Zone{Name: "example.com.", Kind: "Native", Nameservers: []string{}})
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if z.Serial != 1 {
		t.Errorf("unexpected created zone: %+v", z)
	}
}

func TestPatchRRSets(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/servers/localhost/zones/example.com." {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RRSets []RRSet `json:"rrsets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(body.RRSets) != 1 || body.RRSets[0].ChangeType != "REPLACE" || body.RRSets[0].Records[0].Content != "203.0.113.10" {
			t.Errorf("unexpected rrsets payload: %+v", body.RRSets)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	err := c.PatchRRSets(context.Background(), "example.com.", []RRSet{{
		Name: "www.example.com.", Type: "A", TTL: 300, ChangeType: "REPLACE",
		Records: []Record{{Content: "203.0.113.10"}},
	}})
	if err != nil {
		t.Fatalf("PatchRRSets: %v", err)
	}
}

func TestDeleteZoneNotFoundIsSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.DeleteZone(context.Background(), "gone.example."); err != nil {
		t.Fatalf("DeleteZone on 404 should succeed, got %v", err)
	}
}

func TestCryptokeyLifecycle(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/servers/localhost/zones/example.com./cryptokeys":
			_, _ = w.Write([]byte(`[{"id":7,"keytype":"csk","active":true,"ds":["12345 13 2 deadbeef"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/servers/localhost/zones/example.com./cryptokeys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":8,"keytype":"csk","active":true,"ds":["67890 13 2 cafef00d"]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/servers/localhost/zones/example.com./cryptokeys/7":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()
	keys, err := c.ListCryptokeys(ctx, "example.com.")
	if err != nil || len(keys) != 1 || keys[0].ID != 7 {
		t.Fatalf("ListCryptokeys: keys=%+v err=%v", keys, err)
	}
	created, err := c.CreateCryptokey(ctx, "example.com.", Cryptokey{KeyType: "csk", Active: true})
	if err != nil || created.ID != 8 {
		t.Fatalf("CreateCryptokey: key=%+v err=%v", created, err)
	}
	k := keys[0]
	k.Active = false
	if err := c.UpdateCryptokey(ctx, "example.com.", k); err != nil {
		t.Fatalf("UpdateCryptokey: %v", err)
	}
}

func TestErrorIncludesResponseBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"Nameservers list must be given"}`))
	})
	_, err := c.CreateZone(context.Background(), &Zone{Name: "bad.example.", Nameservers: []string{}})
	if err == nil || !strings.Contains(err.Error(), "Nameservers list must be given") {
		t.Fatalf("error should carry the API message, got %v", err)
	}
}

func TestTransportErrorIsUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1", "secret") // nothing listens on port 1
	_, err := c.GetZone(context.Background(), "example.com.")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pdnsclient/...`
Expected: compile error (`undefined: New`, etc.) — the package doesn't exist yet.

- [ ] **Step 3: Implement `internal/pdnsclient/client.go`**

```go
// Package pdnsclient is a thin client for the PowerDNS authoritative HTTP
// API, wrapping exactly the endpoints the operator uses. In-repo on
// purpose: importing a third-party PowerDNS library would couple our
// release cadence to theirs (same reasoning as building the CNPG Cluster
// as unstructured).
package pdnsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when PowerDNS reports 404 for the target object.
var ErrNotFound = errors.New("pdns: not found")

// ErrUnreachable wraps transport-level failures (connection refused, DNS,
// timeout) so callers can distinguish "the API said no" from "I never
// reached the API" — the latter usually means a NetworkPolicy or a
// not-yet-ready server.
var ErrUnreachable = errors.New("pdns: unreachable")

// Client talks to one PowerDNS server's HTTP API.
type Client struct {
	baseURL string
	apiKey  string
	httpc   *http.Client
}

// New builds a Client for the API at baseURL (e.g. the PowerDNSServer's
// status.apiEndpoint, `http://<name>-api.<ns>.svc:8081`).
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Zone mirrors the subset of the PowerDNS zone object the operator uses.
type Zone struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
	Serial int64  `json:"serial,omitempty"`
	// Masters lists primaries for Secondary zones.
	Masters []string `json:"masters,omitempty"`
	// Nameservers must be PRESENT (possibly empty) on zone creation —
	// PowerDNS 422s otherwise. No omitempty, and callers pass []string{}
	// rather than nil.
	Nameservers []string `json:"nameservers"`
	DNSSEC      bool     `json:"dnssec,omitempty"`
	RRSets      []RRSet  `json:"rrsets,omitempty"`
}

// RRSet is one record set in a PATCH payload or GET response.
type RRSet struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	TTL        int32    `json:"ttl,omitempty"`
	ChangeType string   `json:"changetype,omitempty"` // REPLACE or DELETE
	Records    []Record `json:"records,omitempty"`
}

// Record is one record within an RRSet.
type Record struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

// Cryptokey is a DNSSEC key. DS is only populated by PowerDNS responses.
type Cryptokey struct {
	ID      int      `json:"id,omitempty"`
	KeyType string   `json:"keytype,omitempty"`
	Active  bool     `json:"active"`
	DS      []string `json:"ds,omitempty"`
}

// ZoneUpdate carries the mutable zone metadata for PUT /zones/{id}.
type ZoneUpdate struct {
	Kind    string   `json:"kind"`
	Masters []string `json:"masters"`
}

func zonePath(name string) string {
	return "/servers/localhost/zones/" + url.PathEscape(name)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pdns: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// GetZone fetches one zone (metadata only is fine for our use; PowerDNS
// includes rrsets, which callers may ignore).
func (c *Client) GetZone(ctx context.Context, name string) (*Zone, error) {
	z := &Zone{}
	if err := c.do(ctx, http.MethodGet, zonePath(name), nil, z); err != nil {
		return nil, err
	}
	return z, nil
}

// CreateZone registers a new zone and returns PowerDNS's view of it.
func (c *Client) CreateZone(ctx context.Context, z *Zone) (*Zone, error) {
	created := &Zone{}
	if err := c.do(ctx, http.MethodPost, "/servers/localhost/zones", z, created); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateZone PUTs mutable zone metadata (kind, masters).
func (c *Client) UpdateZone(ctx context.Context, name string, u ZoneUpdate) error {
	if u.Masters == nil {
		u.Masters = []string{}
	}
	return c.do(ctx, http.MethodPut, zonePath(name), u, nil)
}

// DeleteZone removes a zone. A 404 counts as success — the desired state
// (zone gone) already holds.
func (c *Client) DeleteZone(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, zonePath(name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// PatchRRSets applies rrset changes (changetype REPLACE/DELETE) to a zone.
func (c *Client) PatchRRSets(ctx context.Context, zone string, rrsets []RRSet) error {
	payload := struct {
		RRSets []RRSet `json:"rrsets"`
	}{rrsets}
	return c.do(ctx, http.MethodPatch, zonePath(zone), payload, nil)
}

// ListCryptokeys returns the zone's DNSSEC keys (DS included, private
// material omitted by PowerDNS).
func (c *Client) ListCryptokeys(ctx context.Context, zone string) ([]Cryptokey, error) {
	var keys []Cryptokey
	if err := c.do(ctx, http.MethodGet, zonePath(zone)+"/cryptokeys", nil, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// CreateCryptokey adds a key; an active key secures the zone.
func (c *Client) CreateCryptokey(ctx context.Context, zone string, k Cryptokey) (*Cryptokey, error) {
	created := &Cryptokey{}
	if err := c.do(ctx, http.MethodPost, zonePath(zone)+"/cryptokeys", k, created); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateCryptokey PUTs a key, typically to flip Active.
func (c *Client) UpdateCryptokey(ctx context.Context, zone string, k Cryptokey) error {
	return c.do(ctx, http.MethodPut, zonePath(zone)+"/cryptokeys/"+strconv.Itoa(k.ID), k, nil)
}

// RectifyZone recomputes DNSSEC ordering/NSEC data after key changes.
func (c *Client) RectifyZone(ctx context.Context, zone string) error {
	return c.do(ctx, http.MethodPut, zonePath(zone)+"/rectify", nil, nil)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/pdnsclient/... -v`
Expected: all 8 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pdnsclient/
git commit -m "feat: thin PowerDNS HTTP API client"
```

---

### Task 4: Generic condition helper + shared access helpers

**Files:**
- Modify: `internal/controller/conditions.go`
- Create: `internal/controller/pdnsaccess.go`

Small glue with trivial logic; covered indirectly by the reconciler tests in Tasks 6–7, except `namespaceAllowed` which gets a direct unit test.

- [ ] **Step 1: Write the failing test for `namespaceAllowed`**

Create `internal/controller/pdnsaccess_test.go`:

```go
package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func TestNamespaceAllowed(t *testing.T) {
	server := &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "pdns", Namespace: "dns-system"},
	}

	if !namespaceAllowed(server, "dns-system") {
		t.Error("same namespace must always be allowed")
	}
	if namespaceAllowed(server, "team-a") {
		t.Error("foreign namespace must be denied without an allow-list")
	}

	server.Spec.ZoneManagement.AllowedNamespaces = []string{"team-a"}
	if !namespaceAllowed(server, "team-a") {
		t.Error("listed namespace must be allowed")
	}
	if namespaceAllowed(server, "team-b") {
		t.Error("unlisted namespace must be denied")
	}

	server.Spec.ZoneManagement.AllowedNamespaces = []string{"*"}
	if !namespaceAllowed(server, "anything") {
		t.Error("wildcard must allow all namespaces")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/controller/ -run TestNamespaceAllowed`
Expected: compile error `undefined: namespaceAllowed`.

- [ ] **Step 3: Implement**

(a) Append to `internal/controller/conditions.go`:

```go
// setCondOn is setCondition for arbitrary condition lists (Zone, RRSet).
func setCondOn(conds *[]metav1.Condition, gen int64, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
}
```

(b) Create `internal/controller/pdnsaccess.go`:

```go
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/pdnsclient"
)

// pdnsClientFor builds an API client from the server's published endpoint
// and API-key Secret. Both are set once the server reaches Ready; callers
// gate on that phase first.
func pdnsClientFor(ctx context.Context, c client.Client, server *dnsv1alpha1.PowerDNSServer) (*pdnsclient.Client, error) {
	if server.Status.APIEndpoint == "" || server.Status.APIKeySecretName == "" {
		return nil, fmt.Errorf("PowerDNSServer %s/%s has not published its API endpoint yet", server.Namespace, server.Name)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Status.APIKeySecretName}, sec); err != nil {
		return nil, fmt.Errorf("read API key secret %s: %w", server.Status.APIKeySecretName, err)
	}
	key := pickKey(sec, "api-key")
	if key == "" {
		return nil, fmt.Errorf("secret %s/%s has no api-key entry", server.Namespace, server.Status.APIKeySecretName)
	}
	return pdnsclient.New(server.Status.APIEndpoint, key), nil
}

// namespaceAllowed reports whether a Zone/RRSet living in ns may target
// this server. Same-namespace is always allowed; otherwise the server's
// zoneManagement.allowedNamespaces gates it ("*" = all). Trust points the
// same way as Gateway API allowedRoutes: the server owner decides.
func namespaceAllowed(server *dnsv1alpha1.PowerDNSServer, ns string) bool {
	if ns == server.Namespace {
		return true
	}
	for _, a := range server.Spec.ZoneManagement.AllowedNamespaces {
		if a == "*" || a == ns {
			return true
		}
	}
	return false
}

// refKey canonicalizes an ObjectRef to "<ns>/<name>", defaulting the
// namespace to the referrer's. Used both by field indexes and lookups —
// keep them identical or watches silently miss.
func refKey(ref dnsv1alpha1.ObjectRef, defaultNS string) string {
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNS
	}
	return ns + "/" + ref.Name
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/controller/ -run TestNamespaceAllowed -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/conditions.go internal/controller/pdnsaccess.go internal/controller/pdnsaccess_test.go
git commit -m "feat: shared condition + PowerDNS access helpers for zone controllers"
```

---

### Task 5: Fake PowerDNS server for reconciler tests

**Files:**
- Create: `internal/controller/pdnsfake_test.go`

Test infrastructure (a `_test.go` file, ships nothing). The reconciler tests in Tasks 6–7 are its consumers and its verification.

- [ ] **Step 1: Create `internal/controller/pdnsfake_test.go`**

```go
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/pdnsclient"
)

const testAPIKey = "test-api-key"

// fakePDNS is an in-memory PowerDNS API good enough for the reconcilers:
// zones CRUD, rrset PATCH, cryptokeys, rectify.
type fakePDNS struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	zones     map[string]*pdnsclient.Zone              // by dotted name
	rrsets    map[string]map[string]pdnsclient.RRSet   // zone -> "name|type"
	keys      map[string][]pdnsclient.Cryptokey        // zone -> keys
	nextKeyID int
}

func newFakePDNS(t *testing.T) *fakePDNS {
	f := &fakePDNS{
		t:         t,
		zones:     map[string]*pdnsclient.Zone{},
		rrsets:    map[string]map[string]pdnsclient.RRSet{},
		keys:      map[string][]pdnsclient.Cryptokey{},
		nextKeyID: 1,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePDNS) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != testAPIKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const prefix = "/api/v1/servers/localhost/zones"
	path := strings.TrimPrefix(r.URL.Path, prefix)

	f.mu.Lock()
	defer f.mu.Unlock()

	// POST /zones
	if path == "" && r.Method == http.MethodPost {
		z := &pdnsclient.Zone{}
		if err := json.NewDecoder(r.Body).Decode(z); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if z.Nameservers == nil {
			http.Error(w, `{"error":"Nameservers list must be given"}`, http.StatusUnprocessableEntity)
			return
		}
		z.ID, z.Serial = z.Name, 1
		f.zones[z.Name] = z
		f.rrsets[z.Name] = map[string]pdnsclient.RRSet{}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(z)
		return
	}

	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	zoneName := parts[0]
	z, ok := f.zones[zoneName]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(z)
	case len(parts) == 1 && r.Method == http.MethodPut:
		var u pdnsclient.ZoneUpdate
		_ = json.NewDecoder(r.Body).Decode(&u)
		z.Kind, z.Masters = u.Kind, u.Masters
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		delete(f.zones, zoneName)
		delete(f.rrsets, zoneName)
		delete(f.keys, zoneName)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 1 && r.Method == http.MethodPatch:
		var body struct {
			RRSets []pdnsclient.RRSet `json:"rrsets"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, rr := range body.RRSets {
			key := rr.Name + "|" + rr.Type
			switch rr.ChangeType {
			case "REPLACE":
				rr.ChangeType = ""
				f.rrsets[zoneName][key] = rr
			case "DELETE":
				delete(f.rrsets[zoneName], key)
			}
		}
		z.Serial++
		w.WriteHeader(http.StatusNoContent)
	case len(parts) >= 2 && parts[1] == "rectify" && r.Method == http.MethodPut:
		_, _ = w.Write([]byte(`{"result":"Rectified"}`))
	case len(parts) == 2 && parts[1] == "cryptokeys" && r.Method == http.MethodGet:
		keys := f.keys[zoneName]
		if keys == nil {
			keys = []pdnsclient.Cryptokey{}
		}
		_ = json.NewEncoder(w).Encode(keys)
	case len(parts) == 2 && parts[1] == "cryptokeys" && r.Method == http.MethodPost:
		var k pdnsclient.Cryptokey
		_ = json.NewDecoder(r.Body).Decode(&k)
		k.ID = f.nextKeyID
		f.nextKeyID++
		k.DS = []string{strconv.Itoa(k.ID*11111) + " 13 2 deadbeef"}
		f.keys[zoneName] = append(f.keys[zoneName], k)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(k)
	case len(parts) == 3 && parts[1] == "cryptokeys" && r.Method == http.MethodPut:
		id, _ := strconv.Atoi(parts[2])
		var k pdnsclient.Cryptokey
		_ = json.NewDecoder(r.Body).Decode(&k)
		for i := range f.keys[zoneName] {
			if f.keys[zoneName][i].ID == id {
				f.keys[zoneName][i].Active = k.Active
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("fakePDNS: unhandled %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// getRRSet returns the stored rrset and whether it exists.
func (f *fakePDNS) getRRSet(zone, name, typ string) (pdnsclient.RRSet, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rr, ok := f.rrsets[zone][name+"|"+typ]
	return rr, ok
}

// hasZone reports whether the zone exists.
func (f *fakePDNS) hasZone(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.zones[name]
	return ok
}

// seedZone pre-creates a zone, as if made out of band.
func (f *fakePDNS) seedZone(name, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones[name] = &pdnsclient.Zone{ID: name, Name: name, Kind: kind, Serial: 1, Nameservers: []string{}}
	f.rrsets[name] = map[string]pdnsclient.RRSet{}
}

// zoneKeys returns a copy of the zone's cryptokeys.
func (f *fakePDNS) zoneKeys(name string) []pdnsclient.Cryptokey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pdnsclient.Cryptokey, len(f.keys[name]))
	copy(out, f.keys[name])
	return out
}

// readyServer builds a Ready PowerDNSServer wired to the fake, plus its
// API-key Secret — the two objects every reconciler test needs.
func readyServer(name, ns string, f *fakePDNS) (*dnsv1alpha1.PowerDNSServer, *corev1.Secret) {
	server := &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: dnsv1alpha1.PowerDNSServerStatus{
			Phase:            dnsv1alpha1.PhaseReady,
			APIEndpoint:      f.srv.URL,
			APIKeySecretName: name + "-api-key",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-api-key", Namespace: ns},
		Data:       map[string][]byte{"api-key": []byte(testAPIKey)},
	}
	return server, secret
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go vet ./internal/controller/`
Expected: clean. (Unused-helper warnings don't exist in vet; the consumers arrive in Task 6.)

- [ ] **Step 3: Commit**

```bash
git add internal/controller/pdnsfake_test.go
git commit -m "test: in-memory fake PowerDNS API for reconciler tests"
```

---

### Task 6: Zone controller

**Files:**
- Create: `internal/controller/zone_controller.go`
- Test: `internal/controller/zone_controller_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/zone_controller_test.go`:

```go
package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newZoneReconciler(t *testing.T, objs ...client.Object) (*ZoneReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.Zone{}, &dnsv1alpha1.PowerDNSServer{}).
		WithIndex(&dnsv1alpha1.Zone{}, zoneServerIndex, func(o client.Object) []string {
			z := o.(*dnsv1alpha1.Zone)
			return []string{refKey(z.Spec.ServerRef, z.GetNamespace())}
		}).
		WithObjects(objs...).
		Build()
	return &ZoneReconciler{Client: c, Scheme: scheme}, c
}

// reconcileZoneN drives the reconciler through n passes (finalizer-add and
// phase writes requeue, so single calls rarely settle).
func reconcileZoneN(t *testing.T, r *ZoneReconciler, key types.NamespacedName, n int) ctrl.Result {
	t.Helper()
	var res ctrl.Result
	var err error
	for i := 0; i < n; i++ {
		res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
	}
	return res
}

func basicZone(ns string) *dnsv1alpha1.Zone {
	return &dnsv1alpha1.Zone{
		ObjectMeta: metav1.ObjectMeta{Name: "example-com", Namespace: ns},
		Spec: dnsv1alpha1.ZoneSpec{
			ServerRef:   dnsv1alpha1.ObjectRef{Name: "pdns"},
			ZoneName:    "example.com.",
			Kind:        dnsv1alpha1.ZoneKindNative,
			Nameservers: []string{"ns1.example.com."},
		},
	}
}

func getZone(t *testing.T, c client.Client, key types.NamespacedName) *dnsv1alpha1.Zone {
	t.Helper()
	z := &dnsv1alpha1.Zone{}
	if err := c.Get(context.Background(), key, z); err != nil {
		t.Fatal(err)
	}
	return z
}

func TestZoneCreateRegistersInPowerDNS(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	res := reconcileZoneN(t, r, key, 3)

	if !f.hasZone("example.com.") {
		t.Fatal("zone was not created in PowerDNS")
	}
	z := getZone(t, c, key)
	if z.Status.Phase != dnsv1alpha1.ZonePhaseReady {
		t.Errorf("phase = %q, want Ready (conditions: %+v)", z.Status.Phase, z.Status.Conditions)
	}
	if !meta.IsStatusConditionTrue(z.Status.Conditions, dnsv1alpha1.ConditionRegistered) {
		t.Error("Registered condition should be True")
	}
	if res.RequeueAfter != resyncInterval {
		t.Errorf("Ready zones must resync every %v, got %v", resyncInterval, res.RequeueAfter)
	}
	if len(z.Finalizers) == 0 {
		t.Error("finalizer missing")
	}
}

func TestZoneSeedsSOAOnCreate(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.SOA = &dnsv1alpha1.SOASpec{Hostmaster: "hostmaster.example.com."}
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	soa, ok := f.getRRSet("example.com.", "example.com.", "SOA")
	if !ok {
		t.Fatal("SOA was not seeded")
	}
	if soa.Records[0].Content != "ns1.example.com. hostmaster.example.com. 0 10800 3600 604800 3600" {
		t.Errorf("unexpected SOA content: %q", soa.Records[0].Content)
	}
}

func TestZoneServerNotFound(t *testing.T) {
	zone := basicZone("dns")
	r, c := newZoneReconciler(t, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)

	z := getZone(t, c, key)
	cond := meta.FindStatusCondition(z.Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServerNotFound" {
		t.Errorf("want Ready=False/ServerNotFound, got %+v", cond)
	}
}

func TestZoneCrossNamespaceAuthz(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns-system", f)
	zone := basicZone("team-a")
	zone.Spec.ServerRef = dnsv1alpha1.ObjectRef{Name: "pdns", Namespace: "dns-system"}
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "team-a"}

	// denied without an allow-list
	reconcileZoneN(t, r, key, 3)
	z := getZone(t, c, key)
	cond := meta.FindStatusCondition(z.Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "NamespaceNotAllowed" {
		t.Fatalf("want NamespaceNotAllowed, got %+v", cond)
	}

	// allowed once listed
	server2 := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pdns", Namespace: "dns-system"}, server2); err != nil {
		t.Fatal(err)
	}
	server2.Spec.ZoneManagement.AllowedNamespaces = []string{"team-a"}
	if err := c.Update(context.Background(), server2); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 3)
	if !f.hasZone("example.com.") {
		t.Error("zone should be created once the namespace is allowed")
	}
}

func TestZoneSecondaryWithDNSSECIsInvalid(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindSecondary
	zone.Spec.Masters = []string{"198.51.100.1"}
	zone.Spec.Nameservers = nil
	zone.Spec.DNSSEC.Enabled = true
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)

	z := getZone(t, c, key)
	if z.Status.Phase != dnsv1alpha1.ZonePhaseFailed {
		t.Errorf("phase = %q, want Failed", z.Status.Phase)
	}
	if f.hasZone("example.com.") {
		t.Error("invalid zone must not be created in PowerDNS")
	}
}

func TestZoneKindDriftCorrected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindPrimary
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	f.mu.Lock()
	kind := f.zones["example.com."].Kind
	f.mu.Unlock()
	if kind != "Primary" {
		t.Errorf("zone kind = %q, want Primary (drift not corrected)", kind)
	}
}

func TestZoneDNSSECLifecycle(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.DNSSEC.Enabled = true
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)
	z := getZone(t, c, key)
	if len(z.Status.DSRecords) == 0 {
		t.Fatal("dsRecords not surfaced after enabling DNSSEC")
	}
	keys := f.zoneKeys("example.com.")
	if len(keys) != 1 || !keys[0].Active {
		t.Fatalf("want one active key, got %+v", keys)
	}

	// disable: key DEACTIVATED, not deleted
	z.Spec.DNSSEC.Enabled = false
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 3)
	keys = f.zoneKeys("example.com.")
	if len(keys) != 1 || keys[0].Active {
		t.Fatalf("disable must deactivate but KEEP the key, got %+v", keys)
	}

	// re-enable: same key reactivated, no new key minted
	z = getZone(t, c, key)
	z.Spec.DNSSEC.Enabled = true
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 3)
	keys = f.zoneKeys("example.com.")
	if len(keys) != 1 || !keys[0].Active {
		t.Fatalf("re-enable must reuse the existing key, got %+v", keys)
	}
}

func TestZoneDeleteRemovesFromPowerDNS(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)
	if !f.hasZone("example.com.") {
		t.Fatal("precondition: zone exists")
	}
	if err := c.Delete(context.Background(), getZone(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 2)
	if f.hasZone("example.com.") {
		t.Error("zone should be deleted from PowerDNS")
	}
}

func TestZoneDeleteOrphanKeepsZone(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.DeletionPolicy = dnsv1alpha1.DeletionPolicyOrphan
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)
	if err := c.Delete(context.Background(), getZone(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 2)
	if !f.hasZone("example.com.") {
		t.Error("Orphan policy must leave the zone in PowerDNS")
	}
}

func TestZoneDeleteReleasesWhenServerGone(t *testing.T) {
	// Wedge-proofing: a Zone whose server CR no longer exists must still
	// be deletable — the zone data dies with the server's Postgres anyway.
	zone := basicZone("dns")
	zone.Finalizers = []string{zoneFinalizer}
	now := metav1.NewTime(time.Now())
	zone.DeletionTimestamp = &now
	r, c := newZoneReconciler(t, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	err := c.Get(context.Background(), key, &dnsv1alpha1.Zone{})
	if err == nil {
		t.Error("zone should be gone after finalizer release")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestZone`
Expected: compile error (`undefined: ZoneReconciler`, `undefined: zoneServerIndex`).

- [ ] **Step 3: Implement `internal/controller/zone_controller.go`**

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/pdnsclient"
)

const (
	zoneFinalizer = "dns.aetherplatform.cloud/zone-protection"

	// zoneServerIndex indexes Zones by "<ns>/<name>" of their serverRef so
	// PowerDNSServer events fan out to dependent zones.
	zoneServerIndex = "spec.serverRef"

	// resyncInterval re-runs Ready zones/rrsets to correct out-of-band
	// drift on DECLARED fields only — patch-only by design, undeclared
	// records are never touched.
	resyncInterval = 5 * time.Minute
)

// ZoneReconciler reconciles Zone resources against the PowerDNS HTTP API.
// Unlike the server controller this is not a phase machine: every pass
// converges the whole zone from scratch; status.phase is informational.
type ZoneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=zones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=zones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=zones/finalizers,verbs=update

func (r *ZoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	zone := &dnsv1alpha1.Zone{}
	if err := r.Get(ctx, req.NamespacedName, zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !zone.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, zone)
	}
	if !controllerutil.ContainsFinalizer(zone, zoneFinalizer) {
		controllerutil.AddFinalizer(zone, zoneFinalizer)
		if err := r.Update(ctx, zone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Cross-field check CEL can't express against zone semantics:
	// Secondary content is replicated pre-signed; we can't sign it here.
	if zone.Spec.Kind == dnsv1alpha1.ZoneKindSecondary && zone.Spec.DNSSEC.Enabled {
		return r.markFailed(ctx, zone, "dnssec cannot be enabled on Secondary zones — sign at the primary")
	}

	server := &dnsv1alpha1.PowerDNSServer{}
	if err := r.Get(ctx, serverKeyFor(zone), server); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markNotReady(ctx, zone, "ServerNotFound",
				fmt.Sprintf("PowerDNSServer %s not found", serverKeyFor(zone)), requeueLong)
		}
		return ctrl.Result{}, err
	}
	if !namespaceAllowed(server, zone.Namespace) {
		return r.markNotReady(ctx, zone, "NamespaceNotAllowed",
			fmt.Sprintf("namespace %q is not allowed by %s/%s spec.zoneManagement.allowedNamespaces",
				zone.Namespace, server.Namespace, server.Name), requeueLong)
	}
	if server.Status.Phase != dnsv1alpha1.PhaseReady {
		return r.markNotReady(ctx, zone, "ServerNotReady", "PowerDNSServer is not Ready yet", requeueShort)
	}

	pc, err := pdnsClientFor(ctx, r.Client, server)
	if err != nil {
		return r.markNotReady(ctx, zone, "APIUnreachable", err.Error(), requeueLong)
	}
	return r.reconcileZone(ctx, zone, pc)
}

func (r *ZoneReconciler) reconcileZone(ctx context.Context, zone *dnsv1alpha1.Zone, pc *pdnsclient.Client) (ctrl.Result, error) {
	pz, err := pc.GetZone(ctx, zone.Spec.ZoneName)
	if errors.Is(err, pdnsclient.ErrNotFound) {
		pz, err = r.createZone(ctx, zone, pc)
	}
	if err != nil {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionRegistered,
			metav1.ConditionFalse, apiReason(err), err.Error())
		return r.markNotReady(ctx, zone, apiReason(err), err.Error(), requeueLong)
	}
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionRegistered,
		metav1.ConditionTrue, "ZoneExists", "zone registered in PowerDNS")

	// kind/masters drift (the only zone metadata we own post-create).
	wantKind := zone.Spec.Kind
	if wantKind == "" {
		wantKind = dnsv1alpha1.ZoneKindNative
	}
	if pz.Kind != wantKind || !slices.Equal(pz.Masters, zone.Spec.Masters) {
		if uerr := pc.UpdateZone(ctx, zone.Spec.ZoneName, pdnsclient.ZoneUpdate{
			Kind: wantKind, Masters: zone.Spec.Masters,
		}); uerr != nil {
			return r.markNotReady(ctx, zone, apiReason(uerr), uerr.Error(), requeueLong)
		}
	}

	ds, derr := r.ensureDNSSEC(ctx, zone, pc)
	if derr != nil {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionDNSSECReady,
			metav1.ConditionFalse, apiReason(derr), derr.Error())
		return r.markNotReady(ctx, zone, apiReason(derr), derr.Error(), requeueLong)
	}
	zone.Status.DSRecords = ds
	if zone.Spec.DNSSEC.Enabled {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionDNSSECReady,
			metav1.ConditionTrue, "Secured", "zone is signed; lodge status.dsRecords at the registrar")
	} else {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionDNSSECReady,
			metav1.ConditionFalse, "Disabled", "dnssec not enabled")
	}

	zone.Status.Phase = dnsv1alpha1.ZonePhaseReady
	zone.Status.Serial = pz.Serial
	zone.Status.ObservedGeneration = zone.Generation
	zone.Status.FailureMessage = ""
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, zone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// createZone registers the zone, seeding NS and (optionally) SOA exactly
// ONCE. After creation the apex NS/SOA are ordinary records owned by
// whoever manages them — RRSet CRs or the HTTP API. Create-only by design.
func (r *ZoneReconciler) createZone(ctx context.Context, zone *dnsv1alpha1.Zone, pc *pdnsclient.Client) (*pdnsclient.Zone, error) {
	kind := zone.Spec.Kind
	if kind == "" {
		kind = dnsv1alpha1.ZoneKindNative
	}
	ns := zone.Spec.Nameservers
	if ns == nil {
		ns = []string{} // PowerDNS requires the field to be present
	}
	created, err := pc.CreateZone(ctx, &pdnsclient.Zone{
		Name:        zone.Spec.ZoneName,
		Kind:        kind,
		Masters:     zone.Spec.Masters,
		Nameservers: ns,
	})
	if err != nil {
		return nil, err
	}

	if zone.Spec.SOA != nil && kind != dnsv1alpha1.ZoneKindSecondary {
		primary := zone.Spec.ZoneName
		if len(zone.Spec.Nameservers) > 0 {
			primary = zone.Spec.Nameservers[0]
		}
		ttl := int32(3600)
		if zone.Spec.SOA.TTL != nil {
			ttl = *zone.Spec.SOA.TTL
		}
		// serial 0: PowerDNS bumps it on every API change.
		soa := pdnsclient.RRSet{
			Name: zone.Spec.ZoneName, Type: "SOA", TTL: ttl, ChangeType: "REPLACE",
			Records: []pdnsclient.Record{{
				Content: fmt.Sprintf("%s %s 0 10800 3600 604800 3600", primary, zone.Spec.SOA.Hostmaster),
			}},
		}
		if err := pc.PatchRRSets(ctx, zone.Spec.ZoneName, []pdnsclient.RRSet{soa}); err != nil {
			return nil, fmt.Errorf("seed SOA: %w", err)
		}
	}
	r.event(zone, corev1.EventTypeNormal, "ZoneCreated", "registered "+zone.Spec.ZoneName+" in PowerDNS")
	return created, nil
}

// ensureDNSSEC converges key state and returns the DS records of active
// keys. Disable DEACTIVATES keys instead of deleting them, so a later
// re-enable reuses the same key and any DS lodged at the registrar stays
// valid.
func (r *ZoneReconciler) ensureDNSSEC(ctx context.Context, zone *dnsv1alpha1.Zone, pc *pdnsclient.Client) ([]string, error) {
	keys, err := pc.ListCryptokeys(ctx, zone.Spec.ZoneName)
	if err != nil {
		return nil, err
	}

	if !zone.Spec.DNSSEC.Enabled {
		for _, k := range keys {
			if !k.Active {
				continue
			}
			k.Active = false
			if err := pc.UpdateCryptokey(ctx, zone.Spec.ZoneName, k); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if !slices.ContainsFunc(keys, func(k pdnsclient.Cryptokey) bool { return k.Active }) {
		if len(keys) > 0 {
			k := keys[0]
			k.Active = true
			if err := pc.UpdateCryptokey(ctx, zone.Spec.ZoneName, k); err != nil {
				return nil, err
			}
		} else {
			if _, err := pc.CreateCryptokey(ctx, zone.Spec.ZoneName, pdnsclient.Cryptokey{KeyType: "csk", Active: true}); err != nil {
				return nil, err
			}
		}
		if err := pc.RectifyZone(ctx, zone.Spec.ZoneName); err != nil {
			return nil, err
		}
		if keys, err = pc.ListCryptokeys(ctx, zone.Spec.ZoneName); err != nil {
			return nil, err
		}
	}

	var ds []string
	for _, k := range keys {
		if k.Active {
			ds = append(ds, k.DS...)
		}
	}
	return ds, nil
}

// reconcileDelete removes the zone from PowerDNS (unless Orphan), with
// wedge-proofing: when the server CR is gone, deleting, or Failed, the
// finalizer releases WITHOUT an API call — the zone data dies with the
// server's Postgres anyway, and blocking deletion on an unreachable
// backend is how teardowns wedge.
func (r *ZoneReconciler) reconcileDelete(ctx context.Context, zone *dnsv1alpha1.Zone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(zone, zoneFinalizer) {
		return ctrl.Result{}, nil
	}
	if zone.Spec.DeletionPolicy != dnsv1alpha1.DeletionPolicyOrphan {
		server := &dnsv1alpha1.PowerDNSServer{}
		err := r.Get(ctx, serverKeyFor(zone), server)
		switch {
		case apierrors.IsNotFound(err),
			err == nil && !server.DeletionTimestamp.IsZero(),
			err == nil && server.Status.Phase == dnsv1alpha1.PhaseFailed:
			// release without touching the API
		case err != nil:
			return ctrl.Result{}, err
		default:
			pc, cerr := pdnsClientFor(ctx, r.Client, server)
			if cerr != nil {
				return ctrl.Result{}, cerr
			}
			if derr := pc.DeleteZone(ctx, zone.Spec.ZoneName); derr != nil {
				return ctrl.Result{}, derr
			}
			r.event(zone, corev1.EventTypeNormal, "ZoneDeleted", "removed "+zone.Spec.ZoneName+" from PowerDNS")
		}
	}
	controllerutil.RemoveFinalizer(zone, zoneFinalizer)
	return ctrl.Result{}, r.Update(ctx, zone)
}

func (r *ZoneReconciler) markNotReady(ctx context.Context, zone *dnsv1alpha1.Zone, reason, msg string, after time.Duration) (ctrl.Result, error) {
	zone.Status.Phase = dnsv1alpha1.ZonePhasePending
	zone.Status.FailureMessage = msg
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, zone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// markFailed flags invalid specs. Not terminal machinery — the reconciler
// re-validates from scratch on every pass, so fixing the spec recovers.
func (r *ZoneReconciler) markFailed(ctx context.Context, zone *dnsv1alpha1.Zone, msg string) (ctrl.Result, error) {
	zone.Status.Phase = dnsv1alpha1.ZonePhaseFailed
	zone.Status.FailureMessage = msg
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, "InvalidSpec", msg)
	if err := r.Status().Update(ctx, zone); err != nil {
		return ctrl.Result{}, err
	}
	r.event(zone, corev1.EventTypeWarning, "InvalidSpec", msg)
	return ctrl.Result{}, nil
}

func (r *ZoneReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(obj, eventType, reason, message)
}

// serverKeyFor resolves the serverRef, defaulting to the zone's namespace.
func serverKeyFor(zone *dnsv1alpha1.Zone) types.NamespacedName {
	ns := zone.Spec.ServerRef.Namespace
	if ns == "" {
		ns = zone.Namespace
	}
	return types.NamespacedName{Namespace: ns, Name: zone.Spec.ServerRef.Name}
}

// apiReason maps client errors to a condition reason: transport-level
// failures surface as APIUnreachable (usually NetworkPolicy or a dying
// server), everything else as APIError.
func apiReason(err error) string {
	if errors.Is(err, pdnsclient.ErrUnreachable) {
		return "APIUnreachable"
	}
	return "APIError"
}

func (r *ZoneReconciler) zonesForServer(ctx context.Context, obj client.Object) []reconcile.Request {
	var zones dnsv1alpha1.ZoneList
	if err := r.List(ctx, &zones, client.MatchingFields{
		zoneServerIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(zones.Items))
	for i := range zones.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&zones.Items[i])})
	}
	return reqs
}

// SetupWithManager registers the index and the PowerDNSServer watch so a
// server becoming Ready (or changing its allow-list) wakes its zones.
func (r *ZoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dnsv1alpha1.Zone{}, zoneServerIndex,
		func(o client.Object) []string {
			z := o.(*dnsv1alpha1.Zone)
			return []string{refKey(z.Spec.ServerRef, z.GetNamespace())}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.Zone{}).
		Watches(&dnsv1alpha1.PowerDNSServer{}, handler.EnqueueRequestsFromMapFunc(r.zonesForServer)).
		Complete(r)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestZone -v`
Expected: all 10 zone tests PASS. Common failure: `WithIndex` extractor in the test must EXACTLY match the one in `SetupWithManager` (both use `refKey`) — if `TestZoneCrossNamespaceAuthz` can't find zones, check that first.

- [ ] **Step 5: Run the full suite**

Run: `make test && make vet`
Expected: PASS, no vet findings.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/zone_controller.go internal/controller/zone_controller_test.go
git commit -m "feat: Zone reconciler — create/drift/DNSSEC/delete against the PowerDNS API"
```

---

### Task 7: RRSet controller

**Files:**
- Create: `internal/controller/rrset_controller.go`
- Test: `internal/controller/rrset_controller_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/rrset_controller_test.go`:

```go
package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func newRRSetReconciler(t *testing.T, objs ...client.Object) (*RRSetReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.Zone{}, &dnsv1alpha1.RRSet{}, &dnsv1alpha1.PowerDNSServer{}).
		WithIndex(&dnsv1alpha1.RRSet{}, rrsetZoneIndex, func(o client.Object) []string {
			rr := o.(*dnsv1alpha1.RRSet)
			return []string{refKey(rr.Spec.ZoneRef, rr.GetNamespace())}
		}).
		WithObjects(objs...).
		Build()
	return &RRSetReconciler{Client: c, Scheme: scheme}, c
}

func reconcileRRSetN(t *testing.T, r *RRSetReconciler, key types.NamespacedName, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
	}
}

// readyZone returns a Zone CR already marked Ready, as the zone
// controller would leave it.
func readyZone(ns string) *dnsv1alpha1.Zone {
	z := basicZone(ns)
	z.Finalizers = []string{zoneFinalizer}
	z.Status.Phase = dnsv1alpha1.ZonePhaseReady
	return z
}

func wwwRRSet(ns string) *dnsv1alpha1.RRSet {
	ttl := int32(300)
	return &dnsv1alpha1.RRSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www-example-com", Namespace: ns},
		Spec: dnsv1alpha1.RRSetSpec{
			ZoneRef: dnsv1alpha1.ObjectRef{Name: "example-com"},
			Name:    "www.example.com.",
			Type:    "A",
			TTL:     &ttl,
			Records: []string{"203.0.113.10"},
		},
	}
}

func getRR(t *testing.T, c client.Client, key types.NamespacedName) *dnsv1alpha1.RRSet {
	t.Helper()
	rr := &dnsv1alpha1.RRSet{}
	if err := c.Get(context.Background(), key, rr); err != nil {
		t.Fatal(err)
	}
	return rr
}

func TestRRSetApplyReplaces(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), wwwRRSet("dns"))
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)

	got, ok := f.getRRSet("example.com.", "www.example.com.", "A")
	if !ok {
		t.Fatal("rrset was not applied")
	}
	if got.TTL != 300 || got.Records[0].Content != "203.0.113.10" {
		t.Errorf("unexpected applied rrset: %+v", got)
	}
	rr := getRR(t, c, key)
	if !meta.IsStatusConditionTrue(rr.Status.Conditions, dnsv1alpha1.ConditionReady) {
		t.Errorf("Ready should be True, conditions: %+v", rr.Status.Conditions)
	}
	if rr.Status.ObservedGeneration != rr.Generation {
		t.Error("observedGeneration not updated")
	}
}

func TestRRSetDefaultTTL(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	rr := wwwRRSet("dns")
	rr.Spec.TTL = nil
	r, _ := newRRSetReconciler(t, server, secret, readyZone("dns"), rr)

	reconcileRRSetN(t, r, types.NamespacedName{Name: "www-example-com", Namespace: "dns"}, 3)

	got, _ := f.getRRSet("example.com.", "www.example.com.", "A")
	if got.TTL != 3600 {
		t.Errorf("default TTL = %d, want 3600", got.TTL)
	}
}

func TestRRSetNameOutsideZoneRejected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	rr := wwwRRSet("dns")
	rr.Spec.Name = "www.other.org."
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), rr)
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)

	cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "NameOutsideZone" {
		t.Errorf("want NameOutsideZone, got %+v", cond)
	}
}

func TestRRSetSecondaryZoneRejected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Secondary")
	server, secret := readyServer("pdns", "dns", f)
	zone := readyZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindSecondary
	zone.Spec.Masters = []string{"198.51.100.1"}
	zone.Spec.Nameservers = nil
	r, c := newRRSetReconciler(t, server, secret, zone, wwwRRSet("dns"))
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)

	cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "SecondaryZone" {
		t.Errorf("want SecondaryZone, got %+v", cond)
	}
}

func TestRRSetConflictRejectsBoth(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	a := wwwRRSet("dns")
	b := wwwRRSet("dns")
	b.Name = "www-example-com-dup"
	b.Spec.Records = []string{"198.51.100.99"}
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), a, b)
	keyA := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}
	keyB := types.NamespacedName{Name: "www-example-com-dup", Namespace: "dns"}

	reconcileRRSetN(t, r, keyA, 3)
	reconcileRRSetN(t, r, keyB, 3)

	for _, key := range []types.NamespacedName{keyA, keyB} {
		cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Conflict" {
			t.Errorf("%s: want Ready=False/Conflict, got %+v", key.Name, cond)
		}
	}
	if _, ok := f.getRRSet("example.com.", "www.example.com.", "A"); ok {
		t.Error("neither conflicting rrset may be applied")
	}
}

func TestRRSetDeleteRemovesFromPowerDNS(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), wwwRRSet("dns"))
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)
	if _, ok := f.getRRSet("example.com.", "www.example.com.", "A"); !ok {
		t.Fatal("precondition: rrset applied")
	}
	if err := c.Delete(context.Background(), getRR(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileRRSetN(t, r, key, 2)
	if _, ok := f.getRRSet("example.com.", "www.example.com.", "A"); ok {
		t.Error("rrset should be deleted from PowerDNS")
	}
}

func TestRRSetDeleteSkipsAPIWhenSiblingSurvives(t *testing.T) {
	// With reject-both conflicts, deleting one claimant must NOT wipe the
	// rrset out from under the survivor — skip the API delete and let the
	// survivor re-apply on its next pass.
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	a := wwwRRSet("dns")
	b := wwwRRSet("dns")
	b.Name = "www-example-com-dup"
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), a, b)
	keyA := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}
	keyB := types.NamespacedName{Name: "www-example-com-dup", Namespace: "dns"}

	reconcileRRSetN(t, r, keyA, 3)
	reconcileRRSetN(t, r, keyB, 3)
	if err := c.Delete(context.Background(), getRR(t, c, keyB)); err != nil {
		t.Fatal(err)
	}
	reconcileRRSetN(t, r, keyB, 2)

	// survivor recovers
	reconcileRRSetN(t, r, keyA, 3)
	got, ok := f.getRRSet("example.com.", "www.example.com.", "A")
	if !ok {
		t.Fatal("survivor's rrset should be applied after the conflict clears")
	}
	if got.Records[0].Content != "203.0.113.10" {
		t.Errorf("survivor content = %q", got.Records[0].Content)
	}
}

func TestRRSetDeleteReleasesWhenZoneGone(t *testing.T) {
	rr := wwwRRSet("dns")
	rr.Finalizers = []string{rrsetFinalizer}
	r, c := newRRSetReconciler(t, rr)
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	if err := c.Delete(context.Background(), getRR(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileRRSetN(t, r, key, 2)
	if err := c.Get(context.Background(), key, &dnsv1alpha1.RRSet{}); err == nil {
		t.Error("rrset should be gone after finalizer release")
	}
}

func TestRRSetCrossNamespaceDenied(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	rr := wwwRRSet("app-team")
	rr.Spec.ZoneRef = dnsv1alpha1.ObjectRef{Name: "example-com", Namespace: "dns"}
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), rr)
	key := types.NamespacedName{Name: "www-example-com", Namespace: "app-team"}

	reconcileRRSetN(t, r, key, 3)

	cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "NamespaceNotAllowed" {
		t.Errorf("want NamespaceNotAllowed, got %+v", cond)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestRRSet`
Expected: compile error (`undefined: RRSetReconciler`, `undefined: rrsetZoneIndex`, `undefined: rrsetFinalizer`).

- [ ] **Step 3: Implement `internal/controller/rrset_controller.go`**

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/pdnsclient"
)

const (
	rrsetFinalizer = "dns.aetherplatform.cloud/rrset-protection"

	// rrsetZoneIndex indexes RRSets by "<ns>/<name>" of their zoneRef so
	// Zone events fan out to dependent rrsets.
	rrsetZoneIndex = "spec.zoneRef"
)

// RRSetReconciler applies declared record sets to PowerDNS. Patch-only:
// the only rrsets it ever writes are the ones declared by RRSet
// resources; spec name/type/zoneRef are immutable (CEL) so reconcile is a
// stateless idempotent PATCH changetype=REPLACE.
type RRSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=rrsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=rrsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=rrsets/finalizers,verbs=update

func (r *RRSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rr := &dnsv1alpha1.RRSet{}
	if err := r.Get(ctx, req.NamespacedName, rr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !rr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, rr)
	}
	if !controllerutil.ContainsFinalizer(rr, rrsetFinalizer) {
		controllerutil.AddFinalizer(rr, rrsetFinalizer)
		if err := r.Update(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	zone := &dnsv1alpha1.Zone{}
	if err := r.Get(ctx, zoneKeyFor(rr), zone); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markNotReady(ctx, rr, "ZoneNotFound",
				fmt.Sprintf("Zone %s not found", zoneKeyFor(rr)), requeueLong)
		}
		return ctrl.Result{}, err
	}
	if !zone.DeletionTimestamp.IsZero() {
		return r.markNotReady(ctx, rr, "ZoneDeleting", "referenced Zone is being deleted", requeueLong)
	}
	if zone.Spec.Kind == dnsv1alpha1.ZoneKindSecondary {
		return r.markNotReady(ctx, rr, "SecondaryZone",
			"Secondary zone content is replicated from its primaries — manage records there", requeueLong)
	}
	if !nameInZone(rr.Spec.Name, zone.Spec.ZoneName) {
		return r.markNotReady(ctx, rr, "NameOutsideZone",
			fmt.Sprintf("%q is not within zone %q", rr.Spec.Name, zone.Spec.ZoneName), requeueLong)
	}

	server := &dnsv1alpha1.PowerDNSServer{}
	if err := r.Get(ctx, serverKeyFor(zone), server); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markNotReady(ctx, rr, "ServerNotFound",
				fmt.Sprintf("PowerDNSServer %s not found", serverKeyFor(zone)), requeueLong)
		}
		return ctrl.Result{}, err
	}
	if !namespaceAllowed(server, rr.Namespace) {
		return r.markNotReady(ctx, rr, "NamespaceNotAllowed",
			fmt.Sprintf("namespace %q is not allowed by %s/%s spec.zoneManagement.allowedNamespaces",
				rr.Namespace, server.Namespace, server.Name), requeueLong)
	}

	other, err := r.findConflict(ctx, rr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if other != "" {
		// Reject BOTH claimants — symmetric and stateless. requeueLong
		// doubles as the recovery poll once the other side is deleted.
		return r.markNotReady(ctx, rr, "Conflict",
			fmt.Sprintf("RRSet %s also declares %s %s in this zone — delete one", other, rr.Spec.Name, rr.Spec.Type),
			requeueLong)
	}

	if zone.Status.Phase != dnsv1alpha1.ZonePhaseReady {
		return r.markNotReady(ctx, rr, "ZoneNotReady", "Zone is not Ready yet", requeueShort)
	}

	pc, err := pdnsClientFor(ctx, r.Client, server)
	if err != nil {
		return r.markNotReady(ctx, rr, "APIUnreachable", err.Error(), requeueLong)
	}
	if err := pc.PatchRRSets(ctx, zone.Spec.ZoneName, []pdnsclient.RRSet{desiredRRSet(rr)}); err != nil {
		return r.markNotReady(ctx, rr, apiReason(err), err.Error(), requeueLong)
	}

	rr.Status.ObservedGeneration = rr.Generation
	rr.Status.FailureMessage = ""
	setCondOn(&rr.Status.Conditions, rr.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Applied", "")
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// reconcileDelete removes the rrset from PowerDNS (unless Orphan), with
// two escape hatches: (a) zone/server already gone or going — release
// without an API call, the data dies with them; (b) a surviving
// conflicting CR claims the same rrset — skip the API delete, the
// survivor re-applies on its next pass.
func (r *RRSetReconciler) reconcileDelete(ctx context.Context, rr *dnsv1alpha1.RRSet) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rr, rrsetFinalizer) {
		return ctrl.Result{}, nil
	}
	if rr.Spec.DeletionPolicy != dnsv1alpha1.DeletionPolicyOrphan {
		if err := r.deleteFromPowerDNS(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(rr, rrsetFinalizer)
	return ctrl.Result{}, r.Update(ctx, rr)
}

func (r *RRSetReconciler) deleteFromPowerDNS(ctx context.Context, rr *dnsv1alpha1.RRSet) error {
	zone := &dnsv1alpha1.Zone{}
	err := r.Get(ctx, zoneKeyFor(rr), zone)
	if apierrors.IsNotFound(err) || (err == nil && !zone.DeletionTimestamp.IsZero()) {
		return nil
	}
	if err != nil {
		return err
	}

	other, err := r.findConflict(ctx, rr)
	if err != nil {
		return err
	}
	if other != "" {
		return nil // survivor owns the rrset now
	}

	server := &dnsv1alpha1.PowerDNSServer{}
	err = r.Get(ctx, serverKeyFor(zone), server)
	if apierrors.IsNotFound(err) ||
		(err == nil && (!server.DeletionTimestamp.IsZero() || server.Status.Phase == dnsv1alpha1.PhaseFailed)) {
		return nil
	}
	if err != nil {
		return err
	}

	pc, err := pdnsClientFor(ctx, r.Client, server)
	if err != nil {
		return err
	}
	derr := pc.PatchRRSets(ctx, zone.Spec.ZoneName, []pdnsclient.RRSet{{
		Name: rr.Spec.Name, Type: rr.Spec.Type, ChangeType: "DELETE",
	}})
	if errors.Is(derr, pdnsclient.ErrNotFound) {
		return nil // zone already gone in PowerDNS
	}
	return derr
}

// findConflict returns "<ns>/<name>" of another live RRSet claiming the
// same (zone, name, type), or "".
func (r *RRSetReconciler) findConflict(ctx context.Context, rr *dnsv1alpha1.RRSet) (string, error) {
	var list dnsv1alpha1.RRSetList
	if err := r.List(ctx, &list, client.MatchingFields{
		rrsetZoneIndex: refKey(rr.Spec.ZoneRef, rr.Namespace),
	}); err != nil {
		return "", err
	}
	for i := range list.Items {
		o := &list.Items[i]
		if o.UID == rr.UID || !o.DeletionTimestamp.IsZero() {
			continue
		}
		if o.Spec.Name == rr.Spec.Name && o.Spec.Type == rr.Spec.Type {
			return o.Namespace + "/" + o.Name, nil
		}
	}
	return "", nil
}

func (r *RRSetReconciler) markNotReady(ctx context.Context, rr *dnsv1alpha1.RRSet, reason, msg string, after time.Duration) (ctrl.Result, error) {
	rr.Status.FailureMessage = msg
	setCondOn(&rr.Status.Conditions, rr.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// desiredRRSet renders the PATCH payload for one RRSet resource.
func desiredRRSet(rr *dnsv1alpha1.RRSet) pdnsclient.RRSet {
	ttl := int32(3600)
	if rr.Spec.TTL != nil {
		ttl = *rr.Spec.TTL
	}
	recs := make([]pdnsclient.Record, 0, len(rr.Spec.Records))
	for _, c := range rr.Spec.Records {
		recs = append(recs, pdnsclient.Record{Content: c})
	}
	return pdnsclient.RRSet{
		Name: rr.Spec.Name, Type: rr.Spec.Type, TTL: ttl,
		ChangeType: "REPLACE", Records: recs,
	}
}

// nameInZone reports whether name equals the zone apex or sits beneath it.
func nameInZone(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// zoneKeyFor resolves the zoneRef, defaulting to the RRSet's namespace.
func zoneKeyFor(rr *dnsv1alpha1.RRSet) types.NamespacedName {
	ns := rr.Spec.ZoneRef.Namespace
	if ns == "" {
		ns = rr.Namespace
	}
	return types.NamespacedName{Namespace: ns, Name: rr.Spec.ZoneRef.Name}
}

func (r *RRSetReconciler) rrsetsForZone(ctx context.Context, obj client.Object) []reconcile.Request {
	var list dnsv1alpha1.RRSetList
	if err := r.List(ctx, &list, client.MatchingFields{
		rrsetZoneIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// SetupWithManager registers the index and the Zone watch so a zone going
// Ready wakes its records.
func (r *RRSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dnsv1alpha1.RRSet{}, rrsetZoneIndex,
		func(o client.Object) []string {
			rr := o.(*dnsv1alpha1.RRSet)
			return []string{refKey(rr.Spec.ZoneRef, rr.GetNamespace())}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.RRSet{}).
		Watches(&dnsv1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.rrsetsForZone)).
		Complete(r)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestRRSet -v`
Expected: all 9 rrset tests PASS.

- [ ] **Step 5: Full suite**

Run: `make test && make vet`
Expected: PASS, clean vet.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/rrset_controller.go internal/controller/rrset_controller_test.go
git commit -m "feat: RRSet reconciler — declarative record sets with reject-both conflicts"
```

---

### Task 8: Wire reconcilers into the manager

**Files:**
- Modify: `cmd/operator/main.go`

- [ ] **Step 1: Register the controllers**

In `cmd/operator/main.go`, after the existing `PowerDNSServerReconciler` setup block, add:

```go
	if err := (&controller.ZoneReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("aether-powerdns"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Zone")
		os.Exit(1)
	}

	if err := (&controller.RRSetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("aether-powerdns"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RRSet")
		os.Exit(1)
	}
```

- [ ] **Step 2: Verify**

Run: `make build && make vet && make test`
Expected: all green.

- [ ] **Step 3: Commit**

```bash
git add cmd/operator/main.go
git commit -m "feat: wire Zone and RRSet reconcilers into the manager"
```

---

### Task 9: Examples and docs

**Files:**
- Create: `examples/zone-basic.yaml`, `examples/zone-secondary.yaml`, `examples/zone-dnssec.yaml`, `examples/rrset-cross-namespace.yaml`
- Modify: `examples/README.md`, `README.md`, `docs/managing-zones.md`, `CLAUDE.md`

- [ ] **Step 1: Create `examples/zone-basic.yaml`**

(The `demo` server matches `examples/managed-postgres.yaml`.)

```yaml
# A Native zone plus records on the `demo` server from
# examples/managed-postgres.yaml. The operator seeds the apex NS (and SOA)
# once at creation; afterwards the apex NS is an ordinary record.
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: Zone
metadata:
  name: example-com
  namespace: aether-system
spec:
  serverRef:
    name: demo
  zoneName: example.com.
  kind: Native
  nameservers:
    - ns1.example.com.
    - ns2.example.com.
---
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: RRSet
metadata:
  name: example-com-ns1
  namespace: aether-system
spec:
  zoneRef:
    name: example-com
  name: ns1.example.com.
  type: A
  records: ["203.0.113.53"]
---
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: RRSet
metadata:
  name: example-com-www
  namespace: aether-system
spec:
  zoneRef:
    name: example-com
  name: www.example.com.
  type: A
  ttl: 300
  records: ["203.0.113.10", "203.0.113.11"]
---
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: RRSet
metadata:
  name: example-com-mx
  namespace: aether-system
spec:
  zoneRef:
    name: example-com
  name: example.com.
  type: MX
  records: ["10 mail.example.com."]
```

- [ ] **Step 2: Create `examples/zone-secondary.yaml`**

```yaml
# A Secondary zone replicating from external primaries. Record content is
# replicated via AXFR/IXFR — RRSet resources are rejected for it.
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: Zone
metadata:
  name: partner-org-secondary
  namespace: aether-system
spec:
  serverRef:
    name: demo
  zoneName: partner.org.
  kind: Secondary
  masters:
    - "198.51.100.1"
    - "198.51.100.2"
```

- [ ] **Step 3: Create `examples/zone-dnssec.yaml`**

```yaml
# A signed zone. The operator creates an active CSK with PowerDNS
# defaults; lodge status.dsRecords at your registrar. Setting
# dnssec.enabled=false later DEACTIVATES the key but keeps it, so
# re-enabling does not invalidate the registrar DS.
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: Zone
metadata:
  name: secure-example-org
  namespace: aether-system
spec:
  serverRef:
    name: demo
  zoneName: secure-example.org.
  kind: Native
  nameservers:
    - ns1.secure-example.org.
  dnssec:
    enabled: true
  soa:
    hostmaster: hostmaster.secure-example.org.
    ttl: 3600
```

- [ ] **Step 4: Create `examples/rrset-cross-namespace.yaml`**

```yaml
# An app team manages its own record in a platform-owned zone. The
# PowerDNSServer must allow the team's namespace:
#
#   spec:
#     zoneManagement:
#       allowedNamespaces: ["app-team"]   # or ["*"]
#
apiVersion: dns.aetherplatform.cloud/v1alpha1
kind: RRSet
metadata:
  name: myapp-example-com
  namespace: app-team
spec:
  zoneRef:
    name: example-com
    namespace: aether-system
  name: myapp.example.com.
  type: A
  ttl: 300
  records: ["203.0.113.42"]
```

- [ ] **Step 5: Update `examples/README.md`**

Append a section listing the four new examples with one-line descriptions (match the file's existing format — read it first).

- [ ] **Step 6: Update `docs/managing-zones.md`**

Read the existing document first. Add a new top section "Declarative management with Zone and RRSet resources" BEFORE the HTTP API / pdnsutil sections, containing: (a) the `zone-basic.yaml` example inline, (b) this semantics table, (c) a pointer that the HTTP API / pdnsutil workflows below still work and coexist:

```markdown
| Behavior | Semantics |
|---|---|
| Drift | Patch-only: the operator reconciles ONLY rrsets declared in RRSet resources (5m resync). Records created via the API/pdnsutil are never touched. |
| `spec.nameservers` / `spec.soa` | Seeded once at zone creation; afterwards the apex NS/SOA are ordinary records. |
| Deletion | `kubectl delete zone/rrset` removes the data from PowerDNS. Set `deletionPolicy: Orphan` to detach instead. |
| Renames | `spec.name`/`spec.type`/refs are immutable — delete and recreate the resource. |
| Conflicts | Two RRSets claiming the same (zone, name, type) BOTH go `Ready=False reason=Conflict`; neither is applied until one is deleted. |
| DNSSEC | `dnssec.enabled: true` creates an active CSK; DS records appear in `status.dsRecords`. Disabling deactivates (keeps) keys. |
| Cross-namespace | Gated by the server's `spec.zoneManagement.allowedNamespaces` (`"*"` = all). Same namespace always allowed. |
| Secondary zones | Content replicates from `spec.masters`; RRSet resources targeting them are rejected. |
| NetworkPolicy | If `spec.networkPolicy.enabled=true` and the operator runs outside the server's namespace, add the operator's namespace to `additionalAllowedAPINamespaces` — otherwise zones sit at `Ready=False reason=APIUnreachable`. |
```

- [ ] **Step 7: Update `README.md`**

Read it first; add Zone/RRSet to the feature list and a short "Zones and records" subsection pointing at `docs/managing-zones.md` and the new examples.

- [ ] **Step 8: Update `CLAUDE.md`**

(a) Replace the `## Scope` section body with:

```markdown
Operator owns the PowerDNS server lifecycle (provisioning, config,
exposure, API key) **plus declarative zone/record management** via two
CRDs: `Zone` and one generic `RRSet` (the record type is a spec field).
Do NOT add per-record-type CRDs — ~50 kinds would reinvent the PowerDNS
API; that decision stands. Reconciliation is patch-only/coexist: only
CR-declared rrsets are ever written, so managing records via the
PowerDNS HTTP API or `pdnsutil` still works alongside the CRDs.
```

(b) In `## Architecture`, after the existing phase diagram paragraph, add:

```markdown
Zone/RRSet reconcilers (`zone_controller.go`, `rrset_controller.go`) are
single-pass, not phase machines — every reconcile converges from scratch;
`status.phase` is informational. They talk to PowerDNS through
`internal/pdnsclient` (thin in-repo client; same no-third-party-types
reasoning as the unstructured CNPG builder) using the server's
`status.apiEndpoint` + API-key Secret. Key semantics: `spec.nameservers`
and `spec.soa` seed ONCE at zone creation; DNSSEC disable deactivates
keys but never deletes them (registrar DS stays valid); rrset conflicts
reject BOTH claimants; deletion is via finalizer with `deletionPolicy:
Orphan` opt-out and wedge-proof release when the server/zone CR is
already gone or Failed. Cross-namespace refs are gated by
`PowerDNSServer.spec.zoneManagement.allowedNamespaces`. The
`refKey`-based field indexes in SetupWithManager and any test
`WithIndex` must stay identical or watches silently miss.
```

(c) In `## Out of scope (do not add without asking)`, replace the first bullet (`Zone/Record/DNSSEC-key CRDs — explicitly deferred…`) with:

```markdown
- Per-record-type CRDs (A/AAAA/MX/… as separate kinds) — the generic
  RRSet covers all types. Also out: TSIG keys, catalog zones, zone
  comments, AXFR allow-lists beyond `masters`.
```

- [ ] **Step 9: Commit**

```bash
git add examples/ README.md docs/managing-zones.md CLAUDE.md
git commit -m "docs: examples and documentation for Zone/RRSet CRDs"
```

---

### Task 10: Final verification + spec coverage check

- [ ] **Step 1: Full build/test/vet**

Run: `make tidy && make build && make vet && make test`
Expected: `go mod tidy` makes no changes (no new deps); everything green.

- [ ] **Step 2: Spec coverage walk**

Open `docs/superpowers/specs/2026-06-11-zone-rrset-crds-design.md` and verify each spec section maps to landed code: §1 shape (Tasks 1, 3, 6, 7), §2 Zone CRD (Tasks 1, 2, 6), §3 RRSet CRD (Tasks 1, 2, 7), §4 coexistence/deletion/cross-ns (Tasks 6, 7 + netpol doc note in Task 9), §5 wiring/validation/testing/docs (Tasks 2, 8, 9). Anything missing → fix before declaring done.

- [ ] **Step 3: Verify against a real cluster (optional but recommended on dev)**

```bash
make crd
kubectl apply -f examples/managed-postgres.yaml
kubectl apply -f examples/zone-basic.yaml
kubectl get zones,rrsets -n aether-system
kubectl describe zone example-com -n aether-system
```
Expected: zone reaches `Phase=Ready`, `kubectl get zone` shows the serial; `dig @<dns-endpoint> www.example.com A` answers. CEL check: try `kubectl patch zone example-com -n aether-system --type=merge -p '{"spec":{"zoneName":"other.org."}}'` — must be rejected with "zoneName is immutable".

- [ ] **Step 4: Finish the branch**

Use the superpowers:finishing-a-development-branch skill — typically: push `feat/zone-rrset-crds-design`, open a PR against `plane-shift/aether-powerdns` main (CodeQL + CodeRabbit run; 0 reviewers), squash-merge.

---

## Self-review notes (already applied)

- Type names cross-checked: `ObjectRef`/`refKey`/`zoneServerIndex`/`rrsetZoneIndex`/`setCondOn`/`pdnsClientFor`/`namespaceAllowed`/`serverKeyFor`/`zoneKeyFor`/`apiReason` are each defined exactly once (Tasks 4, 6, 7) and used consistently.
- The deepcopy file imports metav1 as `v1` — new methods use `v1.Condition`, not `metav1.Condition`.
- `pdnsclient.Zone.Nameservers` deliberately has NO omitempty; callers pass `[]string{}` — tested by `TestCreateZoneSendsNameserversField`.
- Existing package consts reused: `requeueShort`, `requeueLong`, `pickKey`; new finalizers are distinct from the existing `finalizer` const.
- `resyncInterval` is defined once (Task 6, zone controller) and shared by the rrset controller — Task 7 must NOT redefine it.
- fake client note: `WithStatusSubresource` is required for `r.Status().Update` to behave; both builders include it.
