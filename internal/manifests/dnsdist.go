package manifests

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

const (
	dotPort = 853
	dohPort = 443
)

// DNSDistNames groups the resource names derived from a DNSDist. All are
// suffixed `-dnsdist*` so a DNSDist and a PowerDNSServer sharing a
// metadata.name can't collide.
type DNSDistNames struct {
	Deployment, ConfigMap, DNSService string
	PDB, TCPRoute, UDPRoute           string
}

// DNSDistNameSet returns the canonical names for all owned resources.
func DNSDistNameSet(d *dnsv1alpha1.DNSDist) DNSDistNames {
	n := d.Name
	return DNSDistNames{
		Deployment: n + "-dnsdist",
		ConfigMap:  n + "-dnsdist-config",
		DNSService: n + "-dnsdist-dns",
		PDB:        n + "-dnsdist-pdb",
		TCPRoute:   n + "-dnsdist-tcp",
		UDPRoute:   n + "-dnsdist-udp",
	}
}

// dnsdistLabels parallel the server labels with component=frontend.
func dnsdistLabels(d *dnsv1alpha1.DNSDist) map[string]string {
	return map[string]string{
		labelApp:       "dnsdist",
		labelComponent: "frontend",
		labelManagedBy: managedBy,
		labelInstance:  d.Name,
	}
}

func dnsdistReplicas(d *dnsv1alpha1.DNSDist) int32 {
	if d.Spec.Replicas != nil {
		return *d.Spec.Replicas
	}
	return 1
}

// DNSDistConfig renders dnsdist.conf (Lua). Deterministic: backends are
// sorted so the config hash is stable across reconciles.
func DNSDistConfig(d *dnsv1alpha1.DNSDist) string {
	var b strings.Builder
	b.WriteString("-- managed by aether-powerdns\n")
	// dnsdist's DEFAULT ACL allows only RFC1918 — a public frontend
	// would silently refuse everything without this.
	b.WriteString(`setACL({"0.0.0.0/0", "::/0"})` + "\n")
	b.WriteString(`setLocal("0.0.0.0:53", {reusePort=true})` + "\n")

	names := make([]string, 0, len(d.Spec.BackendRefs))
	for _, ref := range d.Spec.BackendRefs {
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	for _, n := range names {
		// Backend = the server's DNS Service FQDN (v1: service-level
		// addressing; per-pod discovery is v2). Active health checks
		// with fast up/down.
		fmt.Fprintf(&b, `newServer({address="%s-dns.%s.svc.cluster.local:53", name="%s", checkInterval=2, maxCheckFailures=2, rise=1})`+"\n",
			n, d.Namespace, n)
	}

	if d.Spec.Cache.Enabled == nil || *d.Spec.Cache.Enabled {
		maxEntries := int32(100000)
		if d.Spec.Cache.MaxEntries != nil {
			maxEntries = *d.Spec.Cache.MaxEntries
		}
		fmt.Fprintf(&b, "pc = newPacketCache(%d, {maxTTL=86400})\n", maxEntries)
		b.WriteString(`getPool(""):setCache(pc)` + "\n")
	}

	if qps := d.Spec.RateLimit.QPSPerClient; qps > 0 {
		b.WriteString("local dbr = dynBlockRulesGroup()\n")
		fmt.Fprintf(&b, `dbr:setQueryRate(%d, 10, "rate-limited", 60)`+"\n", qps)
		b.WriteString("function maintenance() dbr:apply() end\n")
	}

	if d.Spec.TLS.DoT.Enabled {
		b.WriteString(`addTLSLocal("0.0.0.0:853", "/tls/dot/tls.crt", "/tls/dot/tls.key")` + "\n")
	}
	if d.Spec.TLS.DoH.Enabled {
		b.WriteString(`addDOHLocal("0.0.0.0:443", "/tls/doh/tls.crt", "/tls/doh/tls.key", { "/dns-query" })` + "\n")
	}
	return b.String()
}

// DNSDistConfigMap wraps the rendered conf.
func DNSDistConfigMap(d *dnsv1alpha1.DNSDist) *corev1.ConfigMap {
	names := DNSDistNameSet(d)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap, Namespace: d.Namespace, Labels: dnsdistLabels(d)},
		Data:       map[string]string{"dnsdist.conf": DNSDistConfig(d)},
	}
}

