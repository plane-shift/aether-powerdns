package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/manifests"
)

// dnsdistBackendIndex indexes DNSDists by each backendRef "<ns>/<name>"
// so PowerDNSServer events fan out to dependent tiers.
const dnsdistBackendIndex = "spec.backendRefs"

// DNSDistReconciler converges DNSDist tiers. Single-pass (no phase
// machine) and NO finalizer: a DNSDist owns no external state — every
// child is a Kubernetes object reaped by GC through owner references.
type DNSDistReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=dnsdists,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=dnsdists/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=dnsdists/finalizers,verbs=update

func (r *DNSDistReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	d := &dnsv1alpha1.DNSDist{}
	if err := r.Get(ctx, req.NamespacedName, d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !d.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // owner-ref GC handles everything
	}

	if msg := validateDNSDist(d); msg != "" {
		return r.markDNSDistFailed(ctx, d, msg)
	}

	allReady, reason, msg, err := r.backendsReady(ctx, d)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allReady {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionBackendsReady,
			metav1.ConditionFalse, reason, msg)
		return r.markDNSDistNotReady(ctx, d, reason, msg, requeueShort)
	}
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionBackendsReady,
		metav1.ConditionTrue, "BackendsReady", "all backends Ready")

	// Converge children. ConfigMap/Service/PDB writes are steady-state no-ops; the Deployment converge rewrites spec each pass (server updateDeployment parity — the apiserver no-ops identical canonical updates).
	cm := manifests.DNSDistConfigMap(d)
	if err := upsertOwned(ctx, r.Client, r.Scheme, d, &corev1.ConfigMap{}, cm.Name, cm.Namespace, func(live *corev1.ConfigMap) {
		live.Labels = cm.Labels
		live.Data = cm.Data
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure configmap: %w", err)
	}

	hash := sha256.Sum256([]byte(cm.Data["dnsdist.conf"] +
		d.Spec.TLS.DoT.CertificateSecretRef.Name + d.Spec.TLS.DoH.CertificateSecretRef.Name))
	dep := manifests.DNSDistDeployment(d, hex.EncodeToString(hash[:16]))
	if err := upsertOwned(ctx, r.Client, r.Scheme, d, &appsv1.Deployment{}, dep.Name, dep.Namespace, func(live *appsv1.Deployment) {
		live.Labels = dep.Labels
		live.Spec = dep.Spec
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure deployment: %w", err)
	}

	svc := manifests.DNSDistDNSService(d)
	if err := ctrl.SetControllerReference(d, svc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner ref on dns service: %w", err)
	}
	if err := updateService(ctx, r.Client, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure dns service: %w", err)
	}

	if err := r.reconcileDNSDistAdditionalServices(ctx, d); err != nil {
		return ctrl.Result{}, err
	}

	names := manifests.DNSDistNameSet(d)
	if pdb := manifests.DNSDistPDB(d); pdb != nil {
		if err := upsertOwned(ctx, r.Client, r.Scheme, d, &policyv1.PodDisruptionBudget{}, pdb.Name, pdb.Namespace, func(live *policyv1.PodDisruptionBudget) {
			live.Labels = pdb.Labels
			live.Spec.MinAvailable = pdb.Spec.MinAvailable
			live.Spec.Selector = pdb.Spec.Selector
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure pdb: %w", err)
		}
	} else if err := deleteIfExists(ctx, r.Client, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: names.PDB, Namespace: d.Namespace},
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete pdb: %w", err)
	}

	if err := r.reconcileDNSDistRoutes(ctx, d); err != nil {
		return ctrl.Result{}, err
	}

	return r.refreshDNSDistStatus(ctx, d)
}

// validateDNSDist is the controller-side validation pass (cross-field
// checks CEL doesn't cover).
func validateDNSDist(d *dnsv1alpha1.DNSDist) string {
	seen := make(map[string]struct{}, len(d.Spec.BackendRefs))
	for i, ref := range d.Spec.BackendRefs {
		if ref.Name == "" {
			return fmt.Sprintf("backendRefs[%d].name is required", i)
		}
		if ref.Namespace != "" {
			return fmt.Sprintf("backendRefs[%d].namespace must be empty — cross-namespace backends are not supported", i)
		}
		if _, dup := seen[ref.Name]; dup {
			return fmt.Sprintf("backendRefs[%d].name %q is a duplicate", i, ref.Name)
		}
		seen[ref.Name] = struct{}{}
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	if pdb := d.Spec.PodDisruptionBudget; pdb != nil && pdb.MinAvailable != nil &&
		replicas > 1 && *pdb.MinAvailable >= replicas {
		return fmt.Sprintf("podDisruptionBudget.minAvailable (%d) must be < replicas (%d)", *pdb.MinAvailable, replicas)
	}
	if d.Spec.TLS.DoT.Enabled && d.Spec.TLS.DoT.CertificateSecretRef.Name == "" {
		return "tls.dot.certificateSecretRef.name is required when dot is enabled"
	}
	if d.Spec.TLS.DoH.Enabled && d.Spec.TLS.DoH.CertificateSecretRef.Name == "" {
		return "tls.doh.certificateSecretRef.name is required when doh is enabled"
	}
	if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureGateway &&
		(d.Spec.DNS.Gateway == nil || len(d.Spec.DNS.Gateway.ParentRefs) == 0) {
		return "dns.exposure=gateway requires dns.gateway.parentRefs (at least one)"
	}
	return ""
}

func (r *DNSDistReconciler) backendsReady(ctx context.Context, d *dnsv1alpha1.DNSDist) (bool, string, string, error) {
	for _, ref := range d.Spec.BackendRefs {
		s := &dnsv1alpha1.PowerDNSServer{}
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: d.Namespace}, s)
		if apierrors.IsNotFound(err) {
			return false, "BackendNotFound", fmt.Sprintf("PowerDNSServer %s/%s not found", d.Namespace, ref.Name), nil
		}
		if err != nil {
			return false, "", "", err
		}
		if s.Status.Phase != dnsv1alpha1.PhaseReady {
			return false, "BackendNotReady", fmt.Sprintf("PowerDNSServer %s is %s", ref.Name, s.Status.Phase), nil
		}
	}
	return true, "", "", nil
}

