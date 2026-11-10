// Package authz implements path normalization and the wildcard policy
// evaluation semantics of ADR-0001 §6.1/§6.2:
//
//	deny > allow > default deny (fail-closed)
//
// Path patterns are segment-based: '*' matches exactly one segment, '**'
// matches one or more segments and is only valid as the last segment.
package authz

import (
	"strings"
	"unicode/utf8"
)

// NormalizePath canonicalizes a request path for matching (ADR-0001 §6.2):
// one percent-decode pass, RFC 3986 remove_dot_segments, collapse of empty
// segments, and trailing-slash removal. The query string is not part of the
// path and never reaches this function.
//
// The result is used for BOTH route matching and policy matching so a path
// trick (e.g. "/v1/../v1/users/123") cannot make matching and forwarding
// disagree.
func NormalizePath(raw string) string {
	if raw == "" {
		return "/"
	}
	decoded := decodePercentOnce(raw)
	segs := strings.Split(decoded, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		switch seg {
		case "", ".":
			// skip empty (from leading '/', '//', trailing '/') and current-dir
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	if len(out) == 0 {
		return "/"
	}
	return "/" + strings.Join(out, "/")
}

// decodePercentOnce decodes %XX sequences exactly once. Malformed escapes are
// preserved verbatim (never re-decoded) so "%252e" stays "%2e" and cannot
// smuggle a second-level dot segment past normalization.
func decodePercentOnce(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(hexVal(s[i+1])<<4 | hexVal(s[i+2]))
			i += 3
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// ParsePattern validates and splits a policy path pattern into segments.
// Rules (ADR-0001 §6.1): must start with '/'; '**' only as the final segment;
// '*' only as a whole segment; no '*' or '?' inside literal segments.
func ParsePattern(pattern string) ([]string, error) {
	if pattern == "" || !strings.HasPrefix(pattern, "/") {
		return nil, errInvalidPattern("pattern must start with '/'")
	}
	if !utf8.ValidString(pattern) {
		return nil, errInvalidPattern("pattern must be valid UTF-8")
	}
	raw := strings.Split(strings.TrimSuffix(pattern, "/"), "/")
	segs := make([]string, 0, len(raw))
	for i, seg := range raw {
		if seg == "" {
			continue // leading slash; interior empties collapse like paths do
		}
		last := i == len(raw)-1
		switch seg {
		case "**":
			if !last {
				return nil, errInvalidPattern("'**' is only allowed as the last segment")
			}
			segs = append(segs, "**")
		case "*":
			segs = append(segs, "*")
		default:
			if strings.ContainsAny(seg, "*?") {
				return nil, errInvalidPattern("wildcards are not allowed inside literal segments")
			}
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return nil, errInvalidPattern("pattern must match at least one segment")
	}
	return segs, nil
}

type errInvalidPattern string

func (e errInvalidPattern) Error() string { return string(e) }

// MatchSegments reports whether a normalized path (as segments) matches a
// pattern (as segments from ParsePattern).
func MatchSegments(pattern, path []string) bool {
	pi := 0
	for i, ps := range pattern {
		switch ps {
		case "**":
			// Last position (guaranteed by ParsePattern): matches one or more
			// of the remaining path segments.
			return len(path)-pi >= 1
		case "*":
			if pi >= len(path) {
				return false
			}
			pi++
		default:
			if pi >= len(path) || path[pi] != ps {
				return false
			}
			pi++
		}
		_ = i
	}
	return pi == len(path)
}

// MethodMatches compares an HTTP method against a policy method
// ('*' or case-insensitive exact).
func MethodMatches(policyMethod, method string) bool {
	if policyMethod == "*" {
		return true
	}
	return strings.EqualFold(policyMethod, method)
}

// CallerMatches compares a caller name against a policy caller ('*' or exact).
func CallerMatches(policyCaller, caller string) bool {
	if policyCaller == "*" {
		return true
	}
	return policyCaller == caller
}
