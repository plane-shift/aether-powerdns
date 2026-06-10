package pdnsclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient returns a Client pointed at an httptest server that first
// enforces the API key, then delegates to fn.
func newTestClient(t *testing.T, fn http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "secret")
}

func TestGetZone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/servers/localhost/zones/example.com." {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"example.com.","name":"example.com.","kind":"Native","serial":2026061101,"dnssec":false}`))
	})
	z, err := c.GetZone(context.Background(), "example.com.")
	if err != nil {
		t.Fatalf("GetZone: %v", err)
	}
	if z.Kind != "Native" || z.Serial != 2026061101 {
		t.Errorf("unexpected zone: %+v", z)
	}
}

func TestGetZoneNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.GetZone(context.Background(), "missing.example.")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateZoneSendsNameserversField(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/servers/localhost/zones" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["nameservers"]; !ok {
			t.Error("nameservers field missing — PowerDNS requires it on create")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"example.com.","name":"example.com.","kind":"Native","serial":1}`))
	})
	z, err := c.CreateZone(context.Background(), &Zone{Name: "example.com.", Kind: "Native", Nameservers: []string{}})
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if z.Serial != 1 {
		t.Errorf("unexpected created zone: %+v", z)
	}
}

func TestPatchRRSets(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/servers/localhost/zones/example.com." {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RRSets []RRSet `json:"rrsets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(body.RRSets) != 1 || body.RRSets[0].ChangeType != "REPLACE" || body.RRSets[0].Records[0].Content != "203.0.113.10" {
			t.Errorf("unexpected rrsets payload: %+v", body.RRSets)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	err := c.PatchRRSets(context.Background(), "example.com.", []RRSet{{
		Name: "www.example.com.", Type: "A", TTL: 300, ChangeType: "REPLACE",
		Records: []Record{{Content: "203.0.113.10"}},
	}})
	if err != nil {
		t.Fatalf("PatchRRSets: %v", err)
	}
}

func TestDeleteZoneNotFoundIsSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.DeleteZone(context.Background(), "gone.example."); err != nil {
		t.Fatalf("DeleteZone on 404 should succeed, got %v", err)
	}
}

func TestCryptokeyLifecycle(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/servers/localhost/zones/example.com./cryptokeys":
			_, _ = w.Write([]byte(`[{"id":7,"keytype":"csk","active":true,"ds":["12345 13 2 deadbeef"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/servers/localhost/zones/example.com./cryptokeys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":8,"keytype":"csk","active":true,"ds":["67890 13 2 cafef00d"]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/servers/localhost/zones/example.com./cryptokeys/7":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()
	keys, err := c.ListCryptokeys(ctx, "example.com.")
	if err != nil || len(keys) != 1 || keys[0].ID != 7 {
		t.Fatalf("ListCryptokeys: keys=%+v err=%v", keys, err)
	}
	created, err := c.CreateCryptokey(ctx, "example.com.", Cryptokey{KeyType: "csk", Active: true})
	if err != nil || created.ID != 8 {
		t.Fatalf("CreateCryptokey: key=%+v err=%v", created, err)
	}
	k := keys[0]
	k.Active = false
	if err := c.UpdateCryptokey(ctx, "example.com.", k); err != nil {
		t.Fatalf("UpdateCryptokey: %v", err)
	}
}

func TestErrorIncludesResponseBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"Nameservers list must be given"}`))
	})
	_, err := c.CreateZone(context.Background(), &Zone{Name: "bad.example.", Nameservers: []string{}})
	if err == nil || !strings.Contains(err.Error(), "Nameservers list must be given") {
		t.Fatalf("error should carry the API message, got %v", err)
	}
}

func TestTransportErrorIsUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1", "secret") // nothing listens on port 1
	_, err := c.GetZone(context.Background(), "example.com.")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}
