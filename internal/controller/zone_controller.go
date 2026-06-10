package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/pdnsclient"
)

const (
	zoneFinalizer = "dns.aetherplatform.cloud/zone-protection"

	// zoneServerIndex indexes Zones by "<ns>/<name>" of their serverRef so
	// PowerDNSServer events fan out to dependent zones.
	zoneServerIndex = "spec.serverRef"

	// resyncInterval re-runs Ready zones/rrsets to correct out-of-band
	// drift on DECLARED fields only — patch-only by design, undeclared
	// records are never touched.
	resyncInterval = 5 * time.Minute
)

// ZoneReconciler reconciles Zone resources against the PowerDNS HTTP API.
// Unlike the server controller this is not a phase machine: every pass
// converges the whole zone from scratch; status.phase is informational.
type ZoneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=zones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=zones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=zones/finalizers,verbs=update

func (r *ZoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	zone := &dnsv1alpha1.Zone{}
	if err := r.Get(ctx, req.NamespacedName, zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !zone.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, zone)
	}
	if !controllerutil.ContainsFinalizer(zone, zoneFinalizer) {
		controllerutil.AddFinalizer(zone, zoneFinalizer)
		if err := r.Update(ctx, zone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Cross-field check CEL can't express against zone semantics:
	// Secondary content is replicated pre-signed; we can't sign it here.
	if zone.Spec.Kind == dnsv1alpha1.ZoneKindSecondary && zone.Spec.DNSSEC.Enabled {
		return r.markFailed(ctx, zone, "dnssec cannot be enabled on Secondary zones — sign at the primary")
	}

	server := &dnsv1alpha1.PowerDNSServer{}
	if err := r.Get(ctx, serverKeyFor(zone), server); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markNotReady(ctx, zone, "ServerNotFound",
				fmt.Sprintf("PowerDNSServer %s not found", serverKeyFor(zone)), requeueLong)
		}
		return ctrl.Result{}, err
	}
	if !namespaceAllowed(server, zone.Namespace) {
		return r.markNotReady(ctx, zone, "NamespaceNotAllowed",
			fmt.Sprintf("namespace %q is not allowed by %s/%s spec.zoneManagement.allowedNamespaces",
				zone.Namespace, server.Namespace, server.Name), requeueLong)
	}
	if server.Status.Phase != dnsv1alpha1.PhaseReady {
		return r.markNotReady(ctx, zone, "ServerNotReady", "PowerDNSServer is not Ready yet", requeueShort)
	}

	pc, err := pdnsClientFor(ctx, r.Client, server)
	if err != nil {
		return r.markNotReady(ctx, zone, "APIUnreachable", err.Error(), requeueLong)
	}
	return r.reconcileZone(ctx, zone, pc)
}

