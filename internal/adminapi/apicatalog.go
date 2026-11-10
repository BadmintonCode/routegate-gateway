package adminapi

import (
	"net/http"
	"sort"
	"strings"
)

// The endpoint catalog is the hand-maintained documentation backbone of the
// admin API: one entry for EVERY route registered on the api mux in
// server.go. Both GET /admin/api/v1/meta/endpoints and GET
// /admin/api/v1/openapi.json render from this single list, and
// apicatalog_test.go counts the apiMux.HandleFunc registrations in server.go
// and fails when a registered route is missing here — so every new route must
// land together with its catalog row.
//
// Auth encodes how a request can authenticate:
//   - "session+token": session cookie OR admin API token (rgt_…), i.e. every
//     requireAuth / requireSuperadmin route;
//   - "session": cookie only (logout — a Bearer header is ignored there);
//   - "none": no authentication (login).
//
// Summaries are plain English sentences; params records the actual query and
// path parameters the handlers read; BodyExample is a minimal realistic
// request body (nil when the endpoint reads none).

// APIEndpointParam describes one documented parameter of an endpoint.
type APIEndpointParam struct {
	Name     string `json:"name"`
	In       string `json:"in"`   // "path" | "query" | "body"
	Type     string `json:"type"` // "string" | "integer" | "boolean" | "object"
	Required bool   `json:"required"`
	Desc     string `json:"desc"`
}

// APIEndpoint is one catalog row and the unit both meta endpoints render.
type APIEndpoint struct {
	Group       string             `json:"group"`
	Method      string             `json:"method"`
	Path        string             `json:"path"`
	Auth        string             `json:"auth"`
	Summary     string             `json:"summary"`
	Params      []APIEndpointParam `json:"params,omitempty"`
	BodyExample any                `json:"body_example,omitempty"`
	// ContentType overrides the OpenAPI 200-response media type for
	// non-JSON endpoints (empty = application/json). It is not serialized
	// into meta/endpoints — the response shape stays stable.
	ContentType string `json:"-"`
}

// knownGroups / knownAuth bound the catalog values the tests assert against.
var knownGroups = map[string]bool{
	"auth": true, "users": true, "routes": true, "policies": true,
	"identities": true, "audit": true, "logs": true, "gateway": true,
	"settings": true, "alerts": true, "meta": true,
	"llm": true, "assistant": true,
}

var knownAuth = map[string]bool{
	"session+token": true,
	"session":       true,
	"none":          true,
}

// pathParam builds the required {id}-style path parameter row.
func pathParam(name, desc string) APIEndpointParam {
	return APIEndpointParam{Name: name, In: "path", Type: "integer", Required: true, Desc: desc}
}

// queryParam builds an optional query parameter row.
func queryParam(name, typ, desc string) APIEndpointParam {
	return APIEndpointParam{Name: name, In: "query", Type: typ, Required: false, Desc: desc}
}

// revisionBody is the optimistic-concurrency body several deletes accept.
func revisionBody() map[string]any {
	return map[string]any{"expected_revision": 0}
}

