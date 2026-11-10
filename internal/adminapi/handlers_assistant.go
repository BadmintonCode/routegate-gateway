package adminapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BadmintonCode/routegate-gateway/internal/assistant"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
	"github.com/cloudwego/eino/schema"
)

var assistantNotFound = errors.New("assistant session not found")
var errNoLLMModel = errors.New("no enabled default llm model")
var errLLMModelDisabled = errors.New("llm model is disabled")

const (
	maxChatContent = 8000
	maxTitleRunes  = 60
)

func assistantSessionFor(r *http.Request, db *sql.DB, key string) (store.AssistantSession, error) {
	sess, err := store.LoadAssistantSessionByKey(r.Context(), db, key)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AssistantSession{}, assistantNotFound
	}
	return sess, err
}

func writeAssistantError(r *http.Request, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, assistantNotFound):
		errorJSON(r, w, http.StatusNotFound, "admin.assistant_session_not_found", "assistant session not found")
	case errors.Is(err, errNoLLMModel):
		errorJSON(r, w, http.StatusBadRequest, "admin.no_llm_model", "no enabled default LLM model is configured; add one under AI Agent → 模型")
	case errors.Is(err, errLLMModelDisabled):
		errorJSON(r, w, http.StatusBadRequest, "admin.llm_model_disabled", "the selected LLM model or endpoint is disabled")
	default:
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
	}
}

func canAccessSession(sess store.AssistantSession, uid int64, role string) bool {
	return role == "superadmin" || (uid != 0 && sess.UserID == uid)
}

