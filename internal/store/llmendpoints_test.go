package store

import (
	"strings"
	"testing"
)

func TestValidateLLMEndpoint(t *testing.T) {
	valid := LLMEndpoint{Name: "prod", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-1"}
	if err := ValidateLLMEndpoint(valid); err != nil {
		t.Fatalf("valid endpoint rejected: %v", err)
	}
	cases := []LLMEndpoint{
		{Name: "", BaseURL: "https://x"},
		{Name: strings.Repeat("n", 65), BaseURL: "https://x"},
		{Name: "x", BaseURL: "ftp://x"},
		{Name: "x", BaseURL: "/relative"},
		{Name: "x", BaseURL: "https://user:pass@x"},
		{Name: "x", BaseURL: "https://x/v1?key=secret"},
		{Name: "x", BaseURL: "https://x/v1#fragment"},
		{Name: "x", BaseURL: "https://x", APIKey: strings.Repeat("k", 513)},
	}
	for i, c := range cases {
		if err := ValidateLLMEndpoint(c); err == nil {
			t.Errorf("case %d must be rejected: %+v", i, c)
		}
	}
}

func TestValidateLLMEndpointModel(t *testing.T) {
	valid := LLMEndpointModel{EndpointID: 1, ProviderModelName: "deepseek-chat", DisplayName: "DeepSeek Chat", Enabled: true}
	if err := ValidateLLMEndpointModel(valid); err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
	bad := []LLMEndpointModel{
		{ProviderModelName: "m", DisplayName: "M"},
		{EndpointID: 1, DisplayName: "M"},
		{EndpointID: 1, ProviderModelName: "m"},
		{EndpointID: 1, ProviderModelName: "m", DisplayName: "M", Temperature: ptrFloat32(-1)},
	}
	for i, c := range bad {
		if err := ValidateLLMEndpointModel(c); err == nil {
			t.Errorf("model case %d must be rejected: %+v", i, c)
		}
	}
}

func ptrFloat32(v float32) *float32 { return &v }