// apiCatalog lists every admin API endpoint, grouped by feature area.
var apiCatalog = []APIEndpoint{
	// --- auth ---
	{
		Group: "auth", Method: "POST", Path: "/admin/api/v1/auth/login", Auth: "none",
		Summary: "Exchange a username and password for an admin session cookie",
		Params: []APIEndpointParam{
			{Name: "username", In: "body", Type: "string", Required: true, Desc: "admin username"},
			{Name: "password", In: "body", Type: "string", Required: true, Desc: "admin password"},
		},
		BodyExample: map[string]any{"username": "admin", "password": "correct horse"},
	},
	{
		Group: "auth", Method: "POST", Path: "/admin/api/v1/auth/logout", Auth: "session",
		Summary: "Revoke the current session and clear the session cookie",
	},
	{
		Group: "auth", Method: "GET", Path: "/admin/api/v1/auth/me", Auth: "session+token",
		Summary: "Return the authenticated identity's username, role, and password-change flag",
	},
	{
		Group: "auth", Method: "GET", Path: "/admin/api/v1/auth/tokens", Auth: "session+token",
		Summary: "List the caller's own admin API tokens, including revoked and expired ones",
	},
	{
		Group: "auth", Method: "POST", Path: "/admin/api/v1/auth/tokens", Auth: "session+token",
		Summary: "Mint a new admin API token acting as the calling user; the raw secret is returned once",
		Params: []APIEndpointParam{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "display name, 1..64 chars after trim"},
			{Name: "expires_in_days", In: "body", Type: "integer", Required: true, Desc: "token lifetime in days, 1..365"},
		},
		BodyExample: map[string]any{"name": "ci-bot", "expires_in_days": 30},
	},
	{
		Group: "auth", Method: "DELETE", Path: "/admin/api/v1/auth/tokens/{id}", Auth: "session+token",
		Summary: "Revoke one of the caller's own admin API tokens",
		Params:  []APIEndpointParam{pathParam("id", "token id to revoke; must belong to the caller")},
	},

	// --- users ---
	{
		Group: "users", Method: "POST", Path: "/admin/api/v1/users/me/password", Auth: "session+token",
		Summary: "Change the calling user's own password (current password required unless the login forced a change)",
		Params: []APIEndpointParam{
			{Name: "current", In: "body", Type: "string", Required: false, Desc: "current password, required unless must_change_password"},
			{Name: "next", In: "body", Type: "string", Required: true, Desc: "new password, at least 10 chars"},
		},
		BodyExample: map[string]any{"current": "correct horse", "next": "newer stable phrase"},
	},
	{
		Group: "users", Method: "GET", Path: "/admin/api/v1/users", Auth: "session+token",
		Summary: "List every admin user (superadmin only)",
	},
	{
		Group: "users", Method: "POST", Path: "/admin/api/v1/users", Auth: "session+token",
		Summary: "Create an admin user (superadmin only)",
		Params: []APIEndpointParam{
			{Name: "username", In: "body", Type: "string", Required: true, Desc: "unique username, max 128 chars"},
			{Name: "password", In: "body", Type: "string", Required: true, Desc: "initial password, at least 10 chars"},
			{Name: "display_name", In: "body", Type: "string", Required: false, Desc: "friendly display name"},
			{Name: "role", In: "body", Type: "string", Required: false, Desc: "user or superadmin (default user)"},
		},
		BodyExample: map[string]any{"username": "bob", "password": "a long initial phrase", "display_name": "Bob", "role": "user"},
	},
	{
		Group: "users", Method: "PUT", Path: "/admin/api/v1/users/{id}", Auth: "session+token",
		Summary: "Update one admin user's status, role, or password (superadmin only)",
		Params: []APIEndpointParam{
			pathParam("id", "user id to update"),
			{Name: "status", In: "body", Type: "string", Required: false, Desc: "active or disabled; disabling revokes the user's sessions"},
			{Name: "role", In: "body", Type: "string", Required: false, Desc: "user or superadmin"},
			{Name: "new_password", In: "body", Type: "string", Required: false, Desc: "replacement password, at least 10 chars"},
		},
		BodyExample: map[string]any{"status": "active", "role": "user"},
	},

	// --- routes ---
	{
		Group: "routes", Method: "GET", Path: "/admin/api/v1/routes", Auth: "session+token",
		Summary: "List every route with its registered-operation count",
	},
	{
		Group: "routes", Method: "POST", Path: "/admin/api/v1/routes", Auth: "session+token",
		Summary: "Create a route (superadmin only)",
		Params: []APIEndpointParam{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "unique route name, max 128 chars"},
			{Name: "path_prefix", In: "body", Type: "string", Required: true, Desc: "normalized path prefix whitelist"},
			{Name: "targets", In: "body", Type: "object", Required: true, Desc: "upstream list like [{host,port,weight}]"},
			{Name: "methods", In: "body", Type: "object", Required: false, Desc: "restricted HTTP methods; absent means all"},
			{Name: "strip_prefix", In: "body", Type: "boolean", Required: false, Desc: "strip the matched prefix before proxying"},
			{Name: "remark", In: "body", Type: "string", Required: false, Desc: "free-text remark"},
			{Name: "expected_revision", In: "body", Type: "integer", Required: false, Desc: "optimistic concurrency revision"},
		},
		BodyExample: map[string]any{
			"name": "r1", "path_prefix": "/demo",
			"targets": []map[string]any{{"host": "upstream.test", "port": 9000, "weight": 1}},
		},
	},
	{
		Group: "routes", Method: "PUT", Path: "/admin/api/v1/routes/{id}", Auth: "session+token",
		Summary: "Update one route (superadmin only)",
		Params:  []APIEndpointParam{pathParam("id", "route id to update")},
		BodyExample: map[string]any{
			"name": "r1", "path_prefix": "/demo", "expected_revision": 0,
			"targets": []map[string]any{{"host": "upstream.test", "port": 9000, "weight": 1}},
		},
	},
	{
		Group: "routes", Method: "DELETE", Path: "/admin/api/v1/routes/{id}", Auth: "session+token",
		Summary:     "Delete one route together with its policies and registered operations (superadmin only)",
		Params:      []APIEndpointParam{pathParam("id", "route id to delete")},
		BodyExample: revisionBody(),
	},
	{
		Group: "routes", Method: "GET", Path: "/admin/api/v1/routes/{id}/operations", Auth: "session+token",
		Summary: "List the registered operations of one route",
		Params:  []APIEndpointParam{pathParam("id", "route id")},
	},
	{
		Group: "routes", Method: "POST", Path: "/admin/api/v1/routes/{id}/operations", Auth: "session+token",
		Summary: "Register an operation on one route (superadmin only)",
		Params: []APIEndpointParam{
			pathParam("id", "route id"),
			{Name: "method", In: "body", Type: "string", Required: false, Desc: "'*' or a known HTTP method (default '*')"},
			{Name: "path", In: "body", Type: "string", Required: true, Desc: "normalized path pattern, wildcards allowed"},
			{Name: "name", In: "body", Type: "string", Required: false, Desc: "operation display name"},
			{Name: "remark", In: "body", Type: "string", Required: false, Desc: "free-text remark"},
		},
		BodyExample: map[string]any{"method": "GET", "path": "/v1/orders", "name": "list orders"},
	},
	{
		Group: "routes", Method: "PUT", Path: "/admin/api/v1/operations/{id}", Auth: "session+token",
		Summary:     "Update one registered operation (superadmin only)",
		Params:      []APIEndpointParam{pathParam("id", "operation id to update")},
		BodyExample: map[string]any{"method": "GET", "path": "/v1/orders", "name": "list orders", "expected_revision": 0},
	},
	{
		Group: "routes", Method: "DELETE", Path: "/admin/api/v1/operations/{id}", Auth: "session+token",
		Summary:     "Delete one registered operation (superadmin only)",
		Params:      []APIEndpointParam{pathParam("id", "operation id to delete")},
		BodyExample: revisionBody(),
	},
	{
		Group: "routes", Method: "GET", Path: "/admin/api/v1/routes/{id}/callers", Auth: "session+token",
		Summary: "List the distinct callers of one route's enabled policies with their certificate status",
		Params:  []APIEndpointParam{pathParam("id", "route id")},
	},

	// --- policies ---
	{
		Group: "policies", Method: "GET", Path: "/admin/api/v1/policies", Auth: "session+token",
		Summary: "List every policy with its route name",
	},
	{
		Group: "policies", Method: "POST", Path: "/admin/api/v1/policies", Auth: "session+token",
		Summary: "Create a policy (superadmin only)",
		Params: []APIEndpointParam{
			{Name: "route_id", In: "body", Type: "integer", Required: true, Desc: "owning route id"},
			{Name: "caller", In: "body", Type: "string", Required: true, Desc: "identity name, or '*' for a temporary wide rule"},
			{Name: "method", In: "body", Type: "string", Required: false, Desc: "'*' or a known HTTP method (default '*')"},
			{Name: "path_pattern", In: "body", Type: "string", Required: false, Desc: "normalized pattern (default '/**')"},
			{Name: "effect", In: "body", Type: "string", Required: true, Desc: "allow or deny"},
			{Name: "expires_at", In: "body", Type: "string", Required: false, Desc: "required for wide rules; RFC3339, 'YYYY-MM-DD HH:MM:SS', or date"},
			{Name: "remark", In: "body", Type: "string", Required: false, Desc: "free-text remark"},
		},
		BodyExample: map[string]any{
			"route_id": 1, "caller": "svc-a", "method": "GET",
			"path_pattern": "/v1/**", "effect": "allow",
		},
	},
	{
		Group: "policies", Method: "PUT", Path: "/admin/api/v1/policies/{id}", Auth: "session+token",
		Summary: "Update one policy (superadmin only)",
		Params:  []APIEndpointParam{pathParam("id", "policy id to update")},
		BodyExample: map[string]any{
			"route_id": 1, "caller": "svc-a", "method": "GET",
			"path_pattern": "/v1/**", "effect": "allow", "expected_revision": 0,
		},
	},
	{
		Group: "policies", Method: "DELETE", Path: "/admin/api/v1/policies/{id}", Auth: "session+token",
		Summary:     "Delete one policy (superadmin only)",
		Params:      []APIEndpointParam{pathParam("id", "policy id to delete")},
		BodyExample: revisionBody(),
	},
	{
		Group: "policies", Method: "GET", Path: "/admin/api/v1/policies/expiring", Auth: "session+token",
		Summary: "List enabled wide or expiring policies, soonest expiry first",
	},
	{
		Group: "policies", Method: "POST", Path: "/admin/api/v1/policies/test", Auth: "session+token",
		Summary: "Evaluate a caller, method, and path against the loaded snapshot exactly like the data plane",
		Params: []APIEndpointParam{
			{Name: "caller", In: "body", Type: "string", Required: true, Desc: "identity name to test"},
			{Name: "method", In: "body", Type: "string", Required: false, Desc: "HTTP method (default GET)"},
			{Name: "path", In: "body", Type: "string", Required: true, Desc: "request path to test"},
		},
		BodyExample: map[string]any{"caller": "svc-a", "method": "GET", "path": "/v1/orders"},
	},

	// --- identities ---
	{
		Group: "identities", Method: "GET", Path: "/admin/api/v1/identities", Auth: "session+token",
		Summary: "List every client identity with its policy-use count",
	},
	{
		Group: "identities", Method: "POST", Path: "/admin/api/v1/identities", Auth: "session+token",
		Summary: "Register a client certificate as an identity (superadmin only)",
		Params: []APIEndpointParam{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "identity name referenced by policies"},
			{Name: "pem", In: "body", Type: "string", Required: true, Desc: "leaf certificate PEM"},
			{Name: "remark", In: "body", Type: "string", Required: false, Desc: "free-text remark"},
		},
		BodyExample: map[string]any{"name": "svc-a", "pem": "-----BEGIN CERTIFICATE-----\\n...\\n-----END CERTIFICATE-----\\n"},
	},
	{
		Group: "identities", Method: "POST", Path: "/admin/api/v1/identities/validate", Auth: "session+token",
		Summary:     "Parse a PEM certificate and report its subject, fingerprint, and validity window without writing anything",
		Params:      []APIEndpointParam{{Name: "pem", In: "body", Type: "string", Required: true, Desc: "certificate PEM to probe"}},
		BodyExample: map[string]any{"pem": "-----BEGIN CERTIFICATE-----\\n...\\n-----END CERTIFICATE-----\\n"},
	},
	{
		Group: "identities", Method: "POST", Path: "/admin/api/v1/identities/generate", Auth: "session+token",
		Summary: "Generate a self-signed client certificate; the private key is returned once and never stored",
		Params: []APIEndpointParam{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "identity name"},
			{Name: "days", In: "body", Type: "integer", Required: false, Desc: "validity in days (default 90, max 365)"},
			{Name: "remark", In: "body", Type: "string", Required: false, Desc: "free-text remark"},
		},
		BodyExample: map[string]any{"name": "svc-a", "days": 90},
	},
	{
		Group: "identities", Method: "POST", Path: "/admin/api/v1/identities/{id}/revoke", Auth: "session+token",
		Summary: "Revoke an active identity (superadmin only)",
		Params: []APIEndpointParam{
			pathParam("id", "identity id to revoke"),
			{Name: "reason", In: "body", Type: "string", Required: false, Desc: "revocation reason"},
		},
		BodyExample: map[string]any{"reason": "key rotated"},
	},
	{
		Group: "identities", Method: "DELETE", Path: "/admin/api/v1/identities/{id}", Auth: "session+token",
		Summary:     "Delete one identity (superadmin only)",
		Params:      []APIEndpointParam{pathParam("id", "identity id to delete")},
		BodyExample: revisionBody(),
	},

	// --- audit ---
	{
		Group: "audit", Method: "GET", Path: "/admin/api/v1/audit/admin", Auth: "session+token",
		Summary: "List admin-plane audit rows, newest first, filterable by actor and action prefix",
		Params: []APIEndpointParam{
			queryParam("actor", "string", "exact actor_name match"),
			queryParam("action", "string", "action prefix match"),
			queryParam("page", "integer", "1-based page number"),
			queryParam("page_size", "integer", "rows per page, 1..100 (default 50)"),
		},
	},
	{
		Group: "audit", Method: "GET", Path: "/admin/api/v1/audit/access", Auth: "session+token",
		Summary: "Report the access_log row count; rows require the MySQL access-log sink",
	},

	// --- logs ---
	{
		Group: "logs", Method: "GET", Path: "/admin/api/v1/logs/access", Auth: "session+token",
		Summary: "Read recent access decisions from the rotating file sink, newest first",
		Params: []APIEndpointParam{
			queryParam("limit", "integer", "max records 1..1000 (default 200)"),
			queryParam("caller", "string", "exact caller filter"),
			queryParam("decision", "string", "exact decision filter"),
			queryParam("route", "string", "exact route-name filter"),
			queryParam("outcome", "string", "exact outcome filter"),
		},
	},
	{
		Group: "logs", Method: "GET", Path: "/admin/api/v1/logs/debug", Auth: "session+token",
		Summary: "List debug-capture record metadata; payloads live behind the superadmin detail endpoint",
		Params: []APIEndpointParam{
			queryParam("limit", "integer", "max records 1..500 (default 200)"),
			queryParam("caller", "string", "exact caller filter"),
			queryParam("decision", "string", "exact decision filter"),
			queryParam("route", "string", "exact route-name filter"),
		},
	},
	{
		Group: "logs", Method: "GET", Path: "/admin/api/v1/logs/debug/{request_id}", Auth: "session+token",
		Summary: "Return one full debug record with sanitized headers, cookies, and payloads (superadmin only, audited)",
		Params:  []APIEndpointParam{{Name: "request_id", In: "path", Type: "string", Required: true, Desc: "request id of the captured record"}},
	},

	// --- gateway ---
	{
		Group: "gateway", Method: "GET", Path: "/admin/api/v1/gateway/config", Auth: "session+token",
		Summary: "Render the deployed configuration: revision, routes, policies, and identities",
	},
	{
		Group: "gateway", Method: "GET", Path: "/admin/api/v1/gateway/instances", Auth: "session+token",
		Summary: "List the running gateway instances and their sync state",
	},
	{
		Group: "gateway", Method: "GET", Path: "/admin/api/v1/gateway/snapshot", Auth: "session+token",
		Summary: "Return exactly what this instance currently enforces",
	},

	// --- settings ---
	{
		Group: "settings", Method: "GET", Path: "/admin/api/v1/settings/debug", Auth: "session+token",
		Summary: "Return the debug-logging settings and the config revision",
	},
	{
		Group: "settings", Method: "PUT", Path: "/admin/api/v1/settings/debug", Auth: "session+token",
		Summary: "Update the debug-logging settings (superadmin only); instances apply them on their next sync tick",
		Params: []APIEndpointParam{
			{Name: "enabled", In: "body", Type: "boolean", Required: true, Desc: "enable debug capture"},
			{Name: "file", In: "body", Type: "string", Required: true, Desc: "debug sink base path, required when enabled"},
			{Name: "max_mb", In: "body", Type: "integer", Required: true, Desc: "segment size cap in MB"},
			{Name: "retain_days", In: "body", Type: "integer", Required: true, Desc: "segment retention in days"},
			{Name: "request_body_max_bytes", In: "body", Type: "integer", Required: true, Desc: "per-request capture cap"},
			{Name: "response_body_max_bytes", In: "body", Type: "integer", Required: true, Desc: "per-response capture cap"},
			{Name: "headers_max_bytes", In: "body", Type: "integer", Required: true, Desc: "per-record header cap"},
			{Name: "queue_capacity", In: "body", Type: "integer", Required: true, Desc: "emit queue capacity"},
		},
		BodyExample: map[string]any{
			"enabled": true, "file": "/var/log/routegate/debug.log", "max_mb": 64, "retain_days": 7,
			"request_body_max_bytes": 65536, "response_body_max_bytes": 65536,
			"headers_max_bytes": 32768, "queue_capacity": 1024,
		},
	},
	{
		Group: "settings", Method: "GET", Path: "/admin/api/v1/settings/biz", Auth: "session+token",
		Summary: "Return the global business-envelope fallback paths",
	},
	{
		Group: "settings", Method: "PUT", Path: "/admin/api/v1/settings/biz", Auth: "session+token",
		Summary: "Update the global business-envelope fallback paths (superadmin only); instances apply them on their next sync tick",
		Params: []APIEndpointParam{
			{Name: "code_path", In: "body", Type: "string", Required: false, Desc: "JSON path of the business code"},
			{Name: "message_path", In: "body", Type: "string", Required: false, Desc: "JSON path of the business message"},
		},
		BodyExample: map[string]any{"code_path": "code", "message_path": "message"},
	},

	// --- alerts ---
	{
		Group: "alerts", Method: "GET", Path: "/admin/api/v1/alerts/channels", Auth: "session+token",
		Summary: "List every alert channel with its webhook token masked",
	},
	{
		Group: "alerts", Method: "POST", Path: "/admin/api/v1/alerts/channels", Auth: "session+token",
		Summary: "Create an alert channel (superadmin only)",
		Params: []APIEndpointParam{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "unique channel name, max 64 chars"},
			{Name: "enabled", In: "body", Type: "boolean", Required: false, Desc: "start enabled (requires a token)"},
			{Name: "platform", In: "body", Type: "string", Required: true, Desc: "feishu or lark"},
			{Name: "webhook_token", In: "body", Type: "string", Required: true, Desc: "bot webhook token, max 256 chars"},
		},
		BodyExample: map[string]any{"name": "ops", "enabled": true, "platform": "feishu", "webhook_token": "webhook-token-from-the-bot"},
	},
	{
		Group: "alerts", Method: "PUT", Path: "/admin/api/v1/alerts/channels/{id}", Auth: "session+token",
		Summary:     "Update one alert channel; an empty webhook_token keeps the stored credential (superadmin only)",
		Params:      []APIEndpointParam{pathParam("id", "channel id to update")},
		BodyExample: map[string]any{"name": "ops", "enabled": true, "platform": "feishu", "webhook_token": "webhook-token-from-the-bot"},
	},
	{
		Group: "alerts", Method: "DELETE", Path: "/admin/api/v1/alerts/channels/{id}", Auth: "session+token",
		Summary: "Delete one alert channel (superadmin only)",
		Params:  []APIEndpointParam{pathParam("id", "channel id to delete")},
	},
	{
		Group: "alerts", Method: "POST", Path: "/admin/api/v1/alerts/channels/{id}/test", Auth: "session+token",
		Summary: "Send a test message through one alert channel (superadmin only)",
		Params:  []APIEndpointParam{pathParam("id", "channel id to test")},
	},
	{
		Group: "alerts", Method: "GET", Path: "/admin/api/v1/alerts/rules", Auth: "session+token",
		Summary: "List every alert rule with its parsed match conditions",
	},
	{
		Group: "alerts", Method: "POST", Path: "/admin/api/v1/alerts/rules", Auth: "session+token",
		Summary: "Create an alert rule (superadmin only)",
		Params: []APIEndpointParam{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "unique rule name, max 64 chars"},
			{Name: "enabled", In: "body", Type: "boolean", Required: false, Desc: "start enabled"},
			{Name: "scope_type", In: "body", Type: "string", Required: true, Desc: "all or routes"},
			{Name: "route_ids", In: "body", Type: "object", Required: false, Desc: "route ids when scope_type=routes"},
			{Name: "channel_id", In: "body", Type: "integer", Required: true, Desc: "alert channel that receives firings"},
			{Name: "match_mode", In: "body", Type: "string", Required: false, Desc: "all or any (default all)"},
			{Name: "conditions", In: "body", Type: "object", Required: true, Desc: "match document: a group of leaf conditions"},
		},
		BodyExample: map[string]any{
			"name": "upstream errors", "enabled": true, "scope_type": "all", "channel_id": 1,
			"match_mode": "all",
			"conditions": map[string]any{
				"mode":     "all",
				"children": []map[string]any{{"source": "status", "op": "gte", "values": []string{"500"}}},
			},
		},
	},
	{
		Group: "alerts", Method: "PUT", Path: "/admin/api/v1/alerts/rules/{id}", Auth: "session+token",
		Summary: "Update one alert rule (superadmin only)",
		Params:  []APIEndpointParam{pathParam("id", "rule id to update")},
		BodyExample: map[string]any{
			"name": "upstream errors", "enabled": true, "scope_type": "all", "channel_id": 1,
			"match_mode": "all",
			"conditions": map[string]any{
				"mode":     "all",
				"children": []map[string]any{{"source": "status", "op": "gte", "values": []string{"500"}}},
			},
		},
	},
	{
		Group: "alerts", Method: "DELETE", Path: "/admin/api/v1/alerts/rules/{id}", Auth: "session+token",
		Summary: "Delete one alert rule (superadmin only)",
		Params:  []APIEndpointParam{pathParam("id", "rule id to delete")},
	},
	{
		Group: "alerts", Method: "GET", Path: "/admin/api/v1/alerts/events", Auth: "session+token",
		Summary: "List recent alert firings, newest first, optionally scoped to one rule",
		Params: []APIEndpointParam{
			queryParam("rule_id", "integer", "only firings of this rule"),
			queryParam("page", "integer", "1-based page number"),
			queryParam("page_size", "integer", "rows per page, 1..100 (default 50)"),
		},
	},

	// --- meta ---
	{
		Group: "meta", Method: "GET", Path: "/admin/api/v1/meta/endpoints", Auth: "none",
		Summary: "List every admin API endpoint from the maintained catalog, sorted by group then path (public: no authentication required)",
	},
	{
		Group: "meta", Method: "GET", Path: "/admin/api/v1/openapi.json", Auth: "none",
		Summary: "Serve the OpenAPI 3.1 document generated from the endpoint catalog (public: no authentication required)",
	},
	{
		Group: "meta", Method: "GET", Path: "/admin/api/v1/meta/prompt", Auth: "none", ContentType: "text/markdown",
		Summary: "Serve the paste-ready AI bootstrap prompt (AI-PROMPT.md) that installs the routegate-admin skill and guides credential configuration (public: no authentication required)",
	},
	{
		Group: "meta", Method: "GET", Path: "/admin/api/v1/meta/skill/SKILL.md", Auth: "none", ContentType: "text/markdown",
		Summary: "Serve the routegate-admin skill entry point: API usage, credential setup, network strategy, safety notes (public: no authentication required)",
	},
	{
		Group: "meta", Method: "GET", Path: "/admin/api/v1/meta/skill/ARCHITECTURE.md", Auth: "none", ContentType: "text/markdown",
		Summary: "Serve the skill's concept layer: architecture, design philosophy, domain concepts, capability map (public: no authentication required)",
	},
	{
		Group: "meta", Method: "GET", Path: "/admin/api/v1/meta/skill/AI-PROMPT.md", Auth: "none", ContentType: "text/markdown",
		Summary: "Serve the paste-ready bootstrap template whose download-based install steps reference these very endpoints (public: no authentication required)",
	},

	// --- AI assistant: connection endpoints and nested model bindings ---
	{
		Group: "llm", Method: "GET", Path: "/admin/api/v1/llm/endpoints", Auth: "session+token",
		Summary: "List configured AI endpoints with their model bindings and masked credentials",
	},
	{
		Group: "llm", Method: "POST", Path: "/admin/api/v1/llm/endpoints", Auth: "session+token",
		Summary:     "Add an OpenAI-compatible connection endpoint without selecting a model",
		Params:      []APIEndpointParam{{Name: "body", In: "body", Type: "object", Required: true, Desc: "{name, base_url, api_key?, enabled?, remark?}"}},
		BodyExample: map[string]any{"name": "DeepSeek Cloud", "base_url": "https://api.deepseek.com/v1", "api_key": "sk-...", "enabled": true},
	},
	{
		Group: "llm", Method: "PUT", Path: "/admin/api/v1/llm/endpoints/{id}", Auth: "session+token",
		Summary:     "Update an AI connection endpoint; an empty api_key keeps the stored credential",
		Params:      []APIEndpointParam{{Name: "id", In: "path", Type: "integer", Required: true, Desc: "endpoint id"}, {Name: "body", In: "body", Type: "object", Required: true, Desc: "patch fields; no model field"}},
		BodyExample: map[string]any{"name": "DeepSeek Cloud", "enabled": true},
	},
	{
		Group: "llm", Method: "DELETE", Path: "/admin/api/v1/llm/endpoints/{id}", Auth: "session+token",
		Summary: "Delete an AI endpoint and reset sessions using its models to the global default",
		Params:  []APIEndpointParam{{Name: "id", In: "path", Type: "integer", Required: true, Desc: "endpoint id"}},
	},
	{
		Group: "llm", Method: "POST", Path: "/admin/api/v1/llm/endpoints/{endpoint_id}/models/discover", Auth: "session+token",
		Summary: "Probe an OpenAI-compatible endpoint models catalog and import newly discovered model bindings",
		Params:  []APIEndpointParam{{Name: "endpoint_id", In: "path", Type: "integer", Required: true, Desc: "parent endpoint id"}},
	},
	{
		Group: "llm", Method: "POST", Path: "/admin/api/v1/llm/endpoints/{endpoint_id}/models", Auth: "session+token",
		Summary:     "Add a model binding under an AI endpoint",
		Params:      []APIEndpointParam{{Name: "endpoint_id", In: "path", Type: "integer", Required: true, Desc: "parent endpoint id"}, {Name: "body", In: "body", Type: "object", Required: true, Desc: "{provider_model_name, display_name, enabled?, is_default?, max_tokens?, temperature?, remark?}"}},
		BodyExample: map[string]any{"provider_model_name": "deepseek-reasoner", "display_name": "DeepSeek Reasoner", "enabled": true, "is_default": true},
	},
	{
		Group: "llm", Method: "PUT", Path: "/admin/api/v1/llm/models/{id}", Auth: "session+token",
		Summary:     "Update a model binding and its default or generation settings",
		Params:      []APIEndpointParam{{Name: "id", In: "path", Type: "integer", Required: true, Desc: "model binding id"}, {Name: "body", In: "body", Type: "object", Required: true, Desc: "patch model binding fields"}},
		BodyExample: map[string]any{"display_name": "DeepSeek Chat", "temperature": 0.2},
	},
	{
		Group: "llm", Method: "DELETE", Path: "/admin/api/v1/llm/models/{id}", Auth: "session+token",
		Summary: "Delete a model binding and reset sessions that selected it",
		Params:  []APIEndpointParam{{Name: "id", In: "path", Type: "integer", Required: true, Desc: "model binding id"}},
	},
	{
		Group: "llm", Method: "POST", Path: "/admin/api/v1/llm/models/{id}/test", Auth: "session+token",
		Summary:     "Send a minimal chat completion through the selected endpoint and model binding",
		Params:      []APIEndpointParam{{Name: "id", In: "path", Type: "integer", Required: true, Desc: "model binding id"}},
		BodyExample: map[string]any{},
	},

	// --- AI assistant: chat sessions (user-owned; the model surface is the
	// read-only tool whitelist in internal/assistant) ---
	{
		Group: "assistant", Method: "POST", Path: "/admin/api/v1/assistant/sessions", Auth: "session+token",
		Summary: "Open a chat thread owned by the caller",
		Params: []APIEndpointParam{
			{Name: "body", In: "body", Type: "object", Required: false, Desc: "{title?, model_id?} (model_id 0 = global default model)"},
		},
		BodyExample: map[string]any{"title": "investigate 403"},
	},
	{
		Group: "assistant", Method: "GET", Path: "/admin/api/v1/assistant/sessions", Auth: "session+token",
		Summary: "List the caller's own chat sessions, newest activity first",
	},
	{
		Group: "assistant", Method: "PUT", Path: "/admin/api/v1/assistant/sessions/{key}", Auth: "session+token",
		Summary: "Rename a thread or switch its model binding (owner or superadmin)",
		Params: []APIEndpointParam{
			{Name: "key", In: "path", Type: "string", Required: true, Desc: "session key (as-…)"},
			{Name: "body", In: "body", Type: "object", Required: false, Desc: "{title?, model_id?}"},
		},
		BodyExample: map[string]any{"title": "investigate caller denial"},
	},
	{
		Group: "assistant", Method: "GET", Path: "/admin/api/v1/assistant/sessions/{key}/messages", Auth: "session+token",
		Summary: "Read one thread's full history with the per-turn tool-call audit trail (owner or superadmin)",
		Params: []APIEndpointParam{
			{Name: "key", In: "path", Type: "string", Required: true, Desc: "session key (as-…)"},
		},
	},
	{
		Group: "assistant", Method: "DELETE", Path: "/admin/api/v1/assistant/sessions/{key}", Auth: "session+token",
		Summary: "Delete a thread and its messages (owner or superadmin)",
		Params: []APIEndpointParam{
			{Name: "key", In: "path", Type: "string", Required: true, Desc: "session key (as-…)"},
		},
	},
	{
		Group: "assistant", Method: "POST", Path: "/admin/api/v1/assistant/chat", Auth: "session+token", ContentType: "text/event-stream",
		Summary: "Stream one chat turn as SSE (meta, thinking, delta, tool, error, done events); the model can only invoke read-only tools",
		Params: []APIEndpointParam{
			{Name: "body", In: "body", Type: "object", Required: true, Desc: "{session_key, content, model_id?}; response is text/event-stream"},
		},
		BodyExample: map[string]any{"session_key": "as-0123abcd0123abcd0123abcd0123abcd", "content": "why does caller=service-a get 403 on /alpha/v1/ping?"},
	},
}

