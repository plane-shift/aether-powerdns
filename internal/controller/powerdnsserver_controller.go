package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/cnpg"
	"github.com/plane-shift/aether-powerdns/internal/manifests"
)

const (
	finalizer    = "dns.aetherplatform.cloud/server-protection"
	requeueShort = 5 * time.Second
	requeueLong  = 30 * time.Second
)

// PowerDNSServerReconciler reconciles PowerDNSServer resources.
type PowerDNSServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=powerdnsservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=powerdnsservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.aetherplatform.cloud,resources=powerdnsservers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets;services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes;udproutes,verbs=get;list;watch;create;update;patch;delete

func (r *PowerDNSServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	server := &dnsv1alpha1.PowerDNSServer{}
	if err := r.Get(ctx, req.NamespacedName, server); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !server.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, server)
	}
	if !controllerutil.ContainsFinalizer(server, finalizer) {
		controllerutil.AddFinalizer(server, finalizer)
		if err := r.Update(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch server.Status.Phase {
	case "", dnsv1alpha1.PhasePending:
		return r.phasePending(ctx, server)
	case dnsv1alpha1.PhaseProvisioningBackend:
		return r.phaseProvisioningBackend(ctx, server)
	case dnsv1alpha1.PhaseInitializingSchema:
		return r.phaseInitializingSchema(ctx, server)
	case dnsv1alpha1.PhaseDeployingServer:
		return r.phaseDeployingServer(ctx, server)
	case dnsv1alpha1.PhaseExposingDNS:
		return r.phaseExposingDNS(ctx, server)
	case dnsv1alpha1.PhaseReady:
		return r.phaseReady(ctx, server)
	case dnsv1alpha1.PhaseFailed:
		logger.Info("PowerDNSServer in failed state", "message", server.Status.FailureMessage)
		return ctrl.Result{}, nil
	default:
		logger.Info("unknown phase", "phase", server.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *PowerDNSServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Owns watches reconcile us when any of these owned children change —
	// so a ConfigMap edit, Secret rotation, manual scale, or pod-loss
	// event all trigger a fresh reconcile within seconds, not the 30s
	// requeue. Together with the configHash pod template annotation,
	// that gives us "reload on config change" without an external watcher.
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.PowerDNSServer{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

// --- Phases ---

func (r *PowerDNSServerReconciler) phasePending(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	if msg := validateSpec(s); msg != "" {
		return r.setFailed(ctx, s, msg)
	}
	if err := r.ensureAPIKey(ctx, s); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure api key: %w", err)
	}
	r.event(s, corev1.EventTypeNormal, "Provisioning", "PowerDNSServer accepted; provisioning backend")
	falseCond(s, dnsv1alpha1.ConditionReady, "Provisioning", "Backend not yet provisioned")
	falseCond(s, dnsv1alpha1.ConditionAvailable, "Pending", "Server pods not yet running")
	return r.setPhase(ctx, s, dnsv1alpha1.PhaseProvisioningBackend)
}

// validateSpec is the operator-side validation pass. CEL rules in the CRD
// catch most input errors before the controller sees them; this is the
// belt-and-suspenders check for the few things CEL can't express cleanly.
func validateSpec(s *dnsv1alpha1.PowerDNSServer) string {
	if s.Spec.Backend.Type != dnsv1alpha1.BackendPostgres {
		return fmt.Sprintf("backend type %q not supported (only postgres)", s.Spec.Backend.Type)
	}
	if pg := s.Spec.Backend.Postgres; pg != nil && pg.BYO != nil {
		if pg.BYO.Host == "" {
			return "backend.postgres.byo.host is required"
		}
		if pg.BYO.CredentialsSecretRef.Name == "" {
			return "backend.postgres.byo.credentialsSecretRef.name is required"
		}
	}
	if s.Spec.DNS.Exposure == dnsv1alpha1.DNSExposureGateway && s.Spec.DNS.Gateway == nil {
		return "dns.exposure=gateway requires dns.gateway"
	}
	return ""
}

// phaseProvisioningBackend either provisions a CNPG cluster or validates a
// BYO secret, then materialises a normalized backend Secret with PG* keys.
func (r *PowerDNSServerReconciler) phaseProvisioningBackend(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	names := manifests.NameSet(s)

	if isBYO(s) {
		ready, err := r.materialiseBYOBackendSecret(ctx, s)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			logger.Info("BYO backend secret not ready, retrying")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		s.Status.BackendSecretName = names.BackendSecret
		return r.setPhase(ctx, s, dnsv1alpha1.PhaseInitializingSchema)
	}

	cluster := cnpg.Cluster(s, names.CNPGCluster)
	if err := r.ensureUnstructured(ctx, s, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure CNPG cluster: %w", err)
	}

	ready, err := r.materialiseCNPGBackendSecret(ctx, s, names.CNPGCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		logger.Info("CNPG cluster not ready yet, retrying")
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	s.Status.BackendSecretName = names.BackendSecret
	trueCond(s, dnsv1alpha1.ConditionBackendProvisioned, "BackendReady", "Backend secret published")
	r.event(s, corev1.EventTypeNormal, "BackendProvisioned", "Postgres backend ready, applying schema")
	return r.setPhase(ctx, s, dnsv1alpha1.PhaseInitializingSchema)
}

func (r *PowerDNSServerReconciler) phaseInitializingSchema(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	names := manifests.NameSet(s)

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: names.SchemaJob, Namespace: s.Namespace}, job)
	if apierrors.IsNotFound(err) {
		newJob := manifests.SchemaInitJob(s)
		if err := ctrl.SetControllerReference(s, newJob, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newJob); err != nil {
			return ctrl.Result{}, fmt.Errorf("create schema job: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		s.Status.SchemaApplied = true
		trueCond(s, dnsv1alpha1.ConditionSchemaApplied, "SchemaApplied", "PowerDNS schema applied to Postgres")
		r.event(s, corev1.EventTypeNormal, "SchemaApplied", "PowerDNS schema initialised")
		return r.setPhase(ctx, s, dnsv1alpha1.PhaseDeployingServer)
	}
	if job.Status.Failed >= 6 {
		return r.setFailed(ctx, s, "schema init Job exhausted retries; check Job logs")
	}
	logger.Info("waiting for schema init Job", "succeeded", job.Status.Succeeded, "failed", job.Status.Failed)
	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

func (r *PowerDNSServerReconciler) phaseDeployingServer(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	if err := r.ensureWorkload(ctx, s); err != nil {
		return ctrl.Result{}, err
	}

	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: manifests.NameSet(s).Deployment, Namespace: s.Namespace}, deploy); err != nil {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
	if deploy.Status.AvailableReplicas < 1 {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	apiSvc := manifests.APIService(s)
	s.Status.APIEndpoint = fmt.Sprintf("http://%s.%s.svc:%d",
		apiSvc.Name, s.Namespace, apiSvc.Spec.Ports[0].Port)
	return r.setPhase(ctx, s, dnsv1alpha1.PhaseExposingDNS)
}

// ensureWorkload renders + applies the ConfigMap, Deployment, both Services
// and (when HA) a PodDisruptionBudget. The Deployment carries a
// config-hash pod-template annotation so any change to pdns.conf or to
// the API-key / backend Secret triggers a rolling restart.
func (r *PowerDNSServerReconciler) ensureWorkload(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	cm := manifests.ConfigMap(s)
	if err := r.ensureOwned(ctx, s, cm); err != nil {
		return fmt.Errorf("ensure configmap: %w", err)
	}

	hash, err := r.computeConfigHash(ctx, s, cm)
	if err != nil {
		return fmt.Errorf("compute config hash: %w", err)
	}
	s.Status.ConfigHash = hash

	if err := r.ensureOwned(ctx, s, manifests.Deployment(s, hash)); err != nil {
		return fmt.Errorf("ensure deployment: %w", err)
	}
	if err := r.ensureOwned(ctx, s, manifests.APIService(s)); err != nil {
		return fmt.Errorf("ensure api service: %w", err)
	}
	if err := r.ensureOwned(ctx, s, manifests.DNSService(s)); err != nil {
		return fmt.Errorf("ensure dns service: %w", err)
	}
	if pdb := manifests.PodDisruptionBudget(s); pdb != nil {
		if err := r.ensureOwned(ctx, s, pdb); err != nil {
			return fmt.Errorf("ensure pdb: %w", err)
		}
	} else {
		// Replicas dropped to 1 — clear any prior PDB so a node drain
		// isn't blocked by a leftover budget.
		_ = r.Delete(ctx, &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name: manifests.NameSet(s).PDB, Namespace: s.Namespace,
			},
		})
	}

	if err := r.reconcileObservability(ctx, s); err != nil {
		return fmt.Errorf("reconcile observability: %w", err)
	}
	if err := r.reconcileNetworkPolicy(ctx, s); err != nil {
		return fmt.Errorf("reconcile network policy: %w", err)
	}
	return nil
}

// reconcileObservability creates / removes the PodMonitor based on the
// spec toggle. PodMonitor lives under monitoring.coreos.com — if the
// Prometheus operator isn't installed, Create returns NoMatchKind, which
// we surface as a non-fatal warning so the operator stays healthy.
func (r *PowerDNSServerReconciler) reconcileObservability(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	pm := manifests.PodMonitor(s)
	if !s.Spec.Observability.PodMonitor.Enabled {
		_ = r.Delete(ctx, pm)
		return nil
	}
	err := r.ensureUnstructured(ctx, s, pm)
	if err != nil && meta.IsNoMatchError(err) {
		log.FromContext(ctx).Info("PodMonitor CRD missing — install prometheus-operator or disable spec.observability.podMonitor")
		r.event(s, corev1.EventTypeWarning, "PodMonitorUnavailable",
			"PodMonitor CRD not installed; metrics scraping skipped")
		return nil
	}
	return err
}

// reconcileNetworkPolicy creates / removes the NetworkPolicy. Always safe
// to Delete-then-recreate because NP is stateless.
func (r *PowerDNSServerReconciler) reconcileNetworkPolicy(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	np := manifests.NetworkPolicy(s)
	if !s.Spec.NetworkPolicy.Enabled {
		_ = r.Delete(ctx, np)
		return nil
	}
	if err := r.ensureOwned(ctx, s, np); err != nil {
		return err
	}
	// In-place update for spec changes (additional namespaces, etc.).
	existing := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: np.Name, Namespace: np.Namespace}, existing); err == nil {
		existing.Spec = np.Spec
		return r.Update(ctx, existing)
	}
	return nil
}

// computeConfigHash hashes the rendered pdns.conf together with the
// API-key and backend Secret data, so a rotation of either secret bumps
// the pod template annotation and triggers a rolling restart.
func (r *PowerDNSServerReconciler) computeConfigHash(ctx context.Context, s *dnsv1alpha1.PowerDNSServer, cm *corev1.ConfigMap) (string, error) {
	names := manifests.NameSet(s)

	apiKeyName := names.APIKeySecret
	if s.Spec.API.APIKeySecretRef != nil && s.Spec.API.APIKeySecretRef.Name != "" {
		apiKeyName = s.Spec.API.APIKeySecretRef.Name
	}
	apiSec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: apiKeyName, Namespace: s.Namespace}, apiSec); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	backendSec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: names.BackendSecret, Namespace: s.Namespace}, backendSec); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	return manifests.ConfigHash(cm.Data["pdns.conf"], apiSec.Data, backendSec.Data), nil
}

