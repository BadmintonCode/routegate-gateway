package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// mock-llm is a deterministic OpenAI Chat Completions-compatible endpoint for
// the quickstart demo. It deliberately has no credentials or external
// dependencies: the first streamed request that advertises tools calls the
// read-only list_routes tool, and the follow-up request returns a fixed
// troubleshooting answer. It is for local verification only.
type chatRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []chatMessage     `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

type chatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
}

type choice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/chat/completions", handleChat)
	addr := os.Getenv("MOCK_LLM_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("mock OpenAI endpoint listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]string{
		{"id": "routegate-demo-chat", "object": "model", "owned_by": "routegate"},
		{"id": "routegate-demo-reasoner", "object": "model", "owned_by": "routegate"},
	}})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	model := req.Model
	if model == "" {
		model = "routegate-demo"
	}
	toolRound := len(req.Tools) > 0 && !hasToolMessage(req.Messages)
	if !req.Stream {
		if toolRound {
			writeJSON(w, chatResponseForTool(model))
			return
		}
		writeJSON(w, chatResponseForText(model, "Demo mock found the request path and gateway state. Configure a real OpenAI-compatible endpoint for production analysis."))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if toolRound {
		writeChunk(w, flusher, map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"index": 0, "id": "demo-call-1", "type": "function",
				"function": map[string]any{"name": "list_routes", "arguments": "{}"},
			}},
		}, nil, model)
		writeChunk(w, flusher, map[string]any{}, "tool_calls", model)
		writeSSELine(w, flusher, "[DONE]")
		return
	}

	answer := "Demo mock found the request path and gateway state. Configure a real OpenAI-compatible endpoint for production analysis."
	for _, part := range []string{answer[:len(answer)/2], answer[len(answer)/2:]} {
		writeChunk(w, flusher, map[string]any{"role": "assistant", "content": part}, nil, model)
	}
	writeChunk(w, flusher, map[string]any{}, "stop", model)
	writeSSELine(w, flusher, "[DONE]")
}

func hasToolMessage(messages []chatMessage) bool {
	for _, message := range messages {
		if message.Role == "tool" {
			return true
		}
	}
	return false
}

func chatResponseForTool(model string) chatResponse {
	return chatResponse{
		ID: "demo-chatcmpl-tool", Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []choice{{Index: 0, Message: chatMessage{
			Role:      "assistant",
			ToolCalls: []toolCall{{ID: "demo-call-1", Type: "function", Function: toolFunction{Name: "list_routes", Arguments: "{}"}}},
		}, FinishReason: "tool_calls"}},
	}
}

func chatResponseForText(model, text string) chatResponse {
	return chatResponse{
		ID: "demo-chatcmpl-text", Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []choice{{Index: 0, Message: chatMessage{Role: "assistant", Content: text}, FinishReason: "stop"}},
	}
}

func writeJSON(w http.ResponseWriter, response chatResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func writeChunk(w http.ResponseWriter, f http.Flusher, delta map[string]any, finish any, model string) {
	chunk := map[string]any{
		"id": "demo-chatcmpl-stream", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	b, _ := json.Marshal(chunk)
	writeSSELine(w, f, string(b))
}

func writeSSELine(w http.ResponseWriter, f http.Flusher, data string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.TrimSpace(data))
	f.Flush()
}
