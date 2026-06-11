package manifests

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

type gatewayProto int

const (
	gatewayProtoTCP gatewayProto = iota
	gatewayProtoUDP
)

// dnsRouteParents builds ParentReferences from a DNSSpec's gateway block —
// shared by PowerDNSServer and DNSDist exposure (same DNSSpec type).
func dnsRouteParents(dns *dnsv1alpha1.DNSSpec, localNS string, proto gatewayProto) []gatewayv1.ParentReference {
	if dns.Gateway == nil {
		return nil
	}
	out := make([]gatewayv1.ParentReference, 0, len(dns.Gateway.ParentRefs))
	for _, p := range dns.Gateway.ParentRefs {
		ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p.Name)}
		if p.Group != "" {
			g := gatewayv1.Group(p.Group)
			ref.Group = &g
		}
		if p.Kind != "" {
			k := gatewayv1.Kind(p.Kind)
			ref.Kind = &k
		}
		if p.Namespace != "" && p.Namespace != localNS {
			nsRef := gatewayv1.Namespace(p.Namespace)
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
			snRef := gatewayv1.SectionName(sn)
			ref.SectionName = &snRef
		}
		out = append(out, ref)
	}
	return out
}

// buildTCPRoute / buildUDPRoute render one route for any owner.
func buildTCPRoute(name, namespace string, lbls map[string]string, parents []gatewayv1.ParentReference, backendSvc string, port int32) *gatewayv1alpha2.TCPRoute {
	p := gatewayv1.PortNumber(port)
	return &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: gatewayv1alpha2.TCPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Rules: []gatewayv1alpha2.TCPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backendSvc), Port: &p,
					},
				}},
			}},
		},
	}
}

func buildUDPRoute(name, namespace string, lbls map[string]string, parents []gatewayv1.ParentReference, backendSvc string, port int32) *gatewayv1alpha2.UDPRoute {
	p := gatewayv1.PortNumber(port)
	return &gatewayv1alpha2.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: gatewayv1alpha2.UDPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Rules: []gatewayv1alpha2.UDPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backendSvc), Port: &p,
					},
				}},
			}},
		},
	}
}

// TCPRoute creates a single Gateway API TCPRoute for the DNS TCP port,
// attached to every Gateway listed in spec.dns.gateway.parentRefs. Each
// parent may specify its own TCP listener via parentRef.tcpSectionName.
func TCPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.TCPRoute {
	names := NameSet(s)
	return buildTCPRoute(names.TCPRoute, s.Namespace, labels(s),
		dnsRouteParents(&s.Spec.DNS, s.Namespace, gatewayProtoTCP), names.DNSService, dnsTCPPort)
}

// UDPRoute creates a single Gateway API UDPRoute for the DNS UDP port,
// attached to every Gateway listed in spec.dns.gateway.parentRefs.
func UDPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1alpha2.UDPRoute {
	names := NameSet(s)
	return buildUDPRoute(names.UDPRoute, s.Namespace, labels(s),
		dnsRouteParents(&s.Spec.DNS, s.Namespace, gatewayProtoUDP), names.DNSService, dnsUDPPort)
}

// HTTPRoute exposes the PowerDNS HTTP API through the Gateways listed in
// spec.api.gateway.parentRefs. Opt-in only — the API is an admin surface
// (a leaked key gives full zone control); pair with TLS listeners and
// hostnames. Returns nil when spec.api.gateway is unset.
func HTTPRoute(s *dnsv1alpha1.PowerDNSServer) *gatewayv1.HTTPRoute {
	gw := s.Spec.API.Gateway
	if gw == nil {
		return nil
	}
	names := NameSet(s)
	api := apiSpecOrDefault(s)

	parents := make([]gatewayv1.ParentReference, 0, len(gw.ParentRefs))
	// Same construction rules as gatewayParents — keep the two in sync.
	for _, p := range gw.ParentRefs {
		ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p.Name)}
		if p.Group != "" {
			g := gatewayv1.Group(p.Group)
			ref.Group = &g
		}
		if p.Kind != "" {
			k := gatewayv1.Kind(p.Kind)
			ref.Kind = &k
		}
		if p.Namespace != "" && p.Namespace != s.Namespace {
			ns := gatewayv1.Namespace(p.Namespace)
			ref.Namespace = &ns
		}
		if p.SectionName != "" {
			sn := gatewayv1.SectionName(p.SectionName)
			ref.SectionName = &sn
		}
		parents = append(parents, ref)
	}

	var hostnames []gatewayv1.Hostname
	for _, h := range gw.Hostnames {
		hostnames = append(hostnames, gatewayv1.Hostname(h))
	}

	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/"
	port := gatewayv1.PortNumber(api.Port)
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.HTTPRoute,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Hostnames:       hostnames,
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &pathValue},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(names.APIService),
							Port: &port,
						},
					},
				}},
			}},
		},
	}
}
