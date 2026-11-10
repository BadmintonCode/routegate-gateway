package alerts

import (
	"fmt"
	"strings"
	"time"
)

// buildAlertCard renders the Feishu/Lark interactive card for one firing
// (ADR-0001 §9.2): header themed by the verdict (red=failure, green=success,
// orange=other), lark_md body, and a note footer with instance and time.
// Empty fields are omitted rather than rendered blank.
func buildAlertCard(j job, instance string, now time.Time) map[string]any {
	icon, verdict, template := "🔔", "—", "orange"
	switch j.outcome {
	case "failure":
		icon, verdict, template = "🚨", "❌ 失败", "red"
	case "success":
		icon, verdict, template = "✅", "✅ 成功", "green"
	}
	// The title names the rule and the route so the receiver knows at a
	// glance which interface produced the alert.
	title := icon + " RouteGate 告警 · " + j.rule.Name
	if j.routeName != "" {
		title += " · " + j.routeName
	}

	var md []string
	md = append(md, fmt.Sprintf("**规则:** %s", j.rule.Name))
	md = append(md, fmt.Sprintf("**路由:** %s", j.routeName))
	md = append(md, fmt.Sprintf("**请求:** %s", j.requestID))
	line := fmt.Sprintf("**HTTP:** %d · **判定:** %s", j.status, verdict)
	if j.bizCode != "" {
		line += fmt.Sprintf(" · **业务码:** %s", j.bizCode)
	}
	md = append(md, line)
	if len(j.matched) > 0 {
		md = append(md, fmt.Sprintf("**命中条件:** %s", strings.Join(j.matched, "、")))
	}

	elements := []map[string]any{
		{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": strings.Join(md, "\n")}},
	}
	if j.path != "" {
		elements = append(elements, map[string]any{
			"tag": "div", "text": map[string]any{"tag": "plain_text", "content": "API 路径: " + j.path},
		})
	}
	if j.bizMessage != "" {
		elements = append(elements,
			map[string]any{"tag": "hr"},
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": "**消息:** " + j.bizMessage}},
		)
	}
	elements = append(elements,
		map[string]any{"tag": "hr"},
		map[string]any{"tag": "note", "elements": []map[string]any{
			{"tag": "plain_text", "content": fmt.Sprintf("RouteGate · instance %s · %s", instance, now.UTC().Format("2006-01-02 15:04:05 UTC"))},
		}},
	)

	return map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config":   map[string]any{"wide_screen_mode": true},
			"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": title}, "template": template},
			"elements": elements,
		},
	}
}

// TestMessageCard renders the interactive card for the channel send-test
// action (exported for the admin plane). The channel name is shown in the
// body so the tester knows which credential delivered.
func TestMessageCard(instance, channel string, platform string, now time.Time) map[string]any {
	elements := []map[string]any{
		{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf(
			"**告警渠道 %q 配置成功。**\n**平台:** %s\n后续命中的告警规则将通过本渠道推送。", channel, platform)}},
		map[string]any{"tag": "hr"},
		map[string]any{"tag": "note", "elements": []map[string]any{
			{"tag": "plain_text", "content": fmt.Sprintf("RouteGate · instance %s · %s", instance, now.UTC().Format("2006-01-02 15:04:05 UTC"))},
		}},
	}
	return map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config":   map[string]any{"wide_screen_mode": true},
			"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": "✅ RouteGate 测试消息"}, "template": "green"},
			"elements": elements,
		},
	}
}