// DNSDistDeployment renders the dnsdist workload with the same HA posture
// as the pdns Deployment (anti-affinity + hostname topology spread when
// replicas > 1, config-hash rolling restarts).
func DNSDistDeployment(d *dnsv1alpha1.DNSDist, configHash string) *appsv1.Deployment {
	names := DNSDistNameSet(d)
	lbls := dnsdistLabels(d)
	replicas := dnsdistReplicas(d)
	image := d.Spec.Image
	if image == "" {
		image = dnsv1alpha1.DefaultDNSDistImage
	}

	ports := []corev1.ContainerPort{
		{Name: "dns-tcp", ContainerPort: dnsTCPPort, Protocol: corev1.ProtocolTCP},
		{Name: "dns-udp", ContainerPort: dnsUDPPort, Protocol: corev1.ProtocolUDP},
	}
	mounts := []corev1.VolumeMount{{Name: "config", MountPath: "/etc/dnsdist", ReadOnly: true}}
	volumes := []corev1.Volume{{
		Name: "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: names.ConfigMap},
		}},
	}}
	if d.Spec.TLS.DoT.Enabled {
		ports = append(ports, corev1.ContainerPort{Name: "dot", ContainerPort: dotPort, Protocol: corev1.ProtocolTCP})
		mounts = append(mounts, corev1.VolumeMount{Name: "dot-cert", MountPath: "/tls/dot", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "dot-cert",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: d.Spec.TLS.DoT.CertificateSecretRef.Name,
			}},
		})
	}
	if d.Spec.TLS.DoH.Enabled {
		ports = append(ports, corev1.ContainerPort{Name: "doh", ContainerPort: dohPort, Protocol: corev1.ProtocolTCP})
		mounts = append(mounts, corev1.VolumeMount{Name: "doh-cert", MountPath: "/tls/doh", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "doh-cert",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: d.Spec.TLS.DoH.CertificateSecretRef.Name,
			}},
		})
	}

	podAnnotations := map[string]string{}
	if configHash != "" {
		podAnnotations[ConfigHashAnnotation] = configHash
	}

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:         "dnsdist",
			Image:        image,
			Args:         []string{"--supervised", "--disable-syslog", "-C", "/etc/dnsdist/dnsdist.conf"},
			Ports:        ports,
			Resources:    d.Spec.Resources,
			VolumeMounts: mounts,
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(dnsTCPPort)},
				},
				InitialDelaySeconds: 2,
				PeriodSeconds:       5,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(dnsTCPPort)},
				},
				InitialDelaySeconds: 10,
				PeriodSeconds:       10,
			},
		}},
		Volumes:           volumes,
		NodeSelector:      d.Spec.Scheduling.NodeSelector,
		Tolerations:       d.Spec.Scheduling.Tolerations,
		PriorityClassName: d.Spec.Scheduling.PriorityClassName,
	}

	if replicas > 1 {
		podSpec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: lbls},
						TopologyKey:   corev1.LabelHostname,
					},
				}},
			},
		}
		podSpec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelHostname,
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: lbls},
		}}
		if d.Spec.Scheduling.SpreadAcrossZones {
			podSpec.TopologySpreadConstraints = append(podSpec.TopologySpreadConstraints,
				corev1.TopologySpreadConstraint{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: lbls},
				})
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: names.Deployment, Namespace: d.Namespace, Labels: lbls},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: podAnnotations},
				Spec:       podSpec,
			},
		},
	}
}

// DNSDistDNSService renders the primary DNS Service for the tier — the
// backend target for gateway routes (gateway → dnsdist → pdns) and the
// LoadBalancer when exposure=loadBalancer.
func DNSDistDNSService(d *dnsv1alpha1.DNSDist) *corev1.Service {
	names := DNSDistNameSet(d)
	lbls := dnsdistLabels(d)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: names.DNSService, Namespace: d.Namespace, Labels: lbls},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: lbls,
			Ports: []corev1.ServicePort{
				{Name: "dns-tcp", Port: dnsTCPPort, TargetPort: intstr.FromInt(dnsTCPPort), Protocol: corev1.ProtocolTCP},
				{Name: "dns-udp", Port: dnsUDPPort, TargetPort: intstr.FromInt(dnsUDPPort), Protocol: corev1.ProtocolUDP},
			},
		},
	}
	if d.Spec.TLS.DoT.Enabled {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "dot", Port: dotPort, TargetPort: intstr.FromInt(dotPort), Protocol: corev1.ProtocolTCP})
	}
	if d.Spec.TLS.DoH.Enabled {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "doh", Port: dohPort, TargetPort: intstr.FromInt(dohPort), Protocol: corev1.ProtocolTCP})
	}
	if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureLoadBalancer {
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		applyLBPrimary(svc, d.Spec.DNS.LoadBalancer)
	}
	return svc
}

