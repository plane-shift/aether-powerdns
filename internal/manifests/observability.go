package manifests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// PodMonitorGVK is the prometheus-operator PodMonitor GroupVersionKind.
// Rendered as unstructured so we don't import the operator-sdk module.
var PodMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "PodMonitor",
}

// PodMonitor scrapes PowerDNS's `/metrics` (built-in Prometheus exporter
// served on the webserver/API port). Returned as unstructured so the
// operator builds without importing prometheus-operator types.
func PodMonitor(s *dnsv1alpha1.PowerDNSServer) *unstructured.Unstructured {
	names := NameSet(s)

	interval := s.Spec.Observability.PodMonitor.Interval
	if interval == "" {
		interval = "30s"
	}

	lb := map[string]string{}
	for k, v := range labels(s) {
		lb[k] = v
	}
	for k, v := range s.Spec.Observability.PodMonitor.Labels {
		lb[k] = v
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(PodMonitorGVK)
	u.SetName(names.PodMonitor)
	u.SetNamespace(s.Namespace)
	u.SetLabels(lb)
	u.Object["spec"] = map[string]interface{}{
		"selector": map[string]interface{}{
			"matchLabels": labels(s),
		},
		// Pin to the server's namespace so a misconfigured Prometheus
		// instance can't accidentally scrape pods in another namespace.
		"namespaceSelector": map[string]interface{}{
			"matchNames": []interface{}{s.Namespace},
		},
		"podMetricsEndpoints": []interface{}{
			map[string]interface{}{
				"port":     "api", // matches the named port on the pdns container
				"path":     "/metrics",
				"interval": interval,
				"scheme":   "http",
			},
		},
	}
	return u
}
