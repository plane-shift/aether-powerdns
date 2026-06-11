package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "github.com/plane-shift/aether-powerdns/api/v1alpha1"
	"github.com/plane-shift/aether-powerdns/internal/pdnsclient"
)

const testAPIKey = "test-api-key"

// fakePDNS is an in-memory PowerDNS API good enough for the reconcilers:
// zones CRUD, rrset PATCH, cryptokeys, rectify.
type fakePDNS struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	zones     map[string]*pdnsclient.Zone              // by dotted name
	rrsets    map[string]map[string]pdnsclient.RRSet   // zone -> "name|type"
	keys      map[string][]pdnsclient.Cryptokey        // zone -> keys
	nextKeyID int
	failNextPatch bool // when set, the next PATCH returns 500 and clears the flag
}

func newFakePDNS(t *testing.T) *fakePDNS {
	f := &fakePDNS{
		t:         t,
		zones:     map[string]*pdnsclient.Zone{},
		rrsets:    map[string]map[string]pdnsclient.RRSet{},
		keys:      map[string][]pdnsclient.Cryptokey{},
		nextKeyID: 1,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePDNS) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != testAPIKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const prefix = "/api/v1/servers/localhost/zones"
	path := strings.TrimPrefix(r.URL.Path, prefix)

	f.mu.Lock()
	defer f.mu.Unlock()

	// POST /zones
	if path == "" && r.Method == http.MethodPost {
		z := &pdnsclient.Zone{}
		if err := json.NewDecoder(r.Body).Decode(z); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if z.Nameservers == nil {
			http.Error(w, `{"error":"Nameservers list must be given"}`, http.StatusUnprocessableEntity)
			return
		}
		z.ID, z.Serial = z.Name, 1
		f.zones[z.Name] = z
		f.rrsets[z.Name] = map[string]pdnsclient.RRSet{}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(z)
		return
	}

	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	zoneName, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	z, ok := f.zones[zoneName]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		zc := *z
		zc.RRSets = nil
		for _, rr := range f.rrsets[zoneName] {
			zc.RRSets = append(zc.RRSets, rr)
		}
		_ = json.NewEncoder(w).Encode(&zc)
	case len(parts) == 1 && r.Method == http.MethodPut:
		var u pdnsclient.ZoneUpdate
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		z.Kind, z.Masters = u.Kind, u.Masters
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		delete(f.zones, zoneName)
		delete(f.rrsets, zoneName)
		delete(f.keys, zoneName)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 1 && r.Method == http.MethodPatch:
		if f.failNextPatch {
			f.failNextPatch = false
			http.Error(w, `{"error":"injected failure"}`, http.StatusInternalServerError)
			return
		}
		var body struct {
			RRSets []pdnsclient.RRSet `json:"rrsets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, rr := range body.RRSets {
			key := rr.Name + "|" + rr.Type
			switch rr.ChangeType {
			case "REPLACE":
				rr.ChangeType = ""
				f.rrsets[zoneName][key] = rr
			case "DELETE":
				delete(f.rrsets[zoneName], key)
			}
		}
		z.Serial++
		w.WriteHeader(http.StatusNoContent)
	case len(parts) >= 2 && parts[1] == "rectify" && r.Method == http.MethodPut:
		_, _ = w.Write([]byte(`{"result":"Rectified"}`))
	case len(parts) == 2 && parts[1] == "cryptokeys" && r.Method == http.MethodGet:
		keys := f.keys[zoneName]
		if keys == nil {
			keys = []pdnsclient.Cryptokey{}
		}
		_ = json.NewEncoder(w).Encode(keys)
	case len(parts) == 2 && parts[1] == "cryptokeys" && r.Method == http.MethodPost:
		var k pdnsclient.Cryptokey
		if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		k.ID = f.nextKeyID
		f.nextKeyID++
		k.DS = []string{strconv.Itoa(k.ID*11111) + " 13 2 deadbeef"}
		f.keys[zoneName] = append(f.keys[zoneName], k)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(k)
	case len(parts) == 3 && parts[1] == "cryptokeys" && r.Method == http.MethodPut:
		id, err := strconv.Atoi(parts[2])
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var k pdnsclient.Cryptokey
		if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for i := range f.keys[zoneName] {
			if f.keys[zoneName][i].ID == id {
				f.keys[zoneName][i].Active = k.Active
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("fakePDNS: unhandled %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// getRRSet returns the stored rrset and whether it exists.
func (f *fakePDNS) getRRSet(zone, name, typ string) (pdnsclient.RRSet, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rr, ok := f.rrsets[zone][name+"|"+typ]
	return rr, ok
}

// hasZone reports whether the zone exists.
func (f *fakePDNS) hasZone(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.zones[name]
	return ok
}

// seedZone pre-creates a zone, as if made out of band.
func (f *fakePDNS) seedZone(name, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones[name] = &pdnsclient.Zone{ID: name, Name: name, Kind: kind, Serial: 1, Nameservers: []string{}}
	f.rrsets[name] = map[string]pdnsclient.RRSet{}
	f.keys[name] = []pdnsclient.Cryptokey{}
}

// zoneKeys returns a copy of the zone's cryptokeys.
func (f *fakePDNS) zoneKeys(name string) []pdnsclient.Cryptokey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pdnsclient.Cryptokey, len(f.keys[name]))
	copy(out, f.keys[name])
	return out
}

// readyServer builds a Ready PowerDNSServer wired to the fake, plus its
// API-key Secret — the two objects every reconciler test needs.
func readyServer(name, ns string, f *fakePDNS) (*dnsv1alpha1.PowerDNSServer, *corev1.Secret) {
	server := &dnsv1alpha1.PowerDNSServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: dnsv1alpha1.PowerDNSServerStatus{
			Phase:            dnsv1alpha1.PhaseReady,
			APIEndpoint:      f.srv.URL,
			APIKeySecretName: name + "-api-key",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-api-key", Namespace: ns},
		Data:       map[string][]byte{"api-key": []byte(testAPIKey)},
	}
	return server, secret
}
