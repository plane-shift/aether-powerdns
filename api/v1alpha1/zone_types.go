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
// +kubebuilder:resource:shortName=pdnszone
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.zoneName`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Serial",type=integer,JSONPath=`.status.serial`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
