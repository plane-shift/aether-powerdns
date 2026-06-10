package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func newRRSetReconciler(t *testing.T, objs ...client.Object) (*RRSetReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.Zone{}, &dnsv1alpha1.RRSet{}, &dnsv1alpha1.PowerDNSServer{}).
		WithIndex(&dnsv1alpha1.RRSet{}, rrsetZoneIndex, func(o client.Object) []string {
			rr := o.(*dnsv1alpha1.RRSet)
			return []string{refKey(rr.Spec.ZoneRef, rr.GetNamespace())}
		}).
		WithObjects(objs...).
		Build()
	return &RRSetReconciler{Client: c, Scheme: scheme}, c
}

func reconcileRRSetN(t *testing.T, r *RRSetReconciler, key types.NamespacedName, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
	}
}

// readyZone returns a Zone CR already marked Ready, as the zone
// controller would leave it.
func readyZone(ns string) *dnsv1alpha1.Zone {
	z := basicZone(ns)
	z.Finalizers = []string{zoneFinalizer}
	z.Status.Phase = dnsv1alpha1.ZonePhaseReady
	return z
}

func wwwRRSet(ns string) *dnsv1alpha1.RRSet {
	ttl := int32(300)
	return &dnsv1alpha1.RRSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www-example-com", Namespace: ns},
		Spec: dnsv1alpha1.RRSetSpec{
			ZoneRef: dnsv1alpha1.ObjectRef{Name: "example-com"},
			Name:    "www.example.com.",
			Type:    "A",
			TTL:     &ttl,
			Records: []string{"203.0.113.10"},
		},
	}
}

func getRR(t *testing.T, c client.Client, key types.NamespacedName) *dnsv1alpha1.RRSet {
	t.Helper()
	rr := &dnsv1alpha1.RRSet{}
	if err := c.Get(context.Background(), key, rr); err != nil {
		t.Fatal(err)
	}
	return rr
}

func TestRRSetApplyReplaces(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), wwwRRSet("dns"))
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)

	got, ok := f.getRRSet("example.com.", "www.example.com.", "A")
	if !ok {
		t.Fatal("rrset was not applied")
	}
	if got.TTL != 300 || got.Records[0].Content != "203.0.113.10" {
		t.Errorf("unexpected applied rrset: %+v", got)
	}
	rr := getRR(t, c, key)
	if !meta.IsStatusConditionTrue(rr.Status.Conditions, dnsv1alpha1.ConditionReady) {
		t.Errorf("Ready should be True, conditions: %+v", rr.Status.Conditions)
	}
	if rr.Status.ObservedGeneration != rr.Generation {
		t.Error("observedGeneration not updated")
	}
}

func TestRRSetDefaultTTL(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	rr := wwwRRSet("dns")
	rr.Spec.TTL = nil
	r, _ := newRRSetReconciler(t, server, secret, readyZone("dns"), rr)

	reconcileRRSetN(t, r, types.NamespacedName{Name: "www-example-com", Namespace: "dns"}, 3)

	got, _ := f.getRRSet("example.com.", "www.example.com.", "A")
	if got.TTL != 3600 {
		t.Errorf("default TTL = %d, want 3600", got.TTL)
	}
}

func TestRRSetNameOutsideZoneRejected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	rr := wwwRRSet("dns")
	rr.Spec.Name = "www.other.org."
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), rr)
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)

	cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "NameOutsideZone" {
		t.Errorf("want NameOutsideZone, got %+v", cond)
	}
}

func TestRRSetSecondaryZoneRejected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Secondary")
	server, secret := readyServer("pdns", "dns", f)
	zone := readyZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindSecondary
	zone.Spec.Masters = []string{"198.51.100.1"}
	zone.Spec.Nameservers = nil
	r, c := newRRSetReconciler(t, server, secret, zone, wwwRRSet("dns"))
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)

	cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "SecondaryZone" {
		t.Errorf("want SecondaryZone, got %+v", cond)
	}
}

