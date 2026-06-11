package controller

import (
	"context"
	"sync/atomic"
	"testing"

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

	if err := r.updateService(context.Background(), manifests.DNSService(s)); err != nil {
		t.Fatalf("initial create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := r.updateService(context.Background(), manifests.DNSService(s)); err != nil {
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

	if err := r.updateService(ctx, manifests.DNSService(s)); err != nil {
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
	if err := r.updateService(ctx, manifests.DNSService(s)); err != nil {
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

	if err := r.updateService(ctx, manifests.DNSService(s)); err != nil {
		t.Fatal(err)
	}
	// Force a genuine change so an Update is attempted.
	s.Spec.DNS.LoadBalancer.Annotations["metallb.io/address-pool"] = "other-pool"
	if err := r.updateService(ctx, manifests.DNSService(s)); err != nil {
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
