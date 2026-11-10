package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	agent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// Engine executes chat turns against OpenAI-compatible endpoints configured
// in the panel. Each turn builds a request-scoped react agent whose tool set
// is exactly the read-only whitelist from tools.go; the concurrency limit is
// process-wide so a busy chat cannot saturate the admin server.
type Engine struct {
	exec          engine
	db            *sql.DB
	instanceID    string
	sem           chan struct{}
	activeStreams atomic.Int64
}

// Concurrency is the process-wide cap on simultaneous LLM turns.
const Concurrency = 2

// NewEngine wires the read-only execution context. accessLogPath is the
// file-sink base path ("" when the file sink is disabled — the access-log
// tool then reports itself unavailable).
func NewEngine(db *sql.DB, holder *snapshot.Holder, accessLogPath, instanceID string) *Engine {
	return &Engine{
		exec:       engine{db: db, holder: holder, accessLogPath: accessLogPath},
		db:         db,
		instanceID: instanceID,
		sem:        make(chan struct{}, Concurrency),
	}
}

// StreamCallbacks receives streaming progress for one turn. Callbacks are
// invoked while the turn runs and must not block.
type StreamCallbacks struct {
	// OnDelta receives incremental assistant text (final answer only).
	OnDelta func(text string)
	// OnThinking receives incremental reasoning deltas from ANY agent step
	// (the react graph does not route intermediate steps to the output
	// stream, so these come from the model callback observer).
	OnThinking func(text string)
	// OnToolStart fires when the model starts a tool call (its arguments may
	// still be streaming); OnTool fires after the tool completes with its
	// audit record.
	OnToolStart func(name string)
	OnTool      func(rec ToolCallRecord)
}

// MaxHistoryMessages bounds how much conversation the handler may pass in.
const MaxHistoryMessages = 30

// llmTimeout bounds one whole turn against the LLM endpoint independently of
// the HTTP request context. TurnTimeout re-exports it for user-facing
// timeout messages in the admin handlers.
const (
	llmTimeout  = 120 * time.Second
	TurnTimeout = llmTimeout
)

// ChatStream runs one turn: it streams assistant text through cb.OnDelta,
// streams the model's reasoning through cb.OnThinking, executes only
// whitelisted read-only tools, and returns the final text, the accumulated
// reasoning chain, and the tool audit trail. history must already be
// truncated (the handler uses MaxHistoryMessages).
func (e *Engine) ChatStream(ctx context.Context, ep store.LLMEndpoint, mdl store.LLMEndpointModel, history []*schema.Message, userText string, cb StreamCallbacks) (string, string, []ToolCallRecord, error) {
	if !ep.Enabled || ep.BaseURL == "" {
		return "", "", nil, errors.New("assistant: endpoint is not usable (disabled or missing base_url)")
	}
	if !mdl.Enabled || mdl.ProviderModelName == "" {
		return "", "", nil, errors.New("assistant: model is not usable (disabled or missing provider_model_name)")
	}
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return "", "", nil, ctx.Err()
	}
	e.activeStreams.Add(1)
	defer e.activeStreams.Add(-1)

	turnCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()

	rec := &recorder{}
	observer := newTurnObserver(cb)
	tools, err := e.buildTools(rec, cb)
	if err != nil {
		return "", "", nil, fmt.Errorf("assistant: build tools: %w", err)
	}

	cmConfig := &openai.ChatModelConfig{
		BaseURL: ep.BaseURL,
		APIKey:  ep.APIKey,
		Model:   mdl.ProviderModelName,
		Timeout: llmTimeout,
	}
	if mdl.MaxTokens != nil {
		cmConfig.MaxTokens = mdl.MaxTokens
	}
	if mdl.Temperature != nil {
		cmConfig.Temperature = mdl.Temperature
	}
	cm, err := openai.NewChatModel(turnCtx, cmConfig)
	if err != nil {
		return "", "", nil, fmt.Errorf("assistant: chat model: %w", err)
	}

	prompt := e.SystemPrompt()
	chatAgent, err := react.NewAgent(turnCtx, &react.AgentConfig{
		ToolCallingModel: cm,
		ToolsConfig:      compose.ToolsNodeConfig{Tools: tools},
		MaxStep:          8,
		MessageModifier: func(_ context.Context, input []*schema.Message) []*schema.Message {
			out := make([]*schema.Message, 0, len(input)+1)
			out = append(out, schema.SystemMessage(prompt))
			return append(out, input...)
		},
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("assistant: agent: %w", err)
	}

	input := make([]*schema.Message, 0, len(history)+1)
	input = append(input, history...)
	input = append(input, schema.UserMessage(userText))

	// The react graph routes intermediate model streams (tool-call args,
	// pre-tool reasoning) to the tools node, so the agent output stream only
	// carries the FINAL answer. The callback observer sees every model step
	// and feeds thinking deltas + tool-start events from there.
	stream, err := chatAgent.Stream(turnCtx, input, agent.WithComposeOptions(compose.WithCallbacks(observer.handler())))
	if err != nil {
		return "", "", nil, fmt.Errorf("assistant: stream: %w", err)
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break // clean end of stream
		}
		if errors.Is(err, context.Canceled) {
			return sb.String(), observer.reasoning(), rec.snapshot(), err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return sb.String(), observer.reasoning(), rec.snapshot(), ErrTurnTimeout
		}
		if err != nil {
			if sb.Len() > 0 {
				// Partial answer: return what streamed so the caller can
				// persist it, with the error reported upstream.
				return sb.String(), observer.reasoning(), rec.snapshot(), err
			}
			return "", observer.reasoning(), rec.snapshot(), fmt.Errorf("assistant: recv: %w", err)
		}
		if chunk == nil {
			break
		}
		if chunk.Content != "" {
			sb.WriteString(chunk.Content)
			if cb.OnDelta != nil {
				cb.OnDelta(chunk.Content)
			}
		}
	}
	return sb.String(), observer.reasoning(), rec.snapshot(), nil
}

