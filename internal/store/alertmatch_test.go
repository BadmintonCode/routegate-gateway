package store

import (
	"strings"
	"testing"
)

func TestParseAlertMatchNested(t *testing.T) {
	doc := `{"mode":"all","children":[
		{"mode":"any","children":[
			{"source":"status","op":"in","values":[500,502]},
			{"source":"outcome","op":"eq","values":["failure"]}
		]},
		{"source":"body","path":"retryable","op":"eq","values":[true]}
	]}`
	root, err := ParseAlertMatch([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if root.Mode != "all" || len(root.Children) != 2 || root.Children[0].Mode != "any" {
		t.Fatalf("nested parse wrong: %+v", root)
	}
	inner := root.Children[0]
	if inner.Children[1].Values[0] != "failure" {
		t.Errorf("outcome value = %q", inner.Children[1].Values[0])
	}
	if root.Children[1].Values[0] != "true" {
		t.Errorf("bool value = %q, want true", root.Children[1].Values[0])
	}
}

func TestParseAlertMatchLegacyFlat(t *testing.T) {
	root, err := ParseAlertMatch([]byte(`{"mode":"all","conditions":[{"source":"outcome","op":"in","values":["failure"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 1 || root.Children[0].Source != "outcome" {
		t.Fatalf("legacy flat parse wrong: %+v", root)
	}
}

func TestParseAlertMatchBizSources(t *testing.T) {
	root, err := ParseAlertMatch([]byte(`{"mode":"all","children":[
		{"source":"biz_code","op":"in","values":["PAY.FAILED"]},
		{"source":"biz_message","op":"contains","values":["insufficient balance"]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if root.Children[0].Source != "biz_code" || root.Children[0].Values[0] != "PAY.FAILED" {
		t.Errorf("biz_code leaf wrong: %+v", root.Children[0])
	}
	if root.Children[1].Source != "biz_message" || root.Children[1].Values[0] != "insufficient balance" {
		t.Errorf("biz_message leaf wrong: %+v", root.Children[1])
	}
}

func TestParseAlertMatchUnknownSource(t *testing.T) {
	_, err := ParseAlertMatch([]byte(`{"mode":"all","children":[{"source":"headers","op":"in","values":["x"]}]}`))
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Errorf("unknown source must be rejected, got %v", err)
	}
}

func TestParseAlertMatchLimits(t *testing.T) {
	deep := `{"mode":"all","children":[{"mode":"any","children":[{"mode":"all","children":[{"mode":"any","children":[{"source":"status","op":"in","values":["200"]}]}]}]}]}`
	if _, err := ParseAlertMatch([]byte(deep)); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Errorf("depth > 3 must be rejected, got %v", err)
	}
	empty := `{"mode":"all","children":[]}`
	if _, err := ParseAlertMatch([]byte(empty)); err == nil {
		t.Error("empty group must be rejected")
	}
}