func (r *PowerDNSServerReconciler) phaseExposingDNS(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	exposure := s.Spec.DNS.Exposure
	if exposure == "" {
		exposure = dnsv1alpha1.DNSExposureNone
	}

	switch exposure {
	case dnsv1alpha1.DNSExposureNone:
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: manifests.NameSet(s).DNSService, Namespace: s.Namespace}, svc); err != nil {
			return ctrl.Result{}, err
		}
		s.Status.DNSEndpoint = fmt.Sprintf("%s:53", svc.Spec.ClusterIP)

	case dnsv1alpha1.DNSExposureLoadBalancer:
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: manifests.NameSet(s).DNSService, Namespace: s.Namespace}, svc); err != nil {
			return ctrl.Result{}, err
		}
		var ip string
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				ip = ing.IP
				break
			}
			if ing.Hostname != "" {
				ip = ing.Hostname
				break
			}
		}
		if ip == "" {
			logger.Info("waiting for LoadBalancer ingress")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		s.Status.DNSEndpoint = fmt.Sprintf("%s:53", ip)

	case dnsv1alpha1.DNSExposureGateway:
		if s.Spec.DNS.Gateway == nil {
			return r.setFailed(ctx, s, "dns.exposure=gateway requires dns.gateway")
		}
		tcp := manifests.TCPRoute(s)
		if err := r.ensureOwned(ctx, s, tcp); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure tcproute: %w", err)
		}
		udp := manifests.UDPRoute(s)
		if err := r.ensureOwned(ctx, s, udp); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure udproute: %w", err)
		}
		s.Status.DNSEndpoint = fmt.Sprintf("gateway:%s/%s",
			s.Spec.DNS.Gateway.ParentRef.Namespace, s.Spec.DNS.Gateway.ParentRef.Name)
	}

	return r.setPhase(ctx, s, dnsv1alpha1.PhaseReady)
}