// ErrTurnTimeout marks a turn that exceeded TurnTimeout so the SSE handler
// can tell the operator exactly why the answer was cut short.
var ErrTurnTimeout = errors.New("assistant: turn timed out")

// turnObserver accumulates reasoning deltas and tool-start events from every
// chat-model step via the eino callbacks system. Callbacks run on separate
// goroutines while the output stream is consumed elsewhere, so state is
// mutex-guarded.
type turnObserver struct {
	mu          sync.Mutex
	reasoningSB strings.Builder
	seenTools   map[string]bool
	cb          StreamCallbacks
}

func newTurnObserver(cb StreamCallbacks) *turnObserver {
	return &turnObserver{seenTools: make(map[string]bool), cb: cb}
}

func (o *turnObserver) reasoning() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reasoningSB.String()
}

// handler builds the eino callbacks handler. OnEndWithStreamOutput fires once
// per chat-model step with a copy of that step's output stream; the copy MUST
// be closed after reading. Items are the component-agnostic CallbackOutput,
// converted via model.ConvCallbackOutput.
func (o *turnObserver) handler() callbacks.Handler {
	hb := callbacks.NewHandlerBuilder()
	hb.OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, stream *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
		defer stream.Close()
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return ctx
			}
			out := model.ConvCallbackOutput(chunk)
			if out == nil || out.Message == nil {
				continue
			}
			msg := out.Message
			if msg.ReasoningContent != "" {
				o.mu.Lock()
				o.reasoningSB.WriteString(msg.ReasoningContent)
				o.mu.Unlock()
				if o.cb.OnThinking != nil {
					o.cb.OnThinking(msg.ReasoningContent)
				}
			}
			for _, call := range msg.ToolCalls {
				name := call.Function.Name
				if name == "" {
					continue
				}
				o.mu.Lock()
				first := !o.seenTools[name]
				o.seenTools[name] = true
				o.mu.Unlock()
				if first && o.cb.OnToolStart != nil {
					o.cb.OnToolStart(name)
				}
			}
		}
		return ctx
	})
	return hb.Build()
}

