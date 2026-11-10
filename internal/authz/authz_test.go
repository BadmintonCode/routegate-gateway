package authz

import "testing"

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/v1/users", "/v1/users"},
		{"/v1/users/", "/v1/users"},              // trailing slash equivalence
		{"//v1//users//", "/v1/users"},           // empty segment collapse
		{"/v1/../v1/users/123", "/v1/users/123"}, // dot segments removed
		{"/v1/%2e%2e/v1/users", "/v1/users"},     // percent-encoded dot segment
		{"/v1/users/%31%32%33", "/v1/users/123"},
		{"/%2f", "/"},                                // decoded '/' splits into empty segments
		{"/v1/%252e%252e/users", "/v1/%2e%2e/users"}, // double-encoded stays literal once
		{"/v1/./users", "/v1/users"},
	}
	for _, c := range cases {
		if got := NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParsePattern(t *testing.T) {
	ok := []string{
		"/v1/users",
		"/v1/users/*",
		"/v1/*/detail",
		"/v1/users/**",
		"/**",
	}
	for _, p := range ok {
		if _, err := ParsePattern(p); err != nil {
			t.Errorf("ParsePattern(%q) unexpected error: %v", p, err)
		}
	}
	bad := []string{
		"",
		"v1/users",
		"/v1/users/**/x", // '**' not last
		"/v1/te*t",       // wildcard inside literal
		"/v1/t?st",       // '?' not allowed
		"/",              // must match at least one segment
	}
	for _, p := range bad {
		if _, err := ParsePattern(p); err == nil {
			t.Errorf("ParsePattern(%q) expected error, got none", p)
		}
	}
}

func TestMatchSegments(t *testing.T) {
	mk := func(p string) []string {
		segs, err := ParsePattern(p)
		if err != nil {
			t.Fatalf("ParsePattern(%q): %v", p, err)
		}
		return segs
	}
	type c struct {
		pattern string
		path    []string
		want    bool
	}
	cases := []c{
		{"/v1/users", split("/v1/users"), true},
		{"/v1/users", split("/v1/users/123"), false}, // exact does not match deeper
		{"/v1/users/*", split("/v1/users/123"), true},
		{"/v1/users/*", split("/v1/users"), false},       // '*' needs exactly one
		{"/v1/users/*", split("/v1/users/123/x"), false}, // '*' is one segment only
		{"/v1/*/detail", split("/v1/orders/detail"), true},
		{"/v1/*/detail", split("/v1/orders/x/detail"), false},
		{"/v1/users/**", split("/v1/users/123"), true},
		{"/v1/users/**", split("/v1/users/123/x/y"), true},
		{"/v1/users/**", split("/v1/users"), false}, // '**' matches >= 1 segment
		{"/**", split("/anything/here"), true},
		{"/**", split("/x"), true},
	}
	for _, c := range cases {
		if got := MatchSegments(mk(c.pattern), c.path); got != c.want {
			t.Errorf("MatchSegments(%q, %v) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// TestNoBypassViaNormalization exercises the ADR §17 acceptance case: dot
// segments must not let a denied path sneak past pattern matching, because the
// policy engine only ever sees normalized segments.
func TestNoBypassViaNormalization(t *testing.T) {
	pattern, err := ParsePattern("/admin/**")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"/admin/secret",
		"/admin/../admin/secret",
		"/%61dmin/secret", // percent-decoded to admin/secret
		"/admin/%2e%2e/admin/secret",
		"/admin/./secret",
		"/admin//secret",
	} {
		normalized := NormalizePath(raw)
		got := MatchSegments(pattern, split(normalized))
		if !got {
			t.Errorf("normalized %q (from %q) should match /admin/**", normalized, raw)
		}
	}
}

func split(p string) []string {
	n := NormalizePath(p)
	segs := []string{}
	cur := ""
	for i := 1; i < len(n); i++ {
		if n[i] == '/' {
			segs = append(segs, cur)
			cur = ""
			continue
		}
		cur += string(n[i])
	}
	if cur != "" {
		segs = append(segs, cur)
	}
	return segs
}