// handleMetaEndpoints renders the catalog sorted by group then path. PUBLIC
// by owner decision: the catalog is static documentation — no state, no
// secrets — so agents and integrators can discover the API pre-auth.
func (s *Server) handleMetaEndpoints(w http.ResponseWriter, r *http.Request) {
	eps := make([]APIEndpoint, len(apiCatalog))
	copy(eps, apiCatalog)
	sort.SliceStable(eps, func(i, j int) bool {
		if eps[i].Group != eps[j].Group {
			return eps[i].Group < eps[j].Group
		}
		return eps[i].Path < eps[j].Path
	})
	writeJSON(w, http.StatusOK, map[string]any{"version": "1", "endpoints": eps})
}

// handleOpenAPI renders the OpenAPI 3.1 document generated from the same
// catalog that backs meta/endpoints, so the machine-readable contract and the
// human-readable listing can never drift apart.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildOpenAPI())
}

// openAPISecurity maps the catalog Auth value onto OpenAPI security
// requirements. A requirement list is alternatives (OR), so session+token
// endpoints accept EITHER the Bearer token OR the session cookie.
func openAPISecurity(auth string) []map[string]any {
	switch auth {
	case "session+token":
		return []map[string]any{{"bearerAuth": []any{}}, {"cookieAuth": []any{}}}
	case "session":
		return []map[string]any{{"cookieAuth": []any{}}}
	default: // "none"
		return []map[string]any{}
	}
}

