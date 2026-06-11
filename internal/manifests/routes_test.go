package manifests

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func gatewayServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			DNS: dnsv1alpha1.DNSSpec{
				Exposure: dnsv1alpha1.DNSExposureGateway,
				Gateway: &dnsv1alpha1.DNSGatewaySpec{
					ParentRefs: []dnsv1alpha1.GatewayParentRef{{
						Group:          "gateway.networking.k8s.io",
						Kind:           "Gateway",
						Name:           "eg",
						Namespace:      "envoy-gateway-system",
						TCPSectionName: "dns-tcp",
						UDPSectionName: "dns-udp",
					}},
				},
			},
		},
	}
}

func TestTCPRouteCarriesFullParentRef(t *testing.T) {
	route := TCPRoute(gatewayServer())
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("want 1 parentRef, got %d", len(route.Spec.ParentRefs))
	}
	p := route.Spec.ParentRefs[0]
	if p.Group == nil || string(*p.Group) != "gateway.networking.k8s.io" {
		t.Errorf("group not propagated: %v", p.Group)
	}
	if p.Kind == nil || string(*p.Kind) != "Gateway" {
		t.Errorf("kind not propagated: %v", p.Kind)
	}
	if string(p.Name) != "eg" {
		t.Errorf("name = %q", p.Name)
	}
	if p.Namespace == nil || string(*p.Namespace) != "envoy-gateway-system" {
		t.Errorf("namespace not propagated: %v", p.Namespace)
	}
	if p.SectionName == nil || string(*p.SectionName) != "dns-tcp" {
		t.Errorf("tcp sectionName = %v", p.SectionName)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatal("want exactly one rule with one backend")
	}
	b := route.Spec.Rules[0].BackendRefs[0]
	if string(b.Name) != "test-dns" || b.Port == nil || int32(*b.Port) != 53 {
		t.Errorf("backend = %s:%v, want test-dns:53", b.Name, b.Port)
	}
}

func TestUDPRouteUsesUDPSectionName(t *testing.T) {
	route := UDPRoute(gatewayServer())
	p := route.Spec.ParentRefs[0]
	if p.SectionName == nil || string(*p.SectionName) != "dns-udp" {
		t.Errorf("udp sectionName = %v, want dns-udp", p.SectionName)
	}
}

func TestParentRefOmitsOptionalFieldsWhenUnset(t *testing.T) {
	s := gatewayServer()
	s.Spec.DNS.Gateway.ParentRefs = []dnsv1alpha1.GatewayParentRef{{Name: "local-gw"}}
	route := TCPRoute(s)
	p := route.Spec.ParentRefs[0]
	if p.Group != nil || p.Kind != nil {
		t.Errorf("unset group/kind must stay nil (Gateway API defaults them): %v/%v", p.Group, p.Kind)
	}
	if p.Namespace != nil {
		t.Errorf("same-namespace parent must omit namespace, got %v", *p.Namespace)
	}
	if p.SectionName != nil {
		t.Errorf("unset sectionName must stay nil, got %v", *p.SectionName)
	}
}

func apiGatewayServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			API: dnsv1alpha1.APISpec{
				Port: 8081,
				Gateway: &dnsv1alpha1.APIGatewaySpec{
					Hostnames: []string{"pdns-api.internal.example.com"},
					ParentRefs: []dnsv1alpha1.APIGatewayParentRef{{
						Group:       "gateway.networking.k8s.io",
						Kind:        "Gateway",
						Name:        "eg",
						Namespace:   "envoy-gateway-system",
						SectionName: "https",
					}},
				},
			},
		},
	}
}

func TestHTTPRouteRendersParentsHostnamesAndBackend(t *testing.T) {
	route := HTTPRoute(apiGatewayServer())
	if route.Name != "test-api-http" {
		t.Errorf("route name = %q, want test-api-http", route.Name)
	}
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("want 1 parentRef, got %d", len(route.Spec.ParentRefs))
	}
	p := route.Spec.ParentRefs[0]
	if p.Namespace == nil || string(*p.Namespace) != "envoy-gateway-system" ||
		p.SectionName == nil || string(*p.SectionName) != "https" {
		t.Errorf("parentRef not fully propagated: %+v", p)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "pdns-api.internal.example.com" {
		t.Errorf("hostnames = %v", route.Spec.Hostnames)
	}
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(route.Spec.Rules))
	}
	rule := route.Spec.Rules[0]
	if len(rule.Matches) != 1 || rule.Matches[0].Path == nil ||
		rule.Matches[0].Path.Value == nil || *rule.Matches[0].Path.Value != "/" {
		t.Errorf("want a single PathPrefix / match, got %+v", rule.Matches)
	}
	if len(rule.BackendRefs) != 1 {
		t.Fatalf("want 1 backend, got %d", len(rule.BackendRefs))
	}
	b := rule.BackendRefs[0]
	if string(b.Name) != "test-api" || b.Port == nil || int32(*b.Port) != 8081 {
		t.Errorf("backend = %s:%v, want test-api:8081", b.Name, b.Port)
	}
}

func TestHTTPRouteNoHostnamesMeansNil(t *testing.T) {
	s := apiGatewayServer()
	s.Spec.API.Gateway.Hostnames = nil
	route := HTTPRoute(s)
	if route.Spec.Hostnames != nil {
		t.Errorf("empty hostnames must render nil (match-all), got %v", route.Spec.Hostnames)
	}
}

func TestHTTPRouteDefaultAPIPort(t *testing.T) {
	s := apiGatewayServer()
	s.Spec.API.Port = 0 // operator default is 8081
	route := HTTPRoute(s)
	b := route.Spec.Rules[0].BackendRefs[0]
	if b.Port == nil || int32(*b.Port) != 8081 {
		t.Errorf("default api port = %v, want 8081", b.Port)
	}
}

func TestHTTPRouteNilWhenNoGateway(t *testing.T) {
	s := &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	if HTTPRoute(s) != nil {
		t.Error("HTTPRoute must return nil when spec.api.gateway is unset")
	}
}
