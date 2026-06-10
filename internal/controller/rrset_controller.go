package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	rrsetFinalizer = "dns.aetherplatform.cloud/rrset-protection"

	// rrsetZoneIndex indexes RRSets by "<ns>/<name>" of their zoneRef so
	// Zone events fan out to dependent rrsets.
	rrsetZoneIndex = "spec.zoneRef"
)

// RRSetReconciler applies declared record sets to PowerDNS. Patch-only:
// the only rrsets it ever writes are the ones declared by RRSet
// resources; spec name/type/zoneRef are immutable (CEL) so reconcile is a
// stateless idempotent PATCH changetype=REPLACE.
type RRSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=rrsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=rrsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=rrsets/finalizers,verbs=update

func (r *RRSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rr := &dnsv1alpha1.RRSet{}
	if err := r.Get(ctx, req.NamespacedName, rr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !rr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, rr)
	}
	if !controllerutil.ContainsFinalizer(rr, rrsetFinalizer) {
		controllerutil.AddFinalizer(rr, rrsetFinalizer)
		if err := r.Update(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	zone := &dnsv1alpha1.Zone{}
	if err := r.Get(ctx, zoneKeyFor(rr), zone); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markNotReady(ctx, rr, "ZoneNotFound",
				fmt.Sprintf("Zone %s not found", zoneKeyFor(rr)), requeueLong)
		}
		return ctrl.Result{}, err
	}
	if !zone.DeletionTimestamp.IsZero() {
		return r.markNotReady(ctx, rr, "ZoneDeleting", "referenced Zone is being deleted", requeueLong)
	}
	if zone.Spec.Kind == dnsv1alpha1.ZoneKindSecondary {
		return r.markNotReady(ctx, rr, "SecondaryZone",
			"Secondary zone content is replicated from its primaries — manage records there", requeueLong)
	}
	if !nameInZone(rr.Spec.Name, zone.Spec.ZoneName) {
		return r.markNotReady(ctx, rr, "NameOutsideZone",
			fmt.Sprintf("%q is not within zone %q", rr.Spec.Name, zone.Spec.ZoneName), requeueLong)
	}

	server := &dnsv1alpha1.PowerDNSServer{}
	if err := r.Get(ctx, serverKeyFor(zone), server); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markNotReady(ctx, rr, "ServerNotFound",
				fmt.Sprintf("PowerDNSServer %s not found", serverKeyFor(zone)), requeueLong)
		}
		return ctrl.Result{}, err
	}
	if !namespaceAllowed(server, rr.Namespace) {
		return r.markNotReady(ctx, rr, "NamespaceNotAllowed",
			fmt.Sprintf("namespace %q is not allowed by %s/%s spec.zoneManagement.allowedNamespaces",
				rr.Namespace, server.Namespace, server.Name), requeueLong)
	}

	other, err := r.findConflict(ctx, rr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if other != "" {
		// Reject BOTH claimants — symmetric and stateless. requeueLong
		// doubles as the recovery poll once the other side is deleted.
		return r.markNotReady(ctx, rr, "Conflict",
			fmt.Sprintf("RRSet %s also declares %s %s in this zone — delete one", other, rr.Spec.Name, rr.Spec.Type),
			requeueLong)
	}

	if zone.Status.Phase != dnsv1alpha1.ZonePhaseReady {
		return r.markNotReady(ctx, rr, "ZoneNotReady", "Zone is not Ready yet", requeueShort)
	}

	pc, err := pdnsClientFor(ctx, r.Client, server)
	if err != nil {
		return r.markNotReady(ctx, rr, "APIUnreachable", err.Error(), requeueLong)
	}
	if err := pc.PatchRRSets(ctx, zone.Spec.ZoneName, []pdnsclient.RRSet{desiredRRSet(rr)}); err != nil {
		return r.markNotReady(ctx, rr, apiReason(err), err.Error(), requeueLong)
	}

	rr.Status.ObservedGeneration = rr.Generation
	rr.Status.FailureMessage = ""
	setCondOn(&rr.Status.Conditions, rr.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Applied", "")
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// reconcileDelete removes the rrset from PowerDNS (unless Orphan), with
// two escape hatches: (a) zone/server already gone or going — release
// without an API call, the data dies with them; (b) a surviving
// conflicting CR claims the same rrset — skip the API delete, the
// survivor re-applies on its next pass.
func (r *RRSetReconciler) reconcileDelete(ctx context.Context, rr *dnsv1alpha1.RRSet) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rr, rrsetFinalizer) {
		return ctrl.Result{}, nil
	}
	if rr.Spec.DeletionPolicy != dnsv1alpha1.DeletionPolicyOrphan {
		if err := r.deleteFromPowerDNS(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(rr, rrsetFinalizer)
	return ctrl.Result{}, r.Update(ctx, rr)
}

func (r *RRSetReconciler) deleteFromPowerDNS(ctx context.Context, rr *dnsv1alpha1.RRSet) error {
	zone := &dnsv1alpha1.Zone{}
	err := r.Get(ctx, zoneKeyFor(rr), zone)
	if apierrors.IsNotFound(err) || (err == nil && !zone.DeletionTimestamp.IsZero()) {
		return nil
	}
	if err != nil {
		return err
	}

	other, err := r.findConflict(ctx, rr)
	if err != nil {
		return err
	}
	if other != "" {
		return nil // survivor owns the rrset now
	}

	server := &dnsv1alpha1.PowerDNSServer{}
	err = r.Get(ctx, serverKeyFor(zone), server)
	if apierrors.IsNotFound(err) ||
		(err == nil && (!server.DeletionTimestamp.IsZero() || server.Status.Phase == dnsv1alpha1.PhaseFailed)) {
		return nil
	}
	if err != nil {
		return err
	}

	pc, err := pdnsClientFor(ctx, r.Client, server)
	if err != nil {
		return err
	}
	derr := pc.PatchRRSets(ctx, zone.Spec.ZoneName, []pdnsclient.RRSet{{
		Name: rr.Spec.Name, Type: rr.Spec.Type, ChangeType: "DELETE",
	}})
	if errors.Is(derr, pdnsclient.ErrNotFound) {
		return nil // zone already gone in PowerDNS
	}
	return derr
}

// findConflict returns "<ns>/<name>" of another live RRSet claiming the
// same (zone, name, type), or "".
func (r *RRSetReconciler) findConflict(ctx context.Context, rr *dnsv1alpha1.RRSet) (string, error) {
	var list dnsv1alpha1.RRSetList
	if err := r.List(ctx, &list, client.MatchingFields{
		rrsetZoneIndex: refKey(rr.Spec.ZoneRef, rr.Namespace),
	}); err != nil {
		return "", err
	}
	for i := range list.Items {
		o := &list.Items[i]
		// Skip self. Use namespace+name because the fake client leaves UID
		// empty, and a real cluster's UIDs are always unique — both forms
		// correctly identify the same object.
		if (o.Namespace == rr.Namespace && o.Name == rr.Name) || !o.DeletionTimestamp.IsZero() {
			continue
		}
		if o.Spec.Name == rr.Spec.Name && o.Spec.Type == rr.Spec.Type {
			return o.Namespace + "/" + o.Name, nil
		}
	}
	return "", nil
}

func (r *RRSetReconciler) markNotReady(ctx context.Context, rr *dnsv1alpha1.RRSet, reason, msg string, after time.Duration) (ctrl.Result, error) {
	rr.Status.FailureMessage = msg
	setCondOn(&rr.Status.Conditions, rr.Generation, dnsv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// desiredRRSet renders the PATCH payload for one RRSet resource.
func desiredRRSet(rr *dnsv1alpha1.RRSet) pdnsclient.RRSet {
	ttl := int32(3600)
	if rr.Spec.TTL != nil {
		ttl = *rr.Spec.TTL
	}
	recs := make([]pdnsclient.Record, 0, len(rr.Spec.Records))
	for _, c := range rr.Spec.Records {
		recs = append(recs, pdnsclient.Record{Content: c})
	}
	return pdnsclient.RRSet{
		Name: rr.Spec.Name, Type: rr.Spec.Type, TTL: ttl,
		ChangeType: "REPLACE", Records: recs,
	}
}

// nameInZone reports whether name equals the zone apex or sits beneath it.
func nameInZone(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// zoneKeyFor resolves the zoneRef, defaulting to the RRSet's namespace.
func zoneKeyFor(rr *dnsv1alpha1.RRSet) types.NamespacedName {
	ns := rr.Spec.ZoneRef.Namespace
	if ns == "" {
		ns = rr.Namespace
	}
	return types.NamespacedName{Namespace: ns, Name: rr.Spec.ZoneRef.Name}
}

func (r *RRSetReconciler) rrsetsForZone(ctx context.Context, obj client.Object) []reconcile.Request {
	var list dnsv1alpha1.RRSetList
	if err := r.List(ctx, &list, client.MatchingFields{
		rrsetZoneIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list rrsets for zone", "zone", client.ObjectKeyFromObject(obj))
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// SetupWithManager registers the index and the Zone watch so a zone going
// Ready wakes its records.
func (r *RRSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dnsv1alpha1.RRSet{}, rrsetZoneIndex,
		func(o client.Object) []string {
			rr := o.(*dnsv1alpha1.RRSet)
			return []string{refKey(rr.Spec.ZoneRef, rr.GetNamespace())}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.RRSet{}).
		Watches(&dnsv1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.rrsetsForZone)).
		Complete(r)
}