// buildTools instantiates the read-only whitelist with recording closures.
// This function is the single registration point: a tool exists for the
// model if and only if it appears here.
func (e *Engine) buildTools(rec *recorder, cb StreamCallbacks) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool

	// recorded wraps a read-only implementation so every invocation lands in
	// the audit trail (and the live SSE event) with its produced input,
	// outcome and duration.
	recorded := func(name string, args any, err error, start time.Time) {
		tc := mkRec(name, args, err, start)
		rec.add(tc)
		if cb.OnTool != nil {
			cb.OnTool(tc)
		}
	}

	t, err := toolutils.InferTool("policy_simulate",
		"Evaluate which route and policy decision the gateway would produce right now for a caller+method+path, using the live config snapshot. Use this to explain 403/deny/route_not_found outcomes.",
		func(ctx context.Context, in policySimulateIn) (map[string]any, error) {
			start := time.Now()
			out, err := e.exec.simulatePolicy(ctx, in)
			recorded("policy_simulate", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool policy_simulate: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("list_routes",
		"List the gateway's currently loaded routes with path prefixes, methods, upstream targets and success-rule presence.",
		func(ctx context.Context, in routeListIn) ([]map[string]any, error) {
			start := time.Now()
			out, err := e.exec.listRoutes(ctx, in)
			recorded("list_routes", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool list_routes: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("list_policies",
		"List the currently loaded authorization policies (caller, method, path pattern, allow/deny effect, expiry).",
		func(ctx context.Context, in policyListIn) ([]map[string]any, error) {
			start := time.Now()
			out, err := e.exec.listPolicies(ctx, in)
			recorded("list_policies", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool list_policies: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("list_identities",
		"List the loaded mTLS client identities with truncated fingerprints and certificate validity windows.",
		func(ctx context.Context, in identityListIn) ([]map[string]any, error) {
			start := time.Now()
			out, err := e.exec.listIdentities(ctx, in)
			recorded("list_identities", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool list_identities: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("get_instances",
		"List gateway instances and their config-sync health (loaded revision, last sync time, staleness). Use to diagnose configuration propagation problems.",
		func(ctx context.Context, in instanceListIn) ([]map[string]any, error) {
			start := time.Now()
			out, err := e.exec.listInstances(ctx, in)
			recorded("get_instances", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool get_instances: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("query_admin_audit",
		"Query recent admin-plane audit entries (who changed which config, when). Filter by action prefix such as route., policy., identity.",
		func(ctx context.Context, in auditQueryIn) ([]map[string]any, error) {
			start := time.Now()
			out, err := e.exec.queryAdminAudit(ctx, in)
			recorded("query_admin_audit", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool query_admin_audit: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("query_alert_events",
		"Query recent alert events (rule, route, request, business outcome). Use to correlate failures with configured alerts.",
		func(ctx context.Context, in alertEventQueryIn) ([]map[string]any, error) {
			start := time.Now()
			out, err := e.exec.queryAlertEvents(ctx, in)
			recorded("query_alert_events", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool query_alert_events: %w", err)
	}
	tools = append(tools, t)

	t, err = toolutils.InferTool("query_access_logs",
		"Query the newest matching access-decision records (caller, route, decision, status, duration, business verdict) from the local access-log file sink. Newest first.",
		func(ctx context.Context, in accessLogQueryIn) (map[string]any, error) {
			start := time.Now()
			out, err := e.exec.queryAccessLogs(ctx, in)
			recorded("query_access_logs", in, err, start)
			return out, err
		})
	if err != nil {
		return nil, fmt.Errorf("assistant: tool query_access_logs: %w", err)
	}
	tools = append(tools, t)

	return tools, nil
}

// mkRec builds the audit record for one completed tool invocation.
func mkRec(name string, args any, err error, start time.Time) ToolCallRecord {
	return ToolCallRecord{Tool: name, Args: boundedJSON(args), OK: err == nil, DurationMS: time.Since(start).Milliseconds()}
}

// ToolNames lists the registered whitelist tools, used by the handler to
// report the assistant's capability set and by the whitelist guard test.
func (e *Engine) ToolNames() []string {
	// Aligned with buildTools registration order.
	return []string{
		"policy_simulate", "list_routes", "list_policies", "list_identities",
		"get_instances", "query_admin_audit", "query_alert_events", "query_access_logs",
	}
}

// SystemPrompt builds the per-turn system prompt: the read-only charter plus
// a live snapshot status digest so the model starts grounded.
func (e *Engine) SystemPrompt() string {
	var b strings.Builder
	b.WriteString(`You are the RouteGate troubleshooting assistant embedded in the gateway's admin panel.
Your job: help operators diagnose service-to-service traffic problems (decisions, policies, identity, config propagation, business verdicts) through conversation.

Hard rules:
- You can ONLY inspect. Every action available to you is read-only (snapshot reads and SELECT-style queries). You cannot and must not claim to change anything; if asked to change configuration, point the operator to the relevant panel page instead.
- Ground every claim in tool evidence and cite what you queried. If tools returned nothing, say so explicitly instead of guessing.
- Never ask for or repeat secrets, certificates, private keys or tokens; those never appear in your tool outputs by design.
- Reply in the language the user writes in. Be concise; prefer tables or short bullet lists.

Tool guidance:
- "why is this call denied / 403?" -> policy_simulate with the exact caller+method+path, then list_policies and list_routes for context.
- "is my config change live?" -> get_instances (revision + sync staleness), query_admin_audit (who changed what when).
- "what happened to request X / traffic recently?" -> query_access_logs (newest first), query_alert_events for rule firings.
- query_access_logs needs the local file sink; if its note says unavailable, say what to enable instead of treating it as "no traffic".
`)
	snap := e.exec.holder.Load()
	if snap == nil {
		b.WriteString("\nCurrent gateway state: no config snapshot loaded yet (everything is denied until the first sync succeeds).\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\nCurrent gateway state: config revision %d loaded at %s, %d routes, %d identities.\n",
		snap.Revision, snap.LoadedAt.UTC().Format(time.RFC3339), len(snap.Routes), len(snap.Identities))
	return b.String()
}
