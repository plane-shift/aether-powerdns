package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/cnpg"
	"github.com/plane-shift/aether-powerdns/internal/manifests"
)

func fixesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(cnpg.GVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(cnpg.GVK.GroupVersion().WithKind(cnpg.GVK.Kind+"List"), &unstructured.UnstructuredList{})
	return scheme
}

func lbServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			Backend: dnsv1alpha1.BackendSpec{Type: dnsv1alpha1.BackendPostgres},
			DNS: dnsv1alpha1.DNSSpec{
				Exposure: dnsv1alpha1.DNSExposureLoadBalancer,
				LoadBalancer: &dnsv1alpha1.DNSLoadBalancerSpec{
					IP:          "10.1.0.241",
					Annotations: map[string]string{"metallb.io/address-pool": "mgmt-pool"},
				},
			},
		},
	}
}

// --- issue #13: updateService churn / conflicts ---

func TestUpdateServiceSteadyStateIsNoOp(t *testing.T) {
	scheme := fixesTestScheme(t)
	var updates int32
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Service); ok {
					atomic.AddInt32(&updates, 1)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	s := lbServer()

	if err := r.updateService(context.Background(), s, manifests.DNSService(s)); err != nil {
		t.Fatalf("initial create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := r.updateService(context.Background(), s, manifests.DNSService(s)); err != nil {
			t.Fatalf("steady-state pass %d: %v", i+1, err)
		}
	}
	if updates != 0 {
		t.Errorf("steady state must not write (resourceVersion churn races the LB controller), got %d updates", updates)
	}
}

func TestUpdateServicePreservesForeignFields(t *testing.T) {
	scheme := fixesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	s := lbServer()
	ctx := context.Background()

	if err := r.updateService(ctx, s, manifests.DNSService(s)); err != nil {
		t.Fatal(err)
	}

	// Simulate the LB controller: annotate + assign NodePorts.
	svc := &corev1.Service{}
	key := types.NamespacedName{Name: "test-dns", Namespace: "default"}
	if err := c.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	svc.Annotations["metallb.io/ip-allocated-from-pool"] = "mgmt-pool"
	for i := range svc.Spec.Ports {
		svc.Spec.Ports[i].NodePort = int32(31000 + i)
	}
	if err := c.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}

	// Operator drift pass must keep what the LB controller wrote.
	if err := r.updateService(ctx, s, manifests.DNSService(s)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Annotations["metallb.io/ip-allocated-from-pool"] != "mgmt-pool" {
		t.Error("foreign annotation wiped — this is the MetalLB ping-pong from issue #13")
	}
	if svc.Annotations["metallb.io/address-pool"] != "mgmt-pool" {
		t.Error("operator-managed annotation lost")
	}
	for i, p := range svc.Spec.Ports {
		if p.NodePort != int32(31000+i) {
			t.Errorf("port %q NodePort = %d, want %d (assigned NodePorts must survive)", p.Name, p.NodePort, 31000+i)
		}
	}
}

func TestUpdateServiceRetriesOnConflict(t *testing.T) {
	scheme := fixesTestScheme(t)
	var conflicted int32
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Service); ok && atomic.CompareAndSwapInt32(&conflicted, 0, 1) {
					return apierrors.NewConflict(corev1.Resource("services"), obj.GetName(), nil)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	s := lbServer()
	ctx := context.Background()

	if err := r.updateService(ctx, s, manifests.DNSService(s)); err != nil {
		t.Fatal(err)
	}
	// Force a genuine change so an Update is attempted.
	s.Spec.DNS.LoadBalancer.Annotations["metallb.io/address-pool"] = "other-pool"
	if err := r.updateService(ctx, s, manifests.DNSService(s)); err != nil {
		t.Fatalf("a single conflict must be retried, not bubbled: %v", err)
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns", Namespace: "default"}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Annotations["metallb.io/address-pool"] != "other-pool" {
		t.Error("update lost after conflict retry")
	}
}

// TestUpdateServiceOwnerRefOnRecreate verifies that when a Service is
// deleted and recreated via the server's updateService wrapper, the new
// object carries an owner reference to the PowerDNSServer (GC safety).
func TestUpdateServiceOwnerRefOnRecreate(t *testing.T) {
	scheme := fixesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	s := lbServer()
	ctx := context.Background()

	// Create the Service.
	if err := r.updateService(ctx, s, manifests.DNSService(s)); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Simulate manual deletion.
	svc := &corev1.Service{}
	key := types.NamespacedName{Name: manifests.NameSet(s).DNSService, Namespace: "default"}
	if err := c.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}

	// Recreate via updateService.
	if err := r.updateService(ctx, s, manifests.DNSService(s)); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	recreated := &corev1.Service{}
	if err := c.Get(ctx, key, recreated); err != nil {
		t.Fatalf("recreated service: %v", err)
	}
	if len(recreated.OwnerReferences) == 0 {
		t.Error("recreated Service must carry an owner reference to the PowerDNSServer")
	}
	if recreated.OwnerReferences[0].Name != s.Name {
		t.Errorf("owner ref name = %q, want %q", recreated.OwnerReferences[0].Name, s.Name)
	}
}

