package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

const (
	dnsTCPPort = 53
	dnsUDPPort = 53
	apiPort    = 8081

	labelApp       = "app.kubernetes.io/name"
	labelComponent = "app.kubernetes.io/component"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelInstance  = "app.kubernetes.io/instance"
	managedBy      = "aether-powerdns"

	// pgClientImage is used by the schema-init Job to apply the PowerDNS
	// schema bundled with the pdns-auth image to the backend Postgres.
	pgClientImage = "postgres:16-alpine"
	// pdnsSchemaPath is where the gpgsql schema lives inside the official
	// powerdns/pdns-auth image.
	pdnsSchemaPath = "/usr/share/doc/pdns/schema.pgsql.sql"
)

// ConfigHashAnnotation forces a Deployment rollout when the rendered
// pdns.conf or referenced credentials change. Pod template annotation.
const ConfigHashAnnotation = "dns.aetherplatform.cloud/config-hash"

// Names groups the resource names derived from a PowerDNSServer.
type Names struct {
	Server, ConfigMap, Deployment string
	APIService, DNSService        string
	APIKeySecret, BackendSecret   string
	SchemaJob, CNPGCluster        string
	TCPRoute, UDPRoute            string
	PDB, PodMonitor, NetPolicy    string
}

// NameSet returns the canonical names for all owned resources.
func NameSet(s *dnsv1alpha1.PowerDNSServer) Names {
	n := s.Name
	return Names{
		Server:        n,
		ConfigMap:     n + "-config",
		Deployment:    n,
		APIService:    n + "-api",
		DNSService:    n + "-dns",
		APIKeySecret:  n + "-api-key",
		BackendSecret: n + "-backend",
		SchemaJob:     n + "-schema-init",
		CNPGCluster:   n + "-pg",
		TCPRoute:      n + "-dns-tcp",
		UDPRoute:      n + "-dns-udp",
		PDB:           n + "-pdb",
		PodMonitor:    n + "-metrics",
		NetPolicy:     n + "-netpol",
	}
}