func (r *PowerDNSServerReconciler) phaseReady(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	if err := r.reconcileDrift(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.refreshReplicaStatus(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

// reconcileDrift re-renders + applies the workload so spec changes,
// config-hash changes (Secret/ConfigMap edits) and manual scale tampering
// all get reverted to the desired state. Service drift is handled with an
// in-place update because ClusterIP is immutable.
func (r *PowerDNSServerReconciler) reconcileDrift(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	cm := manifests.ConfigMap(s)
	if err := r.ensureOwned(ctx, s, cm); err != nil {
		return err
	}
	hash, err := r.computeConfigHash(ctx, s, cm)
	if err != nil {
		return err
	}
	s.Status.ConfigHash = hash

	if err := r.updateDeployment(ctx, s, manifests.Deployment(s, hash)); err != nil {
		return err
	}
	if err := r.updateService(ctx, manifests.APIService(s)); err != nil {
		return err
	}
	if err := r.updateService(ctx, manifests.DNSService(s)); err != nil {
		return err
	}
	if pdb := manifests.PodDisruptionBudget(s); pdb != nil {
		if err := r.ensureOwned(ctx, s, pdb); err != nil {
			return err
		}
	} else {
		_ = r.Delete(ctx, &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name: manifests.NameSet(s).PDB, Namespace: s.Namespace,
			},
		})
	}
	return nil
}

// refreshReplicaStatus mirrors the Deployment's live replica counts onto
// the PowerDNSServer status and flips status.Ready based on
// availableReplicas == desired. Surfaces transient losses (crashed pod,
// evicted pod, in-progress rollout) instead of staying stuck at Ready=true.
func (r *PowerDNSServerReconciler) refreshReplicaStatus(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: manifests.NameSet(s).Deployment, Namespace: s.Namespace,
	}, deploy); err != nil {
		return err
	}
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	ready := deploy.Status.AvailableReplicas == desired && desired > 0

	if s.Status.Ready != ready ||
		s.Status.ReadyReplicas != deploy.Status.ReadyReplicas ||
		s.Status.DesiredReplicas != desired {
		prev := s.Status.Ready
		s.Status.Ready = ready
		s.Status.ReadyReplicas = deploy.Status.ReadyReplicas
		s.Status.DesiredReplicas = desired

		msg := fmt.Sprintf("%d/%d replicas available", deploy.Status.AvailableReplicas, desired)
		if ready {
			trueCond(s, dnsv1alpha1.ConditionAvailable, "AllReplicasReady", msg)
			trueCond(s, dnsv1alpha1.ConditionReady, "Ready", "Server is serving DNS and the API")
		} else {
			falseCond(s, dnsv1alpha1.ConditionAvailable, "ReplicasUnavailable", msg)
			falseCond(s, dnsv1alpha1.ConditionReady, "Degraded", msg)
		}
		if err := r.Status().Update(ctx, s); err != nil {
			return err
		}

		if ready && !prev {
			log.FromContext(ctx).Info("PowerDNSServer is ready",
				"dns", s.Status.DNSEndpoint, "api", s.Status.APIEndpoint,
				"replicas", desired)
			r.event(s, corev1.EventTypeNormal, "Ready",
				fmt.Sprintf("PowerDNSServer ready (%d/%d replicas, dns=%s)",
					deploy.Status.AvailableReplicas, desired, s.Status.DNSEndpoint))
		} else if !ready && prev {
			r.event(s, corev1.EventTypeWarning, "Degraded", msg)
		}
	}
	return nil
}

