package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// --- whitelist guard: the single most important test in this package ---
//
// It pins the model-reachable surface: the tools handed to the react agent
// are exactly the read-only allowlist, no more, no less. Any new tool must
// land here AND in buildTools, in the open, with a reviewed diff.

var allowlist = []string{
	"policy_simulate", "list_routes", "list_policies", "list_identities",
	"get_instances", "query_admin_audit", "query_alert_events", "query_access_logs",
}

func TestRegisteredToolsMatchReadOnlyAllowlist(t *testing.T) {
	e := NewEngine(nil, &snapshot.Holder{}, "", "test-instance")
	tools, err := e.buildTools(&recorder{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range tools {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		got[info.Name] = true
	}
	if len(tools) != len(allowlist) {
		t.Fatalf("registered %d tools, allowlist has %d", len(tools), len(allowlist))
	}
	for _, name := range allowlist {
		if !got[name] {
			t.Errorf("allowlisted tool %q is not registered", name)
		}
		delete(got, name)
	}
	for name := range got {
		t.Errorf("tool %q is registered but not on the read-only allowlist", name)
	}
	if names := e.ToolNames(); len(names) != len(allowlist) {
		t.Errorf("ToolNames() out of sync with the allowlist: %v", names)
	}
}

// --- tool implementations against an in-memory snapshot ---

func testSnapshot() *snapshot.Snapshot {
	return &snapshot.Snapshot{
		Revision: 7,
		LoadedAt: time.Now(),
		Routes: []*snapshot.Route{
			{ID: 1, Name: "alpha", PathPrefix: "/alpha", Targets: []snapshot.Target{{Host: "alpha-svc", Port: 8080, Weight: 100}}},
			{ID: 2, Name: "beta", PathPrefix: "/beta", Targets: []snapshot.Target{{Host: "beta-svc", Port: 8080, Weight: 100}}},
		},
		Policies: map[int64][]*snapshot.Policy{
			1: {{ID: 11, RouteID: 1, Caller: "service-a", Method: "*", Segments: []string{"**"}, PathPattern: "/alpha/**", Effect: "allow"}},
		},
		Identities: map[string]*snapshot.Identity{
			"fp": {Name: "caller-cert", KeyID: "caller", Fingerprint: "0123456789abcdef0123456789abcdef", NotAfter: time.Now().Add(time.Hour)},
		},
	}
}

func newTestEngine(holder *snapshot.Holder) *Engine {
	return NewEngine(nil, holder, "", "test-instance")
}

func TestSimulatePolicyAllowAndDeny(t *testing.T) {
	e := newTestEngine(&snapshot.Holder{})
	e.exec.holder.Store(testSnapshot())

	out, err := e.exec.simulatePolicy(context.Background(), policySimulateIn{Caller: "service-a", Method: "get", Path: "/alpha/v1/ping"})
	if err != nil || out["decision"] != "allow" || out["route"] != "alpha" {
		t.Fatalf("allow case: out=%v err=%v", out, err)
	}
	out, err = e.exec.simulatePolicy(context.Background(), policySimulateIn{Caller: "nobody", Path: "/beta/x"})
	if err != nil || out["decision"] != "deny" || out["route"] != "beta" {
		t.Fatalf("deny case: out=%v err=%v", out, err)
	}
	out, err = e.exec.simulatePolicy(context.Background(), policySimulateIn{Caller: "x", Path: "/no-such"})
	if err != nil || out["decision"] != "route_not_found" {
		t.Fatalf("route_not_found case: out=%v err=%v", out, err)
	}
	if _, err := e.exec.simulatePolicy(context.Background(), policySimulateIn{Caller: "x"}); err == nil {
		t.Error("empty path must be rejected")
	}
}

func TestListToolsWithNilSnapshot(t *testing.T) {
	e := newTestEngine(&snapshot.Holder{})
	for _, tc := range []struct {
		name string
		call func() (any, error)
	}{} {
		_ = tc
	}
	routes, err := e.exec.listRoutes(context.Background(), routeListIn{})
	if err != nil || len(routes) != 0 {
		t.Fatalf("nil snapshot routes: %v %v", routes, err)
	}
	pols, err := e.exec.listPolicies(context.Background(), policyListIn{})
	if err != nil || len(pols) != 0 {
		t.Fatalf("nil snapshot policies: %v %v", pols, err)
	}
	ids, err := e.exec.listIdentities(context.Background(), identityListIn{})
	if err != nil || len(ids) != 0 {
		t.Fatalf("nil snapshot identities: %v %v", ids, err)
	}
}

func TestListRoutesAndPoliciesFromSnapshot(t *testing.T) {
	e := newTestEngine(&snapshot.Holder{})
	e.exec.holder.Store(testSnapshot())

	routes, err := e.exec.listRoutes(context.Background(), routeListIn{Name: "alp"})
	if err != nil || len(routes) != 1 || routes[0]["name"] != "alpha" {
		t.Fatalf("filtered routes: %v %v", routes, err)
	}
	pols, err := e.exec.listPolicies(context.Background(), policyListIn{RouteName: "alpha"})
	if err != nil || len(pols) != 1 || pols[0]["effect"] != "allow" {
		t.Fatalf("filtered policies: %v %v", pols, err)
	}
	ids, err := e.exec.listIdentities(context.Background(), identityListIn{})
	if err != nil || len(ids) != 1 {
		t.Fatalf("identities: %v %v", ids, err)
	}
	if fp := ids[0]["fingerprint"].(string); strings.Contains(fp, "0123456789abcdef0123456789abcdef") {
		t.Errorf("identity fingerprint not truncated: %s", fp)
	}
}

func TestQueryAccessLogsUnavailableWithoutFileSink(t *testing.T) {
	e := newTestEngine(&snapshot.Holder{})
	out, err := e.exec.queryAccessLogs(context.Background(), accessLogQueryIn{})
	if err != nil {
		t.Fatalf("queryAccessLogs: %v", err)
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "not enabled") {
		t.Errorf("expected an explicit unavailable note, got %q", note)
	}
}

func TestBoundedJSONTruncates(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := boundedJSON(map[string]string{"v": long})
	if len(got) > maxArgJSON {
		t.Errorf("boundedJSON length %d exceeds cap %d", len(got), maxArgJSON)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated JSON must end with ellipsis marker: %q", got[len(got)-10:])
	}
}

// --- ChatStream against a fake OpenAI-compatible server ---
//
// Turn 1: the model calls list_routes; turn 2 (after the tool result comes
// back) it streams the final text. This exercises the full react loop: tool
// registration, execution, audit recording and result round-trip.

type fakeTurn struct {
	// respondWithTool non-empty → stream a tool call for that tool.
	respondWithTool string
	// reasoning chunks streamed before the tool call / answer.
	reasoningChunks []string
	// content chunks for the final text answer.
	contentChunks []string
}

func chunkJSON(delta map[string]any, finish any) string {
	b := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func writeSSE(w http.ResponseWriter, lines []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	f := w.(http.Flusher)
	for _, l := range lines {
		_, _ = w.Write([]byte("data: " + l + "\n\n"))
		f.Flush()
	}
}

// newFakeLLM serves the turns in order: a request expecting a tool call is
// detected by the absence of a "tool" role message in the history.
func newFakeLLM(t *testing.T, turns []fakeTurn) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	idx := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		mu.Lock()
		i := idx
		idx++
		mu.Unlock()
		if i >= len(turns) {
			i = len(turns) - 1
		}
		turn := turns[i]
		if turn.respondWithTool != "" && !strings.Contains(body, `"role":"tool"`) && !strings.Contains(body, `"role":"tool",`) {
			// Turn 1: stream reasoning deltas, then a whole tool call (name +
			// arguments split across two chunks like real providers do).
			lines := make([]string, 0, len(turn.reasoningChunks)+4)
			for _, rc := range turn.reasoningChunks {
				lines = append(lines, chunkJSON(map[string]any{"role": "assistant", "content": "", "reasoning_content": rc}, nil))
			}
			lines = append(lines,
				chunkJSON(map[string]any{"role": "assistant", "tool_calls": []map[string]any{
					{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": turn.respondWithTool, "arguments": ""}},
				}}, nil),
				chunkJSON(map[string]any{"tool_calls": []map[string]any{
					{"index": 0, "function": map[string]any{"arguments": "{}"}},
				}}, nil),
				chunkJSON(map[string]any{}, "tool_calls"),
				"[DONE]",
			)
			writeSSE(w, lines)
			return
		}
		// Final turn: stream the answer in chunks.
		lines := make([]string, 0, len(turn.contentChunks)+2)
		for _, c := range turn.contentChunks {
			lines = append(lines, chunkJSON(map[string]any{"role": "assistant", "content": c}, nil))
		}
		lines = append(lines, chunkJSON(map[string]any{}, "stop"), "[DONE]")
		writeSSE(w, lines)
	}))
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()
	b := make([]byte, 1<<20)
	n, _ := r.Body.Read(b)
	return string(b[:n])
}