// --- issue #11: schema Job vs slow CNPG, failed-Job wedge ---

func cnpgCluster(ready int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpg.GVK)
	u.SetName("test-pg")
	u.SetNamespace("default")
	_ = unstructured.SetNestedField(u.Object, ready, "status", "readyInstances")
	return u
}

func appSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pg-app", Namespace: "default"},
		Data: map[string][]byte{
			"host": []byte("test-pg-rw.default.svc"), "port": []byte("5432"),
			"username": []byte("powerdns"), "password": []byte("hunter2"),
			"dbname": []byte("powerdns"),
		},
	}
}

func TestPhaseProvisioningBackendWaitsForCNPGInstance(t *testing.T) {
	scheme := fixesTestScheme(t)
	s := lbServer()
	s.Status.Phase = dnsv1alpha1.PhaseProvisioningBackend
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.PowerDNSServer{}).
		WithObjects(s, appSecret(), cnpgCluster(0)).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	key := types.NamespacedName{Name: "test", Namespace: "default"}

	// app Secret exists but no instance is ready: must hold, not advance.
	if _, err := r.phaseProvisioningBackend(ctx, s); err != nil {
		t.Fatal(err)
	}
	got := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == dnsv1alpha1.PhaseInitializingSchema {
		t.Fatal("advanced to InitializingSchema before the CNPG instance was ready (issue #11 race)")
	}

	// Instance comes up: now it advances.
	u := cnpgCluster(1)
	u.SetResourceVersion("")
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(cnpg.GVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "test-pg", Namespace: "default"}, cur); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(cur.Object, int64(1), "status", "readyInstances")
	if err := c.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.phaseProvisioningBackend(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != dnsv1alpha1.PhaseInitializingSchema {
		t.Fatalf("phase = %q, want InitializingSchema once readyInstances>=1", got.Status.Phase)
	}
}

func TestPhaseInitializingSchemaReplacesFailedJob(t *testing.T) {
	scheme := fixesTestScheme(t)
	s := lbServer()
	s.Status.Phase = dnsv1alpha1.PhaseInitializingSchema
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-schema-init", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.PowerDNSServer{}).
		WithObjects(s, failed).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	// Pass 1: the failed Job is replaced (deleted), not treated as terminal.
	if _, err := r.phaseInitializingSchema(ctx, s); err != nil {
		t.Fatal(err)
	}
	got := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == dnsv1alpha1.PhaseFailed {
		t.Fatal("a backoff-exhausted schema Job must be retried, not terminal (issue #11)")
	}
	err := c.Get(ctx, types.NamespacedName{Name: "test-schema-init", Namespace: "default"}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("failed Job should be deleted for recreation, got err=%v", err)
	}

	// Pass 2: a fresh Job is created.
	if _, err := r.phaseInitializingSchema(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-schema-init", Namespace: "default"}, &batchv1.Job{}); err != nil {
		t.Fatalf("fresh schema Job should exist: %v", err)
	}
}

// --- issue #9: ConfigMap content convergence + live dnsEndpoint ---

func TestUpdateConfigMapConvergesData(t *testing.T) {
	scheme := fixesTestScheme(t)
	var updates int32
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					atomic.AddInt32(&updates, 1)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	s := lbServer()
	ctx := context.Background()

	if err := r.updateConfigMap(ctx, s, manifests.ConfigMap(s)); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Spec change re-renders pdns.conf — the live ConfigMap must follow
	// (ensureOwned was create-only: pods rolled on the new hash while
	// mounting stale content).
	s.Spec.API.Port = 9090
	if err := r.updateConfigMap(ctx, s, manifests.ConfigMap(s)); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: manifests.NameSet(s).ConfigMap, Namespace: "default"}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["pdns.conf"], "9090") {
		t.Error("live ConfigMap still serves stale pdns.conf after spec change")
	}
	if updates != 1 {
		t.Errorf("expected exactly 1 update, got %d", updates)
	}

	// Steady state: no write.
	if err := r.updateConfigMap(ctx, s, manifests.ConfigMap(s)); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Errorf("steady state must not write, got %d updates", updates)
	}
}