// DNSDistAdditionalDNSServices renders the extra LoadBalancer Services
// declared under spec.dns.loadBalancer.additionalServices. Returns nil
// when exposure != loadBalancer or no additional services are configured.
// Each Service uses the same pod selector/ports as the primary DNS Service
// (including optional DoT/DoH ports).
func DNSDistAdditionalDNSServices(d *dnsv1alpha1.DNSDist) []*corev1.Service {
	if d.Spec.DNS.Exposure != dnsv1alpha1.DNSExposureLoadBalancer ||
		d.Spec.DNS.LoadBalancer == nil ||
		len(d.Spec.DNS.LoadBalancer.AdditionalServices) == 0 {
		return nil
	}
	names := DNSDistNameSet(d)
	lbls := dnsdistLabels(d)
	// Build the base port list once (matches primary Service ports).
	basePorts := []corev1.ServicePort{
		{Name: "dns-tcp", Port: dnsTCPPort, TargetPort: intstr.FromInt(dnsTCPPort), Protocol: corev1.ProtocolTCP},
		{Name: "dns-udp", Port: dnsUDPPort, TargetPort: intstr.FromInt(dnsUDPPort), Protocol: corev1.ProtocolUDP},
	}
	if d.Spec.TLS.DoT.Enabled {
		basePorts = append(basePorts, corev1.ServicePort{Name: "dot", Port: dotPort, TargetPort: intstr.FromInt(dotPort), Protocol: corev1.ProtocolTCP})
	}
	if d.Spec.TLS.DoH.Enabled {
		basePorts = append(basePorts, corev1.ServicePort{Name: "doh", Port: dohPort, TargetPort: intstr.FromInt(dohPort), Protocol: corev1.ProtocolTCP})
	}

	out := make([]*corev1.Service, 0, len(d.Spec.DNS.LoadBalancer.AdditionalServices))
	for _, extra := range d.Spec.DNS.LoadBalancer.AdditionalServices {
		ports := make([]corev1.ServicePort, len(basePorts))
		copy(ports, basePorts)
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      names.DNSService + extra.NameSuffix,
				Namespace: d.Namespace,
				Labels:    lbls,
			},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeLoadBalancer,
				Selector: lbls,
				Ports:    ports,
			},
		}
		if extra.IP != "" {
			svc.Spec.LoadBalancerIP = extra.IP
		}
		if len(extra.Annotations) > 0 {
			svc.Annotations = map[string]string{}
			for k, v := range extra.Annotations {
				svc.Annotations[k] = v
			}
		}
		if extra.ExternalTrafficPolicy != "" {
			svc.Spec.ExternalTrafficPolicy = extra.ExternalTrafficPolicy
		} else {
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		}
		out = append(out, svc)
	}
	return out
}

// DNSDistPDB delegates to the shared pdbFor (same semantics as servers).
func DNSDistPDB(d *dnsv1alpha1.DNSDist) *policyv1.PodDisruptionBudget {
	var override *int32
	if d.Spec.PodDisruptionBudget != nil {
		override = d.Spec.PodDisruptionBudget.MinAvailable
	}
	return pdbFor(DNSDistNameSet(d).PDB, d.Namespace, dnsdistLabels(d), dnsdistReplicas(d), override)
}

// DNSDistTCPRoute / DNSDistUDPRoute expose the tier via Gateway API,
// backending the DNSDIST Service.
func DNSDistTCPRoute(d *dnsv1alpha1.DNSDist) *gatewayv1alpha2.TCPRoute {
	names := DNSDistNameSet(d)
	return buildTCPRoute(names.TCPRoute, d.Namespace, dnsdistLabels(d),
		dnsRouteParents(&d.Spec.DNS, d.Namespace, gatewayProtoTCP), names.DNSService, dnsTCPPort)
}

func DNSDistUDPRoute(d *dnsv1alpha1.DNSDist) *gatewayv1alpha2.UDPRoute {
	names := DNSDistNameSet(d)
	return buildUDPRoute(names.UDPRoute, d.Namespace, dnsdistLabels(d),
		dnsRouteParents(&d.Spec.DNS, d.Namespace, gatewayProtoUDP), names.DNSService, dnsUDPPort)
}