// ConfigHash returns a deterministic short hash over the rendered pdns.conf
// plus every key/value in the API-key and backend Secrets. Any change in
// any of those rolls the Deployment by mutating the pod template
// annotation.
func ConfigHash(conf string, secrets ...map[string][]byte) string {
	h := sha256.New()
	h.Write([]byte(conf))
	for _, s := range secrets {
		// Stable ordering — Secret data is a map; iterating directly
		// would produce a different hash on every reconcile.
		keys := make([]string, 0, len(s))
		for k := range s {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write(s[k])
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func labels(s *dnsv1alpha1.PowerDNSServer) map[string]string {
	return map[string]string{
		labelApp:       "powerdns",
		labelComponent: "auth",
		labelManagedBy: managedBy,
		labelInstance:  s.Name,
	}
}

// PDNSConfig renders the pdns.conf file. Database fields are read from the
// backend Secret via env vars (PDNS_AUTH_API_KEY, PDNS_GPGSQL_*) so a key
// rotation only requires a Secret update + pod restart.
func PDNSConfig(s *dnsv1alpha1.PowerDNSServer) string {
	api := apiSpecOrDefault(s)
	return fmt.Sprintf(`# managed by aether-powerdns
launch=gpgsql
local-address=0.0.0.0
local-port=%d
webserver=yes
webserver-address=0.0.0.0
webserver-port=%d
webserver-allow-from=0.0.0.0/0,::/0
api=yes
# api-key, gpgsql-host, gpgsql-port, gpgsql-dbname, gpgsql-user, gpgsql-password
# come from the env (PDNS_AUTH_API_KEY, PDNS_GPGSQL_HOST, ...).
loglevel=4
`, dnsTCPPort, api.Port)
}

// ConfigMap holds pdns.conf for the deployment.
func ConfigMap(s *dnsv1alpha1.PowerDNSServer) *corev1.ConfigMap {
	names := NameSet(s)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.ConfigMap,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Data: map[string]string{
			"pdns.conf": PDNSConfig(s),
		},
	}
}

// SchemaInitJob copies the bundled gpgsql schema out of the pdns-auth image
// (init container) and applies it to Postgres with `psql -f`. Job is
// idempotent because PowerDNS uses CREATE TABLE IF NOT EXISTS — re-runs are
// safe on a configured database.
func SchemaInitJob(s *dnsv1alpha1.PowerDNSServer) *batchv1.Job {
	names := NameSet(s)
	image := s.Spec.Image
	if image == "" {
		image = dnsv1alpha1.DefaultImage
	}

	backoff := int32(6)
	ttl := int32(3600)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SchemaJob,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(s)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					InitContainers: []corev1.Container{
						{
							Name:    "extract-schema",
							Image:   image,
							Command: []string{"/bin/sh", "-c"},
							Args: []string{fmt.Sprintf(
								"cp %s /schema/schema.sql", pdnsSchemaPath,
							)},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "schema", MountPath: "/schema"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "apply-schema",
							Image:   pgClientImage,
							Command: []string{"/bin/sh", "-c"},
							Args: []string{
								// PGPASSWORD comes via env from the backend Secret.
								`set -e
echo "applying PowerDNS schema to ${PGHOST}:${PGPORT}/${PGDATABASE}"
psql -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -f /schema/schema.sql
echo "schema applied"`,
							},
							Env: backendEnv(names.BackendSecret),
							VolumeMounts: []corev1.VolumeMount{
								{Name: "schema", MountPath: "/schema"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "schema",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
}

// Deployment runs pdns-auth with gpgsql + the API enabled. configHash is
// stamped onto the pod template so pods roll on any config / credential
// change. When replicas > 1, soft pod anti-affinity + topology spread
// across hostname keep replicas off the same node where possible.
func Deployment(s *dnsv1alpha1.PowerDNSServer, configHash string) *appsv1.Deployment {
	names := NameSet(s)
	lb := labels(s)
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	image := s.Spec.Image
	if image == "" {
		image = dnsv1alpha1.DefaultImage
	}

	res := s.Spec.Resources
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		}
	}
	if res.Limits == nil {
		res.Limits = corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		}
	}

	api := apiSpecOrDefault(s)

	env := backendEnv(names.BackendSecret)
	env = append(env,
		corev1.EnvVar{Name: "PDNS_AUTH_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: names.APIKeySecret},
				Key:                  "api-key",
			},
		}},
	)

	// pdns reads gpgsql-* from the env when those keys are set in pdns.conf;
	// here we expose the same data under both PG* (for the schema Job) and
	// PDNS_GPGSQL_* names so pdns picks them up automatically.
	gpgsqlMappings := map[string]string{
		"PDNS_GPGSQL_HOST":     "PGHOST",
		"PDNS_GPGSQL_PORT":     "PGPORT",
		"PDNS_GPGSQL_DBNAME":   "PGDATABASE",
		"PDNS_GPGSQL_USER":     "PGUSER",
		"PDNS_GPGSQL_PASSWORD": "PGPASSWORD",
	}
	for k, v := range gpgsqlMappings {
		env = append(env, corev1.EnvVar{Name: k, Value: "$(" + v + ")"})
	}

	podAnnotations := map[string]string{}
	if configHash != "" {
		podAnnotations[ConfigHashAnnotation] = configHash
	}

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:    "pdns-auth",
				Image:   image,
				Command: []string{"pdns_server"},
				Args:    []string{"--config-dir=/etc/powerdns", "--socket-dir=/var/run"},
				Ports: []corev1.ContainerPort{
					{Name: "dns-tcp", ContainerPort: dnsTCPPort, Protocol: corev1.ProtocolTCP},
					{Name: "dns-udp", ContainerPort: dnsUDPPort, Protocol: corev1.ProtocolUDP},
					{Name: "api", ContainerPort: api.Port, Protocol: corev1.ProtocolTCP},
				},
				Env:       env,
				Resources: res,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/powerdns", ReadOnly: true},
					{Name: "run", MountPath: "/var/run"},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(api.Port))},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(api.Port))},
					},
					InitialDelaySeconds: 30,
					PeriodSeconds:       10,
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: names.ConfigMap},
					},
				},
			},
			{
				Name: "run",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		},
	}

	if replicas > 1 {
		podSpec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: lb},
						TopologyKey:   corev1.LabelHostname,
					},
				}},
			},
		}
		podSpec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelHostname,
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: lb},
		}}
		if s.Spec.Scheduling.SpreadAcrossZones {
			podSpec.TopologySpreadConstraints = append(podSpec.TopologySpreadConstraints,
				corev1.TopologySpreadConstraint{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: lb},
				})
		}
	}

	if len(s.Spec.Scheduling.NodeSelector) > 0 {
		podSpec.NodeSelector = s.Spec.Scheduling.NodeSelector
	}
	if len(s.Spec.Scheduling.Tolerations) > 0 {
		podSpec.Tolerations = s.Spec.Scheduling.Tolerations
	}
	if s.Spec.Scheduling.PriorityClassName != "" {
		podSpec.PriorityClassName = s.Spec.Scheduling.PriorityClassName
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Deployment,
			Namespace: s.Namespace,
			Labels:    lb,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: lb},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lb, Annotations: podAnnotations},
				Spec:       podSpec,
			},
		},
	}
}