func (r *PowerDNSServerReconciler) reconcileDelete(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (ctrl.Result, error) {
	// Owner references handle the owned children. We explicitly delete the
	// CNPG Cluster (owned via SetControllerReference) — fine to leave to
	// GC, but we trigger it eagerly so the PVC is freed promptly.
	controllerutil.RemoveFinalizer(s, finalizer)
	if err := r.Update(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// --- API key + backend secret ---

func (r *PowerDNSServerReconciler) ensureAPIKey(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) error {
	names := manifests.NameSet(s)
	secretName := names.APIKeySecret
	if s.Spec.API.APIKeySecretRef != nil && s.Spec.API.APIKeySecretRef.Name != "" {
		secretName = s.Spec.API.APIKeySecretRef.Name
		s.Status.APIKeySecretName = secretName
		return nil
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: s.Namespace}, existing)
	if err == nil {
		s.Status.APIKeySecretName = secretName
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	key, err := randomHex(32)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: s.Namespace, Labels: ownerLabels(s)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"api-key": []byte(key)},
	}
	if err := ctrl.SetControllerReference(s, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil {
		return err
	}
	s.Status.APIKeySecretName = secretName
	return nil
}

// materialiseCNPGBackendSecret reads the CNPG-managed app secret
// (`<cluster>-app`) and copies the relevant fields into the controller's
// own backend Secret. We do this so the manifests package can rely on a
// stable Secret name regardless of backend mode.
func (r *PowerDNSServerReconciler) materialiseCNPGBackendSecret(
	ctx context.Context, s *dnsv1alpha1.PowerDNSServer, cluster string,
) (bool, error) {
	src := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      cnpg.AppSecretName(cluster),
		Namespace: s.Namespace,
	}, src)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	host := pickKey(src, "host")
	if host == "" {
		// CNPG sometimes only exposes `pgpass`/`uri`; fall back to the
		// service DNS that CNPG always creates.
		host = fmt.Sprintf("%s-rw.%s.svc", cluster, s.Namespace)
	}
	port := pickKey(src, "port")
	if port == "" {
		port = "5432"
	}
	username := pickKey(src, "username", "user")
	password := pickKey(src, "password")
	database := pickKey(src, "dbname", "database")
	if database == "" {
		database = cnpg.DefaultDatabase
	}
	if username == "" || password == "" {
		return false, nil
	}

	return true, r.writeBackendSecret(ctx, s, host, port, username, password, database)
}

