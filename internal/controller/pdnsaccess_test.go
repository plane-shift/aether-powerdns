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