func TestRRSetConflictRejectsBoth(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	a := wwwRRSet("dns")
	b := wwwRRSet("dns")
	b.Name = "www-example-com-dup"
	b.Spec.Records = []string{"198.51.100.99"}
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), a, b)
	keyA := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}
	keyB := types.NamespacedName{Name: "www-example-com-dup", Namespace: "dns"}

	reconcileRRSetN(t, r, keyA, 3)
	reconcileRRSetN(t, r, keyB, 3)

	for _, key := range []types.NamespacedName{keyA, keyB} {
		cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Conflict" {
			t.Errorf("%s: want Ready=False/Conflict, got %+v", key.Name, cond)
		}
	}
	if _, ok := f.getRRSet("example.com.", "www.example.com.", "A"); ok {
		t.Error("neither conflicting rrset may be applied")
	}
}

func TestRRSetDeleteRemovesFromPowerDNS(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), wwwRRSet("dns"))
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	reconcileRRSetN(t, r, key, 3)
	if _, ok := f.getRRSet("example.com.", "www.example.com.", "A"); !ok {
		t.Fatal("precondition: rrset applied")
	}
	if err := c.Delete(context.Background(), getRR(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileRRSetN(t, r, key, 2)
	if _, ok := f.getRRSet("example.com.", "www.example.com.", "A"); ok {
		t.Error("rrset should be deleted from PowerDNS")
	}
}

func TestRRSetDeleteSkipsAPIWhenSiblingSurvives(t *testing.T) {
	// With reject-both conflicts, deleting one claimant must NOT wipe the
	// rrset out from under the survivor — skip the API delete and let the
	// survivor re-apply on its next pass.
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	a := wwwRRSet("dns")
	b := wwwRRSet("dns")
	b.Name = "www-example-com-dup"
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), a, b)
	keyA := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}
	keyB := types.NamespacedName{Name: "www-example-com-dup", Namespace: "dns"}

	reconcileRRSetN(t, r, keyA, 3)
	reconcileRRSetN(t, r, keyB, 3)
	if err := c.Delete(context.Background(), getRR(t, c, keyB)); err != nil {
		t.Fatal(err)
	}
	reconcileRRSetN(t, r, keyB, 2)

	// survivor recovers
	reconcileRRSetN(t, r, keyA, 3)
	got, ok := f.getRRSet("example.com.", "www.example.com.", "A")
	if !ok {
		t.Fatal("survivor's rrset should be applied after the conflict clears")
	}
	if got.Records[0].Content != "203.0.113.10" {
		t.Errorf("survivor content = %q", got.Records[0].Content)
	}
}

func TestRRSetDeleteReleasesWhenZoneGone(t *testing.T) {
	rr := wwwRRSet("dns")
	rr.Finalizers = []string{rrsetFinalizer}
	r, c := newRRSetReconciler(t, rr)
	key := types.NamespacedName{Name: "www-example-com", Namespace: "dns"}

	if err := c.Delete(context.Background(), getRR(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileRRSetN(t, r, key, 2)
	if err := c.Get(context.Background(), key, &dnsv1alpha1.RRSet{}); err == nil {
		t.Error("rrset should be gone after finalizer release")
	}
}

func TestRRSetCrossNamespaceDenied(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	rr := wwwRRSet("app-team")
	rr.Spec.ZoneRef = dnsv1alpha1.ObjectRef{Name: "example-com", Namespace: "dns"}
	r, c := newRRSetReconciler(t, server, secret, readyZone("dns"), rr)
	key := types.NamespacedName{Name: "www-example-com", Namespace: "app-team"}

	reconcileRRSetN(t, r, key, 3)

	cond := meta.FindStatusCondition(getRR(t, c, key).Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "NamespaceNotAllowed" {
		t.Errorf("want NamespaceNotAllowed, got %+v", cond)
	}
}
