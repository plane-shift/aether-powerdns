package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/manifests"
)

func extraSettingsScheme(t *testing.T) *runtime.Scheme {
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

// extraSettingsServer is a minimal, spec-valid PowerDNSServer carrying the
// finalizer (so Reconcile dispatches straight to the phase handler).
func extraSettingsServer(phase string, extra map[string]string) *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "default", UID: "test-uid",
			Finalizers: []string{finalizer},
		},
		Spec: dnsv1alpha1.PowerDNSServerSpec{
			Backend:       dnsv1alpha1.BackendSpec{Type: dnsv1alpha1.BackendPostgres},
			ExtraSettings: extra,
		},
		Status: dnsv1alpha1.PowerDNSServerStatus{Phase: phase},
	}
}

var extraSettingsKey = types.NamespacedName{Name: "test", Namespace: "default"}

// drainEvents empties a FakeRecorder's buffered channel.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// The create path must refuse a reserved key outright — validateSpec is
// what turns it into a terminal Failed phase.
func TestValidateSpecRejectsReservedExtraSetting(t *testing.T) {
	msg := validateSpec(extraSettingsServer("", map[string]string{"gpgsql-host": "attacker.example"}))
	if msg == "" {
		t.Fatal("a reserved extraSettings key must fail spec validation")
	}
	if !strings.HasPrefix(msg, "invalid extraSettings: ") {
		t.Errorf("message should be prefixed so the operator surface is obvious, got %q", msg)
	}
	if !strings.Contains(msg, "gpgsql-host") {
		t.Errorf("message must name the offending key, got %q", msg)
	}

	if msg := validateSpec(extraSettingsServer("", map[string]string{"primary": "yes"})); msg != "" {
		t.Errorf("a legitimate extraSetting must pass validateSpec, got %q", msg)
	}
}

// Phase "" → phasePending: a bad extraSetting on CREATE is terminal, so the
// server never provisions with a config it cannot honour.
func TestPhasePendingFailsOnReservedExtraSetting(t *testing.T) {
	scheme := extraSettingsScheme(t)
	s := extraSettingsServer("", map[string]string{"launch": "bind"})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.PowerDNSServer{}).
		WithObjects(s).Build()
	rec := record.NewFakeRecorder(16)
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme, Recorder: rec}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: extraSettingsKey}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(context.Background(), extraSettingsKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != dnsv1alpha1.PhaseFailed {
		t.Errorf("phase = %q, want Failed for a reserved extraSettings key", got.Status.Phase)
	}
	if !strings.Contains(got.Status.FailureMessage, "invalid extraSettings") {
		t.Errorf("failureMessage should explain the rejection, got %q", got.Status.FailureMessage)
	}
}

// A spec edited AFTER Ready never re-enters phasePending. The Ready loop
// must therefore re-check: drop the rejected entry, keep the valid one,
// SAY SO via an event — and specifically NOT go terminal, which would take
// a live DNS server's reconcile loop offline over a config typo.
func TestPhaseReadyDropsInvalidExtraSettingsWithoutFailing(t *testing.T) {
	scheme := extraSettingsScheme(t)
	s := extraSettingsServer(dnsv1alpha1.PhaseReady, map[string]string{
		"primary": "yes",
		"launch":  "bind",
	})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.PowerDNSServer{}).
		WithObjects(s).Build()
	rec := record.NewFakeRecorder(16)
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme, Recorder: rec}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: extraSettingsKey}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(ctx, extraSettingsKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == dnsv1alpha1.PhaseFailed {
		t.Errorf("a rejected extraSetting on a LIVE server must not go terminal; failureMessage=%q", got.Status.FailureMessage)
	}
	if got.Status.Phase != dnsv1alpha1.PhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: manifests.NameSet(s).ConfigMap, Namespace: "default",
	}, cm); err != nil {
		t.Fatalf("config map: %v", err)
	}
	conf := cm.Data["pdns.conf"]
	if !strings.Contains(conf, "primary=yes") {
		t.Errorf("the valid entry must survive — dropping the whole map would silently stop AXFR:\n%s", conf)
	}
	if strings.Contains(conf, "launch=bind") {
		t.Errorf("the rejected entry must never reach the live pdns.conf:\n%s", conf)
	}

	events := drainEvents(rec)
	found := false
	for _, e := range events {
		if strings.Contains(e, "ExtraSettingsRejected") {
			found = true
			if !strings.Contains(e, corev1.EventTypeWarning) {
				t.Errorf("ExtraSettingsRejected must be a Warning, got %q", e)
			}
			if !strings.Contains(e, "launch") {
				t.Errorf("the event must name the dropped key, got %q", e)
			}
		}
	}
	if !found {
		t.Errorf("a silently dropped setting is the failure mode this guards: expected an ExtraSettingsRejected event, got %v", events)
	}
}

// PowerDNS does not reload pdns.conf at runtime, so an extraSettings edit
// only takes effect if the config-hash annotation moves and rolls the pods
// — and it must NOT move when nothing changed.
func TestExtraSettingsEditRollsPods(t *testing.T) {
	scheme := extraSettingsScheme(t)
	s := extraSettingsServer(dnsv1alpha1.PhaseReady, map[string]string{
		"primary":        "yes",
		"allow-axfr-ips": "104.218.120.85/32",
	})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.PowerDNSServer{}).
		WithObjects(s).Build()
	r := &PowerDNSServerReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(16)}
	ctx := context.Background()

	hash := func(t *testing.T) string {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: extraSettingsKey}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		dep := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{
			Name: manifests.NameSet(s).Deployment, Namespace: "default",
		}, dep); err != nil {
			t.Fatalf("deployment: %v", err)
		}
		return dep.Spec.Template.Annotations[manifests.ConfigHashAnnotation]
	}

	before := hash(t)
	if before == "" {
		t.Fatal("config-hash annotation must be set on the pod template")
	}

	// Repoint the AXFR peer — a value-only edit, the shape most likely to
	// be dropped by an identity check that only looks at keys.
	live := &dnsv1alpha1.PowerDNSServer{}
	if err := c.Get(ctx, extraSettingsKey, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.ExtraSettings["allow-axfr-ips"] = "198.51.100.7/32"
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}

	after := hash(t)
	if after == before {
		t.Errorf("an extraSettings edit must move the config hash and roll the pods (pdns has no runtime reload); before=%q after=%q", before, after)
	}

	// Steady state: re-reconciling an unchanged spec must not roll pods.
	if steady := hash(t); steady != after {
		t.Errorf("steady-state reconcile must not move the config hash: %q -> %q", after, steady)
	}
}
