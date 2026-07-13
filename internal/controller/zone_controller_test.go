package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newZoneReconciler(t *testing.T, objs ...client.Object) (*ZoneReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.Zone{}, &dnsv1alpha1.PowerDNSServer{}).
		WithIndex(&dnsv1alpha1.Zone{}, zoneServerIndex, func(o client.Object) []string {
			z := o.(*dnsv1alpha1.Zone)
			return []string{refKey(z.Spec.ServerRef, z.GetNamespace())}
		}).
		WithObjects(objs...).
		Build()
	return &ZoneReconciler{Client: c, Scheme: scheme}, c
}

// reconcileZoneN drives the reconciler through n passes (finalizer-add and
// phase writes requeue, so single calls rarely settle).
func reconcileZoneN(t *testing.T, r *ZoneReconciler, key types.NamespacedName, n int) ctrl.Result {
	t.Helper()
	var res ctrl.Result
	var err error
	for i := 0; i < n; i++ {
		res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
	}
	return res
}

func basicZone(ns string) *dnsv1alpha1.Zone {
	return &dnsv1alpha1.Zone{
		ObjectMeta: metav1.ObjectMeta{Name: "example-com", Namespace: ns},
		Spec: dnsv1alpha1.ZoneSpec{
			ServerRef:   dnsv1alpha1.ObjectRef{Name: "pdns"},
			ZoneName:    "example.com.",
			Kind:        dnsv1alpha1.ZoneKindNative,
			Nameservers: []string{"ns1.example.com."},
		},
	}
}

func getZone(t *testing.T, c client.Client, key types.NamespacedName) *dnsv1alpha1.Zone {
	t.Helper()
	z := &dnsv1alpha1.Zone{}
	if err := c.Get(context.Background(), key, z); err != nil {
		t.Fatal(err)
	}
	return z
}

func TestZoneCreateRegistersInPowerDNS(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	res := reconcileZoneN(t, r, key, 3)

	if !f.hasZone("example.com.") {
		t.Fatal("zone was not created in PowerDNS")
	}
	z := getZone(t, c, key)
	if z.Status.Phase != dnsv1alpha1.ZonePhaseReady {
		t.Errorf("phase = %q, want Ready (conditions: %+v)", z.Status.Phase, z.Status.Conditions)
	}
	if !meta.IsStatusConditionTrue(z.Status.Conditions, dnsv1alpha1.ConditionRegistered) {
		t.Error("Registered condition should be True")
	}
	if res.RequeueAfter != resyncInterval {
		t.Errorf("Ready zones must resync every %v, got %v", resyncInterval, res.RequeueAfter)
	}
	if len(z.Finalizers) == 0 {
		t.Error("finalizer missing")
	}
}

func TestZoneSeedsSOAOnCreate(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.SOA = &dnsv1alpha1.SOASpec{Hostmaster: "hostmaster.example.com."}
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	soa, ok := f.getRRSet("example.com.", "example.com.", "SOA")
	if !ok {
		t.Fatal("SOA was not seeded")
	}
	if soa.Records[0].Content != "ns1.example.com. hostmaster.example.com. 0 10800 3600 604800 3600" {
		t.Errorf("unexpected SOA content: %q", soa.Records[0].Content)
	}
}

// TestZoneAdoptsExistingOnDuplicateCreate covers the zombie-write race
// (aether-powerdns#24): an earlier create committed the domain server-side
// but timed out client-side, so GetZone misses it yet the create POST hits
// the unique constraint. The reconciler must ADOPT the existing zone and go
// Ready — not loop on duplicate-key (that retry storm wedged pdns and took
// dev DNS down).
func TestZoneAdoptsExistingOnDuplicateCreate(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	// The zone already exists in pdns (committed by the timed-out create)...
	f.seedZone("example.com.", "Native")
	// ...but the reconciler's first GetZone misses it, forcing the create path.
	f.failNextGet = true
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)

	z := getZone(t, c, key)
	if z.Status.Phase != dnsv1alpha1.ZonePhaseReady {
		t.Errorf("phase = %q, want Ready (adopt on duplicate; conditions: %+v)", z.Status.Phase, z.Status.Conditions)
	}
	if !meta.IsStatusConditionTrue(z.Status.Conditions, dnsv1alpha1.ConditionRegistered) {
		t.Error("Registered should be True after adopting the existing zone")
	}
}

