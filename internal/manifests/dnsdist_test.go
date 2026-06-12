package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func testDNSDist() *dnsv1alpha1.DNSDist {
	two := int32(2)
	return &dnsv1alpha1.DNSDist{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default"},
		Spec: dnsv1alpha1.DNSDistSpec{
			Replicas: &two,
			BackendRefs: []dnsv1alpha1.ObjectRef{
				{Name: "srv-b"}, {Name: "srv-a"},
			},
		},
	}
}

func TestDNSDistConfigBackendsSortedAndHealthChecked(t *testing.T) {
	// backendAddrs map: backend-name → ClusterIP (resolved by controller)
	addrs := map[string]string{"srv-a": "10.96.0.10", "srv-b": "10.96.0.11"}
	conf := DNSDistConfig(testDNSDist(), addrs)
	// Must use ClusterIP, NOT the FQDN hostname (dnsdist 1.9.x rejects hostnames)
	if strings.Contains(conf, "svc.cluster.local") {
		t.Errorf("backend must use ClusterIP, not FQDN:\n%s", conf)
	}
	ia := strings.Index(conf, `newServer({address="10.96.0.10:53"`)
	ib := strings.Index(conf, `newServer({address="10.96.0.11:53"`)
	if ia < 0 || ib < 0 {
		t.Fatalf("backends missing (want ClusterIPs):\n%s", conf)
	}
	// Sorted by NAME (srv-a before srv-b) — determinism/config-hash stability
	if ia > ib {
		t.Error("backends must render in sorted order by name (deterministic conf = stable config hash)")
	}
	// name= field must still carry the backend name for health-check labelling
	if !strings.Contains(conf, `name="srv-a"`) || !strings.Contains(conf, `name="srv-b"`) {
		t.Errorf("name= fields must be present:\n%s", conf)
	}
	if !strings.Contains(conf, "checkInterval=2") || !strings.Contains(conf, "maxCheckFailures=2") {
		t.Error("active health-check parameters missing")
	}
}

func TestDNSDistConfigACLOpensPublicQueries(t *testing.T) {
	conf := DNSDistConfig(testDNSDist(), map[string]string{"srv-a": "10.96.0.10", "srv-b": "10.96.0.11"})
	if !strings.Contains(conf, `setACL({"0.0.0.0/0", "::/0"})`) {
		t.Error("dnsdist's default ACL allows only RFC1918 — a public frontend MUST setACL wide open")
	}
}

func TestDNSDistConfigCacheDefaultsOnAndTogglesOff(t *testing.T) {
	d := testDNSDist()
	addrs := map[string]string{"srv-a": "10.96.0.10", "srv-b": "10.96.0.11"}
	conf := DNSDistConfig(d, addrs)
	if !strings.Contains(conf, "newPacketCache(100000") {
		t.Errorf("cache must default on with 100000 entries:\n%s", conf)
	}
	off := false
	d.Spec.Cache.Enabled = &off
	if strings.Contains(DNSDistConfig(d, addrs), "newPacketCache") {
		t.Error("cache.enabled=false must omit the packet cache")
	}
}

func TestDNSDistConfigRateLimitAndTLSToggles(t *testing.T) {
	d := testDNSDist()
	addrs := map[string]string{"srv-a": "10.96.0.10", "srv-b": "10.96.0.11"}
	conf := DNSDistConfig(d, addrs)
	if strings.Contains(conf, "setQueryRate") || strings.Contains(conf, "addTLSLocal") || strings.Contains(conf, "addDOHLocal") {
		t.Error("rate limit and TLS listeners must be off by default")
	}
	d.Spec.RateLimit.QPSPerClient = 50
	d.Spec.TLS.DoT = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "dot-cert"}}
	d.Spec.TLS.DoH = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "doh-cert"}}
	conf = DNSDistConfig(d, addrs)
	if !strings.Contains(conf, "setQueryRate(50, 10") {
		t.Errorf("qpsPerClient=50 must render a dynamic-block rule:\n%s", conf)
	}
	if !strings.Contains(conf, `addTLSLocal("0.0.0.0:853", "/tls/dot/tls.crt", "/tls/dot/tls.key")`) {
		t.Errorf("DoT listener missing:\n%s", conf)
	}
	if !strings.Contains(conf, `addDOHLocal("0.0.0.0:443", "/tls/doh/tls.crt", "/tls/doh/tls.key", { "/dns-query" })`) {
		t.Errorf("DoH listener missing:\n%s", conf)
	}
}

