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
