package adminapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

var llmNotFound = errors.New("llm resource not found")
var llmModelNotFound = errors.New("llm model not found")

func llmEndpointAudit(e store.LLMEndpoint) store.LLMEndpoint {
	e.APIKey = store.MaskToken(e.APIKey)
	return e
}

func llmModelAudit(m store.LLMEndpointModel) store.LLMEndpointModel { return m }

func writeLLMTxError(r *http.Request, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, llmNotFound):
		errorJSON(r, w, http.StatusNotFound, "admin.llm_endpoint_not_found", "LLM endpoint not found")
	case errors.Is(err, llmModelNotFound):
		errorJSON(r, w, http.StatusNotFound, "admin.llm_model_not_found", "LLM model not found")
	case isDuplicateKey(err):
		errorJSON(r, w, http.StatusConflict, "admin.duplicate", "a resource with the same unique value already exists")
	default:
		writeTxError(r, w, err)
	}
}

func llmEndpointByID(r *http.Request, db *sql.DB, id int64) (store.LLMEndpoint, error) {
	e, err := store.LoadLLMEndpointByID(r.Context(), db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.LLMEndpoint{}, llmNotFound
	}
	return e, err
}

func llmModelByID(r *http.Request, db *sql.DB, id int64) (store.LLMModelSelection, error) {
	m, err := store.LoadLLMModelSelection(r.Context(), db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.LLMModelSelection{}, llmModelNotFound
	}
	return m, err
}

func endpointItem(e store.LLMEndpoint, role string, models []store.LLMEndpointModel) map[string]any {
	item := map[string]any{
		"id": e.ID, "name": e.Name, "enabled": e.Enabled,
		"models":     models,
		"updated_at": e.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if role == "superadmin" {
		item["base_url"] = e.BaseURL
		item["remark"] = e.Remark
		item["api_key_masked"] = store.MaskToken(e.APIKey)
		item["has_api_key"] = e.APIKey != ""
	}
	return item
}

func (s *Server) handleListLLMEndpoints(w http.ResponseWriter, r *http.Request) {
	catalog, err := store.LoadLLMEndpointCatalog(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	role, _ := r.Context().Value(ctxRole).(string)
	items := make([]map[string]any, 0, len(catalog))
	for _, row := range catalog {
		items = append(items, endpointItem(row.Endpoint, role, row.Models))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type llmEndpointCreatePayload struct {
	Name        string `json:"name"`
	Enabled     *bool  `json:"enabled"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
	Remark      string `json:"remark"`
	ExpectedRev int64  `json:"expected_revision,omitempty"`
}

type llmEndpointUpdatePayload struct {
	Name        *string `json:"name"`
	Enabled     *bool   `json:"enabled"`
	BaseURL     *string `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Remark      *string `json:"remark"`
	ExpectedRev int64   `json:"expected_revision,omitempty"`
}

func (s *Server) handleCreateLLMEndpoint(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var p llmEndpointCreatePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	e := store.LLMEndpoint{Name: strings.TrimSpace(p.Name), Enabled: p.Enabled == nil || *p.Enabled,
		BaseURL: strings.TrimSpace(p.BaseURL), APIKey: strings.TrimSpace(p.APIKey), Remark: strings.TrimSpace(p.Remark)}
	if err := store.ValidateLLMEndpoint(e); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_llm_endpoint", err.Error())
		return
	}
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.endpoint_created", "llm_endpoint", e.Name, p.ExpectedRev, nil, llmEndpointAudit(e), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `INSERT INTO llm_endpoints (name, enabled, base_url, api_key, remark) VALUES (?, ?, ?, ?, ?)`, e.Name, e.Enabled, e.BaseURL, e.APIKey, e.Remark)
		return mustAffectOne(res, err)
	})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "name": e.Name})
}

func (s *Server) handleUpdateLLMEndpoint(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var p llmEndpointUpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	current, err := llmEndpointByID(r, s.db, id)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	e := current
	if p.Name != nil {
		e.Name = strings.TrimSpace(*p.Name)
	}
	if p.Enabled != nil {
		e.Enabled = *p.Enabled
	}
	if p.BaseURL != nil {
		e.BaseURL = strings.TrimSpace(*p.BaseURL)
	}
	if p.Remark != nil {
		e.Remark = strings.TrimSpace(*p.Remark)
	}
	if strings.TrimSpace(p.APIKey) != "" {
		e.APIKey = strings.TrimSpace(p.APIKey)
	}
	if err := store.ValidateLLMEndpoint(e); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_llm_endpoint", err.Error())
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.endpoint_updated", "llm_endpoint", fmt.Sprintf("%d", id), p.ExpectedRev, llmEndpointAudit(current), llmEndpointAudit(e), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `UPDATE llm_endpoints SET name=?, enabled=?, base_url=?, api_key=?, remark=? WHERE id=?`, e.Name, e.Enabled, e.BaseURL, e.APIKey, e.Remark, id)
		return mustAffectOne(res, err)
	})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

func (s *Server) handleDeleteLLMEndpoint(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	current, err := llmEndpointByID(r, s.db, id)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.endpoint_deleted", "llm_endpoint", fmt.Sprintf("%d", id), 0, llmEndpointAudit(current), nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `UPDATE assistant_sessions s JOIN llm_endpoint_models m ON m.id = s.model_id SET s.model_id = 0 WHERE m.endpoint_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM llm_endpoint_models WHERE endpoint_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `DELETE FROM llm_endpoints WHERE id=?`, id)
		return mustAffectOne(res, err)
	})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

