package store

import "testing"

func TestNormalizeDiscoveredModels(t *testing.T) {
	got := NormalizeDiscoveredModels([]string{" deepseek-chat ", "", "deepseek-chat", "qwen-max"})
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got), got)
	}
	if got[0].ProviderModelName != "deepseek-chat" || got[1].ProviderModelName != "qwen-max" {
		t.Fatalf("unexpected models: %+v", got)
	}
}
