package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Record",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.zoneRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
