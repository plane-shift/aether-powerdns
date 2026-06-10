// Package pdnsclient is a thin client for the PowerDNS authoritative HTTP
// API, wrapping exactly the endpoints the operator uses. In-repo on
// purpose: importing a third-party PowerDNS library would couple our
// release cadence to theirs (same reasoning as building the CNPG Cluster
// as unstructured).
package pdnsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when PowerDNS reports 404 for the target object.
var ErrNotFound = errors.New("pdns: not found")

// ErrUnreachable wraps transport-level failures (connection refused, DNS,
// timeout) so callers can distinguish "the API said no" from "I never
// reached the API" — the latter usually means a NetworkPolicy or a
// not-yet-ready server.
var ErrUnreachable = errors.New("pdns: unreachable")

// Client talks to one PowerDNS server's HTTP API.
type Client struct {
	baseURL string
	apiKey  string
	httpc   *http.Client
}

// New builds a Client for the API at baseURL (e.g. the PowerDNSServer's
// status.apiEndpoint, `http://<name>-api.<ns>.svc:8081`).
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Zone mirrors the subset of the PowerDNS zone object the operator uses.
type Zone struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
	Serial int64  `json:"serial,omitempty"`
	// Masters lists primaries for Secondary zones.
	Masters []string `json:"masters,omitempty"`
	// Nameservers must be PRESENT (possibly empty) on zone creation —
	// PowerDNS 422s otherwise. No omitempty, and callers pass []string{}
	// rather than nil.
	Nameservers []string `json:"nameservers"`
	DNSSEC      bool     `json:"dnssec,omitempty"`
	RRSets      []RRSet  `json:"rrsets,omitempty"`
}

// RRSet is one record set in a PATCH payload or GET response.
type RRSet struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	TTL        int32    `json:"ttl,omitempty"`
	ChangeType string   `json:"changetype,omitempty"` // REPLACE or DELETE
	Records    []Record `json:"records,omitempty"`
}

// Record is one record within an RRSet.
type Record struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

// Cryptokey is a DNSSEC key. DS is only populated by PowerDNS responses.
type Cryptokey struct {
	ID      int      `json:"id,omitempty"`
	KeyType string   `json:"keytype,omitempty"`
	Active  bool     `json:"active"`
	DS      []string `json:"ds,omitempty"`
}

// ZoneUpdate carries the mutable zone metadata for PUT /zones/{id}.
type ZoneUpdate struct {
	Kind    string   `json:"kind"`
	Masters []string `json:"masters"`
}

func zonePath(name string) string {
	return "/servers/localhost/zones/" + url.PathEscape(name)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pdns: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// GetZone fetches one zone (metadata only is fine for our use; PowerDNS
// includes rrsets, which callers may ignore).
func (c *Client) GetZone(ctx context.Context, name string) (*Zone, error) {
	z := &Zone{}
	if err := c.do(ctx, http.MethodGet, zonePath(name), nil, z); err != nil {
		return nil, err
	}
	return z, nil
}

// CreateZone registers a new zone and returns PowerDNS's view of it.
func (c *Client) CreateZone(ctx context.Context, z *Zone) (*Zone, error) {
	created := &Zone{}
	if err := c.do(ctx, http.MethodPost, "/servers/localhost/zones", z, created); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateZone PUTs mutable zone metadata (kind, masters).
func (c *Client) UpdateZone(ctx context.Context, name string, u ZoneUpdate) error {
	if u.Masters == nil {
		u.Masters = []string{}
	}
	return c.do(ctx, http.MethodPut, zonePath(name), u, nil)
}

// DeleteZone removes a zone. A 404 counts as success — the desired state
// (zone gone) already holds.
func (c *Client) DeleteZone(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, zonePath(name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// PatchRRSets applies rrset changes (changetype REPLACE/DELETE) to a zone.
func (c *Client) PatchRRSets(ctx context.Context, zone string, rrsets []RRSet) error {
	payload := struct {
		RRSets []RRSet `json:"rrsets"`
	}{rrsets}
	return c.do(ctx, http.MethodPatch, zonePath(zone), payload, nil)
}

// ListCryptokeys returns the zone's DNSSEC keys (DS included, private
// material omitted by PowerDNS).
func (c *Client) ListCryptokeys(ctx context.Context, zone string) ([]Cryptokey, error) {
	var keys []Cryptokey
	if err := c.do(ctx, http.MethodGet, zonePath(zone)+"/cryptokeys", nil, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// CreateCryptokey adds a key; an active key secures the zone.
func (c *Client) CreateCryptokey(ctx context.Context, zone string, k Cryptokey) (*Cryptokey, error) {
	created := &Cryptokey{}
	if err := c.do(ctx, http.MethodPost, zonePath(zone)+"/cryptokeys", k, created); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateCryptokey PUTs a key, typically to flip Active.
func (c *Client) UpdateCryptokey(ctx context.Context, zone string, k Cryptokey) error {
	return c.do(ctx, http.MethodPut, zonePath(zone)+"/cryptokeys/"+strconv.Itoa(k.ID), k, nil)
}

// RectifyZone recomputes DNSSEC ordering/NSEC data after key changes.
func (c *Client) RectifyZone(ctx context.Context, zone string) error {
	return c.do(ctx, http.MethodPut, zonePath(zone)+"/rectify", nil, nil)
}
