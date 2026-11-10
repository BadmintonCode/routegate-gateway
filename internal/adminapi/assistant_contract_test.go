package adminapi

import (
	"net/http"
	"strings"
	"testing"
)

// --- AI assistant contracts: key masking, role gating, session isolation ---

// TestLLMEndpointListNeverLeaksAPIKey: the masked key keeps only the last
// four characters; the plaintext key and its "sk-" prefix must not appear.
func TestLLMEndpointListNeverLeaksAPIKey(t *testing.T) {
	state := &fakeDBState{role: "superadmin"}
	s := newContractServer(t, state)
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/llm/endpoints", "",
		map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-super-secret-9999") || strings.Contains(body, "super-secret") {
		t.Fatalf("endpoint list leaks the plaintext API key: %s", body)
	}
	if !strings.Contains(body, `"api_key_masked":"••••9999"`) {
		t.Fatalf("masked key missing or wrong shape: %s", body)
	}
	if !strings.Contains(body, `"display_name":"DeepSeek Chat"`) {
		t.Fatalf("model binding missing: %s", body)
	}
	if !strings.Contains(body, `"has_api_key":true`) {
		t.Fatalf("has_api_key flag missing: %s", body)
	}
}

// TestLLMEndpointWriteRequiresSuperadmin: a plain admin user must get 403 on
// every endpoint mutation, including the connection test.
func TestLLMEndpointWriteRequiresSuperadmin(t *testing.T) {
	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/admin/api/v1/llm/endpoints", `{"name":"x","base_url":"https://a.example.com/v1"}`},
		{http.MethodPut, "/admin/api/v1/llm/endpoints/3", `{"enabled":false}`},
		{http.MethodDelete, "/admin/api/v1/llm/endpoints/3", `{}`},
		{http.MethodPost, "/admin/api/v1/llm/endpoints/3/models", `{"provider_model_name":"m","display_name":"M"}`},
		{http.MethodPut, "/admin/api/v1/llm/models/11", `{"enabled":false}`},
		{http.MethodDelete, "/admin/api/v1/llm/models/11", `{}`},
		{http.MethodPost, "/admin/api/v1/llm/models/11/test", `{}`},
	}
	for _, c := range cases {
		state := &fakeDBState{role: "user"}
		s := newContractServer(t, state)
		rec := doRequest(s.Handler(nil), c.method, c.path, c.body, map[string]string{"Cookie": sessionCookie})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as plain user: status = %d, want 403; body=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// TestAssistantSessionIsolation: a non-owner plain user cannot see or delete
// another user's thread — the 404 hides its existence entirely.
func TestAssistantSessionIsolation(t *testing.T) {
	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/api/v1/assistant/sessions/as-abc/messages"},
		{http.MethodDelete, "/admin/api/v1/assistant/sessions/as-abc"},
	} {
		state := &fakeDBState{role: "user"}
		s := newContractServer(t, state)
		rec := doRequest(s.Handler(nil), c.method, c.path, "", map[string]string{"Cookie": sessionCookie})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as non-owner: status = %d, want 404; body=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