func TestChatStreamToolLoop(t *testing.T) {
	srv := newFakeLLM(t, []fakeTurn{
		{respondWithTool: "list_routes", reasoningChunks: []string{"需要先查路由", "再汇总。"}},
		{contentChunks: []string{"有 2 条路由", "，其中 alpha 正常。"}},
	})
	defer srv.Close()

	e := newTestEngine(&snapshot.Holder{})
	e.exec.holder.Store(testSnapshot())

	var deltas, thinking, toolStarts []string
	var mu sync.Mutex
	var tools []ToolCallRecord
	history := []*schema.Message{
		schema.UserMessage("网关是做什么的?"),
		schema.AssistantMessage("RouteGate 是 mTLS 服务网关。", nil),
	}
	final, reasoning, rec, err := e.ChatStream(context.Background(), store.LLMEndpoint{
		Name: "test", Enabled: true, BaseURL: srv.URL,
	}, store.LLMEndpointModel{Enabled: true, ProviderModelName: "test-model", DisplayName: "Test Model"}, history, "看一下现在的路由", StreamCallbacks{
		OnDelta:     func(s string) { mu.Lock(); deltas = append(deltas, s); mu.Unlock() },
		OnThinking:  func(s string) { mu.Lock(); thinking = append(thinking, s); mu.Unlock() },
		OnToolStart: func(name string) { mu.Lock(); toolStarts = append(toolStarts, name); mu.Unlock() },
		OnTool:      func(r ToolCallRecord) { mu.Lock(); tools = append(tools, r); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if final != "有 2 条路由，其中 alpha 正常。" {
		t.Errorf("final text = %q", final)
	}
	if got := strings.Join(thinking, ""); got != "需要先查路由再汇总。" {
		t.Errorf("thinking deltas = %q, want the reasoning chain", got)
	}
	if reasoning != "需要先查路由再汇总。" {
		t.Errorf("returned reasoning = %q", reasoning)
	}
	if len(toolStarts) != 1 || toolStarts[0] != "list_routes" {
		t.Errorf("tool starts = %v, want [list_routes]", toolStarts)
	}
	if len(rec) != 1 || rec[0].Tool != "list_routes" || !rec[0].OK {
		t.Errorf("tool audit = %+v", rec)
	}
	if len(tools) != 1 {
		t.Errorf("OnTool fired %d times, want 1", len(tools))
	}
	joined := strings.Join(deltas, "")
	if joined != final {
		t.Errorf("deltas %q do not reassemble the final text %q", joined, final)
	}
}

func TestChatStreamRejectsUnusableEndpoint(t *testing.T) {
	e := newTestEngine(&snapshot.Holder{})
	model := store.LLMEndpointModel{Enabled: true, ProviderModelName: "m", DisplayName: "M"}
	if _, _, _, err := e.ChatStream(context.Background(), store.LLMEndpoint{Enabled: false, BaseURL: "http://x"}, model, nil, "hi", StreamCallbacks{}); err == nil {
		t.Error("disabled endpoint must be rejected")
	}
	if _, _, _, err := e.ChatStream(context.Background(), store.LLMEndpoint{Enabled: true, BaseURL: ""}, model, nil, "hi", StreamCallbacks{}); err == nil {
		t.Error("empty base_url must be rejected")
	}
	if _, _, _, err := e.ChatStream(context.Background(), store.LLMEndpoint{Enabled: true, BaseURL: "http://x"}, store.LLMEndpointModel{Enabled: false, ProviderModelName: "m", DisplayName: "M"}, nil, "hi", StreamCallbacks{}); err == nil {
		t.Error("disabled model must be rejected")
	}
}

func TestSystemPromptGroundsOnSnapshot(t *testing.T) {
	e := newTestEngine(&snapshot.Holder{})
	p := e.SystemPrompt()
	if !strings.Contains(p, "read-only") {
		t.Error("system prompt must state the read-only charter")
	}
	if !strings.Contains(p, "no config snapshot") {
		t.Error("nil snapshot must be surfaced, not hidden")
	}
	e.exec.holder.Store(testSnapshot())
	p = e.SystemPrompt()
	if !strings.Contains(p, "revision 7") || !strings.Contains(p, "2 routes") {
		t.Errorf("prompt lost the live status digest: %q", p)
	}
}
