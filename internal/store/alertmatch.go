package store

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// AlertMatchNode is the recursive match document for alert rules
// (ADR-0001 §9.2): a LEAF condition (Source set) or a GROUP (Mode + Children)
// combining nested nodes with all/any. The flat shape used by route success
// rules is the depth-1 special case and stays accepted for alert rules too.
type AlertMatchNode struct {
	Mode     string            `json:"mode,omitempty"` // group: all | any
	Source   string            `json:"source,omitempty"`
	Path     string            `json:"path,omitempty"`
	Op       string            `json:"op,omitempty"`
	Values   []string          `json:"values,omitempty"`
	Children []*AlertMatchNode `json:"children,omitempty"`
}

// IsLeaf reports whether the node is a leaf condition.
func (n *AlertMatchNode) IsLeaf() bool { return n != nil && n.Source != "" }

const (
	alertMatchMaxDepth  = 3
	alertMatchMaxNodes  = 32
	alertMatchMaxValues = 64
	alertMatchMaxBytes  = 8192
)

// validAlertSource reports whether s is an accepted leaf source. The two biz
// sources read the route's business fields (per-route paths with the global
// fallback, ADR-0001 §6.7) extracted from the response body; they are
// alert-only — route success rules keep the status/body grammar.
func validAlertSource(s string) bool {
	switch s {
	case "status", "body", "outcome", "biz_code", "biz_message":
		return true
	}
	return false
}

// ParseAlertMatch validates and normalizes the recursive match document.
// Accepted shapes:
//
//	{"mode":"all|any","children":[ node... ]}          — group (nested)
//	{"mode":"all|any","conditions":[ leaf... ]}        — legacy flat (depth 1)
//	leaf: {"source":"status|body|outcome|biz_code|biz_message",
//	       "op":...,"values":[...]}
//
// Values are normalized exactly like route success rules (numbers keep their
// literal text, status codes 100-599, outcome success|failure). Depth is
// capped at 3 and the total node count at 32.
func ParseAlertMatch(raw []byte) (*AlertMatchNode, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("match document is required")
	}
	if len(trimmed) > alertMatchMaxBytes {
		return nil, fmt.Errorf("match document exceeds %d bytes", alertMatchMaxBytes)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, fmt.Errorf("match document is not valid JSON: %w", err)
	}
	node, err := parseMatchNode(doc, 1, new(int))
	if err != nil {
		return nil, err
	}
	if node == nil || node.IsLeaf() {
		return nil, fmt.Errorf("match document root must be a group with children")
	}
	if len(node.Children) == 0 {
		return nil, fmt.Errorf("match document needs at least one condition")
	}
	return node, nil
}

func parseMatchNode(raw map[string]any, depth int, count *int) (*AlertMatchNode, error) {
	if depth > alertMatchMaxDepth {
		return nil, fmt.Errorf("match nesting deeper than %d levels", alertMatchMaxDepth)
	}
	*count++
	if *count > alertMatchMaxNodes {
		return nil, fmt.Errorf("match document has more than %d nodes", alertMatchMaxNodes)
	}
	mode, _ := raw["mode"].(string)
	source, _ := raw["source"].(string)
	path, _ := raw["path"].(string)
	op, _ := raw["op"].(string)

	if source != "" { // leaf
		node := &AlertMatchNode{Source: strings.ToLower(source), Path: path, Op: strings.ToLower(op)}
		if !validAlertSource(node.Source) {
			return nil, fmt.Errorf("condition source must be status, body, outcome, biz_code or biz_message, got %q", source)
		}
		if node.Op != "in" && node.Op != "not_in" && node.Op != "eq" && node.Op != "neq" &&
			node.Op != "gt" && node.Op != "gte" && node.Op != "lt" && node.Op != "lte" &&
			node.Op != "contains" && node.Op != "not_contains" {
			return nil, fmt.Errorf("condition op %q is unknown", op)
		}
		rawValues, ok := raw["values"].([]any)
		if !ok || len(rawValues) == 0 {
			return nil, fmt.Errorf("condition values list is empty")
		}
		if len(rawValues) > alertMatchMaxValues {
			return nil, fmt.Errorf("condition has more than %d values", alertMatchMaxValues)
		}
		numericOnly := node.Op == "gt" || node.Op == "gte" || node.Op == "lt" || node.Op == "lte"
		if (node.Op == "eq" || node.Op == "neq" || numericOnly) && len(rawValues) != 1 {
			return nil, fmt.Errorf("op %q takes exactly one value", node.Op)
		}
		for _, rv := range rawValues {
			v := normalizeMatchValue(rv)
			if v == "" || len(v) > 256 {
				return nil, fmt.Errorf("condition value must be 1..256 chars")
			}
			if node.Source == "status" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 100 || n > 599 {
					return nil, fmt.Errorf("status value %q must be an HTTP status (100-599)", v)
				}
				v = strconv.Itoa(n)
			}
			if node.Source == "outcome" {
				v = strings.ToLower(v)
				if v != "success" && v != "failure" {
					return nil, fmt.Errorf("outcome value %q must be success or failure", v)
				}
			}
			if numericOnly {
				if _, err := strconv.ParseFloat(v, 64); err != nil {
					return nil, fmt.Errorf("op %q needs a numeric value, got %q", node.Op, v)
				}
			}
			node.Values = append(node.Values, v)
		}
		if node.Source == "body" {
			if err := ValidateBodyPath(node.Path); err != nil {
				return nil, err
			}
		}
		return node, nil
	}

	// group
	if mode != "all" && mode != "any" {
		return nil, fmt.Errorf("group mode must be all or any, got %q", mode)
	}
	node := &AlertMatchNode{Mode: mode}
	children, ok := raw["children"].([]any)
	if !ok {
		children, ok = raw["conditions"].([]any) // legacy flat alias
		if !ok {
			return nil, fmt.Errorf("group needs children")
		}
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("group has no children")
	}
	if len(children) > alertMatchMaxNodes {
		return nil, fmt.Errorf("group has more than %d children", alertMatchMaxNodes)
	}
	for i, ch := range children {
		childMap, cok := ch.(map[string]any)
		if !cok {
			return nil, fmt.Errorf("child %d is not an object", i+1)
		}
		child, err := parseMatchNode(childMap, depth+1, count)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, child)
	}
	return node, nil
}

// normalizeMatchValue stringifies a JSON value with the same contract as
// route success rules: strings as-is, numbers as their literal text,
// booleans as true/false, containers as compact JSON.
func normalizeMatchValue(rv any) string {
	switch v := rv.(type) {
	case string:
		return strings.TrimSpace(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1e15 {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case nil:
		return "null"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// parseMatchLeafCompat converts a legacy flat []snapshot.SuccessCondition
// document into a root group node (used for backward compatibility when old
// rows carry the flat shape).
func parseMatchLeafCompat(conds []snapshot.SuccessCondition, mode string) *AlertMatchNode {
	node := &AlertMatchNode{Mode: mode}
	for i := range conds {
		c := &conds[i]
		node.Children = append(node.Children, &AlertMatchNode{
			Source: c.Source, Path: c.Path, Op: c.Op, Values: c.Values,
		})
	}
	return node
}