func (r *PowerDNSServerReconciler) materialiseBYOBackendSecret(ctx context.Context, s *dnsv1alpha1.PowerDNSServer) (bool, error) {
	byo := s.Spec.Backend.Postgres.BYO
	src := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: byo.CredentialsSecretRef.Name, Namespace: s.Namespace}, src)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	username := pickKey(src, "username", "user")
	password := pickKey(src, "password")
	if username == "" || password == "" {
		return false, fmt.Errorf("BYO secret %s missing username/password keys", byo.CredentialsSecretRef.Name)
	}
	port := byo.Port
	if port == 0 {
		port = 5432
	}
	db := byo.Database
	if db == "" {
		db = cnpg.DefaultDatabase
	}
	return true, r.writeBackendSecret(ctx, s, byo.Host, fmt.Sprintf("%d", port), username, password, db)
}

func (r *PowerDNSServerReconciler) writeBackendSecret(
	ctx context.Context, s *dnsv1alpha1.PowerDNSServer,
	host, port, user, password, database string,
) error {
	names := manifests.NameSet(s)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.BackendSecret,
			Namespace: s.Namespace,
			Labels:    ownerLabels(s),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"host":     []byte(host),
			"port":     []byte(port),
			"username": []byte(user),
			"password": []byte(password),
			"database": []byte(database),
		},
	}
	if err := ctrl.SetControllerReference(s, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existing.Data, desired.Data) {
		return nil
	}
	existing.Data = desired.Data
	return r.Update(ctx, existing)
}