// openAPIErrorResponses are the $ref bodies every operation reuses for the
// unified error envelope (components.responses in buildOpenAPI).
var openAPIErrorResponses = map[string]string{
	"401": "Unauthorized",
	"403": "Forbidden",
	"409": "Conflict",
}

// buildOpenAPI assembles the OpenAPI 3.1 object from apiCatalog. No servers
// entry: the client supplies the base URL of its own instance.
func buildOpenAPI() map[string]any {
	paths := map[string]any{}
	for _, ep := range apiCatalog {
		ops, ok := paths[ep.Path].(map[string]any)
		if !ok {
			ops = map[string]any{}
			paths[ep.Path] = ops
		}
		op := map[string]any{
			"summary": ep.Summary,
			"tags":    []string{ep.Group},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Success",
					"content": map[string]any{
						responseMediaType(ep): map[string]any{
							"schema": map[string]any{"type": "object"},
						},
					},
				},
			},
		}
		if sec := openAPISecurity(ep.Auth); len(sec) > 0 {
			op["security"] = sec
		}
		var params []map[string]any
		for _, p := range ep.Params {
			if p.In == "body" {
				continue // bodies render as requestBody below
			}
			schema := map[string]any{"type": p.Type}
			param := map[string]any{
				"name": p.Name, "in": p.In, "schema": schema,
				"description": p.Desc, "required": p.Required,
			}
			params = append(params, param)
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if ep.BodyExample != nil {
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/json": map[string]any{
						"schema":  map[string]any{"type": "object"},
						"example": ep.BodyExample,
					},
				},
			}
		}
		// Every operation answers with the unified error envelope on failure.
		// 401 on all authenticated operations; 403 (role) and 409 (revision
		// conflict / duplicate) on writes.
		responses := op["responses"].(map[string]any)
		if ep.Auth != "none" {
			responses["401"] = map[string]any{"$ref": "#/components/responses/Unauthorized"}
		}
		if ep.Method != http.MethodGet {
			responses["403"] = map[string]any{"$ref": "#/components/responses/Forbidden"}
			responses["409"] = map[string]any{"$ref": "#/components/responses/Conflict"}
		}
		ops[strings.ToLower(ep.Method)] = op
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "RouteGate Admin API",
			"version":     "1",
			"description": "Admin plane of the RouteGate gateway. Requests authenticate with the admin session cookie or a user-scoped admin API token (rgt_…); a token acts as its issuing user with the role resolved live on every request. Every non-2xx JSON response renders the ErrorEnvelope schema with Cache-Control: no-store.",
		},
		"paths": paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type": "http", "scheme": "bearer",
					"description": "Admin API token (rgt_…), minted at POST /admin/api/v1/auth/tokens; acts as the issuing user with the role resolved live on every request.",
				},
				"cookieAuth": map[string]any{
					"type": "apiKey", "in": "cookie", "name": cookieName,
					"description": "Admin session cookie issued by POST /admin/api/v1/auth/login.",
				},
			},
			"schemas": map[string]any{
				"ErrorEnvelope": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"error": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"code":       map[string]any{"type": "string", "description": "stable machine-readable reason, e.g. admin.unauthenticated"},
								"message":    map[string]any{"type": "string", "description": "stable friendly English sentence"},
								"request_id": map[string]any{"type": "string", "description": "middleware-assigned request id, present on responses that went through the middleware"},
							},
							"required": []string{"code", "message"},
						},
					},
					"required": []string{"error"},
				},
			},
			"responses": map[string]any{
				"Unauthorized": map[string]any{
					"description": "Missing or invalid credentials (admin.unauthenticated, admin.token_invalid, admin.invalid_credentials)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"},
							"example": map[string]any{
								"error": map[string]any{
									"code": "admin.token_invalid", "message": "the API token is invalid, expired, or revoked",
									"request_id": "0123456789abcdef0123456789abcdef",
								},
							},
						},
					},
				},
				"Forbidden": map[string]any{
					"description": "The authenticated identity lacks the required role (admin.forbidden)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"},
						},
					},
				},
				"Conflict": map[string]any{
					"description": "Revision conflict (admin.revision_conflict, carries a top-level config_revision) or duplicate value (admin.duplicate)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"},
						},
					},
				},
			},
		},
	}
}

// responseMediaType is the OpenAPI 200 media type for a catalog entry:
// application/json unless the entry overrides it (e.g. the markdown prompt).
func responseMediaType(ep APIEndpoint) string {
	if ep.ContentType != "" {
		return ep.ContentType
	}
	return "application/json"
}