func TestDNSDistDeploymentShape(t *testing.T) {
	d := testDNSDist()
	d.Spec.TLS.DoT = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "dot-cert"}}
	dep := DNSDistDeployment(d, "hash123")
	if dep.Name != "edge-dnsdist" {
		t.Errorf("deployment name = %q, want edge-dnsdist", dep.Name)
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d", *dep.Spec.Replicas)
	}
	if dep.Spec.Template.Annotations[ConfigHashAnnotation] != "hash123" {
		t.Error("config-hash pod annotation missing — conf changes must roll pods")
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != dnsv1alpha1.DefaultDNSDistImage {
		t.Errorf("default image = %q", c.Image)
	}
	var ports []string
	for _, p := range c.Ports {
		ports = append(ports, p.Name)
	}
	joined := strings.Join(ports, ",")
	for _, want := range []string{"dns-tcp", "dns-udp", "dot"} {
		if !strings.Contains(joined, want) {
			t.Errorf("port %q missing (have %s)", want, joined)
		}
	}
	if dep.Spec.Template.Spec.Affinity == nil || dep.Spec.Template.Spec.Affinity.PodAntiAffinity == nil {
		t.Error("replicas>1 must set pod anti-affinity (same HA posture as pdns)")
	}
	// cert volume mounted, conf volume mounted
	mounts := c.VolumeMounts
	var hasConf, hasDot bool
	for _, m := range mounts {
		if m.MountPath == "/etc/dnsdist" {
			hasConf = true
		}
		if m.MountPath == "/tls/dot" {
			hasDot = true
		}
	}
	if !hasConf || !hasDot {
		t.Errorf("expected conf + DoT cert mounts, got %+v", mounts)
	}
}

func TestDNSDistServiceAndPDBAndRoutes(t *testing.T) {
	d := testDNSDist()
	svc := DNSDistDNSService(d)
	if svc.Name != "edge-dnsdist-dns" || svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service = %s/%s", svc.Name, svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Errorf("default ports tcp+udp 53, got %+v", svc.Spec.Ports)
	}

	pdb := DNSDistPDB(d)
	if pdb == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("replicas=2 default PDB minAvailable=1, got %v", pdb)
	}

	d.Spec.DNS = dnsv1alpha1.DNSSpec{
		Exposure: dnsv1alpha1.DNSExposureGateway,
		Gateway: &dnsv1alpha1.DNSGatewaySpec{
			ParentRefs: []dnsv1alpha1.GatewayParentRef{{Name: "gw", TCPSectionName: "dns-tcp", UDPSectionName: "dns-udp"}},
		},
	}
	tcp := DNSDistTCPRoute(d)
	if tcp.Name != "edge-dnsdist-tcp" || string(tcp.Spec.Rules[0].BackendRefs[0].Name) != "edge-dnsdist-dns" {
		t.Errorf("TCP route must backend the DNSDIST service (gateway→dnsdist→pdns): %+v", tcp)
	}
	udp := DNSDistUDPRoute(d)
	if string(*udp.Spec.ParentRefs[0].SectionName) != "dns-udp" {
		t.Errorf("UDP sectionName: %+v", udp.Spec.ParentRefs[0])
	}
}