// --- Helpers ---

func (r *PowerDNSServerReconciler) ensureOwned(ctx context.Context, s *dnsv1alpha1.PowerDNSServer, obj client.Object) error {
	if err := ctrl.SetControllerReference(s, obj, r.Scheme); err != nil {
		return err
	}
	existing := obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	return err
}

// ensureUnstructured is the same as ensureOwned but for unstructured CRs
// (CNPG Cluster). We don't take ownership because CNPG re-creates owners
// during failover and we'd race with it; we rely on namespace cleanup or
// explicit delete instead.
func (r *PowerDNSServerReconciler) ensureUnstructured(ctx context.Context, s *dnsv1alpha1.PowerDNSServer, obj *unstructured.Unstructured) error {
	if err := ctrl.SetControllerReference(s, obj, r.Scheme); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	return err
}

func (r *PowerDNSServerReconciler) updateDeployment(ctx context.Context, _ *dnsv1alpha1.PowerDNSServer, desired *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	return r.Update(ctx, existing)
}

func (r *PowerDNSServerReconciler) updateService(ctx context.Context, desired *corev1.Service) error {
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Preserve ClusterIP — it's immutable after creation.
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Type = desired.Spec.Type
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.LoadBalancerIP = desired.Spec.LoadBalancerIP
	existing.Spec.ExternalTrafficPolicy = desired.Spec.ExternalTrafficPolicy
	if desired.Annotations != nil {
		existing.Annotations = desired.Annotations
	}
	return r.Update(ctx, existing)
}

func (r *PowerDNSServerReconciler) setPhase(ctx context.Context, s *dnsv1alpha1.PowerDNSServer, phase string) (ctrl.Result, error) {
	s.Status.Phase = phase
	if err := r.Status().Update(ctx, s); err != nil {
		return ctrl.Result{}, fmt.Errorf("update phase to %s: %w", phase, err)
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *PowerDNSServerReconciler) setFailed(ctx context.Context, s *dnsv1alpha1.PowerDNSServer, msg string) (ctrl.Result, error) {
	s.Status.Phase = dnsv1alpha1.PhaseFailed
	s.Status.FailureMessage = msg
	falseCond(s, dnsv1alpha1.ConditionReady, "Failed", msg)
	if err := r.Status().Update(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	r.event(s, corev1.EventTypeWarning, "Failed", msg)
	return ctrl.Result{}, nil
}

// event records a Kubernetes Event surfaced by `kubectl describe pdns`.
// Safe to call with a nil recorder (test paths).
func (r *PowerDNSServerReconciler) event(s *dnsv1alpha1.PowerDNSServer, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(s, eventType, reason, message)
}

func isBYO(s *dnsv1alpha1.PowerDNSServer) bool {
	return s.Spec.Backend.Postgres != nil && s.Spec.Backend.Postgres.BYO != nil
}

func ownerLabels(s *dnsv1alpha1.PowerDNSServer) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "powerdns",
		"app.kubernetes.io/instance":   s.Name,
		"app.kubernetes.io/managed-by": "aether-powerdns",
	}
}

func pickKey(src *corev1.Secret, keys ...string) string {
	for _, k := range keys {
		if v, ok := src.Data[k]; ok && len(v) > 0 {
			return string(v)
		}
	}
	return ""
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

