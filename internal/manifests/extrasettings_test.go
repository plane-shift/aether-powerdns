package manifests

import (
	"strings"
	"testing"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
)

// lineIndex returns the byte offset of the line that EXACTLY equals want,
// or -1. Matching whole lines (not substrings) is the point: an
// extraSettings entry that ends up glued onto another setting, indented,
// or carrying trailing junk is not a valid pdns.conf line even though a
// strings.Contains check would happily accept it.
func lineIndex(conf, want string) int {
	off := 0
	for _, line := range strings.SplitAfter(conf, "\n") {
		if strings.TrimSuffix(line, "\n") == want {
			return off
		}
		off += len(line)
	}
	return -1
}

// countLinesWithPrefix counts rendered lines starting with prefix — used to
// prove a rejected entry did not ADD a second `launch=` line next to the
// operator's managed one.
func countLinesWithPrefix(conf, prefix string) int {
	n := 0
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// quotedKey extracts the setting name from a ValidateExtraSettings message
// (`key "x" must match …`, `key "x" is managed …`, `value of "x" must not …`).
func quotedKey(msg string) string {
	first := strings.Index(msg, `"`)
	if first < 0 {
		return ""
	}
	rest := msg[first+1:]
	last := strings.Index(rest, `"`)
	if last < 0 {
		return ""
	}
	return rest[:last]
}

func serverWithExtras(extra map[string]string) *dnsv1alpha1.PowerDNSServer {
	s := testServer()
	s.Spec.ExtraSettings = extra
	return s
}

// The hidden-primary case (PR #29): three settings must land after the
// managed block, one per line, sorted by key — sorted so status.configHash
// is a function of the CONTENT, not of Go's map iteration order.
func TestPDNSConfigRendersExtraSettingsSorted(t *testing.T) {
	conf := PDNSConfig(serverWithExtras(map[string]string{
		"primary":        "yes",
		"allow-axfr-ips": "104.218.120.85/32",
		"also-notify":    "192.0.2.1",
	}))

	want := []string{
		"# spec.extraSettings",
		"allow-axfr-ips=104.218.120.85/32",
		"also-notify=192.0.2.1",
		"primary=yes",
	}
	idx := make([]int, len(want))
	for i, line := range want {
		idx[i] = lineIndex(conf, line)
		if idx[i] < 0 {
			t.Fatalf("pdns.conf missing line %q:\n%s", line, conf)
		}
	}
	for i := 1; i < len(idx); i++ {
		if idx[i] <= idx[i-1] {
			t.Errorf("line %q must be rendered after %q (keys are sorted):\n%s",
				want[i], want[i-1], conf)
		}
	}

	// The block belongs AFTER the managed settings — an extraSetting
	// rendered before them would be overridden by the operator's own line.
	loglevel := lineIndex(conf, "loglevel=4")
	if loglevel < 0 {
		t.Fatalf("managed block lost its loglevel line:\n%s", conf)
	}
	if idx[0] <= loglevel {
		t.Errorf("extraSettings block must start after the managed block (header at %d, loglevel=4 at %d):\n%s",
			idx[0], loglevel, conf)
	}
}

// PowerDNS does not reload at runtime, so pods only roll when
// status.configHash moves. A render that is not byte-stable would roll the
// pods on EVERY reconcile — the same class of bug as the map-ordered env
// slice in TestDeploymentRenderIsDeterministic.
func TestPDNSConfigExtraSettingsDeterministic(t *testing.T) {
	s := serverWithExtras(map[string]string{
		"primary":              "yes",
		"allow-axfr-ips":       "104.218.120.85/32",
		"also-notify":          "192.0.2.1",
		"default-ttl":          "300",
		"disable-axfr":         "no",
		"slave-cycle-interval": "60",
	})
	first := PDNSConfig(s)
	for i := 0; i < 100; i++ {
		if got := PDNSConfig(s); got != first {
			t.Fatalf("render %d differs from the first render:\nfirst:\n%s\ngot:\n%s", i, first, got)
		}
	}
}

// No extraSettings (or nothing renderable) must leave pdns.conf exactly as
// it was — no dangling header comment, and above all no second `launch=`.
func TestPDNSConfigNoExtraSettingsHeaderWhenEmpty(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"nil map":           nil,
		"only rejected key": {"launch": "bind"},
	} {
		conf := PDNSConfig(serverWithExtras(extra))
		if strings.Contains(conf, "# spec.extraSettings") {
			t.Errorf("%s: pdns.conf must not carry the extraSettings header:\n%s", name, conf)
		}
		if got := strings.Count(conf, "launch="); got != 1 {
			t.Errorf("%s: pdns.conf must carry exactly one launch= setting (the managed launch=gpgsql), got %d:\n%s",
				name, got, conf)
		}
	}
}

// The hash is what rolls the pods. Adding settings must move it, changing
// only a VALUE must move it, and re-rendering the same spec must not.
func TestExtraSettingsConfigHashChanges(t *testing.T) {
	bare := ConfigHash(PDNSConfig(testServer()))

	extra := map[string]string{"primary": "yes", "allow-axfr-ips": "104.218.120.85/32"}
	with := ConfigHash(PDNSConfig(serverWithExtras(extra)))
	if with == bare {
		t.Errorf("adding extraSettings must move the config hash (both %q) — pods would never pick the settings up", bare)
	}

	// Same content, freshly built map: the hash must be identical or every
	// reconcile rolls the pods.
	again := ConfigHash(PDNSConfig(serverWithExtras(map[string]string{
		"allow-axfr-ips": "104.218.120.85/32", "primary": "yes",
	})))
	if again != with {
		t.Errorf("identical extraSettings must hash identically: %q vs %q", with, again)
	}

	// A value-only edit (a different AXFR peer) is a real config change.
	valueEdit := ConfigHash(PDNSConfig(serverWithExtras(map[string]string{
		"primary": "yes", "allow-axfr-ips": "198.51.100.7/32",
	})))
	if valueEdit == with {
		t.Errorf("a value-only extraSettings edit must move the config hash (both %q)", with)
	}
}

