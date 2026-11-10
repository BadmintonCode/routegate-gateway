package proxy

import (
	"strings"

	"github.com/BadmintonCode/routegate-gateway/internal/authz"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// TestAccess resolves what the data plane would decide for caller/method/path
// against the given snapshot. It is the single evaluation path shared with
// ServeHTTP, so the admin policy tester cannot drift from real enforcement.
// Returns the matched route name, the decision ("allow", "deny", or
// "route_not_found"), the matched policy id, and the sub path after prefix
// strip (the operation path policies match against).
func TestAccess(snap *snapshot.Snapshot, caller, method, rawPath string) (routeName, decision string, policyID int64, sub string) {
	if snap == nil {
		return "", "deny", 0, ""
	}
	normalized := authz.NormalizePath(rawPath)
	var route *snapshot.Route
	for _, cand := range snap.Routes {
		if matchPrefix(cand.PathPrefix, normalized) && methodAllowed(cand, method) {
			route = cand
			break
		}
	}
	if route == nil {
		return "", "route_not_found", 0, ""
	}
	sub = normalized
	if route.StripPrefix {
		sub = strings.TrimPrefix(normalized, route.PathPrefix)
		if sub == "" {
			sub = "/"
		}
	}
	d, id := evaluate(snap, route, caller, method, sub)
	return route.Name, d, id, sub
}
