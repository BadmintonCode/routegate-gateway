package adminapi

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestCatalogCoversRegisteredRoutes pins the documentation backbone: every
// route registered on the api mux must have a catalog row. The registration
// count is read from server.go source (the mux patterns cannot be enumerated
// at runtime), so adding a route without a catalog row fails here.
func TestCatalogCoversRegisteredRoutes(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := strings.Count(string(src), "apiMux.HandleFunc(")
	if len(apiCatalog) < registered {
		t.Fatalf("catalog has %d entries but server.go registers %d api routes; every registered route needs a catalog row", len(apiCatalog), registered)
	}
	seen := map[string]bool{}
	for _, ep := range apiCatalog {
		if !strings.HasPrefix(ep.Path, "/admin/api/v1/") {
			t.Errorf("catalog path %q is not an absolute /admin/api/v1/ path", ep.Path)
		}
		if !knownGroups[ep.Group] {
			t.Errorf("catalog entry %s %s has unknown group %q", ep.Method, ep.Path, ep.Group)
		}
		if !knownAuth[ep.Auth] {
			t.Errorf("catalog entry %s %s has unknown auth %q", ep.Method, ep.Path, ep.Auth)
		}
		if ep.Summary == "" || strings.HasSuffix(ep.Summary, ".") {
			t.Errorf("catalog entry %s %s summary must be a non-empty sentence without a trailing period, got %q", ep.Method, ep.Path, ep.Summary)
		}
		for _, p := range ep.Params {
			switch p.In {
			case "path", "query", "body":
			default:
				t.Errorf("catalog entry %s %s param %q has unknown location %q", ep.Method, ep.Path, p.Name, p.In)
			}
			if p.In == "path" && !strings.Contains(ep.Path, "{"+p.Name+"}") {
				t.Errorf("catalog entry %s %s documents path param {%s} that the path does not contain", ep.Method, ep.Path, p.Name)
			}
		}
		key := ep.Method + " " + ep.Path
		if seen[key] {
			t.Errorf("duplicate catalog entry %s", key)
		}
		seen[key] = true
	}
	for _, want := range []string{
		"POST /admin/api/v1/auth/login",
		"POST /admin/api/v1/auth/logout",
		"GET /admin/api/v1/auth/me",
		"GET /admin/api/v1/auth/tokens",
		"POST /admin/api/v1/auth/tokens",
		"DELETE /admin/api/v1/auth/tokens/{id}",
		"GET /admin/api/v1/routes",
		"POST /admin/api/v1/policies/test",
		"GET /admin/api/v1/meta/endpoints",
		"GET /admin/api/v1/openapi.json",
	} {
		if !seen[want] {
			t.Errorf("catalog is missing the named route %q", want)
		}
	}
}