func TestDNSDistAdditionalDNSServices(t *testing.T) {
	// nil when exposure != loadBalancer
	d := testDNSDist()
	d.Spec.DNS = dnsv1alpha1.DNSSpec{
		Exposure: dnsv1alpha1.DNSExposureGateway,
		Gateway:  &dnsv1alpha1.DNSGatewaySpec{ParentRefs: []dnsv1alpha1.GatewayParentRef{{Name: "gw"}}},
	}
	if svcs := DNSDistAdditionalDNSServices(d); svcs != nil {
		t.Errorf("must be nil when exposure=gateway, got %v", svcs)
	}

	// renders with suffixed name, LB type, ETP defaulting to Local
	d2 := testDNSDist()
	ip1 := "1.2.3.4"
	d2.Spec.DNS = dnsv1alpha1.DNSSpec{
		Exposure: dnsv1alpha1.DNSExposureLoadBalancer,
		LoadBalancer: &dnsv1alpha1.DNSLoadBalancerSpec{
			AdditionalServices: []dnsv1alpha1.AdditionalLoadBalancerService{
				{NameSuffix: "-extra", IP: ip1},
			},
		},
	}
	svcs := DNSDistAdditionalDNSServices(d2)
	if len(svcs) != 1 {
		t.Fatalf("want 1 additional service, got %d", len(svcs))
	}
	svc := svcs[0]
	if svc.Name != "edge-dnsdist-dns-extra" {
		t.Errorf("name = %q, want edge-dnsdist-dns-extra", svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %q, want LoadBalancer", svc.Spec.Type)
	}
	if svc.Spec.LoadBalancerIP != ip1 {
		t.Errorf("LoadBalancerIP = %q, want %q", svc.Spec.LoadBalancerIP, ip1)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("ETP = %q, want Local", svc.Spec.ExternalTrafficPolicy)
	}
	// ports should include dns-tcp and dns-udp
	portNames := map[string]bool{}
	for _, p := range svc.Spec.Ports {
		portNames[p.Name] = true
	}
	if !portNames["dns-tcp"] || !portNames["dns-udp"] {
		t.Errorf("missing required ports, got %v", portNames)
	}

	// DoT port included when enabled
	d3 := testDNSDist()
	d3.Spec.DNS = dnsv1alpha1.DNSSpec{
		Exposure: dnsv1alpha1.DNSExposureLoadBalancer,
		LoadBalancer: &dnsv1alpha1.DNSLoadBalancerSpec{
			AdditionalServices: []dnsv1alpha1.AdditionalLoadBalancerService{{NameSuffix: "-b"}},
		},
	}
	d3.Spec.TLS.DoT = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "c"}}
	svcs3 := DNSDistAdditionalDNSServices(d3)
	if len(svcs3) != 1 {
		t.Fatalf("want 1 service, got %d", len(svcs3))
	}
	dotFound := false
	for _, p := range svcs3[0].Spec.Ports {
		if p.Name == "dot" {
			dotFound = true
		}
	}
	if !dotFound {
		t.Error("DoT port must appear in additional services when TLS.DoT is enabled")
	}
}

func TestDNSDistSpreadAcrossZones(t *testing.T) {
	// replicas=2 + SpreadAcrossZones=true → zone TSC must be present
	d := testDNSDist()
	d.Spec.Scheduling.SpreadAcrossZones = true
	dep := DNSDistDeployment(d, "")
	tscs := dep.Spec.Template.Spec.TopologySpreadConstraints
	var hasZone bool
	for _, tsc := range tscs {
		if tsc.TopologyKey == corev1.LabelTopologyZone {
			hasZone = true
		}
	}
	if !hasZone {
		t.Errorf("replicas>1 + SpreadAcrossZones=true must add a zone TopologySpreadConstraint; got %+v", tscs)
	}

	// replicas=2 + SpreadAcrossZones=false (default) → only hostname TSC
	d2 := testDNSDist()
	dep2 := DNSDistDeployment(d2, "")
	for _, tsc := range dep2.Spec.Template.Spec.TopologySpreadConstraints {
		if tsc.TopologyKey == corev1.LabelTopologyZone {
			t.Errorf("SpreadAcrossZones=false must NOT add zone TSC; got %+v", dep2.Spec.Template.Spec.TopologySpreadConstraints)
		}
	}

	// replicas=1 → no TSC at all (no HA constraints when single replica)
	one := int32(1)
	d3 := testDNSDist()
	d3.Spec.Replicas = &one
	d3.Spec.Scheduling.SpreadAcrossZones = true
	dep3 := DNSDistDeployment(d3, "")
	if len(dep3.Spec.Template.Spec.TopologySpreadConstraints) > 0 {
		t.Errorf("replicas=1 must not set any TopologySpreadConstraints; got %+v", dep3.Spec.Template.Spec.TopologySpreadConstraints)
	}
}

func TestDNSDistConfigDeterministic(t *testing.T) {
	d := testDNSDist()
	addrs := map[string]string{"srv-a": "10.96.0.10", "srv-b": "10.96.0.11"}
	first := DNSDistConfig(d, addrs)
	for i := 0; i < 20; i++ {
		if DNSDistConfig(d, addrs) != first {
			t.Fatal("conf must render identically every time (config hash stability)")
		}
	}
}
