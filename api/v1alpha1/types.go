package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerDNSServer phases.
const (
	PhasePending             = "Pending"
	PhaseProvisioningBackend = "ProvisioningBackend"
	PhaseInitializingSchema  = "InitializingSchema"
	PhaseDeployingServer     = "DeployingServer"
	PhaseExposingDNS         = "ExposingDNS"
	PhaseReady               = "Ready"
	PhaseFailed              = "Failed"
	PhaseDeleting            = "Deleting"
)

// Standard condition types reported on PowerDNSServer.status.conditions.
const (
	ConditionReady              = "Ready"
	ConditionBackendProvisioned = "BackendProvisioned"
	ConditionSchemaApplied      = "SchemaApplied"
	ConditionAvailable          = "Available"
	ConditionProgressing        = "Progressing"
)

// Backend types.
const (
	BackendPostgres = "postgres"
	BackendMySQL    = "mysql"
)

// DNS exposure modes.
const (
	DNSExposureNone         = "none"
	DNSExposureLoadBalancer = "loadBalancer"
	DNSExposureGateway      = "gateway"
)

// DefaultImage is the PowerDNS authoritative image used when
// PowerDNSServer.spec.image is empty.
const DefaultImage = "powerdns/pdns-auth-51"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pdns
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend.type`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="DNS",type=string,JSONPath=`.status.dnsEndpoint`
// +kubebuilder:printcolumn:name="API",type=string,JSONPath=`.status.apiEndpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PowerDNSServer is a PowerDNS authoritative deployment with a database
// backend. Zones and records are managed out of band via the HTTP API or
// `pdnsutil` (e.g. `kubectl exec`); this CRD intentionally only owns the
// server lifecycle.
type PowerDNSServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PowerDNSServerSpec   `json:"spec,omitempty"`
	Status PowerDNSServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PowerDNSServerList contains a list of PowerDNSServer.
type PowerDNSServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PowerDNSServer `json:"items"`
}