func (r *ZoneReconciler) reconcileZone(ctx context.Context, zone *dnsv1alpha1.Zone, pc *pdnsclient.Client) (ctrl.Result, error) {
	pz, err := pc.GetZone(ctx, zone.Spec.ZoneName)
	if errors.Is(err, pdnsclient.ErrNotFound) {
		pz, err = r.createZone(ctx, zone, pc)
	}
	if err != nil {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionRegistered,
			metav1.ConditionFalse, apiReason(err), err.Error())
		return r.markNotReady(ctx, zone, apiReason(err), err.Error(), requeueLong)
	}
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionRegistered,
		metav1.ConditionTrue, "ZoneExists", "zone registered in PowerDNS")

	// kind/masters drift (the only zone metadata we own post-create).
	wantKind := zone.Spec.Kind
	if wantKind == "" {
		wantKind = dnsv1alpha1.ZoneKindNative
	}
	if pz.Kind != wantKind || !slices.Equal(pz.Masters, zone.Spec.Masters) {
		if uerr := pc.UpdateZone(ctx, zone.Spec.ZoneName, pdnsclient.ZoneUpdate{
			Kind: wantKind, Masters: zone.Spec.Masters,
		}); uerr != nil {
			return r.markNotReady(ctx, zone, apiReason(uerr), uerr.Error(), requeueLong)
		}
	}

	ds, derr := r.ensureDNSSEC(ctx, zone, pc)
	if derr != nil {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionDNSSECReady,
			metav1.ConditionFalse, apiReason(derr), derr.Error())
		return r.markNotReady(ctx, zone, apiReason(derr), derr.Error(), requeueLong)
	}
	zone.Status.DSRecords = ds
	if zone.Spec.DNSSEC.Enabled {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionDNSSECReady,
			metav1.ConditionTrue, "Secured", "zone is signed; lodge status.dsRecords at the registrar")
	} else {
		setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionDNSSECReady,
			metav1.ConditionFalse, "Disabled", "dnssec not enabled")
	}

	zone.Status.Phase = dnsv1alpha1.ZonePhaseReady
	zone.Status.Serial = pz.Serial
	zone.Status.ObservedGeneration = zone.Generation
	zone.Status.FailureMessage = ""
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, zone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// createZone registers the zone, seeding NS and (optionally) SOA exactly
// ONCE. After creation the apex NS/SOA are ordinary records owned by
// whoever manages them — RRSet CRs or the HTTP API. Create-only by design.
func (r *ZoneReconciler) createZone(ctx context.Context, zone *dnsv1alpha1.Zone, pc *pdnsclient.Client) (*pdnsclient.Zone, error) {
	kind := zone.Spec.Kind
	if kind == "" {
		kind = dnsv1alpha1.ZoneKindNative
	}
	ns := zone.Spec.Nameservers
	if ns == nil {
		ns = []string{} // PowerDNS requires the field to be present
	}
	created, err := pc.CreateZone(ctx, &pdnsclient.Zone{
		Name:        zone.Spec.ZoneName,
		Kind:        kind,
		Masters:     zone.Spec.Masters,
		Nameservers: ns,
	})
	if err != nil {
		return nil, err
	}

	if zone.Spec.SOA != nil && kind != dnsv1alpha1.ZoneKindSecondary {
		primary := zone.Spec.ZoneName
		if len(zone.Spec.Nameservers) > 0 {
			primary = zone.Spec.Nameservers[0]
		}
		ttl := int32(3600)
		if zone.Spec.SOA.TTL != nil {
			ttl = *zone.Spec.SOA.TTL
		}
		// serial 0: PowerDNS bumps it on every API change.
		soa := pdnsclient.RRSet{
			Name: zone.Spec.ZoneName, Type: "SOA", TTL: ttl, ChangeType: "REPLACE",
			Records: []pdnsclient.Record{{
				Content: fmt.Sprintf("%s %s 0 10800 3600 604800 3600", primary, zone.Spec.SOA.Hostmaster),
			}},
		}
		if err := pc.PatchRRSets(ctx, zone.Spec.ZoneName, []pdnsclient.RRSet{soa}); err != nil {
			// Compensate: SOA seeding is create-only, so a half-created
			// zone would silently keep the default SOA forever. Delete it
			// so the next pass recreates and reseeds atomically-by-retry.
			if derr := pc.DeleteZone(ctx, zone.Spec.ZoneName); derr != nil {
				r.event(zone, corev1.EventTypeWarning, "SOASeedFailed",
					"zone created but SOA seed and compensating delete both failed; zone keeps the PowerDNS default SOA: "+err.Error())
				return nil, fmt.Errorf("seed SOA: %w", err)
			}
			return nil, fmt.Errorf("seed SOA (zone rolled back, will retry): %w", err)
		}
	}
	r.event(zone, corev1.EventTypeNormal, "ZoneCreated", "registered "+zone.Spec.ZoneName+" in PowerDNS")
	return created, nil
}

