package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// successConditionRaw is the wire form of a condition before value
// normalization (values may be JSON strings or numbers).
type successConditionRaw struct {
	Source string            `json:"source"`
	Path   string            `json:"path,omitempty"`
	Op     string            `json:"op"`
	Values []json.RawMessage `json:"values"`
}

// ParseSuccessRule validates and normalizes a route success rule (ADR-0001
// §6.7). It returns nil when raw is empty/NULL (no rule configured).
func ParseSuccessRule(raw []byte) (*snapshot.SuccessRule, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if len(trimmed) > 4096 {
		return nil, fmt.Errorf("success rule exceeds 4 KiB")
	}
	// Values accept JSON strings OR numbers ("0" and 0 both mean the string
	// "0"): parse through a shadow type with raw values, then stringify.
	var shadow struct {
		Mode       string                 `json:"mode"`
		Conditions []*successConditionRaw `json:"conditions"`
	}
	if err := json.Unmarshal([]byte(trimmed), &shadow); err != nil {
		return nil, fmt.Errorf("success rule is not valid JSON: %w", err)
	}
	r := &snapshot.SuccessRule{Mode: shadow.Mode}
	for _, c := range shadow.Conditions {
		if c == nil {
			r.Conditions = append(r.Conditions, nil)
			continue
		}
		vals := make([]string, 0, len(c.Values))
		for _, raw := range c.Values {
			t := strings.TrimSpace(string(raw))
			if strings.HasPrefix(t, "\"") {
				var sv string
				if json.Unmarshal(raw, &sv) != nil {
					return nil, fmt.Errorf("success rule value %s is not a valid string", t)
				}
				vals = append(vals, sv)
			} else {
				vals = append(vals, t) // number/bool literal → its literal text
			}
		}
		r.Conditions = append(r.Conditions, &snapshot.SuccessCondition{
			Source: c.Source, Path: c.Path, Op: c.Op, Values: vals,
		})
	}
	switch r.Mode {
	case "":
		r.Mode = "all"
	case "all", "any":
	default:
		return nil, fmt.Errorf("success rule mode must be all or any, got %q", r.Mode)
	}
	if len(r.Conditions) == 0 {
		return nil, fmt.Errorf("success rule needs at least one condition")
	}
	if len(r.Conditions) > 16 {
		return nil, fmt.Errorf("success rule supports at most 16 conditions")
	}
	for i, c := range r.Conditions {
		if c == nil {
			return nil, fmt.Errorf("condition %d is empty", i+1)
		}
		switch c.Source {
		case "status":
		case "body":
			if err := validateBodyPath(c.Path); err != nil {
				return nil, fmt.Errorf("condition %d: %w", i+1, err)
			}
		default:
			return nil, fmt.Errorf("condition %d: source must be status or body, got %q", i+1, c.Source)
		}
		switch c.Op {
		case "in", "not_in", "contains", "not_contains":
			if len(c.Values) > 64 {
				return nil, fmt.Errorf("condition %d: at most 64 values", i+1)
			}
		case "eq", "neq", "gt", "gte", "lt", "lte":
			if len(c.Values) != 1 {
				return nil, fmt.Errorf("condition %d: op %q takes exactly one value", i+1, c.Op)
			}
		default:
			return nil, fmt.Errorf("condition %d: unknown op %q", i+1, c.Op)
		}
		if len(c.Values) == 0 {
			return nil, fmt.Errorf("condition %d: values list is empty", i+1)
		}
		numericOnly := c.Op == "gt" || c.Op == "gte" || c.Op == "lt" || c.Op == "lte"
		normalized := make([]string, 0, len(c.Values))
		for _, v := range c.Values {
			v = strings.TrimSpace(v)
			if v == "" || len(v) > 256 {
				return nil, fmt.Errorf("condition %d: value must be 1..256 chars", i+1)
			}
			if c.Source == "status" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 100 || n > 599 {
					return nil, fmt.Errorf("condition %d: status value %q must be an HTTP status (100-599)", i+1, v)
				}
				v = strconv.Itoa(n)
			}
			if numericOnly {
				if _, err := strconv.ParseFloat(v, 64); err != nil {
					return nil, fmt.Errorf("condition %d: op %q needs a numeric value, got %q", i+1, c.Op, v)
				}
			}
			normalized = append(normalized, v)
		}
		c.Values = normalized
	}
	return r, nil
}

// validateBodyPath checks the dotted JSON path grammar: 1..128 chars,
// non-empty segments of letters/digits/underscore/dash; purely numeric
// segments address array indexes.
// ValidateBodyPath exposes the dotted JSON path grammar to the admin plane
// (business-envelope fields reuse the success-rule path syntax).
func ValidateBodyPath(path string) error {
	return validateBodyPath(path)
}

func validateBodyPath(path string) error {
	if path == "" || len(path) > 128 {
		return fmt.Errorf("body path must be 1..128 chars")
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "" || len(seg) > 64 {
			return fmt.Errorf("body path %q has an empty or oversized segment", path)
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return fmt.Errorf("body path %q: segment %q may only contain letters, digits, '_' and '-'", path, seg)
			}
		}
	}
	return nil
}
