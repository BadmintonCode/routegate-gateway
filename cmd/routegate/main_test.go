package main

import (
	"strings"
	"testing"
)

func TestVersionNonEmpty(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty")
	}
}

func TestUsageMentionsSubcommands(t *testing.T) {
	for _, want := range []string{"serve", "migrate", "config bump", "version"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
}
