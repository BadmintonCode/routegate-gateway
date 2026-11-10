package alerts

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

func alertCond(source, path, op string, values []string) snapshot.SuccessCondition {
	return snapshot.SuccessCondition{Source: source, Path: path, Op: op, Values: values}
}

func TestScopeCovers(t *testing.T) {
	all := store.AlertRule{ScopeType: "all"}
	if !scopeCovers(all, 42) {
		t.Error("all scope covers any route")
	}
	selected := store.AlertRule{ScopeType: "routes", RouteIDs: []int64{1, 2}}
	if !scopeCovers(selected, 2) || scopeCovers(selected, 3) {
		t.Error("routes scope mismatch")
	}
}

func TestDescribe(t *testing.T) {
	cond := snapshot.SuccessCondition{Source: "status", Op: "in", Values: []string{"500"}}
	if d := describe(&cond); d != "status in [500]" {
		t.Errorf("describe = %q", d)
	}
	body := snapshot.SuccessCondition{Source: "body", Path: "code", Op: "eq", Values: []string{"0"}}
	if d := describe(&body); d != "body.code eq [0]" {
		t.Errorf("describe body = %q", d)
	}
}

// TestEvalMatchNodeBizSources covers the alert-only biz_code/biz_message
// sources: they read the access record's business fields, and an empty
// extraction is "missing" — positive matchers fail, not_in stays vacuously
// true (the §6.7 missing-value asymmetry).
func TestEvalMatchNodeBizSources(t *testing.T) {
	leaf := func(source, op string, values ...string) *store.AlertMatchNode {
		return &store.AlertMatchNode{Source: source, Op: op, Values: values}
	}
	group := func(mode string, children ...*store.AlertMatchNode) *store.AlertMatchNode {
		return &store.AlertMatchNode{Mode: mode, Children: children}
	}
	cases := []struct {
		name                string
		node                *store.AlertMatchNode
		bizCode, bizMessage string
		want                bool
	}{
		{"code in hits", leaf("biz_code", "in", "PAY.FAILED", "PAY.TIMEOUT"), "PAY.FAILED", "", true},
		{"code in misses on other value", leaf("biz_code", "in", "PAY.FAILED"), "PAY.OK", "", false},
		{"code in fails when missing", leaf("biz_code", "in", "PAY.FAILED"), "", "", false},
		{"code not_in vacuously true when missing", leaf("biz_code", "not_in", "PAY.FAILED"), "", "", true},
		{"code neq vacuously true when missing", leaf("biz_code", "neq", "PAY.FAILED"), "", "", true},
		{"message contains hits", leaf("biz_message", "contains", "insufficient"), "", "balance insufficient", true},
		{"message contains fails when missing", leaf("biz_message", "contains", "insufficient"), "PAY.FAILED", "", false},
		{"nested any over code and message", group("any",
			leaf("biz_code", "eq", "PAY.OK"),
			leaf("biz_message", "contains", "insufficient")), "PAY.OK", "", true},
	}
	for _, c := range cases {
		matched := []string{}
		if got := evalMatchNode(c.node, 200, nil, "failure", c.bizCode, c.bizMessage, &matched); got != c.want {
			t.Errorf("%s: evalMatchNode = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEvaluateCarriesRequestPathToAlertJob(t *testing.T) {
	rules := store.NewAlertRulesHolder()
	rules.Set([]store.AlertRule{{
		ID: 1, Name: "business-failure", Enabled: true, ScopeType: "all",
		Match: &store.AlertMatchNode{Source: "status", Op: "in", Values: []string{"500"}},
	}})
	e := &Engine{rules: rules, channels: store.NewAlertChannelsHolder(), queue: make(chan job, 1)}
	e.Evaluate(1, accesslog.Record{Path: "/v1/payments/123"}, 500, nil)

	select {
	case got := <-e.queue:
		if got.path != "/v1/payments/123" {
			t.Fatalf("alert path = %q, want request path", got.path)
		}
	default:
		t.Fatal("matching request did not enqueue an alert")
	}
}

// TestEvaluateTruncatedBodyEnvelope: alert body conditions see the same
// bounded capture as the verdict. An envelope cut past the capture limit
// still carries its completed code field, so a body.code rule must match
// instead of treating the whole body as missing.
func TestEvaluateTruncatedBodyEnvelope(t *testing.T) {
	chunk := `{"id":1,"name":"dictionary-entry","values":["alpha","beta","gamma"]},`
	body := `{"code":"common.success","message":"ok","data":{"items":[` +
		strings.Repeat(chunk, 200) + `{"id":0}]}}`
	truncated := []byte(body[:8*1024])

	rules := store.NewAlertRulesHolder()
	rules.Set([]store.AlertRule{{
		ID: 1, Name: "envelope-match", Enabled: true, ScopeType: "all",
		Match: &store.AlertMatchNode{Source: "body", Path: "code", Op: "eq", Values: []string{"common.success"}},
	}})
	e := &Engine{rules: rules, channels: store.NewAlertChannelsHolder(), queue: make(chan job, 1)}
	e.Evaluate(1, accesslog.Record{Path: "/v1/dictionaries"}, 200, truncated)

	select {
	case <-e.queue:
	default:
		t.Fatal("body.code rule did not match the truncated success envelope")
	}
}

// TestEngineFireRecordsEvent is a live regression for the full fire path:
// one matching rule fires into a missing channel (sent=0 + bounded error)
// and the firing lands in alert_events. Skipped unless ROUTEGATE_TEST_MYSQL_DSN
// points at an EMPTY scratch database.
func TestEngineFireRecordsEvent(t *testing.T) {
	dsn := os.Getenv("ROUTEGATE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ROUTEGATE_TEST_MYSQL_DSN not set; requires an empty scratch MySQL database")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM alert_rules`)
		db.Exec(`DELETE FROM alert_events`)
	})

	rules := store.NewAlertRulesHolder()
	channels := store.NewAlertChannelsHolder()
	e := NewEngine(rules, channels, db, "test-instance")
	t.Cleanup(func() { e.Close() })

	// One enabled rule scoped to route 1, channel 99 (does not exist). The
	// match exercises the biz_code source against the record's business field.
	rules.Set([]store.AlertRule{{
		ID: 1, Name: "r1", Enabled: true, ScopeType: "all", ChannelID: 99,
		Match: &store.AlertMatchNode{Mode: "all", Children: []*store.AlertMatchNode{
			{Source: "status", Op: "in", Values: []string{"200"}},
			{Source: "outcome", Op: "in", Values: []string{"failure"}},
			{Source: "biz_code", Op: "in", Values: []string{"PAY.FAILED"}},
		}},
	}})
	rec := accesslog.Record{
		Route: "demo-alpha", RequestID: "fire-1", Outcome: "failure",
		BizCode: "PAY.FAILED", BizMessage: "insufficient balance",
	}
	e.Evaluate(1, rec, 200, []byte(`{"code":1}`))

	// The dispatcher is asynchronous; poll for the recorded firing.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := store.ListAlertEvents(ctx, db, 1, 10)
		if len(events) == 1 {
			ev := events[0]
			if ev.RuleName != "r1" || ev.RouteName != "demo-alpha" || ev.Outcome != "failure" || ev.Sent {
				t.Fatalf("recorded firing wrong: %+v", ev)
			}
			if ev.BizCode != "PAY.FAILED" || ev.BizMessage != "insufficient balance" {
				t.Fatalf("business fields not persisted: biz_code=%q biz_message=%q", ev.BizCode, ev.BizMessage)
			}
			if ev.SendError == "" {
				t.Fatal("missing-channel firing must carry a send error")
			}
			if strings.Contains(ev.SendError, "tok-") {
				t.Fatal("send error leaked a credential")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("firing never recorded")
}

func TestWebhookBodySaysSuccess(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":0,"msg":"success"}`, true},
		{`{"StatusCode":0,"StatusMessage":"success"}`, true},
		{`{"code":99991400,"msg":"invalid token"}`, false},
		{`{"code":"0"}`, true},
		{`not-json`, true}, // non-JSON falls back to the HTTP verdict
	}
	for _, c := range cases {
		if got := webhookBodySaysSuccess([]byte(c.body)); got != c.want {
			t.Errorf("webhookBodySaysSuccess(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}