// PodDisruptionBudget keeps `replicas - 1` pods available during voluntary
// disruptions when running HA. Returns nil when replicas <= 1 — a single
// replica + PDB would block node drains entirely.
func PodDisruptionBudget(s *dnsv1alpha1.PowerDNSServer) *policyv1.PodDisruptionBudget {
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	if replicas <= 1 {
		return nil
	}
	names := NameSet(s)
	minAvail := intstr.FromInt(int(replicas - 1))
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.PDB,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels(s)},
		},
	}
}

// APIService exposes the PowerDNS HTTP API on ClusterIP only. The API is an
// admin surface; users that need it remote can port-forward or wire their
// own ingress.
func APIService(s *dnsv1alpha1.PowerDNSServer) *corev1.Service {
	names := NameSet(s)
	api := apiSpecOrDefault(s)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.APIService,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels(s),
			Ports: []corev1.ServicePort{{
				Name:       "api",
				Port:       api.Port,
				TargetPort: intstr.FromInt(int(api.Port)),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// DNSService renders the DNS-port Service. Type/IP/annotations follow
// spec.dns.exposure: ClusterIP for `none`/`gateway`, LoadBalancer for
// `loadBalancer`.
func DNSService(s *dnsv1alpha1.PowerDNSServer) *corev1.Service {
	names := NameSet(s)
	exposure := s.Spec.DNS.Exposure
	if exposure == "" {
		exposure = dnsv1alpha1.DNSExposureNone
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.DNSService,
			Namespace: s.Namespace,
			Labels:    labels(s),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels(s),
			Ports: []corev1.ServicePort{
				{Name: "dns-tcp", Port: dnsTCPPort, TargetPort: intstr.FromInt(dnsTCPPort), Protocol: corev1.ProtocolTCP},
				{Name: "dns-udp", Port: dnsUDPPort, TargetPort: intstr.FromInt(dnsUDPPort), Protocol: corev1.ProtocolUDP},
			},
		},
	}

	if exposure == dnsv1alpha1.DNSExposureLoadBalancer {
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		lb := s.Spec.DNS.LoadBalancer
		if lb != nil {
			if lb.IP != "" {
				svc.Spec.LoadBalancerIP = lb.IP
			}
			if len(lb.Annotations) > 0 {
				svc.Annotations = map[string]string{}
				for k, v := range lb.Annotations {
					svc.Annotations[k] = v
				}
			}
			if lb.ExternalTrafficPolicy != "" {
				svc.Spec.ExternalTrafficPolicy = lb.ExternalTrafficPolicy
			} else {
				svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			}
		} else {
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		}
	}

	return svc
}

func apiSpecOrDefault(s *dnsv1alpha1.PowerDNSServer) dnsv1alpha1.APISpec {
	api := s.Spec.API
	if api.Port == 0 {
		api.Port = apiPort
	}
	return api
}

// backendEnv renders the standard PG* env vars sourced from the backend
// Secret. The Secret is written by the controller (managed mode) or fanned
// out from the user-supplied BYO Secret.
func backendEnv(secret string) []corev1.EnvVar {
	src := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		}
	}
	return []corev1.EnvVar{
		{Name: "PGHOST", ValueFrom: src("host")},
		{Name: "PGPORT", ValueFrom: src("port")},
		{Name: "PGDATABASE", ValueFrom: src("database")},
		{Name: "PGUSER", ValueFrom: src("username")},
		{Name: "PGPASSWORD", ValueFrom: src("password")},
	}
}
