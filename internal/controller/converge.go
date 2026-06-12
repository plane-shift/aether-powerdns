package controller

// converge.go holds shared Service/ConfigMap convergence helpers used by
// both PowerDNSServerReconciler and DNSDistReconciler. Each function is a
// standalone free function accepting a client.Client so neither reconciler
// needs to import the other. PowerDNSServerReconciler thin-wrappers kept
// below for backward-compatible call sites.

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// updateService converges a Service without churning it (issue #13):
// apiserver-assigned NodePorts and foreign annotations (LB controllers
// like MetalLB annotate the Service too — wholesale replacement
// ping-pongs with them forever) are preserved, a steady-state no-op
// skips the write entirely, and genuine updates retry on
// optimistic-concurrency conflicts instead of failing the drift pass.
// Trade-off of the annotation merge: removing an annotation from the
// spec no longer removes it from the live Service.
func updateService(ctx context.Context, c client.Client, desired *corev1.Service) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing := &corev1.Service{}
		err := c.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
		if apierrors.IsNotFound(err) {
			return c.Create(ctx, desired)
		}
		if err != nil {
			return err
		}

		// Start from the live object so immutable/foreign fields
		// (ClusterIP, ipFamilies, controller-added metadata) survive.
		updated := existing.DeepCopy()
		updated.Spec.Ports = make([]corev1.ServicePort, len(desired.Spec.Ports))
		copy(updated.Spec.Ports, desired.Spec.Ports)
		if desired.Spec.Type == corev1.ServiceTypeLoadBalancer || desired.Spec.Type == corev1.ServiceTypeNodePort {
			// Keep assigned NodePorts so steady state compares equal.
			for i := range updated.Spec.Ports {
				if updated.Spec.Ports[i].NodePort != 0 {
					continue
				}
				for _, ep := range existing.Spec.Ports {
					if servicePortsMatch(ep, updated.Spec.Ports[i]) {
						updated.Spec.Ports[i].NodePort = ep.NodePort
						break
					}
				}
			}
		}
		updated.Spec.Type = desired.Spec.Type
		updated.Spec.Selector = desired.Spec.Selector
		updated.Spec.LoadBalancerIP = desired.Spec.LoadBalancerIP
		updated.Spec.ExternalTrafficPolicy = desired.Spec.ExternalTrafficPolicy
		for k, v := range desired.Annotations {
			if updated.Annotations == nil {
				updated.Annotations = map[string]string{}
			}
			updated.Annotations[k] = v
		}

		if equality.Semantic.DeepEqual(existing.Spec, updated.Spec) &&
			equality.Semantic.DeepEqual(existing.Annotations, updated.Annotations) {
			return nil
		}
		return c.Update(ctx, updated)
	})
}

// servicePortsMatch pairs ports by name when named, by port+protocol
// otherwise — for carrying assigned NodePorts across re-renders.
func servicePortsMatch(a, b corev1.ServicePort) bool {
	if a.Name != "" || b.Name != "" {
		return a.Name == b.Name
	}
	return a.Port == b.Port && a.Protocol == b.Protocol
}

// deleteIfExists removes obj only when it actually exists — the common
// (non-gateway) path would otherwise fire blind DELETEs on every Ready
// loop. NotFound on the Get is the steady state and costs one cheap read.
// A missing CRD (no-match) also counts as "nothing to delete": Gateway
// API is optional, and servers that never asked for gateway exposure must
// not fail their drift loop on clusters without the route CRDs.
func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return client.IgnoreNotFound(err)
	}
	return client.IgnoreNotFound(c.Delete(ctx, obj))
}

// updateConfigMap converges the rendered ConfigMap (issue #9): ensureOwned
// is create-only, so a config re-render used to roll the pods (via the
// config-hash annotation) while the mounted ConfigMap kept serving stale
// content. Steady state skips the write. Creates using the provided
// createFn so the caller can set owner references before creation.
func updateConfigMap(ctx context.Context, c client.Client, desired *corev1.ConfigMap, createFn func() error) error {
	existing := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return createFn()
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing); err != nil {
			return err
		}
		existing.Data = desired.Data
		return c.Update(ctx, existing)
	})
}

// --- Thin method wrappers on PowerDNSServerReconciler ---
// These keep existing call sites within powerdnsserver_controller.go
// unchanged (they call r.updateService, r.deleteIfExists, etc.).

func (r *PowerDNSServerReconciler) updateService(ctx context.Context, s *dnsv1alpha1.PowerDNSServer, desired *corev1.Service) error {
	// Set owner ref before entering the converge loop so the NotFound→Create
	// path creates the Service with a controller ref (GC safety on recreate).
	if err := ctrl.SetControllerReference(s, desired, r.Scheme); err != nil {
		return err
	}
	return updateService(ctx, r.Client, desired)
}

func (r *PowerDNSServerReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	return deleteIfExists(ctx, r.Client, obj)
}
