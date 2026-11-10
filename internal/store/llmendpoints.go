package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// LLMEndpoint stores only connection configuration. Models are separate rows
// in llm_endpoint_models so one provider endpoint can expose many models.
type LLMEndpoint struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	BaseURL   string    `json:"base_url,omitempty"`
	APIKey    string    `json:"api_key,omitempty"`
	Remark    string    `json:"remark,omitempty"`
	UpdatedAt time.Time `json:"-"`
}

// LLMEndpointModel is one concrete model exposed by an endpoint.
type LLMEndpointModel struct {
	ID                int64     `json:"id"`
	EndpointID        int64     `json:"endpoint_id"`
	ProviderModelName string    `json:"provider_model_name"`
	DisplayName       string    `json:"display_name"`
	Enabled           bool      `json:"enabled"`
	IsDefault         bool      `json:"is_default"`
	MaxTokens         *int      `json:"max_tokens,omitempty"`
	Temperature       *float32  `json:"temperature,omitempty"`
	Remark            string    `json:"remark,omitempty"`
	UpdatedAt         time.Time `json:"-"`
}

// LLMEndpointWithModels is the safe catalog shape used by the admin API.
type LLMEndpointWithModels struct {
	Endpoint LLMEndpoint
	Models   []LLMEndpointModel
}

// LLMModelSelection combines the connection and concrete model selected for a
// chat turn.
type LLMModelSelection struct {
	Endpoint LLMEndpoint
	Model    LLMEndpointModel
}

func LoadLLMEndpoints(ctx context.Context, db *sql.DB) ([]LLMEndpoint, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, enabled, base_url, api_key, remark, updated_at
		 FROM llm_endpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMEndpoint
	for rows.Next() {
		var e LLMEndpoint
		var updated sql.NullTime
		if err := rows.Scan(&e.ID, &e.Name, &e.Enabled, &e.BaseURL, &e.APIKey, &e.Remark, &updated); err != nil {
			continue
		}
		if updated.Valid {
			e.UpdatedAt = updated.Time
		}
		out = append(out, sanitizeLLMEndpoint(e))
	}
	return out, rows.Err()
}

func LoadLLMEndpointByID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (LLMEndpoint, error) {
	var e LLMEndpoint
	var updated sql.NullTime
	err := q.QueryRowContext(ctx,
		`SELECT id, name, enabled, base_url, api_key, remark, updated_at
		 FROM llm_endpoints WHERE id = ?`, id).
		Scan(&e.ID, &e.Name, &e.Enabled, &e.BaseURL, &e.APIKey, &e.Remark, &updated)
	if err != nil {
		return LLMEndpoint{}, err
	}
	if updated.Valid {
		e.UpdatedAt = updated.Time
	}
	return sanitizeLLMEndpoint(e), nil
}

type DiscoveredModel struct {
	ProviderModelName string `json:"provider_model_name"`
	DisplayName       string `json:"display_name"`
}

func NormalizeDiscoveredModels(ids []string) []DiscoveredModel {
	seen := make(map[string]struct{}, len(ids))
	out := make([]DiscoveredModel, 0, len(ids))
	for _, raw := range ids {
		name := strings.TrimSpace(raw)
		if name == "" || len(name) > 128 {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, DiscoveredModel{ProviderModelName: name, DisplayName: name})
	}
	return out
}

func LoadLLMEndpointModels(ctx context.Context, db *sql.DB, endpointID int64) ([]LLMEndpointModel, error) {
	rows, err := db.QueryContext(ctx, modelSelect+` WHERE endpoint_id = ? ORDER BY display_name, id`, endpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLLMModels(rows)
}

func LoadLLMEndpointCatalog(ctx context.Context, db *sql.DB) ([]LLMEndpointWithModels, error) {
	endpoints, err := LoadLLMEndpoints(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]LLMEndpointWithModels, 0, len(endpoints))
	for _, endpoint := range endpoints {
		models, err := LoadLLMEndpointModels(ctx, db, endpoint.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, LLMEndpointWithModels{Endpoint: endpoint, Models: models})
	}
	return out, nil
}

// LoadLLMModelSelection reads a model together with its endpoint and does not
// apply enabled filtering; callers can return a precise disabled/not-found
// error instead of silently switching models.
func LoadLLMModelSelection(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, modelID int64) (LLMModelSelection, error) {
	var selection LLMModelSelection
	var endpointUpdated, modelUpdated sql.NullTime
	var maxTokens sql.NullInt64
	var temperature sql.NullFloat64
	err := q.QueryRowContext(ctx, `
		SELECT e.id, e.name, e.enabled, e.base_url, e.api_key, e.remark, e.updated_at,
		       m.id, m.endpoint_id, m.provider_model_name, m.display_name, m.enabled,
		       m.is_default, m.max_tokens, m.temperature, m.remark, m.updated_at
		FROM llm_endpoint_models m
		JOIN llm_endpoints e ON e.id = m.endpoint_id
		WHERE m.id = ?`, modelID).Scan(
		&selection.Endpoint.ID, &selection.Endpoint.Name, &selection.Endpoint.Enabled,
		&selection.Endpoint.BaseURL, &selection.Endpoint.APIKey, &selection.Endpoint.Remark, &endpointUpdated,
		&selection.Model.ID, &selection.Model.EndpointID, &selection.Model.ProviderModelName,
		&selection.Model.DisplayName, &selection.Model.Enabled, &selection.Model.IsDefault,
		&maxTokens, &temperature, &selection.Model.Remark, &modelUpdated)
	if err != nil {
		return LLMModelSelection{}, err
	}
	if endpointUpdated.Valid {
		selection.Endpoint.UpdatedAt = endpointUpdated.Time
	}
	if modelUpdated.Valid {
		selection.Model.UpdatedAt = modelUpdated.Time
	}
	if maxTokens.Valid {
		v := int(maxTokens.Int64)
		selection.Model.MaxTokens = &v
	}
	if temperature.Valid {
		v := float32(temperature.Float64)
		selection.Model.Temperature = &v
	}
	selection.Endpoint = sanitizeLLMEndpoint(selection.Endpoint)
	selection.Model = sanitizeLLMEndpointModel(selection.Model)
	return selection, nil
}