// TestOpenAPICoversCatalogOneToOne: the generated document parses, carries
// the security schemes and the shared ErrorEnvelope, and its paths match the
// catalog exactly (no missing, no extra operations).
func TestOpenAPICoversCatalogOneToOne(t *testing.T) {
	doc := buildOpenAPI()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("openapi document does not marshal: %v", err)
	}
	var parsed struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
			Schemas         map[string]json.RawMessage `json:"schemas"`
			Responses       map[string]json.RawMessage `json:"responses"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("openapi document does not unmarshal: %v", err)
	}
	if parsed.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0", parsed.OpenAPI)
	}
	if parsed.Info.Title != "RouteGate Admin API" {
		t.Errorf("info.title = %q, want RouteGate Admin API", parsed.Info.Title)
	}
	for _, scheme := range []string{"bearerAuth", "cookieAuth"} {
		if _, ok := parsed.Components.SecuritySchemes[scheme]; !ok {
			t.Errorf("components.securitySchemes missing %q", scheme)
		}
	}
	for _, name := range []string{"ErrorEnvelope"} {
		if _, ok := parsed.Components.Schemas[name]; !ok {
			t.Errorf("components.schemas missing %q", name)
		}
	}
	for _, name := range []string{"Unauthorized", "Forbidden", "Conflict"} {
		if _, ok := parsed.Components.Responses[name]; !ok {
			t.Errorf("components.responses missing %q", name)
		}
	}
	want := map[string]bool{}
	for _, ep := range apiCatalog {
		want[ep.Method+" "+ep.Path] = true
	}
	got := map[string]bool{}
	for path, ops := range parsed.Paths {
		for method := range ops {
			got[strings.ToUpper(method)+" "+path] = true
		}
	}
	for key := range want {
		if !got[key] {
			t.Errorf("openapi paths missing catalog entry %s", key)
		}
	}
	for key := range got {
		if !want[key] {
			t.Errorf("openapi paths has operation %s that is not in the catalog", key)
		}
	}
}

// TestOpenAPISecurityMapping pins the auth translation: none for login,
// cookie-only for logout, cookie-or-bearer for everything else.
func TestOpenAPISecurityMapping(t *testing.T) {
	doc := buildOpenAPI()
	paths := doc["paths"].(map[string]any)
	opSecurity := func(method, path string) [][]string {
		ops := paths[path].(map[string]any)
		op := ops[method].(map[string]any)
		sec, _ := op["security"].([]map[string]any)
		var out [][]string
		for _, req := range sec {
			var names []string
			for name := range req {
				names = append(names, name)
			}
			sort.Strings(names)
			out = append(out, names)
		}
		return out
	}
	if sec := opSecurity("post", "/admin/api/v1/auth/login"); len(sec) != 0 {
		t.Errorf("login security = %v, want none (empty)", sec)
	}
	sec := opSecurity("post", "/admin/api/v1/auth/logout")
	if len(sec) != 1 || len(sec[0]) != 1 || sec[0][0] != "cookieAuth" {
		t.Errorf("logout security = %v, want cookieAuth only", sec)
	}
	sec = opSecurity("get", "/admin/api/v1/auth/me")
	if len(sec) != 2 {
		t.Fatalf("me security = %v, want bearerAuth OR cookieAuth", sec)
	}
	names := map[string]bool{sec[0][0]: true, sec[1][0]: true}
	if !names["bearerAuth"] || !names["cookieAuth"] {
		t.Errorf("me security = %v, want bearerAuth and cookieAuth alternatives", sec)
	}
}

// TestMetaEndpointsHandler renders the sorted catalog over the real mux with
// the request-id middleware running.
func TestMetaEndpointsHandler(t *testing.T) {
	s := newContractServer(t, &fakeDBState{role: "user"})
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/meta/endpoints", "",
		map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Version   string        `json:"version"`
		Endpoints []APIEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body.Version != "1" {
		t.Fatalf("version = %q, want 1", body.Version)
	}
	if len(body.Endpoints) != len(apiCatalog) {
		t.Fatalf("endpoints = %d, want the full catalog of %d", len(body.Endpoints), len(apiCatalog))
	}
	for i := 1; i < len(body.Endpoints); i++ {
		a, b := body.Endpoints[i-1], body.Endpoints[i]
		if a.Group > b.Group || (a.Group == b.Group && a.Path > b.Path) {
			t.Fatalf("endpoints not sorted by group then path at %d: (%s %s) before (%s %s)",
				i, a.Group, a.Path, b.Group, b.Path)
		}
	}
}

// TestOpenAPIEndpointContract: the document endpoint serves the same object
// the builder returns, through the full auth middleware.
func TestOpenAPIEndpointContract(t *testing.T) {
	s := newContractServer(t, &fakeDBState{role: "user"})
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/openapi.json", "",
		map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("served document has no paths object")
	}
	if got := len(paths); got != len(catalogPaths(apiCatalog)) {
		t.Fatalf("paths = %d, want one per distinct catalog path (%d)", got, len(catalogPaths(apiCatalog)))
	}
}

// catalogPaths returns the distinct path strings of the catalog.
func catalogPaths(eps []APIEndpoint) map[string]bool {
	out := map[string]bool{}
	for _, ep := range eps {
		out[ep.Path] = true
	}
	return out
}

// TestMetaEndpointsArePublic pins the owner decision (2026-11-09): the
// endpoint catalog and the OpenAPI document are static documentation with no
// state, secrets, or user data, and must be reachable WITHOUT any
// authentication so agents can discover the API and verify connectivity
// pre-credential. The catalog must also self-describe them as auth "none"
// with empty OpenAPI security.
func TestMetaEndpointsArePublic(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	h := s.Handler(nil)
	for path, topKey := range map[string]string{
		"/admin/api/v1/meta/endpoints": "version",
		"/admin/api/v1/openapi.json":   "openapi",
	} {
		rec := doRequest(h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s anonymous = %d, want 200 (public documentation)", path, rec.Code)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("GET %s anonymous returned non-JSON: %v", path, err)
		}
		if _, ok := doc[topKey]; !ok {
			t.Fatalf("GET %s anonymous body lacks the %q key (got an error envelope?): %s", path, topKey, rec.Body.String())
		}
	}
	for _, ep := range apiCatalog {
		if ep.Path != "/admin/api/v1/meta/endpoints" && ep.Path != "/admin/api/v1/openapi.json" {
			continue
		}
		if ep.Auth != "none" {
			t.Errorf("catalog %s auth = %q, want none (public)", ep.Path, ep.Auth)
		}
	}
	meta := findEndpoint(t, "/admin/api/v1/meta/endpoints")
	if sec := openAPISecurity(meta.Auth); len(sec) != 0 {
		t.Errorf("meta/endpoints security = %v, want empty (public)", sec)
	}
}

// findEndpoint returns the catalog entry for an exact path.
func findEndpoint(t *testing.T, path string) APIEndpoint {
	t.Helper()
	for _, ep := range apiCatalog {
		if ep.Path == path {
			return ep
		}
	}
	t.Fatalf("catalog has no entry for %s", path)
	return APIEndpoint{}
}

// TestAIPromptMatchesCanonical is the drift guard for the embedded prompt:
// internal/adminapi/prompt/AI-PROMPT.md must be byte-identical to the
// canonical skills/routegate-admin/AI-PROMPT.md — update both together (or
// better: copy the canonical over the embedded copy) whenever the template
// changes.
func TestAIPromptMatchesCanonical(t *testing.T) {
	embedded, err := skillFiles.ReadFile("skill/AI-PROMPT.md")
	if err != nil {
		t.Fatalf("embedded AI-PROMPT.md: %v", err)
	}
	canon, err := os.ReadFile("../../skills/routegate-admin/AI-PROMPT.md")
	if err != nil {
		t.Fatalf("read canonical AI-PROMPT.md: %v", err)
	}
	if string(embedded) != string(canon) {
		t.Fatal("embedded skill/AI-PROMPT.md drifted from skills/routegate-admin/AI-PROMPT.md — copy the canonical file over the embedded copy")
	}
}

// TestAIPromptEndpointIsPublic pins the anonymous, markdown contract of
// GET /admin/api/v1/meta/prompt.
func TestAIPromptEndpointIsPublic(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/meta/prompt", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /meta/prompt anonymous = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Fatalf("Content-Type = %q, want text/markdown", ct)
	}
	if !strings.Contains(rec.Body.String(), "paste-ready AI bootstrap prompt") {
		t.Fatalf("prompt body lost its heading: %q", rec.Body.String())
	}
	// The OpenAPI document must describe the endpoint with the markdown
	// media type, not the JSON default.
	rec = doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/openapi.json", "", nil)
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]any `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("openapi.json unmarshal: %v", err)
	}
	op, ok := doc.Paths["/admin/api/v1/meta/prompt"]["get"]
	if !ok {
		t.Fatal("openapi.json lacks the /meta/prompt operation")
	}
	if _, ok := op.Responses["200"].Content["text/markdown"]; !ok {
		t.Fatalf("openapi 200 content = %v, want text/markdown", op.Responses["200"].Content)
	}
}