// Keys the operator writes itself must be refused, not merged: an override
// of launch/gpgsql-*/api-* silently breaks backend wiring, API access or
// the probes while the CR still reports Ready.
func TestValidateExtraSettingsRejectsReserved(t *testing.T) {
	reserved := []string{
		"launch",
		"local-address",
		"local-port",
		"webserver",
		"webserver-address",
		"webserver-port",
		"webserver-allow-from",
		"api",
		"api-key",
		"default-soa-content",
		"gpgsql-host",
		"gpgsql-password",
	}
	for _, key := range reserved {
		t.Run(key, func(t *testing.T) {
			msgs := ValidateExtraSettings(map[string]string{key: "whatever"})
			if len(msgs) != 1 {
				t.Fatalf("expected exactly 1 rejection for %q, got %d: %v", key, len(msgs), msgs)
			}
			if !strings.Contains(msgs[0], "is managed by the operator") {
				t.Errorf("rejection of %q should say it is operator-managed, got %q", key, msgs[0])
			}
			if !strings.Contains(msgs[0], key) {
				t.Errorf("rejection message should name the key %q, got %q", key, msgs[0])
			}
		})
	}

	// The whole point of the field: these must pass.
	for _, key := range []string{"primary", "allow-axfr-ips", "also-notify", "default-ttl"} {
		t.Run("allowed/"+key, func(t *testing.T) {
			if msgs := ValidateExtraSettings(map[string]string{key: "yes"}); len(msgs) != 0 {
				t.Errorf("%q must be allowed, got %v", key, msgs)
			}
		})
	}
}

// Keys outside `^[a-z0-9][a-z0-9-]*$` and values carrying a newline are the
// two shapes that can smuggle EXTRA lines into pdns.conf.
func TestValidateExtraSettingsRejectsMalformed(t *testing.T) {
	for _, key := range []string{"Bad_Key", "has space", "-leading", "", "a=b"} {
		t.Run("key/"+key, func(t *testing.T) {
			msgs := ValidateExtraSettings(map[string]string{key: "x"})
			if len(msgs) != 1 {
				t.Fatalf("expected exactly 1 rejection for key %q, got %d: %v", key, len(msgs), msgs)
			}
			if !strings.Contains(msgs[0], "must match") {
				t.Errorf("rejection of key %q should cite the key pattern, got %q", key, msgs[0])
			}
		})
	}

	for name, value := range map[string]string{
		"LF":   "yes\nlaunch=bind",
		"CR":   "yes\rlaunch=bind",
		"CRLF": "yes\r\nlaunch=bind",
	} {
		t.Run("value/"+name, func(t *testing.T) {
			msgs := ValidateExtraSettings(map[string]string{"inject": value})
			if len(msgs) != 1 {
				t.Fatalf("expected exactly 1 rejection for a %s in the value, got %d: %v", name, len(msgs), msgs)
			}
			if !strings.Contains(msgs[0], "must not contain newlines") {
				t.Errorf("rejection should cite the newline, got %q", msgs[0])
			}
		})
	}

	// Several rejections at once must come back sorted by key — the message
	// list ends up in status/events, and map order would make it churn.
	msgs := ValidateExtraSettings(map[string]string{
		"zz-newline":  "a\nb",
		"launch":      "bind",
		"Bad_Key":     "x",
		"gpgsql-host": "elsewhere",
		"primary":     "yes", // valid — contributes no message
	})
	if len(msgs) != 4 {
		t.Fatalf("expected 4 rejections (primary is valid), got %d: %v", len(msgs), msgs)
	}
	keys := make([]string, 0, len(msgs))
	for _, m := range msgs {
		keys = append(keys, quotedKey(m))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			t.Errorf("messages must be sorted by key, got %v (from %v)", keys, msgs)
			break
		}
	}
}

// Rejection is PER ENTRY: one bad new key must not retract the settings
// that are already live (a typo must not silently switch off primary=yes
// and stop AXFR), and the bad ones must never reach the file.
func TestRenderExtraSettingsDropsOnlyRejected(t *testing.T) {
	conf := PDNSConfig(serverWithExtras(map[string]string{
		"ok-setting":  "1",
		"launch":      "bind",
		"gpgsql-host": "attacker.example",
		"Bad_Key":     "x",
	}))

	if lineIndex(conf, "ok-setting=1") < 0 {
		t.Errorf("the one valid entry must still be rendered:\n%s", conf)
	}
	for _, banned := range []string{"launch=bind", "gpgsql-host=attacker.example", "Bad_Key"} {
		if strings.Contains(conf, banned) {
			t.Errorf("rejected entry %q must never reach pdns.conf:\n%s", banned, conf)
		}
	}
	if got := countLinesWithPrefix(conf, "launch="); got != 1 {
		t.Errorf("pdns.conf must keep exactly one launch= line (the managed one), got %d:\n%s", got, conf)
	}
	if got := countLinesWithPrefix(conf, "gpgsql-"); got != 0 {
		t.Errorf("no gpgsql-* line may be rendered into pdns.conf (they come from the Secret via flags), got %d:\n%s",
			got, conf)
	}
	// Sanity: the header appears exactly once even though most entries were
	// dropped — it is emitted lazily by the first ACCEPTED entry.
	if got := strings.Count(conf, "# spec.extraSettings"); got != 1 {
		t.Errorf("expected exactly one extraSettings header, got %d:\n%s", got, conf)
	}
}
