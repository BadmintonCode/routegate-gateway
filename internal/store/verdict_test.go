package store

import (
	"strings"
	"testing"
)

func TestParseSuccessRule(t *testing.T) {
	r, err := ParseSuccessRule([]byte(`{"mode":"all","conditions":[{"source":"status","op":"in","values":["200","201"]},{"source":"body","path":"data.code","op":"not_in","values":["500"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "all" || len(r.Conditions) != 2 || r.Conditions[1].Path != "data.code" {
		t.Errorf("parsed rule wrong: %+v", r)
	}
	// single-value ops normalize
	r, err = ParseSuccessRule([]byte(`{"conditions":[{"source":"body","path":"elapsed_ms","op":"lt","values":[" 1000 "]},{"source":"status","op":"eq","values":[200]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Conditions[0].Values[0] != "1000" || r.Conditions[1].Values[0] != "200" {
		t.Errorf("single-value ops normalization wrong: %+v", r.Conditions)
	}
	// defaults + normalization
	r, err = ParseSuccessRule([]byte(`{"conditions":[{"source":"status","op":"in","values":[200,"201"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "all" || r.Conditions[0].Values[0] != "200" || r.Conditions[0].Values[1] != "201" {
		t.Errorf("defaults/normalization wrong: %+v", r.Conditions[0])
	}
	// empty/null → no rule
	for _, raw := range []string{``, `null`, `   `} {
		if r, _ := ParseSuccessRule([]byte(raw)); r != nil {
			t.Errorf("raw %q should yield no rule", raw)
		}
	}
	for name, raw := range map[string]string{
		"bad mode":        `{"mode":"xor","conditions":[{"source":"status","op":"in","values":["200"]}]}`,
		"no conditions":   `{"mode":"all","conditions":[]}`,
		"bad source":      `{"conditions":[{"source":"header","op":"in","values":["x"]}]}`,
		"body w/o path":   `{"conditions":[{"source":"body","op":"in","values":["x"]}]}`,
		"bad op":          `{"conditions":[{"source":"status","op":"like","values":["200"]}]}`,
		"empty values":    `{"conditions":[{"source":"status","op":"in","values":[]}]}`,
		"bad status":      `{"conditions":[{"source":"status","op":"in","values":["99"]}]}`,
		"non-int status":  `{"conditions":[{"source":"status","op":"in","values":["OK"]}]}`,
		"bad path char":   `{"conditions":[{"source":"body","op":"in","values":["x"],"path":"a..b"}]}`,
		"path with dot*":  `{"conditions":[{"source":"body","op":"in","values":["x"],"path":"a.*"}]}`,
		"too many values": `{"conditions":[{"source":"status","op":"in","values":["` + strings.Repeat("200\",", 65) + `200"]}]}`,
		"eq two values":   `{"conditions":[{"source":"status","op":"eq","values":["200","201"]}]}`,
		"gt non-numeric":  `{"conditions":[{"source":"body","path":"elapsed_ms","op":"gt","values":["fast"]}]}`,
	} {
		if _, err := ParseSuccessRule([]byte(raw)); err == nil {
			t.Errorf("%s: expected error for %s", name, raw)
		}
	}
}
