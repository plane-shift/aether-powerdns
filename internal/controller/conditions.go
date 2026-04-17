package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// setCondition mutates s.Status.Conditions with the given condition. The
// observedGeneration is pinned to the current generation so consumers can
// tell stale conditions from current ones during a rolling update.
func setCondition(s *dnsv1alpha1.PowerDNSServer, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.Generation,
	})
}

// trueCond / falseCond / unknownCond are short helpers to keep call-sites readable.
func trueCond(s *dnsv1alpha1.PowerDNSServer, condType, reason, message string) {
	setCondition(s, condType, metav1.ConditionTrue, reason, message)
}

func falseCond(s *dnsv1alpha1.PowerDNSServer, condType, reason, message string) {
	setCondition(s, condType, metav1.ConditionFalse, reason, message)
}
