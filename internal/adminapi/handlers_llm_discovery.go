package adminapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// handleDiscoverLLMModels probes the standard OpenAI-compatible GET /models
// endpoint and inserts only new model bindings. Newly discovered models are
// enabled but never become the global default automatically.
func (s *Server) handleDiscoverLLMModels(w http.ResponseWriter, r *http.Request) {
	endpointID, _ := parsePathID(r, "endpoint_id")
	endpoint, err := llmEndpointByID(r, s.db, endpointID)
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	if !endpoint.Enabled {
		errorJSON(r, w, http.StatusBadRequest, "admin.llm_endpoint_disabled", "LLM endpoint is not enabled")
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(endpoint.BaseURL, "/")+"/models", nil)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_llm_endpoint", "invalid endpoint configuration")
		return
	}
	request.Header.Set("Accept", "application/json")
	if endpoint.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		log.Printf("admin llm model discovery failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "the model discovery request failed"})
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "the model discovery response could not be read"})
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": fmt.Sprintf("model discovery returned HTTP %d", response.StatusCode)})
		return
	}
	var payload openAIModelsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "model discovery returned invalid JSON"})
		return
	}
	rawIDs := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		rawIDs = append(rawIDs, item.ID)
	}
	models := store.NormalizeDiscoveredModels(rawIDs)
	added := 0
	uid, _ := r.Context().Value(ctxUserID).(int64)
	actor, _ := r.Context().Value(ctxUser).(string)
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "llm.models_discovered", "llm_endpoint", fmt.Sprintf("%d", endpointID), 0,
		map[string]any{"endpoint_id": endpointID}, map[string]any{"count": len(models)}, func(tx *sql.Tx) error {
			var insertErr error
			added, insertErr = store.InsertDiscoveredModels(r.Context(), tx, endpointID, models)
			return insertErr
		})
	if err != nil {
		writeLLMTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "latency_ms": time.Since(started).Milliseconds(),
		"count": len(models), "added": added, "existing": len(models) - added,
		"models": models,
	})
}

// parsePathID keeps path parsing consistent with the other admin handlers
// while returning zero for malformed values (the store lookup maps it to 404).
func parsePathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}
