package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseAlertConditions(t *testing.T) {
	raw := []byte(`{"mode":"any","conditions":[
		{"source":"outcome","op":"in","values":["failure"]},
		{"source":"status","op":"gte","values":[500]},
		{"source":"body","path":"data.code","op":"eq","values":[7]}
	]}`)
	conds, err := ParseAlertConditions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(conds) != 3 || conds[0].Source != "outcome" || conds[0].Values[0] != "failure" || conds[1].Values[0] != "500" {
		t.Fatalf("parsed wrong: %+v", conds)
	}
	for name, raw := range map[string]string{
		"bad source":      `{"conditions":[{"source":"header","op":"in","values":["x"]}]}`,
		"bad outcome":     `{"conditions":[{"source":"outcome","op":"in","values":["maybe"]}]}`,
		"outcome with gt": `{"conditions":[{"source":"outcome","op":"gt","values":["failure"]}]}`,
		"empty":           `{"conditions":[]}`,
		"bad mode":        `{"mode":"xor","conditions":[{"source":"status","op":"in","values":["200"]}]}`,
	} {
		if _, err := ParseAlertConditions([]byte(raw)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// TestAlertRuleJSONDecoding is the regression test for the missing JSON tags:
// AlertRule and AlertEvent are marshaled into admin_audit rows, where Go-style
// keys ("ID", "ScopeType") made the audit trail inconsistent with the panel's
// snake_case contract.
func TestAlertRuleJSONDecoding(t *testing.T) {
	r := AlertRule{
		ID: 3, Name: "5xx storm", Enabled: true,
		ScopeType: "routes", RouteIDs: []int64{1, 2}, ChannelID: 9,
		MatchMode: "all",
		Match: &AlertMatchNode{Mode: "all", Children: []*AlertMatchNode{
			{Source: "status", Op: "gte", Values: []string{"500"}},
		}},
		UpdatedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, key := range []string{`"id":`, `"name":`, `"enabled":`, `"scope_type":`, `"route_ids":`,
		`"channel_id":`, `"match_mode":`, `"match":`, `"updated_at":`} {
		if !strings.Contains(line, key) {
			t.Errorf("AlertRule JSON missing key %s: %s", key, line)
		}
	}
	for _, goKey := range []string{`"ID":`, `"ScopeType":`, `"ChannelID":`, `"MatchMode":`, `"UpdatedAt":`} {
		if strings.Contains(line, goKey) {
			t.Errorf("AlertRule JSON leaked Go-style key %s: %s", goKey, line)
		}
	}
	var back AlertRule
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != r.ID || back.Name != r.Name || !back.Enabled || back.ScopeType != r.ScopeType ||
		len(back.RouteIDs) != 2 || back.ChannelID != r.ChannelID || back.MatchMode != r.MatchMode ||
		back.Match == nil || len(back.Match.Children) != 1 || !back.UpdatedAt.Equal(r.UpdatedAt) {
		t.Errorf("AlertRule round-trip mismatch: %+v", back)
	}
}

func TestAlertEventJSONDecoding(t *testing.T) {
	e := AlertEvent{
		ID: 11, RuleID: 3, RuleName: "5xx storm", ChannelID: 9,
		RouteName: "alpha", RequestID: "req_123", Status: 502,
		Outcome: "failure", BizCode: "B0001",
		Detail: []byte(`{"matched":["status gte [500]"]}`),
		Sent:   false, SendError: "webhook rejected", CreatedAt: time.Date(2026, 11, 8, 10, 1, 0, 0, time.UTC),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, key := range []string{`"id":`, `"rule_id":`, `"rule_name":`, `"channel_id":`, `"route_name":`,
		`"request_id":`, `"http_status":`, `"outcome":`, `"biz_code":`, `"detail":`,
		`"sent":`, `"send_error":`, `"created_at":`} {
		if !strings.Contains(line, key) {
			t.Errorf("AlertEvent JSON missing key %s: %s", key, line)
		}
	}
	for _, goKey := range []string{`"RuleID":`, `"RuleName":`, `"SendError":`, `"CreatedAt":`} {
		if strings.Contains(line, goKey) {
			t.Errorf("AlertEvent JSON leaked Go-style key %s: %s", goKey, line)
		}
	}
	var back AlertEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != e.ID || back.RuleID != e.RuleID || back.RuleName != e.RuleName ||
		back.ChannelID != e.ChannelID || back.RouteName != e.RouteName || back.RequestID != e.RequestID ||
		back.Status != e.Status || back.Outcome != e.Outcome || back.BizCode != e.BizCode ||
		back.Sent != e.Sent || back.SendError != e.SendError || !back.CreatedAt.Equal(e.CreatedAt) {
		t.Errorf("AlertEvent round-trip mismatch: %+v", back)
	}
	if string(back.Detail) != string(e.Detail) {
		t.Errorf("AlertEvent detail mismatch: %s vs %s", back.Detail, e.Detail)
	}
}