func (s *Server) handleCreateAssistantSession(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req struct {
		Title   string `json:"title"`
		ModelID int64  `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if req.ModelID > 0 {
		if _, err := s.resolveModel(r, req.ModelID); err != nil {
			writeAssistantError(r, w, err)
			return
		}
	}
	title := oneLine(req.Title, maxTitleRunes)
	key := "as-" + newRequestID()
	id, err := store.CreateAssistantSession(r.Context(), s.db, store.AssistantSession{SessionKey: key, UserID: uid, UserName: actor, Title: title, ModelID: req.ModelID})
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "id": id, "session_key": key, "title": title, "model_id": req.ModelID})
}

func (s *Server) handleListAssistantSessions(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	sessions, err := store.ListAssistantSessions(r.Context(), s.db, uid, 200)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		items = append(items, map[string]any{"session_key": sess.SessionKey, "title": sess.Title, "model_id": sess.ModelID, "updated_at": sess.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleUpdateAssistantSession(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	role, _ := r.Context().Value(ctxRole).(string)
	var req struct {
		Title   string `json:"title"`
		ModelID *int64 `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	sess, err := assistantSessionFor(r, s.db, r.PathValue("key"))
	if err != nil {
		writeAssistantError(r, w, err)
		return
	}
	if !canAccessSession(sess, uid, role) {
		errorJSON(r, w, http.StatusNotFound, "admin.assistant_session_not_found", "assistant session not found")
		return
	}
	title := sess.Title
	if req.Title != "" {
		title = oneLine(req.Title, maxTitleRunes)
	}
	modelID := sess.ModelID
	if req.ModelID != nil {
		modelID = *req.ModelID
		if modelID > 0 {
			if _, err := s.resolveModel(r, modelID); err != nil {
				writeAssistantError(r, w, err)
				return
			}
		}
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	if err := store.UpdateSessionMeta(r.Context(), tx, sess.ID, title, modelID); err != nil {
		tx.Rollback()
		writeAssistantError(r, w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "title": title, "model_id": modelID})
}

func (s *Server) handleListAssistantMessages(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	role, _ := r.Context().Value(ctxRole).(string)
	sess, err := assistantSessionFor(r, s.db, r.PathValue("key"))
	if err != nil {
		writeAssistantError(r, w, err)
		return
	}
	if !canAccessSession(sess, uid, role) {
		errorJSON(r, w, http.StatusNotFound, "admin.assistant_session_not_found", "assistant session not found")
		return
	}
	msgs, err := store.ListAssistantMessages(r.Context(), s.db, sess.ID, 0)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		item := map[string]any{"id": m.ID, "role": m.Role, "content": m.Content, "created_at": m.CreatedAt.UTC().Format(time.RFC3339)}
		if m.Reasoning != "" {
			item["reasoning"] = m.Reasoning
		}
		if len(m.ToolCalls) > 0 {
			item["tool_calls"] = json.RawMessage(m.ToolCalls)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleDeleteAssistantSession(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	role, _ := r.Context().Value(ctxRole).(string)
	sess, err := assistantSessionFor(r, s.db, r.PathValue("key"))
	if err != nil {
		writeAssistantError(r, w, err)
		return
	}
	if !canAccessSession(sess, uid, role) {
		errorJSON(r, w, http.StatusNotFound, "admin.assistant_session_not_found", "assistant session not found")
		return
	}
	if err := store.DeleteAssistantSession(r.Context(), s.db, sess.ID); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

type chatRequest struct {
	SessionKey string `json:"session_key"`
	Content    string `json:"content"`
	ModelID    int64  `json:"model_id"`
}

func (s *Server) handleAssistantChat(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	role, _ := r.Context().Value(ctxRole).(string)
	actor := r.Context().Value(ctxUser).(string)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "content is required")
		return
	}
	if utf8.RuneCountInString(content) > maxChatContent {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", fmt.Sprintf("content exceeds %d characters", maxChatContent))
		return
	}
	sess, err := assistantSessionFor(r, s.db, req.SessionKey)
	if err != nil {
		writeAssistantError(r, w, err)
		return
	}
	if !canAccessSession(sess, uid, role) {
		errorJSON(r, w, http.StatusNotFound, "admin.assistant_session_not_found", "assistant session not found")
		return
	}
	modelID := req.ModelID
	if modelID == 0 {
		modelID = sess.ModelID
	}
	selection, err := s.resolveModel(r, modelID)
	if err != nil {
		writeAssistantError(r, w, err)
		return
	}

	history, err := store.ListAssistantMessages(r.Context(), s.db, sess.ID, assistant.MaxHistoryMessages)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	inputHistory := make([]*schema.Message, 0, len(history))
	for _, m := range history {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		msgRole := schema.User
		if m.Role == "assistant" {
			msgRole = schema.Assistant
		}
		inputHistory = append(inputHistory, &schema.Message{Role: msgRole, Content: m.Content})
	}

	title := sess.Title
	if title == "" {
		title = oneLine(content, maxTitleRunes)
	}
	if title != sess.Title {
		if tx, txErr := s.db.BeginTx(r.Context(), nil); txErr == nil {
			if uErr := store.UpdateSessionMeta(r.Context(), tx, sess.ID, title, sess.ModelID); uErr == nil {
				_ = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
	}
	if _, err := store.AppendAssistantMessage(r.Context(), s.db, sess.ID, "user", content, "", nil); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}

	em := newSSEEmitter(w)
	if !em.send("meta", map[string]any{"session_key": sess.SessionKey, "endpoint": map[string]any{"id": selection.Endpoint.ID, "name": selection.Endpoint.Name}, "model": map[string]any{"id": selection.Model.ID, "display_name": selection.Model.DisplayName, "provider_model_name": selection.Model.ProviderModelName}}) {
		return
	}
	// Heartbeat: reasoning phases and tool execution can stay silent for a
	// while; SSE comment lines keep the connection (and any intermediary)
	// alive without touching the client-side event stream.
	stopHeartbeat := em.startHeartbeat(15 * time.Second)
	defer stopHeartbeat()

	finalText, finalReasoning, toolCalls, streamErr := s.assistant.ChatStream(r.Context(), selection.Endpoint, selection.Model, inputHistory, content, assistant.StreamCallbacks{
		OnDelta:     func(text string) { em.send("delta", map[string]any{"text": text}) },
		OnThinking:  func(text string) { em.send("thinking", map[string]any{"text": text}) },
		OnToolStart: func(name string) { em.send("tool", map[string]any{"tool": name, "phase": "start"}) },
		OnTool: func(rec assistant.ToolCallRecord) {
			em.send("tool", map[string]any{"tool": rec.Tool, "phase": "done", "args": rec.Args, "ok": rec.OK, "duration_ms": rec.DurationMS})
		},
	})

	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	persistCtx = context.WithValue(persistCtx, ctxRequestID, requestID(r))
	defer persistCancel()
	var tcJSON []byte
	if len(toolCalls) > 0 {
		tcJSON, _ = json.Marshal(toolCalls)
	}
	if _, dbErr := store.AppendAssistantMessage(persistCtx, s.db, sess.ID, "assistant", finalText, finalReasoning, tcJSON); dbErr != nil {
		log.Printf("admin api error: persist assistant message: %v", dbErr)
	}
	if touchErr := store.TouchAssistantSession(persistCtx, s.db, sess.ID); touchErr != nil {
		log.Printf("admin api error: touch assistant session: %v", touchErr)
	}
	if streamErr != nil {
		s.auditSimple(persistCtx, uid, actor, "assistant.chat", sess.SessionKey, clientIP(r))
		log.Printf("assistant chat stream failed: %v", streamErr)
		message := "the chat stream failed; the partial answer was saved"
		if errors.Is(streamErr, assistant.ErrTurnTimeout) {
			message = fmt.Sprintf("the model response timed out after %s; the partial answer was saved", assistant.TurnTimeout)
		}
		em.send("error", map[string]any{"message": message})
		em.send("done", map[string]any{"finish": "error"})
		return
	}
	s.auditSimple(persistCtx, uid, actor, "assistant.chat", sess.SessionKey, clientIP(r))
	em.send("done", map[string]any{"finish": "ok", "tool_calls": toolCalls})
}

func (s *Server) resolveModel(r *http.Request, modelID int64) (store.LLMModelSelection, error) {
	if modelID == 0 {
		selection, ok, err := store.LoadDefaultLLMModel(r.Context(), s.db)
		if err != nil {
			return store.LLMModelSelection{}, err
		}
		if !ok {
			return store.LLMModelSelection{}, errNoLLMModel
		}
		if !selection.Endpoint.Enabled || !selection.Model.Enabled {
			return store.LLMModelSelection{}, errLLMModelDisabled
		}
		return selection, nil
	}
	selection, err := store.LoadLLMModelSelection(r.Context(), s.db, modelID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.LLMModelSelection{}, llmModelNotFound
	}
	if err != nil {
		return store.LLMModelSelection{}, err
	}
	if !selection.Endpoint.Enabled || !selection.Model.Enabled {
		return store.LLMModelSelection{}, errLLMModelDisabled
	}
	return selection, nil
}

func oneLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes]) + "…"
	}
	return s
}