// PowerDNSServerSpec defines the desired PowerDNS deployment.
type PowerDNSServerSpec struct {
	// Replicas for the pdns-auth Deployment. Multiple replicas share the
	// same backend database, so they serve identical zones.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image overrides the default `powerdns/pdns-auth-51` image.
	// +optional
	Image string `json:"image,omitempty"`

	// Resources for the pdns-auth container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Backend configures the database that PowerDNS uses to store zones
	// and records.
	// +kubebuilder:validation:Required
	Backend BackendSpec `json:"backend"`

	// DNS configures how the server's DNS port (UDP+TCP 53) is exposed
	// outside the cluster.
	// +optional
	DNS DNSSpec `json:"dns,omitempty"`

	// API configures the PowerDNS HTTP API. The API key is auto-generated
	// into a Secret on create. The API surface is admin-only and stays on
	// ClusterIP; expose it deliberately if needed.
	// +optional
	API APISpec `json:"api,omitempty"`

	// Scheduling pins pods to specific nodes / zones / priority classes.
	// +optional
	Scheduling SchedulingSpec `json:"scheduling,omitempty"`

	// Observability toggles a Prometheus PodMonitor scraping PowerDNS's
	// `/metrics` endpoint. Requires the Prometheus Operator (CRDs from
	// `monitoring.coreos.com`) to be installed.
	// +optional
	Observability ObservabilitySpec `json:"observability,omitempty"`

	// NetworkPolicy locks down ingress to the pdns pods. DNS (53) is
	// allowed from anywhere; the API port is restricted to the operator's
	// namespace by default. Disabled by default — enable explicitly.
	// +optional
	NetworkPolicy NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

// SchedulingSpec controls where pdns pods land.
type SchedulingSpec struct {
	// NodeSelector pins pods to nodes matching these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let pods schedule onto tainted nodes (e.g. dedicated
	// DNS pool).
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// PriorityClassName for pdns pods. DNS is critical infra — set
	// `system-cluster-critical` on shared clusters.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// SpreadAcrossZones adds a topology spread constraint on
	// `topology.kubernetes.io/zone` (in addition to hostname spread)
	// when replicas > 1. Defaults to false.
	// +optional
	SpreadAcrossZones bool `json:"spreadAcrossZones,omitempty"`
}

// ObservabilitySpec controls metrics scraping resources.
type ObservabilitySpec struct {
	// PodMonitor creates a Prometheus PodMonitor pointing at PowerDNS's
	// `/metrics` endpoint on the API webserver port.
	// +optional
	PodMonitor PodMonitorSpec `json:"podMonitor,omitempty"`
}

// PodMonitorSpec configures the optional Prometheus PodMonitor.
type PodMonitorSpec struct {
	// Enabled toggles PodMonitor creation.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Interval between scrapes (e.g. `30s`).
	// +kubebuilder:default="30s"
	// +optional
	Interval string `json:"interval,omitempty"`

	// Labels added to the PodMonitor (e.g. release: kube-prometheus-stack).
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// NetworkPolicySpec configures the optional NetworkPolicy.
type NetworkPolicySpec struct {
	// Enabled toggles NetworkPolicy creation. Default false because the
	// policy denies anything not explicitly allowed — turning it on
	// without checking your cluster's CNI / namespace labels can blackhole
	// the API surface.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// AdditionalAllowedAPINamespaces lists namespaces (by name) from which
	// the PowerDNS HTTP API may be reached, in addition to the pdns
	// pod's own namespace. Each must label itself with
	// `kubernetes.io/metadata.name=<name>` (the standard since K8s 1.22).
	// +optional
	AdditionalAllowedAPINamespaces []string `json:"additionalAllowedAPINamespaces,omitempty"`
}

// BackendSpec selects and configures the storage backend.
type BackendSpec struct {
	// Type is the database flavour. Only `postgres` is implemented today;
	// `mysql` is reserved.
	// +kubebuilder:validation:Enum=postgres;mysql
	// +kubebuilder:default=postgres
	Type string `json:"type"`

	// Postgres holds Postgres-specific options. When `byo` is unset the
	// operator provisions a CloudNativePG `Cluster` named after the
	// PowerDNSServer (suffix `-pg`).
	// +optional
	Postgres *PostgresBackendSpec `json:"postgres,omitempty"`
}

// PostgresBackendSpec controls the Postgres backend.
type PostgresBackendSpec struct {
	// Instances for the operator-managed CNPG Cluster. Ignored when BYO is set.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances *int32 `json:"instances,omitempty"`

	// Storage size for the operator-managed CNPG Cluster.
	// +kubebuilder:default="5Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClass for the operator-managed CNPG Cluster.
	// Defaults to the cluster default StorageClass.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// BYO points the server at a pre-existing Postgres database instead of
	// provisioning one. When set, the operator does not create or manage a
	// CNPG Cluster.
	// +optional
	BYO *BYOPostgres `json:"byo,omitempty"`
}

// BYOPostgres references a pre-existing Postgres instance.
type BYOPostgres struct {
	// Host is the Postgres hostname.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port for Postgres.
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database name. PowerDNS schema must already be applied or
	// `applySchema` must be left enabled (default) so the operator runs it.
	// +kubebuilder:default="powerdns"
	// +optional
	Database string `json:"database,omitempty"`

	// CredentialsSecretRef points to a Secret with `username` and
	// `password` keys.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

	// SSLMode for the Postgres connection (e.g. `disable`, `require`,
	// `verify-full`). Defaults to `prefer`.
	// +optional
	SSLMode string `json:"sslMode,omitempty"`
}

// DNSSpec configures how the DNS port is exposed.
type DNSSpec struct {
	// Exposure picks the exposure mode for the DNS port. Defaults to `none`
	// (ClusterIP only, no external reach).
	// +kubebuilder:validation:Enum=none;loadBalancer;gateway
	// +kubebuilder:default=none
	// +optional
	Exposure string `json:"exposure,omitempty"`

	// LoadBalancer applies when Exposure is `loadBalancer`.
	// +optional
	LoadBalancer *DNSLoadBalancerSpec `json:"loadBalancer,omitempty"`

	// Gateway applies when Exposure is `gateway`. The operator creates
	// `TCPRoute` and `UDPRoute` resources targeting the referenced Gateway.
	// +optional
	Gateway *DNSGatewaySpec `json:"gateway,omitempty"`
}

// DNSLoadBalancerSpec configures Service type=LoadBalancer for DNS.
type DNSLoadBalancerSpec struct {
	// IP requested from the LB controller for the primary Service
	// (e.g. MetalLB). Optional.
	// +optional
	IP string `json:"ip,omitempty"`

	// Annotations applied to the primary Service (e.g.
	// `metallb.io/address-pool`).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ExternalTrafficPolicy for the primary Service. Defaults to `Local`
	// so PowerDNS sees the real client source IP.
	// +kubebuilder:validation:Enum=Cluster;Local
	// +kubebuilder:default=Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`

	// AdditionalServices renders extra LoadBalancer Services targeting the
	// same pods, each with its own IP / pool / annotations. Use this when
	// you need IPs from heterogeneous LB sources (e.g. one MetalLB pool +
	// one cloud LB) or different `externalTrafficPolicy` per IP. For a
	// single Service with multiple IPs from the same LB controller (e.g.
	// MetalLB's `metallb.io/loadBalancerIPs`), prefer the primary
	// `annotations` field.
	// +optional
	AdditionalServices []AdditionalLoadBalancerService `json:"additionalServices,omitempty"`
}

// AdditionalLoadBalancerService describes one extra LB Service alongside
// the primary `<server>-dns` Service.
type AdditionalLoadBalancerService struct {
	// NameSuffix is appended to `<server>-dns` to form the Service name
	// (e.g. `-2` → `<server>-dns-2`). Must be RFC 1123 compatible and
	// unique within the PowerDNSServer.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	NameSuffix string `json:"nameSuffix"`

	// IP requested from the LB controller for this Service.
	// +optional
	IP string `json:"ip,omitempty"`

	// Annotations applied to this Service.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ExternalTrafficPolicy for this Service. Defaults to `Local`.
	// +kubebuilder:validation:Enum=Cluster;Local
	// +kubebuilder:default=Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
}

// DNSGatewaySpec routes DNS traffic via one or more Gateway API Gateways.
// All listed Gateways are attached as parentRefs on the same TCPRoute /
// UDPRoute pair, so the operator only manages a single route per protocol
// regardless of how many Gateways front it.
type DNSGatewaySpec struct {
	// ParentRefs lists the Gateways to attach to. At least one required.
	// +kubebuilder:validation:MinItems=1
	ParentRefs []GatewayParentRef `json:"parentRefs"`
}

// GatewayParentRef is a minimal subset of gateway.networking.k8s.io
// ParentReference, with optional per-parent listener section names.
type GatewayParentRef struct {
	// Name of the Gateway.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Gateway. Defaults to the PowerDNSServer's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// TCPSectionName picks a TCP listener on this Gateway. Optional —
	// when unset, the route attaches to the Gateway as a whole.
	// +optional
	TCPSectionName string `json:"tcpSectionName,omitempty"`

	// UDPSectionName picks a UDP listener on this Gateway.
	// +optional
	UDPSectionName string `json:"udpSectionName,omitempty"`
}

// APISpec configures the PowerDNS HTTP API.
type APISpec struct {
	// Port for the PowerDNS API. Defaults to 8081.
	// +kubebuilder:default=8081
	// +optional
	Port int32 `json:"port,omitempty"`

	// APIKeySecretRef overrides the auto-generated key. The Secret must
	// have an `api-key` key.
	// +optional
	APIKeySecretRef *corev1.LocalObjectReference `json:"apiKeySecretRef,omitempty"`
}

// PowerDNSServerStatus reflects the observed state of the deployment.
type PowerDNSServerStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Ready indicates the PowerDNS server is serving DNS and the API.
	// +optional
	Ready bool `json:"ready"`

	// DNSEndpoint is the externally reachable DNS address (host:port). For
	// `loadBalancer` exposure this is the assigned LB IP; for `gateway` it
	// is the Gateway address; for `none` it is the ClusterIP.
	// +optional
	DNSEndpoint string `json:"dnsEndpoint,omitempty"`

	// APIEndpoint is the in-cluster URL of the PowerDNS HTTP API.
	// +optional
	APIEndpoint string `json:"apiEndpoint,omitempty"`

	// APIKeySecretName is the Secret holding the API key (key: `api-key`).
	// +optional
	APIKeySecretName string `json:"apiKeySecretName,omitempty"`

	// BackendSecretName is the Secret holding the database DSN
	// (key: `dsn`) and connection fields.
	// +optional
	BackendSecretName string `json:"backendSecretName,omitempty"`

	// SchemaApplied indicates the PowerDNS schema has been initialised in
	// the backend.
	// +optional
	SchemaApplied bool `json:"schemaApplied,omitempty"`

	// DesiredReplicas mirrors the running Deployment's replica count.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// ReadyReplicas is the number of pdns-auth pods currently Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ConfigHash is the hash of the rendered pdns.conf, API key and
	// backend credentials. Pods carry the same value as a template
	// annotation so a config or credential change forces a rolling
	// restart.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// Conditions represents the latest observations of the server state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// FailureMessage details the cause when Phase is `Failed`.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`
}
