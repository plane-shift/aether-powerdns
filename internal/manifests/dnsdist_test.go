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
	conf := DNSDistConfig(testDNSDist())
	ia := strings.Index(conf, `newServer({address="srv-a-dns.default.svc.cluster.local:53"`)
	ib := strings.Index(conf, `newServer({address="srv-b-dns.default.svc.cluster.local:53"`)
	if ia < 0 || ib < 0 {
		t.Fatalf("backends missing:\n%s", conf)
	}
	if ia > ib {
		t.Error("backends must render in sorted order (deterministic conf = stable config hash)")
	}
	if !strings.Contains(conf, "checkInterval=2") || !strings.Contains(conf, "maxCheckFailures=2") {
		t.Error("active health-check parameters missing")
	}
}

func TestDNSDistConfigACLOpensPublicQueries(t *testing.T) {
	conf := DNSDistConfig(testDNSDist())
	if !strings.Contains(conf, `setACL({"0.0.0.0/0", "::/0"})`) {
		t.Error("dnsdist's default ACL allows only RFC1918 — a public frontend MUST setACL wide open")
	}
}

func TestDNSDistConfigCacheDefaultsOnAndTogglesOff(t *testing.T) {
	d := testDNSDist()
	conf := DNSDistConfig(d)
	if !strings.Contains(conf, "newPacketCache(100000") {
		t.Errorf("cache must default on with 100000 entries:\n%s", conf)
	}
	off := false
	d.Spec.Cache.Enabled = &off
	if strings.Contains(DNSDistConfig(d), "newPacketCache") {
		t.Error("cache.enabled=false must omit the packet cache")
	}
}

func TestDNSDistConfigRateLimitAndTLSToggles(t *testing.T) {
	d := testDNSDist()
	conf := DNSDistConfig(d)
	if strings.Contains(conf, "setQueryRate") || strings.Contains(conf, "addTLSLocal") || strings.Contains(conf, "addDOHLocal") {
		t.Error("rate limit and TLS listeners must be off by default")
	}
	d.Spec.RateLimit.QPSPerClient = 50
	d.Spec.TLS.DoT = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "dot-cert"}}
	d.Spec.TLS.DoH = dnsv1alpha1.DNSDistTLSListener{Enabled: true, CertificateSecretRef: corev1.LocalObjectReference{Name: "doh-cert"}}
	conf = DNSDistConfig(d)
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

func TestDNSDistConfigDeterministic(t *testing.T) {
	d := testDNSDist()
	first := DNSDistConfig(d)
	for i := 0; i < 20; i++ {
		if DNSDistConfig(d) != first {
			t.Fatal("conf must render identically every time (config hash stability)")
		}
	}
}
