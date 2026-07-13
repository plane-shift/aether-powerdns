package manifests

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

func testServer() *dnsv1alpha1.PowerDNSServer {
	return &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
}

// Bug 1: pdns-auth-51 ships the schema at /usr/local/share/doc/pdns/...;
// older images used /usr/share/doc/pdns/... — the extract-schema init
// container must try the new path first and fall back to the old one.
func TestSchemaInitJobTriesBothSchemaPaths(t *testing.T) {
	job := SchemaInitJob(testServer())
	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(job.Spec.Template.Spec.InitContainers))
	}
	args := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, " ")
	newPath := "/usr/local/share/doc/pdns/schema.pgsql.sql"
	oldPath := "/usr/share/doc/pdns/schema.pgsql.sql"
	newIdx := strings.Index(args, newPath)
	if newIdx < 0 {
		t.Errorf("init container args missing new schema path %q: %s", newPath, args)
	}
	oldIdx := strings.LastIndex(args, oldPath)
	if oldIdx < 0 {
		t.Errorf("init container args missing fallback schema path %q: %s", oldPath, args)
	}
	if newIdx >= 0 && oldIdx >= 0 && newIdx > oldIdx {
		t.Errorf("new schema path must be tried before the fallback: %s", args)
	}
	if !strings.Contains(args, "||") {
		t.Errorf("init container args missing fallback (||): %s", args)
	}
}

// Bug 2: env ordering must be deterministic — a map-ordered env slice
// changes the pod-template hash every reconcile and causes endless
// ReplicaSet churn. Render many times and require deep equality.
func TestDeploymentRenderIsDeterministic(t *testing.T) {
	s := testServer()
	first := Deployment(s, "abc123")
	for i := 0; i < 50; i++ {
		next := Deployment(s, "abc123")
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("render %d differs from first render:\nfirst env: %v\nnext env:  %v",
				i, first.Spec.Template.Spec.Containers[0].Env, next.Spec.Template.Spec.Containers[0].Env)
		}
	}
}

// Bug 3: pdns_server is exec'd directly (no image startup wrapper), so it
// never reads PDNS_AUTH_API_KEY / PDNS_GPGSQL_* from the env. Every setting
// must reach it via $(VAR) arg expansion instead.
func TestDeploymentArgsCarryAPIKeyAndGpgsqlSettings(t *testing.T) {
	dep := Deployment(testServer(), "abc123")
	c := dep.Spec.Template.Spec.Containers[0]
	args := c.Args

	want := []string{
		"--api-key=$(PDNS_AUTH_API_KEY)",
		"--gpgsql-host=$(PDNS_GPGSQL_HOST)",
		"--gpgsql-port=$(PDNS_GPGSQL_PORT)",
		"--gpgsql-dbname=$(PDNS_GPGSQL_DBNAME)",
		"--gpgsql-user=$(PDNS_GPGSQL_USER)",
		"--gpgsql-password=$(PDNS_GPGSQL_PASSWORD)",
	}
	have := map[string]bool{}
	for _, a := range args {
		have[a] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("container args missing %q (args: %v)", w, args)
		}
	}

	// $(VAR) expansion only works when the referenced env var exists on the
	// container — verify all referenced vars are defined.
	envNames := map[string]bool{}
	for _, e := range c.Env {
		envNames[e.Name] = true
	}
	for _, v := range []string{
		"PDNS_AUTH_API_KEY",
		"PDNS_GPGSQL_HOST", "PDNS_GPGSQL_PORT", "PDNS_GPGSQL_DBNAME",
		"PDNS_GPGSQL_USER", "PDNS_GPGSQL_PASSWORD",
	} {
		if !envNames[v] {
			t.Errorf("container env missing %s referenced by args", v)
		}
	}

	// The PDNS_GPGSQL_* vars expand $(PG*), which only resolves when the
	// PG* vars are defined EARLIER in the env list.
	idx := map[string]int{}
	for i, e := range c.Env {
		idx[e.Name] = i
	}
	for pdnsVar, pgVar := range map[string]string{
		"PDNS_GPGSQL_HOST":     "PGHOST",
		"PDNS_GPGSQL_PORT":     "PGPORT",
		"PDNS_GPGSQL_DBNAME":   "PGDATABASE",
		"PDNS_GPGSQL_USER":     "PGUSER",
		"PDNS_GPGSQL_PASSWORD": "PGPASSWORD",
	} {
		pi, pok := idx[pgVar]
		di, dok := idx[pdnsVar]
		if pok && dok && pi > di {
			t.Errorf("%s must be defined before %s for $(%s) expansion", pgVar, pdnsVar, pgVar)
		}
	}
}

// Out-of-band zone creations (HTTP API / pdnsutil) must not inherit the
// "a.misconfigured.dns.server.invalid." placeholder either: pdns.conf
// carries a sane default-soa-content (@ = the zone name).
func TestPDNSConfigSetsDefaultSOAContent(t *testing.T) {
	conf := PDNSConfig(testServer())
	if !strings.Contains(conf, "default-soa-content=@ hostmaster.@ 0 10800 3600 604800 3600") {
		t.Errorf("pdns.conf missing sane default-soa-content:\n%s", conf)
	}
}

func TestPDBMinAvailableOverride(t *testing.T) {
	s := testServer()
	three := int32(3)
	s.Spec.Replicas = &three

	pdb := PodDisruptionBudget(s)
	if pdb == nil || pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Fatalf("default must stay replicas-1, got %v", pdb)
	}

	one := int32(1)
	s.Spec.PodDisruptionBudget = &dnsv1alpha1.PDBSpec{MinAvailable: &one}
	pdb = PodDisruptionBudget(s)
	if pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("override minAvailable=1 not honored, got %v", pdb.Spec.MinAvailable)
	}

	single := int32(1)
	s.Spec.Replicas = &single
	if PodDisruptionBudget(s) != nil {
		t.Error("replicas<=1 must render no PDB even with an override set")
	}
}

func TestDeploymentReadinessProbeIsDNSCheck(t *testing.T) {
	dep := Deployment(testServer(), "h")
	rp := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
	if rp == nil || rp.Exec == nil {
		t.Fatalf("readiness must be an exec DNS check, got %+v", rp)
	}
	joined := strings.Join(rp.Exec.Command, " ")
	if !strings.Contains(joined, "pdns_control") && !strings.Contains(joined, "sdig") {
		t.Errorf("readiness command should use in-image DNS tooling, got %q", joined)
	}
	// The default 1s timeout dropped a busy-but-serving pdns from endpoints
	// under load (aether-powerdns#24) — the exec forks pdns_control, so it
	// needs real headroom.
	if rp.TimeoutSeconds < 3 {
		t.Errorf("readiness TimeoutSeconds = %d, want >=3 (exec probe needs headroom)", rp.TimeoutSeconds)
	}
	lp := dep.Spec.Template.Spec.Containers[0].LivenessProbe
	if lp == nil || lp.TCPSocket == nil {
		t.Errorf("liveness stays TCP, got %+v", lp)
	}
	// Killing must require sustained failure — a transient stall must not
	// restart-loop a pod that still serves DNS (aether-powerdns#24).
	if lp.TimeoutSeconds < 3 || lp.FailureThreshold < 3 {
		t.Errorf("liveness must be lenient: TimeoutSeconds=%d FailureThreshold=%d, want >=3 each",
			lp.TimeoutSeconds, lp.FailureThreshold)
	}
}
