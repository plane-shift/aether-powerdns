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
