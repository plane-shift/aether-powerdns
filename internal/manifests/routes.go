package manifests

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// TCPRoute creates a Gateway API TCPRoute for the DNS TCP port.
func TCPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.TCPRoute {
	names := NameSet(s)
	parents := gatewayParents(s, s.Spec.DNS.Gateway.TCPSectionName)
	port := gatewayv1.PortNumber(dnsTCPPort)
	return &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.TCPRoute,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: gatewayv1alpha2.TCPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Rules: []gatewayv1alpha2.TCPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(names.DNSService),
						Port: &port,
					},
				}},
			}},
		},
	}
}

// UDPRoute creates a Gateway API UDPRoute for the DNS UDP port.
func UDPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.UDPRoute {
	names := NameSet(s)
	parents := gatewayParents(s, s.Spec.DNS.Gateway.UDPSectionName)
	port := gatewayv1.PortNumber(dnsUDPPort)
	return &gatewayv1alpha2.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.UDPRoute,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: gatewayv1alpha2.UDPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Rules: []gatewayv1alpha2.UDPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(names.DNSService),
						Port: &port,
					},
				}},
			}},
		},
	}
}

func gatewayParents(s *dnsv1alpha1.PowerDNSServer, sectionName string) []gatewayv1.ParentReference {
	gw := s.Spec.DNS.Gateway
	ns := gw.ParentRef.Namespace
	if ns == "" {
		ns = s.Namespace
	}
	pr := gatewayv1.ParentReference{
		Name: gatewayv1.ObjectName(gw.ParentRef.Name),
	}
	if ns != s.Namespace {
		nsRef := gatewayv1.Namespace(ns)
		pr.Namespace = &nsRef
	}
	if sectionName != "" {
		sn := gatewayv1.SectionName(sectionName)
		pr.SectionName = &sn
	}
	return []gatewayv1.ParentReference{pr}
}
