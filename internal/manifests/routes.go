package manifests

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// TCPRoute creates a single Gateway API TCPRoute for the DNS TCP port,
// attached to every Gateway listed in spec.dns.gateway.parentRefs. Each
// parent may specify its own TCP listener via parentRef.tcpSectionName.
func TCPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.TCPRoute {
	names := NameSet(s)
	parents := gatewayParents(s, gatewayProtoTCP)
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

// UDPRoute creates a single Gateway API UDPRoute for the DNS UDP port,
// attached to every Gateway listed in spec.dns.gateway.parentRefs.
func UDPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.UDPRoute {
	names := NameSet(s)
	parents := gatewayParents(s, gatewayProtoUDP)
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

type gatewayProto int

const (
	gatewayProtoTCP gatewayProto = iota
	gatewayProtoUDP
)

// gatewayParents builds a ParentReference for each entry in
// spec.dns.gateway.parentRefs, picking the per-protocol section name when
// the parent declares one.
func gatewayParents(s *dnsv1alpha1.PowerDNSServer, proto gatewayProto) []gatewayv1.ParentReference {
	if s.Spec.DNS.Gateway == nil {
		return nil
	}
	out := make([]gatewayv1.ParentReference, 0, len(s.Spec.DNS.Gateway.ParentRefs))
	for _, p := range s.Spec.DNS.Gateway.ParentRefs {
		ns := p.Namespace
		if ns == "" {
			ns = s.Namespace
		}
		ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p.Name)}
		if ns != s.Namespace {
			nsRef := gatewayv1.Namespace(ns)
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
			s := gatewayv1.SectionName(sn)
			ref.SectionName = &s
		}
		out = append(out, ref)
	}
	return out
}