func TestZoneServerNotFound(t *testing.T) {
	zone := basicZone("dns")
	r, c := newZoneReconciler(t, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)

	z := getZone(t, c, key)
	cond := meta.FindStatusCondition(z.Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServerNotFound" {
		t.Errorf("want Ready=False/ServerNotFound, got %+v", cond)
	}
}

func TestZoneCrossNamespaceAuthz(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns-system", f)
	zone := basicZone("team-a")
	zone.Spec.ServerRef = dnsv1alpha1.ObjectRef{Name: "pdns", Namespace: "dns-system"}
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "team-a"}

	// denied without an allow-list
	reconcileZoneN(t, r, key, 3)
	z := getZone(t, c, key)
	cond := meta.FindStatusCondition(z.Status.Conditions, dnsv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "NamespaceNotAllowed" {
		t.Fatalf("want NamespaceNotAllowed, got %+v", cond)
	}

	// allowed once listed
	server2 := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pdns", Namespace: "dns-system"}, server2); err != nil {
		t.Fatal(err)
	}
	server2.Spec.ZoneManagement.AllowedNamespaces = []string{"team-a"}
	if err := c.Update(context.Background(), server2); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 3)
	if !f.hasZone("example.com.") {
		t.Error("zone should be created once the namespace is allowed")
	}
}

func TestZoneSecondaryWithDNSSECIsInvalid(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindSecondary
	zone.Spec.Masters = []string{"198.51.100.1"}
	zone.Spec.Nameservers = nil
	zone.Spec.DNSSEC.Enabled = true
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)

	z := getZone(t, c, key)
	if z.Status.Phase != dnsv1alpha1.ZonePhaseFailed {
		t.Errorf("phase = %q, want Failed", z.Status.Phase)
	}
	if f.hasZone("example.com.") {
		t.Error("invalid zone must not be created in PowerDNS")
	}
}

func TestZoneKindDriftCorrected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Native")
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindPrimary
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	f.mu.Lock()
	kind := f.zones["example.com."].Kind
	f.mu.Unlock()
	if kind != "Primary" {
		t.Errorf("zone kind = %q, want Primary (drift not corrected)", kind)
	}
}

func TestZoneDNSSECLifecycle(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.DNSSEC.Enabled = true
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)
	z := getZone(t, c, key)
	if len(z.Status.DSRecords) == 0 {
		t.Fatal("dsRecords not surfaced after enabling DNSSEC")
	}
	keys := f.zoneKeys("example.com.")
	if len(keys) != 1 || !keys[0].Active {
		t.Fatalf("want one active key, got %+v", keys)
	}

	// disable: key DEACTIVATED, not deleted
	z.Spec.DNSSEC.Enabled = false
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 3)
	keys = f.zoneKeys("example.com.")
	if len(keys) != 1 || keys[0].Active {
		t.Fatalf("disable must deactivate but KEEP the key, got %+v", keys)
	}

	// re-enable: same key reactivated, no new key minted
	z = getZone(t, c, key)
	z.Spec.DNSSEC.Enabled = true
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 3)
	keys = f.zoneKeys("example.com.")
	if len(keys) != 1 || !keys[0].Active {
		t.Fatalf("re-enable must reuse the existing key, got %+v", keys)
	}
}

func TestZoneDeleteRemovesFromPowerDNS(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)
	if !f.hasZone("example.com.") {
		t.Fatal("precondition: zone exists")
	}
	if err := c.Delete(context.Background(), getZone(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 2)
	if f.hasZone("example.com.") {
		t.Error("zone should be deleted from PowerDNS")
	}
}

func TestZoneDeleteOrphanKeepsZone(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.DeletionPolicy = dnsv1alpha1.DeletionPolicyOrphan
	r, c := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	reconcileZoneN(t, r, key, 3)
	if err := c.Delete(context.Background(), getZone(t, c, key)); err != nil {
		t.Fatal(err)
	}
	reconcileZoneN(t, r, key, 2)
	if !f.hasZone("example.com.") {
		t.Error("Orphan policy must leave the zone in PowerDNS")
	}
}

