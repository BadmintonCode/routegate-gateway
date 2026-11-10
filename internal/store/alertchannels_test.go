package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMaskToken(t *testing.T) {
	if got := MaskToken(""); got != "" {
		t.Errorf("MaskToken(\"\") = %q", got)
	}
	if got := MaskToken("abcd"); got != "••••" {
		t.Errorf("short token = %q, want fully masked", got)
	}
	if got := MaskToken("abcdefghijklmnopwxyz"); got != "••••wxyz" {
		t.Errorf("token = %q, want ••••wxyz", got)
	}
}

func TestAlertChannelWebhookURL(t *testing.T) {
	feishu, err := (AlertChannel{Platform: "feishu", WebhookToken: "tok123"}).WebhookURL()
	if err != nil || feishu != "https://open.feishu.cn/open-apis/bot/v2/hook/tok123" {
		t.Errorf("feishu URL = %q, err %v", feishu, err)
	}
	lark, err := (AlertChannel{Platform: "lark", WebhookToken: "tok123"}).WebhookURL()
	if err != nil || lark != "https://open.larksuite.com/open-apis/bot/v2/hook/tok123" {
		t.Errorf("lark URL = %q, err %v", lark, err)
	}
	if _, err := (AlertChannel{Platform: "dingtalk", WebhookToken: "tok"}).WebhookURL(); err == nil {
		t.Error("unknown platform must be rejected")
	}
}

func TestValidateAlertChannel(t *testing.T) {
	if err := ValidateAlertChannel(AlertChannel{Name: "ops", Platform: "feishu"}); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	if err := ValidateAlertChannel(AlertChannel{Name: "", Platform: "feishu"}); err == nil {
		t.Error("empty name must be rejected")
	}
	if err := ValidateAlertChannel(AlertChannel{Name: strings.Repeat("x", 65), Platform: "feishu"}); err == nil {
		t.Error("oversized name must be rejected")
	}
	if err := ValidateAlertChannel(AlertChannel{Name: "ops", Platform: "dingtalk"}); err == nil {
		t.Error("unknown platform must be rejected")
	}
	if err := ValidateAlertChannel(AlertChannel{Name: "ops", Platform: "feishu", WebhookToken: strings.Repeat("x", 257)}); err == nil {
		t.Error("oversized token must be rejected")
	}
}

func TestSanitizeAlertChannel(t *testing.T) {
	c := sanitizeAlertChannel(AlertChannel{Name: "ops", Enabled: true, Platform: "", WebhookToken: "tok"})
	if c.Enabled || c.Platform != "" {
		t.Errorf("enabled without platform must be disabled: %+v", c)
	}
	c = sanitizeAlertChannel(AlertChannel{Name: "ops", Enabled: true, Platform: "feishu", WebhookToken: ""})
	if c.Enabled {
		t.Errorf("enabled without token must be disabled: %+v", c)
	}
	c = sanitizeAlertChannel(AlertChannel{Name: " ops ", Enabled: true, Platform: "lark", WebhookToken: " ok "})
	if !c.Enabled || c.Name != "ops" || c.WebhookToken != "ok" {
		t.Errorf("valid channel trimmed wrongly: %+v", c)
	}
}

func TestAlertChannelsHolder(t *testing.T) {
	h := NewAlertChannelsHolder()
	if len(h.Get()) != 0 {
		t.Fatal("holder must start empty")
	}
	h.Set([]AlertChannel{{ID: 1, Name: "ops", Platform: "feishu", Enabled: true}})
	if len(h.Get()) != 1 || h.Get()[0].Name != "ops" {
		t.Fatalf("holder round-trip failed: %+v", h.Get())
	}
}

// TestAlertChannelJSONDecoding is the regression test for the missing JSON
// tags: the admin plane decodes snake_case request bodies, which Go's
// case-insensitive field matching does NOT cover (webhook_token must map to
// WebhookToken).
func TestAlertChannelJSONDecoding(t *testing.T) {
	var c AlertChannel
	body := `{"name":"ops","enabled":true,"platform":"feishu","webhook_token":"tok-secret"}`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "ops" || !c.Enabled || c.Platform != "feishu" || c.WebhookToken != "tok-secret" {
		t.Fatalf("decoding lost fields: %+v", c)
	}
	// Serialization never leaks an empty credential field.
	b, _ := json.Marshal(AlertChannel{Name: "n", WebhookToken: ""})
	if strings.Contains(string(b), "webhook_token") {
		t.Errorf("empty token serialized into JSON: %s", b)
	}
}