// reconcileDNSDistAdditionalServices creates or updates each Service
// declared under spec.dns.loadBalancer.additionalServices, then
// garbage-collects any Service we previously created that's no longer
// desired. The extra app.kubernetes.io/name=dnsdist label differentiates
// from the server's additional services GC so they can't clobber each
// other when metadata.name is shared.
func (r *DNSDistReconciler) reconcileDNSDistAdditionalServices(ctx context.Context, d *dnsv1alpha1.DNSDist) error {
	desired := manifests.DNSDistAdditionalDNSServices(d)
	desiredNames := make(map[string]struct{}, len(desired))
	for _, svc := range desired {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels["dns.aetherplatform.cloud/role"] = "additional-dns"
		if err := ctrl.SetControllerReference(d, svc, r.Scheme); err != nil {
			return err
		}
		if err := updateService(ctx, r.Client, svc); err != nil {
			return err
		}
		desiredNames[svc.Name] = struct{}{}
	}

	existing := &corev1.ServiceList{}
	if err := r.List(ctx, existing,
		client.InNamespace(d.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/managed-by":  "aether-powerdns",
			"app.kubernetes.io/instance":    d.Name,
			"app.kubernetes.io/name":        "dnsdist",
			"dns.aetherplatform.cloud/role": "additional-dns",
		},
	); err != nil {
		return err
	}
	for i := range existing.Items {
		svc := &existing.Items[i]
		if _, keep := desiredNames[svc.Name]; keep {
			continue
		}
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileDNSDistRoutes mirrors the server-side reconcileRoutes
// semantics for the tier's TCP/UDP routes (no HTTPRoute — dnsdist has no
// admin HTTP API surface here).
func (r *DNSDistReconciler) reconcileDNSDistRoutes(ctx context.Context, d *dnsv1alpha1.DNSDist) error {
	names := manifests.DNSDistNameSet(d)
	if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureGateway {
		desiredTCP := manifests.DNSDistTCPRoute(d)
		tcp := &gatewayv1alpha2.TCPRoute{ObjectMeta: metav1.ObjectMeta{Name: desiredTCP.Name, Namespace: desiredTCP.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, tcp, func() error {
			tcp.Labels = desiredTCP.Labels
			tcp.Spec = desiredTCP.Spec
			return ctrl.SetControllerReference(d, tcp, r.Scheme)
		}); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("ensure tcproute: TCPRoute CRD not installed (Gateway API experimental channel): %w", err)
			}
			return fmt.Errorf("ensure tcproute: %w", err)
		}
		desiredUDP := manifests.DNSDistUDPRoute(d)
		udp := &gatewayv1alpha2.UDPRoute{ObjectMeta: metav1.ObjectMeta{Name: desiredUDP.Name, Namespace: desiredUDP.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, udp, func() error {
			udp.Labels = desiredUDP.Labels
			udp.Spec = desiredUDP.Spec
			return ctrl.SetControllerReference(d, udp, r.Scheme)
		}); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("ensure udproute: UDPRoute CRD not installed (Gateway API experimental channel): %w", err)
			}
			return fmt.Errorf("ensure udproute: %w", err)
		}
		return nil
	}
	if err := deleteIfExists(ctx, r.Client, &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: names.TCPRoute, Namespace: d.Namespace},
	}); err != nil {
		return fmt.Errorf("delete tcproute: %w", err)
	}
	if err := deleteIfExists(ctx, r.Client, &gatewayv1alpha2.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: names.UDPRoute, Namespace: d.Namespace},
	}); err != nil {
		return fmt.Errorf("delete udproute: %w", err)
	}
	return nil
}