func TestZoneDeleteReleasesWhenServerGone(t *testing.T) {
	// Wedge-proofing: a Zone whose server CR no longer exists must still
	// be deletable — the zone data dies with the server's Postgres anyway.
	zone := basicZone("dns")
	zone.Finalizers = []string{zoneFinalizer}
	now := metav1.NewTime(time.Now())
	zone.DeletionTimestamp = &now
	r, c := newZoneReconciler(t, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	err := c.Get(context.Background(), key, &dnsv1alpha1.Zone{})
	if err == nil {
		t.Error("zone should be gone after finalizer release")
	}
}

func TestZoneMastersDriftCorrected(t *testing.T) {
	f := newFakePDNS(t)
	f.seedZone("example.com.", "Secondary")
	f.mu.Lock()
	f.zones["example.com."].Masters = []string{"198.51.100.250"}
	f.mu.Unlock()
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.Kind = dnsv1alpha1.ZoneKindSecondary
	zone.Spec.Masters = []string{"198.51.100.1", "198.51.100.2"}
	zone.Spec.Nameservers = nil
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	f.mu.Lock()
	masters := f.zones["example.com."].Masters
	f.mu.Unlock()
	if len(masters) != 2 || masters[0] != "198.51.100.1" || masters[1] != "198.51.100.2" {
		t.Errorf("masters = %v, want the spec's two primaries (drift not corrected)", masters)
	}
}

func TestZoneSOASeedFailureRollsBackAndRetries(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.SOA = &dnsv1alpha1.SOASpec{Hostmaster: "hostmaster.example.com."}
	r, _ := newZoneReconciler(t, server, secret, zone)
	key := types.NamespacedName{Name: "example-com", Namespace: "dns"}

	// First pass adds the finalizer; inject the failure before the pass
	// that creates the zone, so CreateZone succeeds and the SOA PATCH 500s.
	reconcileZoneN(t, r, key, 1)
	f.mu.Lock()
	f.failNextPatch = true
	f.mu.Unlock()
	reconcileZoneN(t, r, key, 1)

	// Compensation must have rolled the half-created zone back.
	if f.hasZone("example.com.") {
		t.Fatal("zone should have been rolled back after SOA seed failure")
	}

	// Next pass recreates the zone WITH the SOA seed.
	reconcileZoneN(t, r, key, 2)
	if !f.hasZone("example.com.") {
		t.Fatal("zone should be recreated on retry")
	}
	soa, ok := f.getRRSet("example.com.", "example.com.", "SOA")
	if !ok {
		t.Fatal("SOA should be seeded on the retry pass")
	}
	if soa.Records[0].Content != "ns1.example.com. hostmaster.example.com. 0 10800 3600 604800 3600" {
		t.Errorf("unexpected SOA content: %q", soa.Records[0].Content)
	}
}

// Issue: zones created WITHOUT spec.soa used to inherit PowerDNS's
// default-soa-content placeholder ("a.misconfigured.dns.server.invalid.").
// The operator must always seed a correct SOA on creation.
func TestZoneSeedsDefaultSOAWhenUnset(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns") // nameservers: [ns1.example.com.], no spec.soa
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	soa, ok := f.getRRSet("example.com.", "example.com.", "SOA")
	if !ok {
		t.Fatal("SOA must be seeded even without spec.soa")
	}
	want := "ns1.example.com. hostmaster.example.com. 0 10800 3600 604800 3600"
	if soa.Records[0].Content != want {
		t.Errorf("SOA = %q, want %q (primary = first nameserver, default hostmaster)", soa.Records[0].Content, want)
	}
}

func TestZoneDefaultSOAWithoutNameserversUsesApex(t *testing.T) {
	f := newFakePDNS(t)
	server, secret := readyServer("pdns", "dns", f)
	zone := basicZone("dns")
	zone.Spec.Nameservers = nil
	r, _ := newZoneReconciler(t, server, secret, zone)

	reconcileZoneN(t, r, types.NamespacedName{Name: "example-com", Namespace: "dns"}, 3)

	soa, ok := f.getRRSet("example.com.", "example.com.", "SOA")
	if !ok {
		t.Fatal("SOA must be seeded")
	}
	want := "example.com. hostmaster.example.com. 0 10800 3600 604800 3600"
	if soa.Records[0].Content != want {
		t.Errorf("SOA = %q, want apex-as-primary fallback %q", soa.Records[0].Content, want)
	}
}
