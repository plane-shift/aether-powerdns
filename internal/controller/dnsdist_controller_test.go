package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/manifests"
)

func newDNSDistReconciler(t *testing.T, objs ...client.Object) (*DNSDistReconciler, client.Client) {
	t.Helper()
	scheme := routesTestScheme(t) // clientgo + dns + both gateway schemes
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSDist{}, &dnsv1alpha1.PowerDNSServer{}).
		WithIndex(&dnsv1alpha1.DNSDist{}, dnsdistBackendIndex, func(o client.Object) []string {
			d := o.(*dnsv1alpha1.DNSDist)
			keys := make([]string, 0, len(d.Spec.BackendRefs))
			for _, ref := range d.Spec.BackendRefs {
				keys = append(keys, refKey(ref, d.GetNamespace()))
			}
			return keys
		}).
		WithObjects(objs...).Build()
	return &DNSDistReconciler{Client: c, Scheme: scheme}, c
}

func readyBackend(name string) *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Status:     dnsv1alpha1.PowerDNSServerStatus{Phase: dnsv1alpha1.PhaseReady},
	}
}

func edgeDNSDist() *dnsv1alpha1.DNSDist {
	two := int32(2)
	return &dnsv1alpha1.DNSDist{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default", UID: "edge-uid"},
		Spec: dnsv1alpha1.DNSDistSpec{
			Replicas:    &two,
			BackendRefs: []dnsv1alpha1.ObjectRef{{Name: "srv-a"}},
		},
	}
}

func reconcileDNSDistN(t *testing.T, r *DNSDistReconciler, key types.NamespacedName, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
}

func TestDNSDistCreatesWorkload(t *testing.T) {
	r, c := newDNSDistReconciler(t, edgeDNSDist(), readyBackend("srv-a"))
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	reconcileDNSDistN(t, r, key, 2)
	ctx := context.Background()

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-config", Namespace: "default"}, cm); err != nil {
		t.Fatalf("configmap: %v", err)
	}
	if !strings.Contains(cm.Data["dnsdist.conf"], "srv-a-dns.default.svc") {
		t.Error("conf must carry the backend")
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist", Namespace: "default"}, dep); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	if len(dep.OwnerReferences) == 0 {
		t.Error("owner ref missing — GC relies on it (no finalizer by design)")
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-dns", Namespace: "default"}, svc); err != nil {
		t.Fatalf("service: %v", err)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-pdb", Namespace: "default"}, pdb); err != nil {
		t.Fatalf("pdb: %v", err)
	}
	got := &dnsv1alpha1.DNSDist{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, dnsv1alpha1.ConditionBackendsReady) {
		t.Errorf("BackendsReady should be True: %+v", got.Status.Conditions)
	}
}

func TestDNSDistGatesOnBackendReadiness(t *testing.T) {
	backend := readyBackend("srv-a")
	backend.Status.Phase = dnsv1alpha1.PhaseDeployingServer
	r, c := newDNSDistReconciler(t, edgeDNSDist(), backend)
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	reconcileDNSDistN(t, r, key, 2)

	got := &dnsv1alpha1.DNSDist{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, dnsv1alpha1.ConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("BackendsReady must be False while the backend isn't Ready: %+v", cond)
	}
	if got.Status.Phase == dnsv1alpha1.ZonePhaseReady {
		t.Error("tier must not report Ready with unready backends")
	}
}

func TestDNSDistMissingBackendNotReady(t *testing.T) {
	r, c := newDNSDistReconciler(t, edgeDNSDist()) // backend absent
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	reconcileDNSDistN(t, r, key, 2)
	got := &dnsv1alpha1.DNSDist{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, dnsv1alpha1.ConditionBackendsReady)
	if cond == nil || cond.Reason != "BackendNotFound" {
		t.Errorf("want BackendNotFound, got %+v", cond)
	}
}

func TestDNSDistValidateRejectsCrossNamespaceAndBadPDB(t *testing.T) {
	d := edgeDNSDist()
	d.Spec.BackendRefs = []dnsv1alpha1.ObjectRef{{Name: "x", Namespace: "other"}}
	if msg := validateDNSDist(d); !strings.Contains(msg, "namespace") {
		t.Errorf("cross-namespace backendRef must be rejected, got %q", msg)
	}

	d = edgeDNSDist()
	two := int32(2)
	d.Spec.PodDisruptionBudget = &dnsv1alpha1.PDBSpec{MinAvailable: &two}
	if msg := validateDNSDist(d); !strings.Contains(msg, "minAvailable") {
		t.Errorf("minAvailable >= replicas must be rejected, got %q", msg)
	}

	d = edgeDNSDist()
	d.Spec.TLS.DoT.Enabled = true
	if msg := validateDNSDist(d); !strings.Contains(msg, "certificateSecretRef") {
		t.Errorf("DoT without cert must be rejected, got %q", msg)
	}

	// Duplicate backendRef names must be rejected.
	d = edgeDNSDist()
	d.Spec.BackendRefs = []dnsv1alpha1.ObjectRef{{Name: "srv-a"}, {Name: "srv-a"}}
	if msg := validateDNSDist(d); !strings.Contains(msg, "duplicate") {
		t.Errorf("duplicate backendRef name must be rejected, got %q", msg)
	}
}

func TestDNSDistConfChangeUpdatesConfigMap(t *testing.T) {
	r, c := newDNSDistReconciler(t, edgeDNSDist(), readyBackend("srv-a"), readyBackend("srv-b"))
	key := types.NamespacedName{Name: "edge", Namespace: "default"}
	ctx := context.Background()
	reconcileDNSDistN(t, r, key, 2)

	d := &dnsv1alpha1.DNSDist{}
	if err := c.Get(ctx, key, d); err != nil {
		t.Fatal(err)
	}
	d.Spec.BackendRefs = append(d.Spec.BackendRefs, dnsv1alpha1.ObjectRef{Name: "srv-b"})
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	reconcileDNSDistN(t, r, key, 2)

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist-config", Namespace: "default"}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["dnsdist.conf"], "srv-b-dns.default.svc") {
		t.Error("backend addition must converge into the live conf")
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-dnsdist", Namespace: "default"}, dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Annotations[manifests.ConfigHashAnnotation] == "" {
		t.Error("config-hash annotation must be set")
	}
}