func (r *DNSDistReconciler) refreshDNSDistStatus(ctx context.Context, d *dnsv1alpha1.DNSDist) (ctrl.Result, error) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: manifests.DNSDistNameSet(d).Deployment, Namespace: d.Namespace}, dep); err != nil {
		return ctrl.Result{}, err
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	d.Status.DesiredReplicas = desired
	d.Status.ReadyReplicas = dep.Status.ReadyReplicas
	available := dep.Status.AvailableReplicas == desired && desired > 0
	if available {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionAvailable,
			metav1.ConditionTrue, "PodsAvailable", "")
	} else {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionAvailable,
			metav1.ConditionFalse, "PodsUnavailable",
			fmt.Sprintf("%d/%d replicas available", dep.Status.AvailableReplicas, desired))
	}

	// dnsEndpoint mirrors live exposure state (the server's v0.2.3 rule:
	// status reflects reality, never the wish).
	endpoint := ""
	switch d.Spec.DNS.Exposure {
	case dnsv1alpha1.DNSExposureGateway:
		if d.Spec.DNS.Gateway != nil {
			parents := make([]string, 0, len(d.Spec.DNS.Gateway.ParentRefs))
			for _, p := range d.Spec.DNS.Gateway.ParentRefs {
				ns := p.Namespace
				if ns == "" {
					ns = d.Namespace
				}
				parents = append(parents, ns+"/"+p.Name)
			}
			endpoint = "gateway:" + strings.Join(parents, ",")
		}
	default:
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: manifests.DNSDistNameSet(d).DNSService, Namespace: d.Namespace}, svc); err == nil {
			if d.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureLoadBalancer {
				for _, ing := range svc.Status.LoadBalancer.Ingress {
					if ing.IP != "" {
						endpoint = ing.IP + ":53"
						break
					}
					if ing.Hostname != "" {
						endpoint = ing.Hostname + ":53"
						break
					}
				}
			} else {
				endpoint = svc.Spec.ClusterIP + ":53"
			}
		}
	}
	d.Status.DNSEndpoint = endpoint

	d.Status.Phase = dnsv1alpha1.ZonePhaseReady
	d.Status.FailureMessage = ""
	d.Status.ObservedGeneration = d.Generation
	if available {
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
			metav1.ConditionTrue, "Reconciled", "")
	} else {
		degradedMsg := fmt.Sprintf("%d/%d replicas available", dep.Status.AvailableReplicas, desired)
		setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
			metav1.ConditionFalse, "Degraded", degradedMsg)
	}
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

func (r *DNSDistReconciler) markDNSDistNotReady(ctx context.Context, d *dnsv1alpha1.DNSDist, reason, msg string, after time.Duration) (ctrl.Result, error) {
	d.Status.Phase = dnsv1alpha1.ZonePhasePending
	d.Status.FailureMessage = msg
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *DNSDistReconciler) markDNSDistFailed(ctx context.Context, d *dnsv1alpha1.DNSDist, msg string) (ctrl.Result, error) {
	d.Status.Phase = dnsv1alpha1.ZonePhaseFailed
	d.Status.FailureMessage = msg
	setCondOn(&d.Status.Conditions, d.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, "InvalidSpec", msg)
	if err := r.Status().Update(ctx, d); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(d, corev1.EventTypeWarning, "InvalidSpec", msg)
	}
	return ctrl.Result{}, nil
}

func (r *DNSDistReconciler) dnsdistsForServer(ctx context.Context, obj client.Object) []reconcile.Request {
	var list dnsv1alpha1.DNSDistList
	if err := r.List(ctx, &list, client.MatchingFields{
		dnsdistBackendIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list dnsdists for server", "server", client.ObjectKeyFromObject(obj))
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

func (r *DNSDistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dnsv1alpha1.DNSDist{}, dnsdistBackendIndex,
		func(o client.Object) []string {
			d := o.(*dnsv1alpha1.DNSDist)
			keys := make([]string, 0, len(d.Spec.BackendRefs))
			for _, ref := range d.Spec.BackendRefs {
				keys = append(keys, refKey(ref, d.GetNamespace()))
			}
			return keys
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSDist{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// Route kinds deliberately NOT in Owns() (Gateway API optional).
		Watches(&dnsv1alpha1.PowerDNSServer{}, handler.EnqueueRequestsFromMapFunc(r.dnsdistsForServer)).
		Complete(r)
}

// upsertOwned is a generic CreateOrUpdate helper that sets the owner
// reference on blank before converging it against the cluster. The blank
// parameter must be an empty object of the correct type — CreateOrUpdate
// fills it from the cluster on update.
func upsertOwned[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, blank T, name, namespace string, mutate func(live T)) error {
	blank.SetName(name)
	blank.SetNamespace(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, c, blank, func() error {
		mutate(blank)
		return controllerutil.SetControllerReference(owner, blank, scheme)
	})
	return err
}
