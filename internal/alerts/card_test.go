package alerts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

func TestBuildAlertCard(t *testing.T) {
	j := job{
		rule: store.AlertRule{ID: 1, Name: "业务失败告警"}, channel: store.AlertChannel{},
		routeName: "demo-alpha", requestID: "req-1", status: 200,
		outcome: "failure", bizCode: "50001", bizMessage: "余额不足",
		path:    "/v1/payments/123",
		matched: []string{"outcome in [failure]", "body.msg contains [biz-fail]"},
	}
	card := buildAlertCard(j, "inst-1", time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	if card["msg_type"] != "interactive" {
		t.Fatalf("msg_type = %v", card["msg_type"])
	}
	cardBody := card["card"].(map[string]any)
	header := cardBody["header"].(map[string]any)
	if header["template"] != "red" {
		t.Errorf("failure header template = %v, want red", header["template"])
	}
	title := header["title"].(map[string]any)["content"].(string)
	for _, want := range []string{"业务失败告警", "demo-alpha"} {
		if !strings.Contains(title, want) {
			t.Errorf("title %q missing %q", title, want)
		}
	}
	b, _ := json.Marshal(card)
	line := string(b)
	for _, want := range []string{"业务失败告警", "demo-alpha", "req-1", "50001", "余额不足", "API 路径: /v1/payments/123", "outcome in [failure]", "inst-1"} {
		if !strings.Contains(line, want) {
			t.Errorf("card missing %q", want)
		}
	}
	// Success cards go green.
	j.outcome = "success"
	card = buildAlertCard(j, "inst-1", time.Now())
	if card["card"].(map[string]any)["header"].(map[string]any)["template"] != "green" {
		t.Error("success header should be green")
	}
	// Empty fields are omitted rather than rendered blank.
	j.bizCode = ""
	j.bizMessage = ""
	j.path = ""
	b2, _ := json.Marshal(buildAlertCard(j, "inst-1", time.Now()))
	if strings.Contains(string(b2), "业务码") || strings.Contains(string(b2), "**消息:**") || strings.Contains(string(b2), "API 路径") {
		t.Error("empty optional fields must be omitted")
	}
}

func TestTestMessageCard(t *testing.T) {
	card := TestMessageCard("inst-1", "ops-feishu", "lark", time.Now())
	b, _ := json.Marshal(card)
	if card["msg_type"] != "interactive" {
		t.Fatal("test card must be interactive")
	}
	for _, want := range []string{"测试消息", "ops-feishu", "lark", "inst-1"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("test card missing %q", want)
		}
	}
}