// TestSkillFilesMatchCanonical extends the AI-PROMPT drift guard to the whole
// skill bundle: every embedded file in internal/adminapi/skill/ must be
// byte-identical to its canonical source in skills/routegate-admin/.
func TestSkillFilesMatchCanonical(t *testing.T) {
	for _, name := range skillFileNames {
		embedded, err := skillFiles.ReadFile("skill/" + name)
		if err != nil {
			t.Fatalf("embedded %s: %v", name, err)
		}
		canon, err := os.ReadFile("../../skills/routegate-admin/" + name)
		if err != nil {
			t.Fatalf("read canonical %s: %v", name, err)
		}
		if string(embedded) != string(canon) {
			t.Errorf("embedded skill/%s drifted from skills/routegate-admin/%s — copy the canonical files over the embedded copies", name, name)
		}
	}
}

// TestSkillFilesArePublicPins the self-hosted install contract: each skill
// file is anonymously downloadable as text/markdown, unknown filenames 404
// (envelope), and the openapi.json documents them.
func TestSkillFilesArePublicPins(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	h := s.Handler(nil)
	for _, name := range skillFileNames {
		rec := doRequest(h, http.MethodGet, "/admin/api/v1/meta/skill/"+name, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET skill/%s anonymous = %d, want 200", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
			t.Fatalf("skill/%s Content-Type = %q, want text/markdown", name, ct)
		}
		if len(rec.Body.Bytes()) < 100 {
			t.Fatalf("skill/%s body suspiciously short: %d bytes", name, rec.Body.Len())
		}
	}
	rec := doRequest(h, http.MethodGet, "/admin/api/v1/meta/skill/../../etc/passwd", "", nil)
	// ServeMux path-cleaning turns the traversal into a 307 redirect before
	// any handler runs; the point is that it can never answer 200 with file
	// contents.
	if rec.Code == http.StatusOK {
		t.Fatalf("path traversal attempt answered 200: %q", rec.Body.String())
	}
	rec = doRequest(h, http.MethodGet, "/admin/api/v1/meta/skill/NOT-A-FILE.md", "", nil)
	assertEnvelopeShape(t, rec, true, http.StatusNotFound, "admin.not_found", "", false)
	rec = doRequest(h, http.MethodGet, "/admin/api/v1/openapi.json", "", nil)
	if !strings.Contains(rec.Body.String(), "/admin/api/v1/meta/skill/SKILL.md") {
		t.Fatal("openapi.json does not document the skill download endpoints")
	}
}
