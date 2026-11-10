package adminapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

func testSnapshotForRender() *snapshot.Snapshot {
	alpha := &snapshot.Route{
		ID: 1, Name: "demo-alpha", PathPrefix: "/alpha", StripPrefix: true,
		Targets: []snapshot.Target{{Host: "demo-alpha", Port: 8080, Weight: 1}},
		Operations: []*snapshot.Operation{
			{ID: 2, RouteID: 1, Method: "GET", Path: "/admin/**"},
			{ID: 1, RouteID: 1, Method: "GET", Path: "/v1/**", Name: "alpha reads"},
		},
	}
	beta := &snapshot.Route{
		ID: 2, Name: "demo-beta", PathPrefix: "/beta", StripPrefix: true,
		Targets: []snapshot.Target{{Host: "demo-beta", Port: 8080}},
		Operations: []*snapshot.Operation{
			{ID: 5, RouteID: 2, Method: "GET", Path: "/v1/**", Name: "beta reads"},
		},
	}
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := &snapshot.Snapshot{
		Revision: 7, LoadedAt: time.Now(),
		Routes: []*snapshot.Route{alpha, beta},
		Policies: map[int64][]*snapshot.Policy{
			1: {
				// Loader ORDER BY id: per-route slices arrive in id order.
				{ID: 1, RouteID: 1, Caller: "demo-caller", Method: "GET", Segments: []string{"admin", "**"}, PathPattern: "/admin/**", Effect: "deny", Deny: true},
				{ID: 2, RouteID: 1, Caller: "demo-caller", Method: "GET", Segments: []string{"v1", "**"}, PathPattern: "/v1/**", Effect: "allow"},
				{ID: 3, RouteID: 1, Caller: "demo-beta", Method: "GET", Segments: []string{"v1", "**"}, PathPattern: "/v1/**", Effect: "allow", ExpiresAt: &exp, Remark: "temporary"},
			},
			2: {
				{ID: 4, RouteID: 2, Caller: "demo-alpha", Method: "GET", Segments: []string{"v1", "**"}, PathPattern: "/v1/**", Effect: "allow"},
			},
		},
	}
	snapshot.SortRoutes(snap)
	return snap
}

// TestRenderSnapshotComplete exercises the full status document: routes carry
// ids/methods/weighted targets/nested operations; policies carry raw source
// metadata sorted by id; identity metadata is exposed without certificate
// material.
func TestRenderSnapshotComplete(t *testing.T) {
	doc := renderSnapshot(testSnapshotForRender())
	if doc["loaded"] != true {
		t.Fatal("loaded must be true")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	routes := out["routes"].([]any)
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	first := routes[0].(map[string]any)
	if first["id"].(float64) != 1 || first["name"] != "demo-alpha" {
		t.Fatalf("first route = %+v", first)
	}
	if len(first["operations"].([]any)) != 2 {
		t.Fatal("demo-alpha operations missing")
	}
	op := first["operations"].([]any)[0].(map[string]any)
	if op["path"] != "/admin/**" || op["method"] != "GET" {
		t.Fatalf("operations not sorted by method/path: %+v", op)
	}

	policies := out["policies"].([]any)
	if len(policies) != 4 {
		t.Fatalf("policies = %d, want 4", len(policies))
	}
	if policies[0].(map[string]any)["id"].(float64) != 1 {
		t.Fatalf("policies not ordered by id: %+v", policies[0])
	}
	deny := policies[0].(map[string]any)
	if deny["path_pattern"] != "/admin/**" || deny["effect"] != "deny" {
		t.Fatalf("raw policy metadata missing: %+v", deny)
	}
	if _, ok := deny["state"].(map[string]any)["deny"].(bool); !ok {
		t.Fatal("effective state missing")
	}
	withExpiry := policies[2].(map[string]any)
	if withExpiry["expires_at"] != "2027-01-01T00:00:00Z" || withExpiry["remark"] != "temporary" {
		t.Fatalf("policy expiry/remark missing: %+v", withExpiry)
	}

	ids := out["identities"].([]any)
	if len(ids) != 0 {
		t.Fatalf("no identities expected in this fixture, got %d", len(ids))
	}
	if out["operations_count"].(float64) != 3 {
		t.Fatalf("operations_count = %v, want 3", out["operations_count"])
	}
}

// TestRenderSnapshotIdentityMetadataNoPEM: identity metadata must expose
// name/key ids/validity only — never certificate material.
func TestRenderSnapshotIdentityMetadataNoPEM(t *testing.T) {
	snap := testSnapshotForRender()
	snap.Identities = map[string]*snapshot.Identity{
		"fp-b": {Name: "svc-b", KeyID: "bbb", NotAfter: time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)},
		"fp-a": {Name: "svc-a", KeyID: "aaa", NotAfter: time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)},
	}
	doc := renderSnapshot(snap)
	raw, _ := json.Marshal(doc)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	ids := out["identities"].([]any)
	if len(ids) != 2 {
		t.Fatalf("identities = %d, want 2", len(ids))
	}
	if ids[0].(map[string]any)["name"] != "svc-a" || ids[1].(map[string]any)["name"] != "svc-b" {
		t.Fatalf("identities not sorted by name: %+v", ids)
	}
	if string(raw) == "" || containsAny(string(raw), []string{"-----BEGIN", "CERTIFICATE", "PRIVATE"}) {
		t.Fatal("certificate material leaked into snapshot document")
	}
	names := out["identity_names"].([]any)
	if names[0] != "svc-a" {
		t.Fatalf("identity_names not sorted: %v", names)
	}
}

// TestRenderSnapshotNilHolder covers the fail-closed unloaded document.
func TestRenderSnapshotNilHolder(t *testing.T) {
	doc := renderSnapshot(nil)
	if doc["loaded"] != false {
		t.Fatalf("nil snapshot must render loaded=false, got %v", doc["loaded"])
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