type llmModelCreatePayload struct {
	ProviderModelName string   `json:"provider_model_name"`
	DisplayName       string   `json:"display_name"`
	Enabled           *bool    `json:"enabled"`
	IsDefault         bool     `json:"is_default"`
	MaxTokens         *int     `json:"max_tokens"`
	Temperature       *float32 `json:"temperature"`
	Remark            string   `json:"remark"`
	ExpectedRev       int64    `json:"expected_revision,omitempty"`
}

type llmModelUpdatePayload struct {
	ProviderModelName *string  `json:"provider_model_name"`
	DisplayName       *string  `json:"display_name"`
	Enabled           *bool    `json:"enabled"`
	IsDefault         *bool    `json:"is_default"`
	MaxTokens         *int     `json:"max_tokens"`
	Temperature       *float32 `json:"temperature"`
	Remark            *string  `json:"remark"`
	ExpectedRev       int64    `json:"expected_revision,omitempty"`
}

func (s *Server) handleCreateLLMModel(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	endpointID, _ := strconv.ParseInt(r.PathValue("endpoint_id"), 10, 64)
	var p llmModelCreatePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	endpoint, err := llmEndpointByID(r, s.db, endpointID)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	m := store.LLMEndpointModel{EndpointID: endpointID, ProviderModelName: strings.TrimSpace(p.ProviderModelName), DisplayName: strings.TrimSpace(p.DisplayName), Enabled: p.Enabled == nil || *p.Enabled, IsDefault: p.IsDefault && (p.Enabled == nil || *p.Enabled) && endpoint.Enabled, MaxTokens: p.MaxTokens, Temperature: p.Temperature, Remark: strings.TrimSpace(p.Remark)}
	if err := store.ValidateLLMEndpointModel(m); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_llm_model", err.Error())
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.model_created", "llm_model", m.ProviderModelName, p.ExpectedRev, nil, llmModelAudit(m), func(tx *sql.Tx) error {
		if m.IsDefault {
			if _, err := tx.ExecContext(r.Context(), `UPDATE llm_endpoint_models SET is_default=0 WHERE is_default=1`); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(r.Context(), `INSERT INTO llm_endpoint_models (endpoint_id, provider_model_name, display_name, enabled, is_default, max_tokens, temperature, remark) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.EndpointID, m.ProviderModelName, m.DisplayName, m.Enabled, m.IsDefault, m.MaxTokens, m.Temperature, m.Remark)
		return mustAffectOne(res, err)
	})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created"})
}

func (s *Server) handleUpdateLLMModel(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	selection, err := llmModelByID(r, s.db, id)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	var p llmModelUpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	m := selection.Model
	if p.ProviderModelName != nil {
		m.ProviderModelName = strings.TrimSpace(*p.ProviderModelName)
	}
	if p.DisplayName != nil {
		m.DisplayName = strings.TrimSpace(*p.DisplayName)
	}
	if p.Enabled != nil {
		m.Enabled = *p.Enabled
	}
	if p.IsDefault != nil {
		m.IsDefault = *p.IsDefault && m.Enabled
	}
	if p.MaxTokens != nil {
		m.MaxTokens = p.MaxTokens
	}
	if p.Temperature != nil {
		m.Temperature = p.Temperature
	}
	if p.Remark != nil {
		m.Remark = strings.TrimSpace(*p.Remark)
	}
	if !m.Enabled {
		m.IsDefault = false
	}
	if err := store.ValidateLLMEndpointModel(m); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_llm_model", err.Error())
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.model_updated", "llm_model", fmt.Sprintf("%d", id), p.ExpectedRev, llmModelAudit(selection.Model), llmModelAudit(m), func(tx *sql.Tx) error {
		if m.IsDefault {
			if _, err := tx.ExecContext(r.Context(), `UPDATE llm_endpoint_models SET is_default=0 WHERE is_default=1`); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(r.Context(), `UPDATE llm_endpoint_models SET provider_model_name=?, display_name=?, enabled=?, is_default=?, max_tokens=?, temperature=?, remark=? WHERE id=?`, m.ProviderModelName, m.DisplayName, m.Enabled, m.IsDefault, m.MaxTokens, m.Temperature, m.Remark, id)
		return mustAffectOne(res, err)
	})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

func (s *Server) handleDeleteLLMModel(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	selection, err := llmModelByID(r, s.db, id)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.model_deleted", "llm_model", fmt.Sprintf("%d", id), 0, llmModelAudit(selection.Model), nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `UPDATE assistant_sessions SET model_id=0 WHERE model_id=?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `DELETE FROM llm_endpoint_models WHERE id=?`, id)
		return mustAffectOne(res, err)
	})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (s *Server) handleTestLLMModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	selection, err := llmModelByID(r, s.db, id)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	if !selection.Endpoint.Enabled || !selection.Model.Enabled {
		errorJSON(r, w, http.StatusBadRequest, "admin.llm_model_disabled", "LLM endpoint or model is not enabled")
		return
	}
	body, _ := json.Marshal(map[string]any{"model": selection.Model.ProviderModelName, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "max_tokens": 8})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(selection.Endpoint.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "invalid endpoint configuration"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if selection.Endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+selection.Endpoint.APIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("admin llm model test failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "the chat completion request failed"})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	writeJSON(w, http.StatusOK, map[string]any{"ok": resp.StatusCode >= 200 && resp.StatusCode < 300, "status": resp.StatusCode, "latency_ms": time.Since(start).Milliseconds(), "response": strings.TrimSpace(string(respBody))})
}
