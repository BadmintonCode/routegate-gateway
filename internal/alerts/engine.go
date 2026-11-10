// Package alerts evaluates alert rules over finished requests and dispatches
// firings: one bounded asynchronous queue feeds a dispatcher that sends the
// webhook message and records the firing in alert_events. There is no noise
// suppression or rate limiting in v1 (explicit owner decision, ADR-0001
// §9.2): every matching request fires, so a full queue drops and counts
// rather than ever blocking traffic.
package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/proxy"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// Engine evaluates rules and dispatches firings. The rules and channels
// holders are written by the sync worker; the engine only reads them.
type Engine struct {
	rules    *store.AlertRulesHolder
	channels *store.AlertChannelsHolder
	db       *sql.DB
	instance string
	queue    chan job
	dropped  atomic.Uint64
	client   *http.Client
	stop     chan struct{}
	done     chan struct{}
}

type job struct {
	rule       store.AlertRule
	channel    store.AlertChannel // resolved snapshot; disabled/empty when missing
	routeName  string
	path       string
	requestID  string
	status     int
	outcome    string
	bizCode    string
	bizMessage string
	matched    []string
}

// NewEngine starts the dispatcher goroutine.
func NewEngine(rules *store.AlertRulesHolder, channels *store.AlertChannelsHolder, db *sql.DB, instance string) *Engine {
	e := &Engine{
		rules: rules, channels: channels, db: db, instance: instance,
		queue: make(chan job, 256),
		client: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(), // honors HTTPS_PROXY env
			Timeout:   5 * time.Second,
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go e.dispatch()
	return e
}

// HasRules reports whether any enabled rule is loaded (the data plane uses
// it to skip response capture when nothing can fire).
func (e *Engine) HasRules() bool {
	for _, r := range e.rules.Get() {
		if r.Enabled {
			return true
		}
	}
	return false
}

// Dropped reports the number of firings dropped by the full queue.
func (e *Engine) Dropped() uint64 { return e.dropped.Load() }

// Close stops the dispatcher and waits for it to exit. Queued firings are
// dropped, not drained: dispatch returns immediately on stop (documented v1
// decision — a full queue already drops by design, and shutdown must not
// block on webhook sends).
func (e *Engine) Close() {
	close(e.stop)
	<-e.done
}

// Evaluate matches the finished request against the loaded rules. Called on
// the request goroutine; matching is cheap, sending is asynchronous.
func (e *Engine) Evaluate(routeID int64, rec accesslog.Record, status int, body []byte) {
	rules := e.rules.Get()
	if len(rules) == 0 {
		return
	}
	channels := channelIndex(e.channels.Get())

	var root any
	needBody := false
	for _, r := range rules {
		if !r.Enabled || !scopeCovers(r, routeID) {
			continue
		}
		if matchNeedsBody(r.Match) {
			needBody = true
			break
		}
	}
	if needBody {
		// Same bounded capture and decode semantics as the verdict: a prefix
		// cut mid-value keeps the envelope members completed before the cut.
		root = proxy.DecodeJSONBody(body)
	}

	for _, rule := range rules {
		if !rule.Enabled || !scopeCovers(rule, routeID) {
			continue
		}
		matched := make([]string, 0, 8)
		if !evalMatchNode(rule.Match, status, root, rec.Outcome, rec.BizCode, rec.BizMessage, &matched) {
			continue
		}
		ch, ok := channels[rule.ChannelID]
		if !ok {
			ch = store.AlertChannel{ID: rule.ChannelID, Enabled: false}
		}
		select {
		case e.queue <- job{
			rule: rule, channel: ch,
			routeName: rec.Route, path: rec.Path, requestID: rec.RequestID,
			status: status, outcome: rec.Outcome, bizCode: rec.BizCode,
			bizMessage: rec.BizMessage,
			matched:    matched,
		}:
		default:
			e.dropped.Add(1)
		}
	}
}

// evalMatchNode recursively evaluates the match document: leaves resolve and
// match through the shared condition grammar; groups combine children with
// all/any. The alert-only biz_code/biz_message sources read the route's
// business fields carried on the access record (per-route paths with the
// global fallback, §6.7); everything else resolves through the shared
// grammar. Matched leaf descriptions are collected for the alert record.
func evalMatchNode(n *store.AlertMatchNode, status int, root any, outcome, bizCode, bizMessage string, matched *[]string) bool {
	if n == nil {
		return false
	}
	if n.IsLeaf() {
		c := snapshot.SuccessCondition{Source: n.Source, Path: n.Path, Op: n.Op, Values: n.Values}
		var actual string
		var found bool
		switch n.Source {
		case "biz_code", "biz_message":
			// An empty extraction means "missing", so the shared matcher's
			// asymmetric missing-value semantics apply unchanged (§6.7).
			actual, found = bizCode, bizCode != ""
			if n.Source == "biz_message" {
				actual, found = bizMessage, bizMessage != ""
			}
		default:
			actual, found = proxy.ResolveConditionValue(&c, status, root, outcome)
		}
		ok := proxy.MatchCondition(&c, actual, found)
		if ok {
			*matched = append(*matched, describe(&c))
		}
		return ok
	}
	if len(n.Children) == 0 {
		return false
	}
	if n.Mode == "any" {
		for _, child := range n.Children {
			if evalMatchNode(child, status, root, outcome, bizCode, bizMessage, matched) {
				return true
			}
		}
		return false
	}
	for _, child := range n.Children {
		if !evalMatchNode(child, status, root, outcome, bizCode, bizMessage, matched) {
			return false
		}
	}
	return true
}

// matchNeedsBody reports whether the document contains a body source leaf.
func matchNeedsBody(n *store.AlertMatchNode) bool {
	if n == nil {
		return false
	}
	if n.IsLeaf() {
		return n.Source == "body"
	}
	for _, child := range n.Children {
		if matchNeedsBody(child) {
			return true
		}
	}
	return false
}

func (e *Engine) dispatch() {
	defer close(e.done)
	for {
		select {
		case <-e.stop:
			return
		case j := <-e.queue:
			e.fire(j)
		}
	}
}

// fire sends the webhook message and records the firing — always recorded,
// even when the send fails (sent=0 + bounded send_error).
func (e *Engine) fire(j job) {
	sent := false
	sendErr := ""
	if j.channel.Enabled && j.channel.WebhookToken != "" {
		if url, err := j.channel.WebhookURL(); err == nil {
			b, _ := json.Marshal(buildAlertCard(j, e.instance, time.Now()))
			resp, perr := e.client.Post(url, "application/json", bytes.NewReader(b))
			if perr != nil {
				// url.Error embeds the URL, which contains the token — redact.
				sendErr = redact(perr.Error(), j.channel.WebhookToken)
			} else {
				respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				resp.Body.Close()
				ok2xx := resp.StatusCode >= 200 && resp.StatusCode < 300
				// Feishu/Lark answer HTTP 200 even for invalid tokens — the
				// delivery verdict lives in the response body (code==0 /
				// StatusCode==0 / msg success). A non-JSON body falls back to
				// the HTTP status (generic webhooks).
				sent = ok2xx && webhookBodySaysSuccess(respBody)
				if !sent {
					sendErr = truncate("webhook rejected: HTTP "+strconv.Itoa(resp.StatusCode)+" "+string(respBody), 255)
				}
			}
		} else {
			sendErr = redact(err.Error(), j.channel.WebhookToken)
		}
	} else {
		sendErr = "channel missing or disabled"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("alerts: record firing failed: %v", err)
		return
	}
	detail, _ := json.Marshal(map[string]any{"matched": j.matched, "match_mode": j.rule.MatchMode})
	ev := store.AlertEvent{
		RuleID: j.rule.ID, RuleName: j.rule.Name, ChannelID: j.rule.ChannelID,
		RouteName: j.routeName, RequestID: j.requestID, Status: j.status,
		Outcome: j.outcome, BizCode: j.bizCode, BizMessage: j.bizMessage,
		Detail: detail, Sent: sent, SendError: truncate(sendErr, 255),
	}
	if err := store.InsertAlertEvent(ctx, tx, ev); err != nil {
		tx.Rollback()
		log.Printf("alerts: record firing failed: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("alerts: record firing commit failed: %v", err)
	}
}

// scopeCovers reports whether the rule's scope includes the route.
func scopeCovers(r store.AlertRule, routeID int64) bool {
	if r.ScopeType == "all" {
		return true
	}
	for _, id := range r.RouteIDs {
		if id == routeID {
			return true
		}
	}
	return false
}

func channelIndex(channels []store.AlertChannel) map[int64]store.AlertChannel {
	m := make(map[int64]store.AlertChannel, len(channels))
	for _, c := range channels {
		m[c.ID] = c
	}
	return m
}

func describe(c *snapshot.SuccessCondition) string {
	if c.Source == "body" && c.Path != "" {
		return fmt.Sprintf("%s.%s %s %v", c.Source, c.Path, c.Op, c.Values)
	}
	return fmt.Sprintf("%s %s %v", c.Source, c.Op, c.Values)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// webhookBodySaysSuccess interprets a bot-webhook response body: Feishu/Lark
// answer HTTP 200 with {"code":0,...} / {"StatusCode":0,...} on success and a
// non-zero code on rejection. Non-JSON bodies fall back to "looks fine" (the
// caller already required a 2xx).
func webhookBodySaysSuccess(body []byte) bool {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return true
	}
	for _, key := range []string{"code", "StatusCode"} {
		switch v := doc[key].(type) {
		case json.Number:
			if n, err := v.Float64(); err == nil && n != 0 {
				return false
			}
		case float64:
			if v != 0 {
				return false
			}
		case string:
			if v != "" && v != "0" {
				return false
			}
		}
	}
	return true
}

// redact removes the credential from error text (url.Error embeds the URL).
func redact(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, store.MaskToken(token))
}
