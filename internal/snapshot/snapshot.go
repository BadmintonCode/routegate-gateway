// Package snapshot holds the in-memory configuration snapshot served by the
// data plane. Snapshots are built by the sync worker from MySQL and swapped
// atomically; the request hot path never touches the database (ADR-0001 §3).
package snapshot

import (
	"crypto/x509"
	"sort"
	"sync/atomic"
	"time"
)

// Target is one upstream destination of a route.
type Target struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Weight int    `json:"weight,omitempty"`
}

// Route is one explicit-prefix entry of the routing table.
type Route struct {
	ID             int64
	Name           string
	PathPrefix     string
	Methods        []string // nil = all methods
	Targets        []Target
	StripPrefix    bool
	Operations     []*Operation // registered authoring metadata; inert for proxy matching
	SuccessRule    *SuccessRule // per-route success verdict over the proxied response; nil = no verdict
	BizCodePath    string       // optional dotted path to the business status code in the response body
	BizMessagePath string       // optional dotted path to the business message in the response body
}

// SuccessRule decides whether a proxied request counts as success
// (ADR-0001 §6.7): all (AND, default) or any (OR) of the conditions must
// hold. Conditions compare the HTTP status or a response-body JSON field
// against a value list with in / not_in semantics.
type SuccessRule struct {
	Mode       string              `json:"mode"` // all | any
	Conditions []*SuccessCondition `json:"conditions"`
}

// SuccessCondition is one matcher of a SuccessRule. Source is "status" (HTTP
// status code) or "body" (dotted JSON path into the response body). Op is
// "in" or "not_in"; Values is the comparison list (a single value is a
// length-1 list). Values are compared as strings — JSON scalars are
// stringified (0 ↔ "0"), objects/arrays as compact JSON.
type SuccessCondition struct {
	Source string   `json:"source"`
	Path   string   `json:"path,omitempty"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// Operation is one registered operation of a route (ADR-0001 §4): authoring
// metadata describing what the upstream supports. It never participates in
// proxy matching — enforcement comes solely from policies.
type Operation struct {
	ID      int64
	RouteID int64
	Method  string
	Path    string
	Name    string
	Remark  string
}

// Policy is one authorization rule anchored to a route. Segments is the parsed
// path pattern (ADR-0001 §6.1): '*' matches exactly one segment, '**' (last
// position only) matches one or more. The raw source fields (PathPattern,
// Effect, ExpiresAt, Remark) are retained for observability; enforcement uses
// Method/Segments/Deny.
type Policy struct {
	ID          int64
	RouteID     int64
	Caller      string // '*' allowed only on temporary rules (expires_at set)
	Method      string // '*' or uppercase HTTP method
	Segments    []string
	Deny        bool
	PathPattern string     // raw source pattern
	Effect      string     // raw source effect: allow | deny
	ExpiresAt   *time.Time // nil = permanent; always in the future (expired rows are dropped at load)
	Remark      string
}

// Identity is a verified workload identity material entry. For mtls_cert the
// KeyID is the SHA-256 fingerprint (hex) of the leaf certificate.
type Identity struct {
	Name        string
	KeyID       string
	Cert        *x509.Certificate
	Fingerprint string // hex sha256 of Cert.Raw; empty for non-cert types
	NotBefore   time.Time
	NotAfter    time.Time
}

// Snapshot is an immutable view of configuration.
type Snapshot struct {
	Revision   int64
	LoadedAt   time.Time
	Routes     []*Route // sorted by PathPrefix length descending
	Policies   map[int64][]*Policy
	Identities map[string]*Identity // fingerprint hex -> identity (mtls_cert only)
}

// RouteByName returns the route with the given name, if present.
func (s *Snapshot) RouteByName(name string) *Route {
	for _, r := range s.Routes {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// SortRoutes orders routes longest-prefix-first (the loader's ordering rule),
// exposed for tests constructing snapshots by hand.
func SortRoutes(s *Snapshot) {
	sort.Slice(s.Routes, func(i, j int) bool {
		if len(s.Routes[i].PathPrefix) != len(s.Routes[j].PathPrefix) {
			return len(s.Routes[i].PathPrefix) > len(s.Routes[j].PathPrefix)
		}
		return s.Routes[i].PathPrefix < s.Routes[j].PathPrefix
	})
}

// Holder stores the current snapshot behind an atomic pointer. Reads are
// lock-free on the hot path; a load either fully succeeds or leaves the old
// snapshot in place (fail-static).
type Holder struct {
	ptr atomic.Pointer[Snapshot]
}

// Store swaps in a fully built snapshot.
func (h *Holder) Store(s *Snapshot) { h.ptr.Store(s) }

// Load returns the current snapshot, or nil before the first successful load.
func (h *Holder) Load() *Snapshot { return h.ptr.Load() }
