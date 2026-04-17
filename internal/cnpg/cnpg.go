// Package cnpg renders CloudNativePG Cluster manifests via unstructured.
// We deliberately avoid importing the CNPG types so the operator stays
// independent of CNPG's API churn.
package cnpg

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// GVK is the CloudNativePG Cluster GroupVersionKind.
var GVK = schema.GroupVersionKind{
	Group:   "postgresql.cnpg.io",
	Version: "v1",
	Kind:    "Cluster",
}

// DefaultDatabase is the database name created inside the CNPG Cluster for
// PowerDNS.
const DefaultDatabase = "powerdns"

// DefaultOwner is the Postgres role that owns the PowerDNS database.
const DefaultOwner = "powerdns"

// Cluster renders a CloudNativePG Cluster for the given PowerDNSServer.
// Storage size and instance count come from spec.backend.postgres.
func Cluster(s *dnsv1alpha1.PowerDNSServer, name string) *unstructured.Unstructured {
	pg := s.Spec.Backend.Postgres
	instances := int64(1)
	storageSize := "5Gi"
	storageClass := ""
	if pg != nil {
		if pg.Instances != nil {
			instances = int64(*pg.Instances)
		}
		if pg.StorageSize != "" {
			storageSize = pg.StorageSize
		}
		if pg.StorageClass != "" {
			storageClass = pg.StorageClass
		}
	}

	storage := map[string]interface{}{
		"size": storageSize,
	}
	if storageClass != "" {
		storage["storageClass"] = storageClass
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(GVK)
	u.SetName(name)
	u.SetNamespace(s.Namespace)
	u.SetLabels(map[string]string{
		"app.kubernetes.io/name":       "powerdns-postgres",
		"app.kubernetes.io/instance":   s.Name,
		"app.kubernetes.io/managed-by": "aether-powerdns",
	})
	u.Object["spec"] = map[string]interface{}{
		"instances": instances,
		"storage":   storage,
		"bootstrap": map[string]interface{}{
			"initdb": map[string]interface{}{
				"database": DefaultDatabase,
				"owner":    DefaultOwner,
			},
		},
	}
	return u
}

// AppSecretName follows the CNPG convention: `<cluster>-app` holds the
// owner role's credentials with keys `username`, `password`, `host`,
// `port`, `dbname`, `uri`, etc.
func AppSecretName(cluster string) string { return cluster + "-app" }
