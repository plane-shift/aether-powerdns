package manifests

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// NetworkPolicy returns a default-deny + selective-allow ingress policy
// for the pdns pods:
//   - DNS (53/tcp + 53/udp): from anywhere (DNS is fundamentally public).
//   - API (api port / tcp): from the pdns pod's own namespace, plus any
//     namespace listed under spec.networkPolicy.additionalAllowedAPINamespaces
//     (matched by the standard `kubernetes.io/metadata.name` label).
//
// We don't restrict egress — the pod must reach Postgres, which lives in
// a namespace the operator cannot enumerate generically.
func NetworkPolicy(s *dnsv1alpha1.PowerDNSServer) *networkingv1.NetworkPolicy {
	names := NameSet(s)
	api := apiSpecOrDefault(s)

	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(dnsTCPPort)
	apiPort := intstr.FromInt(int(api.Port))

	apiPeers := []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": s.Namespace},
		},
	}}
	for _, ns := range s.Spec.NetworkPolicy.AdditionalAllowedAPINamespaces {
		apiPeers = append(apiPeers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.NetPolicy,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels(s)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &dnsPort},
						{Protocol: &udp, Port: &dnsPort},
					},
				},
				{
					From: apiPeers,
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &apiPort},
					},
				},
			},
		},
	}
}
