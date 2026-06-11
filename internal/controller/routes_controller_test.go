package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func routesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, dnsv1alpha1.AddToScheme,
		gatewayv1alpha2.Install, gatewayv1.Install,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func newRoutesReconciler(t *testing.T, objs ...client.Object) (*PowerDNSServerReconciler, client.Client) {
	t.Helper()
	scheme := routesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PowerDNSServerReconciler{Client: c, Scheme: scheme}, c
}

func gatewayExposedServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			DNS: dnsv1alpha1.DNSSpec{
				Exposure: dnsv1alpha1.DNSExposureGateway,
				Gateway: &dnsv1alpha1.DNSGatewaySpec{
					ParentRefs: []dnsv1alpha1.GatewayParentRef{{Name: "eg", Namespace: "envoy-gateway-system"}},
				},
			},
			API: dnsv1alpha1.APISpec{
				Gateway: &dnsv1alpha1.APIGatewaySpec{
					ParentRefs: []dnsv1alpha1.APIGatewayParentRef{{Name: "eg", Namespace: "envoy-gateway-system", SectionName: "https"}},
				},
			},
		},
	}
}

func TestReconcileRoutesCreatesAllThree(t *testing.T) {
	s := gatewayExposedServer()
	r, c := newRoutesReconciler(t, s)

	if err := r.reconcileRoutes(context.Background(), s); err != nil {
		t.Fatalf("reconcileRoutes: %v", err)
	}

	ctx := context.Background()
	tcp := &gatewayv1alpha2.TCPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns-tcp", Namespace: "default"}, tcp); err != nil {
		t.Errorf("TCPRoute missing: %v", err)
	}
	if len(tcp.OwnerReferences) == 0 {
		t.Error("TCPRoute must carry an owner reference for GC")
	}
	udp := &gatewayv1alpha2.UDPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns-udp", Namespace: "default"}, udp); err != nil {
		t.Errorf("UDPRoute missing: %v", err)
	}
	if len(udp.OwnerReferences) == 0 {
		t.Error("UDPRoute must carry an owner reference for GC")
	}
	http := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-api-http", Namespace: "default"}, http); err != nil {
		t.Fatalf("HTTPRoute missing: %v", err)
	}
	if len(http.OwnerReferences) == 0 {
		t.Error("HTTPRoute must carry an owner reference for GC")
	}
}

func TestReconcileRoutesUpdatesParentRefDrift(t *testing.T) {
	s := gatewayExposedServer()
	r, c := newRoutesReconciler(t, s)
	ctx := context.Background()
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	// live edit: spec moves to a different gateway
	s.Spec.API.Gateway.ParentRefs[0].Name = "internal-gw"
	s.Spec.DNS.Gateway.ParentRefs[0].Name = "internal-gw"
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	http := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-api-http", Namespace: "default"}, http); err != nil {
		t.Fatal(err)
	}
	if string(http.Spec.ParentRefs[0].Name) != "internal-gw" {
		t.Errorf("HTTPRoute parent not updated: %s", http.Spec.ParentRefs[0].Name)
	}
	tcp := &gatewayv1alpha2.TCPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-dns-tcp", Namespace: "default"}, tcp); err != nil {
		t.Fatal(err)
	}
	if string(tcp.Spec.ParentRefs[0].Name) != "internal-gw" {
		t.Errorf("TCPRoute parent not updated: %s", tcp.Spec.ParentRefs[0].Name)
	}
}

func TestReconcileRoutesDeletesWhenDisabled(t *testing.T) {
	s := gatewayExposedServer()
	r, c := newRoutesReconciler(t, s)
	ctx := context.Background()
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	// API gateway switched off, DNS exposure moves to loadBalancer
	s.Spec.API.Gateway = nil
	s.Spec.DNS.Exposure = dnsv1alpha1.DNSExposureLoadBalancer
	if err := r.reconcileRoutes(ctx, s); err != nil {
		t.Fatal(err)
	}

	for name, obj := range map[string]client.Object{
		"test-dns-tcp":  &gatewayv1alpha2.TCPRoute{},
		"test-dns-udp":  &gatewayv1alpha2.UDPRoute{},
		"test-api-http": &gatewayv1.HTTPRoute{},
	} {
		err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, obj)
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s should be deleted, got err=%v", name, err)
		}
	}
}

func TestReconcileRoutesNoOpWhenNeverEnabled(t *testing.T) {
	// loadBalancer server that never had routes: repeated reconciles must
	// stay error-free (steady state is Get→NotFound, no DELETE issued).
	s := gatewayExposedServer()
	s.Spec.DNS.Exposure = dnsv1alpha1.DNSExposureLoadBalancer
	s.Spec.DNS.Gateway = nil
	s.Spec.API.Gateway = nil
	r, _ := newRoutesReconciler(t, s)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := r.reconcileRoutes(ctx, s); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
}

func TestValidateSpecAPIGateway(t *testing.T) {
	s := gatewayExposedServer()
	// validateSpec checks backend.type first — satisfy it so the
	// api.gateway checks under test are actually reached.
	s.Spec.Backend.Type = dnsv1alpha1.BackendPostgres

	s.Spec.API.Gateway.ParentRefs = nil
	if msg := validateSpec(s); !strings.Contains(msg, "api.gateway") {
		t.Errorf("empty parentRefs must fail validation, got %q", msg)
	}

	s.Spec.API.Gateway.ParentRefs = []dnsv1alpha1.APIGatewayParentRef{{}}
	if msg := validateSpec(s); !strings.Contains(msg, "name is required") {
		t.Errorf("nameless parentRef must fail validation, got %q", msg)
	}

	s.Spec.API.Gateway.ParentRefs = []dnsv1alpha1.APIGatewayParentRef{{Name: "eg"}}
	if msg := validateSpec(s); msg != "" {
		t.Errorf("valid api.gateway rejected: %q", msg)
	}
}