// ensureDNSSEC converges key state and returns the DS records of active
// keys. Disable DEACTIVATES keys instead of deleting them, so a later
// re-enable reuses the same key and any DS lodged at the registrar stays
// valid.
func (r *ZoneReconciler) ensureDNSSEC(ctx context.Context, zone *dnsv1alpha1.Zone, pc *pdnsclient.Client) ([]string, error) {
	keys, err := pc.ListCryptokeys(ctx, zone.Spec.ZoneName)
	if err != nil {
		return nil, err
	}

	if !zone.Spec.DNSSEC.Enabled {
		for _, k := range keys {
			if !k.Active {
				continue
			}
			k.Active = false
			if err := pc.UpdateCryptokey(ctx, zone.Spec.ZoneName, k); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if !slices.ContainsFunc(keys, func(k pdnsclient.Cryptokey) bool { return k.Active }) {
		if len(keys) > 0 {
			k := keys[0]
			k.Active = true
			if err := pc.UpdateCryptokey(ctx, zone.Spec.ZoneName, k); err != nil {
				return nil, err
			}
		} else {
			if _, err := pc.CreateCryptokey(ctx, zone.Spec.ZoneName, pdnsclient.Cryptokey{KeyType: "csk", Active: true}); err != nil {
				return nil, err
			}
		}
		if err := pc.RectifyZone(ctx, zone.Spec.ZoneName); err != nil {
			return nil, err
		}
		if keys, err = pc.ListCryptokeys(ctx, zone.Spec.ZoneName); err != nil {
			return nil, err
		}
	}

	var ds []string
	for _, k := range keys {
		if k.Active {
			ds = append(ds, k.DS...)
		}
	}
	return ds, nil
}

// reconcileDelete removes the zone from PowerDNS (unless Orphan), with
// wedge-proofing: when the server CR is gone, deleting, or Failed, the
// finalizer releases WITHOUT an API call — the zone data dies with the
// server's Postgres anyway, and blocking deletion on an unreachable
// backend is how teardowns wedge.
func (r *ZoneReconciler) reconcileDelete(ctx context.Context, zone *dnsv1alpha1.Zone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(zone, zoneFinalizer) {
		return ctrl.Result{}, nil
	}
	if zone.Spec.DeletionPolicy != dnsv1alpha1.DeletionPolicyOrphan {
		server := &dnsv1alpha1.PowerDNSServer{}
		err := r.Get(ctx, serverKeyFor(zone), server)
		switch {
		case apierrors.IsNotFound(err),
			err == nil && !server.DeletionTimestamp.IsZero(),
			err == nil && server.Status.Phase == dnsv1alpha1.PhaseFailed:
			// release without touching the API
		case err != nil:
			return ctrl.Result{}, err
		default:
			pc, cerr := pdnsClientFor(ctx, r.Client, server)
			if cerr != nil {
				return ctrl.Result{}, cerr
			}
			if derr := pc.DeleteZone(ctx, zone.Spec.ZoneName); derr != nil {
				return ctrl.Result{}, derr
			}
			r.event(zone, corev1.EventTypeNormal, "ZoneDeleted", "removed "+zone.Spec.ZoneName+" from PowerDNS")
		}
	}
	controllerutil.RemoveFinalizer(zone, zoneFinalizer)
	return ctrl.Result{}, r.Update(ctx, zone)
}

func (r *ZoneReconciler) markNotReady(ctx context.Context, zone *dnsv1alpha1.Zone, reason, msg string, after time.Duration) (ctrl.Result, error) {
	zone.Status.Phase = dnsv1alpha1.ZonePhasePending
	zone.Status.FailureMessage = msg
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, zone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// markFailed flags invalid specs. Not terminal machinery — the reconciler
// re-validates from scratch on every pass, so fixing the spec recovers.
func (r *ZoneReconciler) markFailed(ctx context.Context, zone *dnsv1alpha1.Zone, msg string) (ctrl.Result, error) {
	zone.Status.Phase = dnsv1alpha1.ZonePhaseFailed
	zone.Status.FailureMessage = msg
	setCondOn(&zone.Status.Conditions, zone.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, "InvalidSpec", msg)
	if err := r.Status().Update(ctx, zone); err != nil {
		return ctrl.Result{}, err
	}
	r.event(zone, corev1.EventTypeWarning, "InvalidSpec", msg)
	return ctrl.Result{}, nil
}

func (r *ZoneReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(obj, eventType, reason, message)
}

// serverKeyFor resolves the serverRef, defaulting to the zone's namespace.
func serverKeyFor(zone *dnsv1alpha1.Zone) types.NamespacedName {
	ns := zone.Spec.ServerRef.Namespace
	if ns == "" {
		ns = zone.Namespace
	}
	return types.NamespacedName{Namespace: ns, Name: zone.Spec.ServerRef.Name}
}

// apiReason maps client errors to a condition reason: transport-level
// failures surface as APIUnreachable (usually NetworkPolicy or a dying
// server), everything else as APIError.
func apiReason(err error) string {
	if errors.Is(err, pdnsclient.ErrUnreachable) {
		return "APIUnreachable"
	}
	return "APIError"
}

func (r *ZoneReconciler) zonesForServer(ctx context.Context, obj client.Object) []reconcile.Request {
	var zones dnsv1alpha1.ZoneList
	if err := r.List(ctx, &zones, client.MatchingFields{
		zoneServerIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list zones for server", "server", client.ObjectKeyFromObject(obj))
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(zones.Items))
	for i := range zones.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&zones.Items[i])})
	}
	return reqs
}

// SetupWithManager registers the index and the PowerDNSServer watch so a
// server becoming Ready (or changing its allow-list) wakes its zones.
func (r *ZoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dnsv1alpha1.Zone{}, zoneServerIndex,
		func(o client.Object) []string {
			z := o.(*dnsv1alpha1.Zone)
			return []string{refKey(z.Spec.ServerRef, z.GetNamespace())}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.Zone{}).
		Watches(&dnsv1alpha1.PowerDNSServer{}, handler.EnqueueRequestsFromMapFunc(r.zonesForServer)).
		Complete(r)
}