// LoadDefaultLLMModel only returns an explicitly configured, enabled default.
func LoadDefaultLLMModel(ctx context.Context, db *sql.DB) (LLMModelSelection, bool, error) {
	var id int64
	err := db.QueryRowContext(ctx, `
		SELECT m.id
		FROM llm_endpoint_models m
		JOIN llm_endpoints e ON e.id = m.endpoint_id
		WHERE m.is_default = 1 AND m.enabled = 1 AND e.enabled = 1
		ORDER BY m.id LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return LLMModelSelection{}, false, nil
	}
	if err != nil {
		return LLMModelSelection{}, false, err
	}
	selection, err := LoadLLMModelSelection(ctx, db, id)
	return selection, err == nil, err
}

func ValidateLLMEndpoint(e LLMEndpoint) error {
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" || len(e.Name) > 64 {
		return fmt.Errorf("name is required (max 64 chars)")
	}
	u, err := url.Parse(strings.TrimSpace(e.BaseURL))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("base_url must be an absolute http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("base_url must not contain credentials, query parameters, or fragments")
	}
	if len(e.BaseURL) > 256 {
		return fmt.Errorf("base_url exceeds 256 chars")
	}
	if len(e.APIKey) > 512 {
		return fmt.Errorf("api_key exceeds 512 chars")
	}
	if len(e.Remark) > 256 {
		return fmt.Errorf("remark exceeds 256 chars")
	}
	return nil
}

func ValidateLLMEndpointModel(m LLMEndpointModel) error {
	if m.EndpointID <= 0 {
		return fmt.Errorf("endpoint_id is required")
	}
	m.ProviderModelName = strings.TrimSpace(m.ProviderModelName)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	if m.ProviderModelName == "" || len(m.ProviderModelName) > 128 {
		return fmt.Errorf("provider_model_name is required (max 128 chars)")
	}
	if m.DisplayName == "" || len(m.DisplayName) > 128 {
		return fmt.Errorf("display_name is required (max 128 chars)")
	}
	if m.MaxTokens != nil && (*m.MaxTokens <= 0 || *m.MaxTokens > 1000000) {
		return fmt.Errorf("max_tokens must be between 1 and 1000000")
	}
	if m.Temperature != nil && (*m.Temperature < 0 || *m.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if len(m.Remark) > 256 {
		return fmt.Errorf("remark exceeds 256 chars")
	}
	return nil
}

func sanitizeLLMEndpoint(e LLMEndpoint) LLMEndpoint {
	e.Name = strings.TrimSpace(e.Name)
	e.BaseURL = strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	e.Remark = strings.TrimSpace(e.Remark)
	return e
}

func sanitizeLLMEndpointModel(m LLMEndpointModel) LLMEndpointModel {
	m.ProviderModelName = strings.TrimSpace(m.ProviderModelName)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	m.Remark = strings.TrimSpace(m.Remark)
	return m
}

const modelSelect = `SELECT id, endpoint_id, provider_model_name, display_name, enabled,
	is_default, max_tokens, temperature, remark, updated_at FROM llm_endpoint_models`

func InsertDiscoveredModels(ctx context.Context, tx *sql.Tx, endpointID int64, models []DiscoveredModel) (int, error) {
	added := 0
	for _, model := range models {
		res, err := tx.ExecContext(ctx, `INSERT INTO llm_endpoint_models (endpoint_id, provider_model_name, display_name, enabled, is_default) VALUES (?, ?, ?, 1, 0) ON DUPLICATE KEY UPDATE id=id`, endpointID, model.ProviderModelName, model.DisplayName)
		if err != nil {
			return added, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return added, err
		}
		if n == 1 {
			added++
		}
	}
	return added, nil
}

func scanLLMModels(rows *sql.Rows) ([]LLMEndpointModel, error) {
	var out []LLMEndpointModel
	for rows.Next() {
		var m LLMEndpointModel
		var updated sql.NullTime
		var maxTokens sql.NullInt64
		var temperature sql.NullFloat64
		if err := rows.Scan(&m.ID, &m.EndpointID, &m.ProviderModelName, &m.DisplayName,
			&m.Enabled, &m.IsDefault, &maxTokens, &temperature, &m.Remark, &updated); err != nil {
			continue
		}
		if maxTokens.Valid {
			v := int(maxTokens.Int64)
			m.MaxTokens = &v
		}
		if temperature.Valid {
			v := float32(temperature.Float64)
			m.Temperature = &v
		}
		if updated.Valid {
			m.UpdatedAt = updated.Time
		}
		out = append(out, sanitizeLLMEndpointModel(m))
	}
	return out, rows.Err()
}