func TestRefreshDNSEndpointTracksLBAssignment(t *testing.T) {
	scheme := fixesTestScheme(t)
	s := lbServer()
	s.Status.Phase = dnsv1alpha1.PhaseReady
	s.Status.DNSEndpoint = "10.1.0.241:53" // stale claim from ExposingDNS
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: manifests.NameSet(s).DNSService, Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.PowerDNSServer{}).
		WithObjects(s, svc).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	key := types.NamespacedName{Name: "test", Namespace: "default"}

	// LB has NO assigned address: the stale endpoint must be cleared.
	if err := r.refreshDNSEndpoint(ctx, s); err != nil {
		t.Fatal(err)
	}
	got := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.DNSEndpoint != "" {
		t.Errorf("pending LB must clear dnsEndpoint, still %q (issue #9)", got.Status.DNSEndpoint)
	}

	// Address assigned: endpoint comes back.
	if err := c.Get(ctx, types.NamespacedName{Name: manifests.NameSet(s).DNSService, Namespace: "default"}, svc); err != nil {
		t.Fatal(err)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.1.0.241"}}
	if err := c.Status().Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := r.refreshDNSEndpoint(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.DNSEndpoint != "10.1.0.241:53" {
		t.Errorf("assigned LB must restore dnsEndpoint, got %q", got.Status.DNSEndpoint)
	}
}

// --- Fix: updateDeployment recreate path must carry owner ref (#1) ---

// TestUpdateDeploymentOwnerRefOnRecreate verifies that when a Deployment is
// deleted after provisioning and updateDeployment recreates it, the new object
// carries a controller reference to the PowerDNSServer. Without it, GC does not
// cascade and the watch fan-out from Owns(&Deployment{}) never fires.
func TestUpdateDeploymentOwnerRefOnRecreate(t *testing.T) {
	scheme := fixesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	s := lbServer()
	ctx := context.Background()

	desired := manifests.Deployment(s, "hash1")

	// Initial create via updateDeployment.
	if err := r.updateDeployment(ctx, s, desired); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Simulate manual deletion (e.g. runaway kubectl delete).
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, dep); err != nil {
		t.Fatal(err)
	}

	// Recreate via updateDeployment — must carry owner ref.
	if err := r.updateDeployment(ctx, s, manifests.Deployment(s, "hash2")); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	recreated := &appsv1.Deployment{}
	if err := c.Get(ctx, key, recreated); err != nil {
		t.Fatalf("get recreated deployment: %v", err)
	}
	if len(recreated.OwnerReferences) == 0 {
		t.Error("recreated Deployment must carry an owner reference to the PowerDNSServer")
	}
	if recreated.OwnerReferences[0].Name != s.Name {
		t.Errorf("owner ref name = %q, want %q", recreated.OwnerReferences[0].Name, s.Name)
	}
}

// --- Fix: reconcileAdditionalDNSServices drift-converges spec changes (#2) ---

// serverWithAdditional returns a PowerDNSServer with one additionalService entry.
func serverWithAdditional(ann map[string]string) *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			Backend: dnsv1alpha1.BackendSpec{Type: dnsv1alpha1.BackendPostgres},
			DNS: dnsv1alpha1.DNSSpec{
				Exposure: dnsv1alpha1.DNSExposureLoadBalancer,
				LoadBalancer: &dnsv1alpha1.DNSLoadBalancerSpec{
					IP: "10.1.0.241",
					AdditionalServices: []dnsv1alpha1.AdditionalLoadBalancerService{
						{NameSuffix: "-extra", IP: "10.1.0.250", Annotations: ann},
					},
				},
			},
		},
	}
}

// TestReconcileAdditionalDNSServicesConvergesDrift verifies that mutating
// spec.dns.loadBalancer.additionalServices[*] (annotations or IP) propagates
// to the live Service. The previous ensureOwned path was create-only and
// silently dropped every subsequent edit.
func TestReconcileAdditionalDNSServicesConvergesDrift(t *testing.T) {
	scheme := fixesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	// Pass 1: initial create.
	s1 := serverWithAdditional(map[string]string{"metallb.io/address-pool": "pool-a"})
	if err := r.reconcileAdditionalDNSServices(ctx, s1); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	svcKey := types.NamespacedName{Name: "test-dns-extra", Namespace: "default"}
	svc := &corev1.Service{}
	if err := c.Get(ctx, svcKey, svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	if svc.Annotations["metallb.io/address-pool"] != "pool-a" {
		t.Errorf("initial annotation = %q, want pool-a", svc.Annotations["metallb.io/address-pool"])
	}
	// role label must be present.
	if svc.Labels["dns.aetherplatform.cloud/role"] != "additional-dns" {
		t.Errorf("role label missing after create, labels = %v", svc.Labels)
	}
	// owner ref must be set.
	if len(svc.OwnerReferences) == 0 {
		t.Error("Service created without owner reference")
	}

	// Pass 2: mutate annotation value — must propagate to live Service.
	s2 := serverWithAdditional(map[string]string{"metallb.io/address-pool": "pool-b"})
	if err := r.reconcileAdditionalDNSServices(ctx, s2); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	updated := &corev1.Service{}
	if err := c.Get(ctx, svcKey, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Annotations["metallb.io/address-pool"] != "pool-b" {
		t.Errorf("annotation not updated: got %q, want pool-b (ensureOwned was create-only)", updated.Annotations["metallb.io/address-pool"])
	}
	// role label must survive the drift-converge path.
	if updated.Labels["dns.aetherplatform.cloud/role"] != "additional-dns" {
		t.Errorf("role label lost after converge, labels = %v", updated.Labels)
	}
}
