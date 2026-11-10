package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// AlertChannel is one outbound alert channel row (ADR-0001 §9.1). Feishu and
// Lark share the same bot-webhook API and differ only in domain. The webhook
// token is a credential — masked on read, redacted in audit rows, never
// logged.
type AlertChannel struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Enabled      bool      `json:"enabled"`
	Platform     string    `json:"platform"` // feishu | lark
	WebhookToken string    `json:"webhook_token,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// AlertChannelsHolder carries the runtime channel list between the sync
// worker (writer) and a future notifier (reader).
type AlertChannelsHolder struct {
	p atomic.Pointer[[]AlertChannel]
}

// NewAlertChannelsHolder starts empty.
func NewAlertChannelsHolder() *AlertChannelsHolder { return &AlertChannelsHolder{} }

func (h *AlertChannelsHolder) Get() []AlertChannel {
	if c := h.p.Load(); c != nil {
		return *c
	}
	return nil
}

func (h *AlertChannelsHolder) Set(c []AlertChannel) { h.p.Store(&c) }

// LoadAlertChannels reads every configured channel ordered by name. Rows are
// sanitized on the way out (enabled requires platform + token).
func LoadAlertChannels(ctx context.Context, db *sql.DB) ([]AlertChannel, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, enabled, platform, webhook_token, updated_at FROM alert_channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertChannel
	for rows.Next() {
		var c AlertChannel
		var updated sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.Enabled, &c.Platform, &c.WebhookToken, &updated); err != nil {
			continue
		}
		if updated.Valid {
			c.UpdatedAt = updated.Time
		}
		out = append(out, sanitizeAlertChannel(c))
	}
	return out, rows.Err()
}

// ValidateAlertChannel checks one channel for the admin plane: name length,
// platform, and token presence are enforced per operation by the handlers.
func ValidateAlertChannel(c AlertChannel) error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 64 {
		return fmt.Errorf("name is required (max 64 chars)")
	}
	if c.Platform != "feishu" && c.Platform != "lark" {
		return fmt.Errorf("platform must be feishu or lark")
	}
	if len(c.WebhookToken) > 256 {
		return fmt.Errorf("webhook token exceeds 256 chars")
	}
	return nil
}

// WebhookURL builds the bot webhook endpoint for the channel's platform.
func (c AlertChannel) WebhookURL() (string, error) {
	switch c.Platform {
	case "feishu":
		return "https://open.feishu.cn/open-apis/bot/v2/hook/" + c.WebhookToken, nil
	case "lark":
		return "https://open.larksuite.com/open-apis/bot/v2/hook/" + c.WebhookToken, nil
	default:
		return "", fmt.Errorf("unknown alert platform %q", c.Platform)
	}
}

// MaskToken renders a credential-safe display form: only the last four
// characters remain.
func MaskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) <= 4 {
		return "••••"
	}
	return "••••" + t[len(t)-4:]
}

// sanitizeAlertChannel enforces the runtime invariants: a disabled-but-
// misconfigured row stays disabled; enabled rows must have platform + token.
func sanitizeAlertChannel(c AlertChannel) AlertChannel {
	c.Name = strings.TrimSpace(c.Name)
	c.WebhookToken = strings.TrimSpace(c.WebhookToken)
	if c.Platform != "feishu" && c.Platform != "lark" {
		c.Platform = ""
	}
	if c.WebhookToken == "" || c.Platform == "" {
		c.Enabled = false
	}
	return c
}
