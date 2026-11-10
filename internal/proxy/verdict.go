package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// hasGlobalBizFields reports whether the global business-envelope fallback
// defines at least one path (cheap atomic read, called once per request).
func (s *Server) hasGlobalBizFields() bool {
	gf := s.bizFields.Get()
	return gf.CodePath != "" || gf.MessagePath != ""
}

// verdictCaptureMaxBytes bounds how much of the response body is captured for
// success-verdict evaluation. Independent of the debug capture: a route with
// a success rule always pays this small copy, debug mode or not. Streaming
// responses are judged on the captured prefix only (documented in §6.7).
const verdictCaptureMaxBytes = 8 * 1024

// DecodeJSONBody decodes a captured response body into the root JSON value
// with UseNumber semantics: numeric literals stay verbatim (json.Number), so
// big IDs beyond float64's 2^53 precision (snowflake IDs) and decimals like
// 1.50 keep their exact source text. A non-JSON or empty body yields nil
// ("missing field" semantics for every consumer).
//
// The verdict capture is bounded (verdictCaptureMaxBytes), so a large body
// arrives as a prefix cut mid-value. When the whole-document decode fails
// only because the input ran out (io.EOF / io.ErrUnexpectedEOF), the
// top-level members completed before the cut are recovered: envelope fields
// like code/message sit at the body start, so a truncated success response
// is still judged on its real fields instead of counting as missing. Members
// cut mid-value stay missing; syntactically invalid bodies still yield nil.
// Exported for the alert engine, which evaluates the same condition grammar
// over the same bounded capture.
func DecodeJSONBody(body []byte) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	err := dec.Decode(&v)
	if err == nil {
		return v
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return recoverTruncatedPrefix(body)
	}
	return nil
}

// recoverTruncatedPrefix salvages the complete top-level members (object) or
// elements (array) of a JSON prefix that ran out mid-value. Anything not
// fully decoded before the cut is dropped, never guessed; an empty salvage
// returns nil so missing-field semantics are unchanged.
func recoverTruncatedPrefix(body []byte) any {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // a truncated top-level scalar has no complete value
	}
	switch delim {
	case '{':
		obj := make(map[string]any)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				break
			}
			key, ok := keyTok.(string)
			if !ok {
				break
			}
			var val any
			if err := dec.Decode(&val); err != nil {
				break
			}
			obj[key] = val
		}
		if len(obj) == 0 {
			return nil
		}
		return obj
	case '[':
		var arr []any
		for dec.More() {
			var val any
			if err := dec.Decode(&val); err != nil {
				break
			}
			arr = append(arr, val)
		}
		if len(arr) == 0 {
			return nil
		}
		return arr
	}
	return nil
}

// evaluateVerdict applies the route's success rule to the proxied response and
// returns "success" or "failure" (ADR-0001 §6.7). mode=all (AND, default) or
// any (OR) combines the conditions; each condition matches the HTTP status or
// a response-body JSON field against a value list with in / not_in semantics.
// root is the pre-decoded response body (DecodeJSONBody; nil for a non-JSON
// body). A missing body field (or a non-JSON body) fails `in` and passes
// `not_in` — decided with the owner on 2026-11-07.
func evaluateVerdict(rule *snapshot.SuccessRule, status int, root any) string {
	results := make([]bool, 0, len(rule.Conditions))
	for _, c := range rule.Conditions {
		if c == nil {
			continue
		}
		results = append(results, evalCondition(c, status, root))
	}
	pass := false
	switch rule.Mode {
	case "any":
		pass = slices.Contains(results, true)
	default: // "all"
		pass = !slices.Contains(results, false)
	}
	if pass {
		return "success"
	}
	return "failure"
}

// extractBizValue pulls one business-envelope field out of the captured
// response body (§6.7): the stringified value at the configured dotted path,
// rune-safe truncated to max bytes. root is the pre-decoded body
// (DecodeJSONBody). Missing field, nil root, or empty value → "".
func extractBizValue(path string, root any, max int) string {
	if root == nil {
		return ""
	}
	s, found := lookupJSONPath(root, path)
	if !found || s == "" {
		return ""
	}
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

func evalCondition(c *snapshot.SuccessCondition, status int, root any) bool {
	actual, found := ResolveConditionValue(c, status, root, "")
	return MatchCondition(c, actual, found)
}

// ResolveConditionValue computes the stringified actual value for a
// condition: status → the HTTP status, body → the dotted JSON path in the
// decoded body, outcome → the route's own success verdict ("success" /
// "failure"; empty when the route has no rule). Exported for the alert
// engine, which evaluates the same condition grammar over finished requests.
func ResolveConditionValue(c *snapshot.SuccessCondition, status int, root any, outcome string) (string, bool) {
	switch c.Source {
	case "status":
		return strconv.Itoa(status), true
	case "body":
		if root == nil {
			return "", false
		}
		return lookupJSONPath(root, c.Path)
	case "outcome":
		return outcome, outcome != ""
	default:
		return "", false
	}
}

// MatchCondition applies one condition's op to a resolved (actual, found)
// pair. Missing values fail positive matchers and pass negative ones (the
// owner-confirmed asymmetry, ADR-0001 §6.7). Exported for the alert engine,
// which evaluates the same condition grammar over finished responses.
func MatchCondition(c *snapshot.SuccessCondition, actual string, found bool) bool {
	switch c.Op {
	case "in":
		return found && slices.Contains(c.Values, actual)
	case "not_in":
		return !slices.Contains(c.Values, actual) // vacuously true when missing
	case "eq":
		return found && len(c.Values) == 1 && actual == c.Values[0]
	case "neq":
		return !found || (len(c.Values) == 1 && actual != c.Values[0])
	case "gt", "gte", "lt", "lte":
		if !found || len(c.Values) != 1 {
			return false
		}
		a, aerr := strconv.ParseFloat(actual, 64)
		b, berr := strconv.ParseFloat(c.Values[0], 64)
		if aerr != nil || berr != nil {
			return false // non-numeric operand: the comparison cannot hold
		}
		switch c.Op {
		case "gt":
			return a > b
		case "gte":
			return a >= b
		case "lt":
			return a < b
		default:
			return a <= b
		}
	case "contains", "not_contains":
		any := false
		for _, v := range c.Values {
			if strings.Contains(actual, v) {
				any = true
				break
			}
		}
		if c.Op == "contains" {
			return found && any
		}
		return !found || !any // not_contains: vacuously true when missing
	default:
		return false
	}
}

// lookupJSONPath walks a decoded JSON document along a dotted path. Object
// keys are matched exactly; purely numeric segments address array indexes.
// The leaf is stringified for comparison; a missing path returns found=false.
func lookupJSONPath(root any, path string) (string, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return "", false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return "", false
			}
			cur = node[idx]
		default:
			return "", false
		}
	}
	return stringifyJSON(cur), true
}

// stringifyJSON renders a JSON scalar for value comparison: json.Number as
// its exact source literal, booleans as true/false, null as "null",
// containers as compact JSON. A plain float64 (only possible when a caller
// pre-decodes without UseNumber) formats without a trailing ".0".
func stringifyJSON(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	case json.Number:
		return t.String()
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
